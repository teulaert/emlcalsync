// Package gmail implements provider.MailProvider on top of the Gmail API
// (DESIGN.md §6.1).
//
// The Gmail data model is mapped onto the JMAP-shaped model in
// internal/model: labels become mailboxes, except STARRED/UNREAD which become
// flags. Raw RFC 822 bytes are fetched with format=raw through the batch
// endpoint (50 messages per HTTP request) and every call is metered against
// Gmail's per-user quota (6 000 units/minute per project) by a token-bucket
// limiter.
//
// Nothing here knows about the local account id: model.Mailbox.AccountID and
// the like are left empty for the sync engine to fill in.
package gmail

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"sort"
	"strconv"
	"strings"
	"sync"

	"golang.org/x/sync/errgroup"
	"golang.org/x/time/rate"
	gmailapi "google.golang.org/api/gmail/v1"
	"google.golang.org/api/option"

	"github.com/teulaert/emlcalsync/internal/model"
	"github.com/teulaert/emlcalsync/internal/provider"
)

// FetchMode selects how FetchRaw retrieves messages.
type FetchMode int

const (
	// FetchBatch uses the multipart/mixed batch endpoint: 50 messages per
	// HTTP request. This is the default and by far the cheapest in round
	// trips.
	FetchBatch FetchMode = iota
	// FetchIndividual issues one messages.get per message, bounded by
	// Concurrency. Same quota cost, more round trips; kept as an escape hatch
	// if the batch endpoint ever misbehaves for an account.
	FetchIndividual
)

// Options configures a Mail provider.
type Options struct {
	// HTTPClient is an authenticated client, typically from
	// oauth.HTTPClient. Required.
	HTTPClient *http.Client
	// Email is the account's address; used for logging only (every API call
	// addresses the "me" user).
	Email string
	// IncludeSpamTrash makes Enumerate list spam and trash too (config key
	// include_spam_trash, §11). The archive normally wants them.
	IncludeSpamTrash bool
	// Logger receives one Debug record per API call. Defaults to
	// slog.Default().
	Logger *slog.Logger
	// Concurrency bounds in-flight HTTP requests (batches, label counts,
	// trash calls). Defaults to 4, as in §7.6.
	Concurrency int
	// FetchMode selects the FetchRaw strategy. Defaults to FetchBatch.
	FetchMode FetchMode
	// QuotaUnitsPerSecond is the token-bucket refill rate in Gmail quota
	// units. Gmail allows one user 6 000 units/minute per project (100/s);
	// the default of 80 leaves headroom for other clients and for retries. A
	// messages.get costs 20 units, so the default sustains ~4 messages/s.
	QuotaUnitsPerSecond float64
	// Endpoint overrides the API base URL. It must end in "/". Tests point
	// this at an httptest server; production leaves it empty.
	Endpoint string
	// BatchEndpoint overrides the batch URL. Defaults to Google's
	// https://gmail.googleapis.com/batch/gmail/v1, or, when Endpoint is set,
	// to <Endpoint>batch/gmail/v1.
	BatchEndpoint string
}

// Mail is a Gmail-backed provider.MailProvider.
type Mail struct {
	svc      *gmailapi.Service
	hc       *http.Client
	log      *slog.Logger
	opts     Options
	limiter  *rate.Limiter
	batchURL string
	conc     int

	mu          sync.Mutex
	knownLabels map[string]struct{} // ids seen by the last Mailboxes call
}

var _ provider.MailProvider = (*Mail)(nil)

const (
	// The user this client acts as. The Gmail API's magic alias for "the
	// account the token belongs to".
	me = "me"

	// Gmail allows one user 6 000 quota units per minute per project (100/s);
	// the default leaves headroom for other clients and for retries. Projects
	// that used the API before May 2026 keep the older, roomier quota and can
	// raise this through Options.QuotaUnitsPerSecond.
	defaultUnitsPerSecond = 80.0

	maxEnumeratePage = 500
	batchSize        = 50
	// Burst covers one full batch of messages.get, which is spent in one go.
	quotaBurst = batchSize * unitsMessagesGet
	// batchModify accepts at most 1000 ids per call.
	maxModifyIDs = 1000

	// defaultBatchURL is the Gmail API's rootUrl plus the batch path
	// "/batch/<api>/<version>" documented in
	// https://developers.google.com/gmail/api/guides/batch. The discovery
	// document (https://gmail.googleapis.com/$discovery/rest?version=v1)
	// gives rootUrl "https://gmail.googleapis.com/" and batchPath "batch";
	// both /batch and /batch/gmail/v1 are served on that host, and the longer
	// one is what the guide and the client libraries use. The old global
	// endpoint on www.googleapis.com is deprecated.
	defaultBatchURL = "https://gmail.googleapis.com/batch/gmail/v1"
)

// Gmail quota unit costs, from the API's published quota table
// (https://developers.google.com/gmail/api/reference/quota, checked
// 2026-08-25).
//
// The table was reissued on 2026-05-01: reads went from 5 to 20 units and the
// per-user allowance from 15 000 to 6 000 units/minute. Cloud projects that
// called the API before then keep their old quota, so these numbers are the
// pessimistic reading — they overstate the cost for a grandfathered project,
// which only means it runs below its ceiling.
const (
	unitsLabelsList     = 1
	unitsLabelsGet      = 1
	unitsGetProfile     = 1
	unitsHistoryList    = 2
	unitsMessagesList   = 5
	unitsMessagesGet    = 20
	unitsMessagesModify = 5
	unitsBatchModify    = 50
	unitsMessagesTrash  = 20
	unitsMessagesSend   = 100
	unitsDraftsCreate   = 10
	unitsAttachmentGet  = 20
)

// New builds a Mail provider. The HTTP client must already carry credentials.
func New(ctx context.Context, opts Options) (*Mail, error) {
	if opts.HTTPClient == nil {
		return nil, errors.New("gmail: Options.HTTPClient is required")
	}
	log := opts.Logger
	if log == nil {
		log = slog.Default()
	}
	log = log.With("provider", "gmail", "email", opts.Email)

	apiOpts := []option.ClientOption{option.WithHTTPClient(opts.HTTPClient)}
	if opts.Endpoint != "" {
		apiOpts = append(apiOpts, option.WithEndpoint(opts.Endpoint))
	}
	svc, err := gmailapi.NewService(ctx, apiOpts...)
	if err != nil {
		return nil, fmt.Errorf("gmail: new service: %w", err)
	}

	units := opts.QuotaUnitsPerSecond
	if units <= 0 {
		units = defaultUnitsPerSecond
	}
	conc := opts.Concurrency
	if conc <= 0 {
		conc = 4
	}

	batchURL := opts.BatchEndpoint
	if batchURL == "" {
		if opts.Endpoint != "" {
			batchURL = strings.TrimSuffix(opts.Endpoint, "/") + "/batch/gmail/v1"
		} else {
			batchURL = defaultBatchURL
		}
	}

	return &Mail{
		svc:      svc,
		hc:       opts.HTTPClient,
		log:      log,
		opts:     opts,
		limiter:  rate.NewLimiter(rate.Limit(units), quotaBurst),
		batchURL: batchURL,
		conc:     conc,
	}, nil
}

// ---------------------------------------------------------------------------
// Mailboxes

type systemLabel struct {
	role  model.MailboxRole
	name  string
	order int
}

// systemLabels maps Gmail's system labels onto mailbox roles. STARRED and
// UNREAD are deliberately absent: they are flags, not mailboxes (see
// labelsToFlags). CHAT has no role but is a real mailbox.
var systemLabels = map[string]systemLabel{
	"INBOX":               {model.RoleInbox, "Inbox", 0},
	"SENT":                {model.RoleSent, "Sent", 10},
	"DRAFT":               {model.RoleDrafts, "Drafts", 20},
	"TRASH":               {model.RoleTrash, "Trash", 30},
	"SPAM":                {model.RoleJunk, "Spam", 40},
	"IMPORTANT":           {model.RoleImportant, "Important", 50},
	"CATEGORY_PERSONAL":   {"category:personal", "Personal", 60},
	"CATEGORY_SOCIAL":     {"category:social", "Social", 61},
	"CATEGORY_PROMOTIONS": {"category:promotions", "Promotions", 62},
	"CATEGORY_UPDATES":    {"category:updates", "Updates", 63},
	"CATEGORY_FORUMS":     {"category:forums", "Forums", 64},
	"CHAT":                {"", "Chat", 90},
}

// flagLabels are the labels that map to model.Flags instead of mailboxes.
var flagLabels = map[string]bool{"STARRED": true, "UNREAD": true}

// maxLabelCountFetches caps how many extra labels.get calls Mailboxes makes to
// fill in message counts. Beyond this, user label counts are left at zero: the
// local index knows them anyway once messages are stored.
const maxLabelCountFetches = 50

// Mailboxes returns Gmail's labels mapped onto model.Mailbox.
func (m *Mail) Mailboxes(ctx context.Context) ([]model.Mailbox, error) {
	var resp *gmailapi.ListLabelsResponse
	err := m.do(ctx, "labels.list", unitsLabelsList, func() error {
		var err error
		resp, err = m.svc.Users.Labels.List(me).Context(ctx).Do()
		return err
	})
	if err != nil {
		return nil, err
	}

	byName := make(map[string]string, len(resp.Labels)) // full name -> id
	for _, l := range resp.Labels {
		byName[l.Name] = l.Id
	}

	var out []model.Mailbox
	known := make(map[string]struct{}, len(resp.Labels))
	userCount := 0
	for _, l := range resp.Labels {
		known[l.Id] = struct{}{}
		if flagLabels[l.Id] {
			continue
		}
		mb := model.Mailbox{RemoteID: l.Id, Name: l.Name}
		if sys, ok := systemLabels[l.Id]; ok {
			mb.Role = sys.role
			mb.Name = sys.name
			mb.SortOrder = sys.order
		} else if l.Type == "system" {
			// An unknown system label: keep it, but it has no role.
			mb.SortOrder = 95
		} else {
			userCount++
			mb.SortOrder = 100
			// Gmail nests user labels by name: "clients/acme/2026".
			if i := strings.LastIndex(l.Name, "/"); i > 0 {
				if parentID, ok := byName[l.Name[:i]]; ok {
					mb.ParentRemote = parentID
					mb.Name = l.Name[i+1:]
				}
			}
		}
		out = append(out, mb)
	}

	sort.SliceStable(out, func(i, j int) bool {
		if out[i].SortOrder != out[j].SortOrder {
			return out[i].SortOrder < out[j].SortOrder
		}
		return out[i].Name < out[j].Name
	})

	m.mu.Lock()
	m.knownLabels = known
	m.mu.Unlock()

	m.fillCounts(ctx, out, userCount)
	return out, nil
}

// fillCounts fetches messagesTotal/messagesUnread with concurrent labels.get
// calls. System labels are always fetched (there are about a dozen); user
// labels only while there are few enough to be cheap.
func (m *Mail) fillCounts(ctx context.Context, boxes []model.Mailbox, userCount int) {
	withUsers := userCount <= maxLabelCountFetches
	if !withUsers {
		m.log.Debug("skipping user label counts", "user_labels", userCount, "limit", maxLabelCountFetches)
	}

	g, gctx := errgroup.WithContext(ctx)
	g.SetLimit(m.conc)
	for i := range boxes {
		mb := &boxes[i]
		_, isSystem := systemLabels[mb.RemoteID]
		if !isSystem && !withUsers {
			continue
		}
		g.Go(func() error {
			var l *gmailapi.Label
			err := m.do(gctx, "labels.get", unitsLabelsGet, func() error {
				var err error
				l, err = m.svc.Users.Labels.Get(me, mb.RemoteID).Context(gctx).Do()
				return err
			})
			if err != nil {
				// Counts are a nicety; never fail Mailboxes over them.
				m.log.Debug("label counts unavailable", "label", mb.RemoteID, "err", err)
				return nil
			}
			mb.TotalCount = int(l.MessagesTotal)
			mb.UnreadCount = int(l.MessagesUnread)
			return nil
		})
	}
	_ = g.Wait()
}

// ---------------------------------------------------------------------------
// State and enumeration

// State returns the mailbox's current historyId, the token Changes deltas
// from.
func (m *Mail) State(ctx context.Context) (string, error) {
	var p *gmailapi.Profile
	err := m.do(ctx, "users.getProfile", unitsGetProfile, func() error {
		var err error
		p, err = m.svc.Users.GetProfile(me).Context(ctx).Do()
		return err
	})
	if err != nil {
		return "", err
	}
	return strconv.FormatUint(p.HistoryId, 10), nil
}

// Enumerate lists message ids page by page. cursor is Gmail's pageToken.
func (m *Mail) Enumerate(ctx context.Context, cursor string, limit int) ([]provider.Envelope, string, error) {
	if limit <= 0 || limit > maxEnumeratePage {
		limit = maxEnumeratePage
	}
	var resp *gmailapi.ListMessagesResponse
	err := m.do(ctx, "messages.list", unitsMessagesList, func() error {
		call := m.svc.Users.Messages.List(me).
			IncludeSpamTrash(m.opts.IncludeSpamTrash).
			MaxResults(int64(limit)).
			Context(ctx)
		if cursor != "" {
			call = call.PageToken(cursor)
		}
		var err error
		resp, err = call.Do()
		return err
	})
	if err != nil {
		return nil, "", err
	}
	page := make([]provider.Envelope, 0, len(resp.Messages))
	for _, msg := range resp.Messages {
		page = append(page, provider.Envelope{RemoteID: msg.Id, ThreadID: msg.ThreadId})
	}
	return page, resp.NextPageToken, nil
}

// ---------------------------------------------------------------------------
// Conversions

// labelsToFlags splits a message's labelIds into model flags and the remote
// mailbox ids it belongs to. STARRED and UNREAD are flags only; DRAFT is both
// a flag and a real mailbox (it appears in Mailboxes as the drafts role).
func labelsToFlags(labelIDs []string) (model.Flags, []string) {
	var f model.Flags
	boxes := make([]string, 0, len(labelIDs))
	for _, id := range labelIDs {
		switch id {
		case "UNREAD":
			f.Unread = true
			continue
		case "STARRED":
			f.Flagged = true
			continue
		case "DRAFT":
			f.Draft = true
		}
		boxes = append(boxes, id)
	}
	return f, boxes
}

// envelopeOf builds an Envelope from a message that carries labels and
// metadata (format=raw, minimal or metadata).
func envelopeOf(msg *gmailapi.Message) provider.Envelope {
	flags, boxes := labelsToFlags(msg.LabelIds)
	env := provider.Envelope{
		RemoteID:  msg.Id,
		ThreadID:  msg.ThreadId,
		Size:      msg.SizeEstimate,
		Flags:     flags,
		Mailboxes: boxes,
	}
	if msg.InternalDate != 0 {
		env.Received = timeFromUnixMilli(msg.InternalDate)
	}
	return env
}
