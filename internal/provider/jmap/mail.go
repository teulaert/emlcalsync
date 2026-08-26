package jmap

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"slices"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/teulaert/emlcalsync/internal/model"
	"github.com/teulaert/emlcalsync/internal/provider"
)

// fetchWorkers is the number of concurrent blob downloads (DESIGN §7.6).
const fetchWorkers = 8

// enumeratePageMax caps a single Enumerate page regardless of server limits.
const enumeratePageMax = 500

// envelopeProperties is the minimal Email property set we ever need: enough to
// build a provider.Envelope, plus blobId to download the raw message.
var envelopeProperties = []string{
	"id", "blobId", "threadId", "mailboxIds", "keywords", "receivedAt", "size",
}

// envelopeOnlyProperties is envelopeProperties without blobId, for the refresh
// path (FetchEnvelopes) that never downloads anything.
var envelopeOnlyProperties = []string{
	"id", "threadId", "mailboxIds", "keywords", "receivedAt", "size",
}

// Mail implements provider.MailProvider against a JMAP mail account.
type Mail struct {
	c *Client

	mu        sync.Mutex
	accountID string
	roles     map[model.MailboxRole]string // cached role → mailbox id
	identity  string                       // cached Identity id
	total     int                          // cached Email/query total
	totalOK   bool
}

// Mail returns the mail provider bound to the token's primary mail account.
func (c *Client) Mail() *Mail { return &Mail{c: c} }

var _ provider.MailProvider = (*Mail)(nil)

// AccountID resolves (and caches) the primary account id for urn:...:mail.
func (m *Mail) AccountID(ctx context.Context) (string, error) {
	m.mu.Lock()
	id := m.accountID
	m.mu.Unlock()
	if id != "" {
		return id, nil
	}
	id, err := m.c.PrimaryAccount(ctx, CapMail)
	if err != nil {
		return "", err
	}
	m.mu.Lock()
	m.accountID = id
	m.mu.Unlock()
	return id, nil
}

func (m *Mail) using() []string { return []string{CapMail} }

// ---------------------------------------------------------------------------
// Wire types

type mailboxObject struct {
	ID           string  `json:"id"`
	Name         string  `json:"name"`
	ParentID     *string `json:"parentId"`
	Role         *string `json:"role"`
	SortOrder    int     `json:"sortOrder"`
	TotalEmails  int     `json:"totalEmails"`
	UnreadEmails int     `json:"unreadEmails"`
}

type mailboxGetResponse struct {
	AccountID string          `json:"accountId"`
	State     string          `json:"state"`
	List      []mailboxObject `json:"list"`
	NotFound  []string        `json:"notFound"`
}

type emailObject struct {
	ID         string          `json:"id"`
	BlobID     string          `json:"blobId"`
	ThreadID   string          `json:"threadId"`
	MailboxIDs map[string]bool `json:"mailboxIds"`
	Keywords   map[string]bool `json:"keywords"`
	ReceivedAt jTime           `json:"receivedAt"`
	Size       int64           `json:"size"`
}

func (e emailObject) envelope() provider.Envelope {
	return provider.Envelope{
		RemoteID:  e.ID,
		ThreadID:  e.ThreadID,
		Received:  e.ReceivedAt.Time,
		Size:      e.Size,
		Flags:     keywordsToFlags(e.Keywords),
		Mailboxes: trueKeys(e.MailboxIDs),
	}
}

type emailGetResponse struct {
	AccountID string        `json:"accountId"`
	State     string        `json:"state"`
	List      []emailObject `json:"list"`
	NotFound  []string      `json:"notFound"`
}

type queryResponse struct {
	AccountID  string   `json:"accountId"`
	QueryState string   `json:"queryState"`
	Position   int      `json:"position"`
	IDs        []string `json:"ids"`
	Total      *int     `json:"total"`
	Limit      *int     `json:"limit"`
}

type changesResponse struct {
	AccountID      string   `json:"accountId"`
	OldState       string   `json:"oldState"`
	NewState       string   `json:"newState"`
	HasMoreChanges bool     `json:"hasMoreChanges"`
	Created        []string `json:"created"`
	Updated        []string `json:"updated"`
	Destroyed      []string `json:"destroyed"`
}

// SetError is a per-record error from a /set method (RFC 8620 §5.3).
type SetError struct {
	Type        string   `json:"type"`
	Description string   `json:"description,omitempty"`
	Properties  []string `json:"properties,omitempty"`
}

func (e SetError) Error() string {
	s := "jmap: set error " + e.Type
	if e.Description != "" {
		s += ": " + e.Description
	}
	return s
}

type setResponse struct {
	AccountID    string                     `json:"accountId"`
	OldState     *string                    `json:"oldState"`
	NewState     string                     `json:"newState"`
	Created      map[string]json.RawMessage `json:"created"`
	Updated      map[string]json.RawMessage `json:"updated"`
	Destroyed    []string                   `json:"destroyed"`
	NotCreated   map[string]SetError        `json:"notCreated"`
	NotUpdated   map[string]SetError        `json:"notUpdated"`
	NotDestroyed map[string]SetError        `json:"notDestroyed"`
}

// firstError returns a combined error for any not* entry, or nil.
func (r *setResponse) firstError(what string) error {
	for _, m := range []map[string]SetError{r.NotCreated, r.NotUpdated, r.NotDestroyed} {
		ids := make([]string, 0, len(m))
		for id := range m {
			ids = append(ids, id)
		}
		slices.Sort(ids)
		if len(ids) > 0 {
			se := m[ids[0]]
			return fmt.Errorf("jmap: %s failed for %d record(s), first %s: %w",
				what, len(ids), ids[0], se)
		}
	}
	return nil
}

// ---------------------------------------------------------------------------
// Flags / keywords

const (
	kwSeen     = "$seen"
	kwFlagged  = "$flagged"
	kwDraft    = "$draft"
	kwAnswered = "$answered"
)

func keywordsToFlags(kw map[string]bool) model.Flags {
	return model.Flags{
		Unread:   !kw[kwSeen], // JMAP records "seen"; the model records "unread"
		Flagged:  kw[kwFlagged],
		Draft:    kw[kwDraft],
		Answered: kw[kwAnswered],
	}
}

// ---------------------------------------------------------------------------
// Mailboxes

func roleFromJMAP(role *string) model.MailboxRole {
	if role == nil {
		return ""
	}
	switch strings.ToLower(*role) {
	case "inbox":
		return model.RoleInbox
	case "archive":
		return model.RoleArchive
	case "sent":
		return model.RoleSent
	case "drafts":
		return model.RoleDrafts
	case "trash":
		return model.RoleTrash
	case "junk", "spam":
		return model.RoleJunk
	case "important":
		return model.RoleImportant
	case "all":
		return model.RoleAll
	default:
		// "flagged", "subscribed" and any unknown/absent role are treated as
		// ordinary user folders.
		return ""
	}
}

// Mailboxes returns every mailbox in the account.
func (m *Mail) Mailboxes(ctx context.Context) ([]model.Mailbox, error) {
	acct, err := m.AccountID(ctx)
	if err != nil {
		return nil, err
	}
	var got mailboxGetResponse
	err = m.c.call(ctx, m.using(), "Mailbox/get", map[string]any{
		"accountId":  acct,
		"ids":        nil,
		"properties": []string{"id", "name", "parentId", "role", "sortOrder", "totalEmails", "unreadEmails"},
	}, &got)
	if err != nil {
		return nil, err
	}
	out := make([]model.Mailbox, 0, len(got.List))
	roles := make(map[model.MailboxRole]string)
	for _, mb := range got.List {
		role := roleFromJMAP(mb.Role)
		parent := ""
		if mb.ParentID != nil {
			parent = *mb.ParentID
		}
		out = append(out, model.Mailbox{
			RemoteID:     mb.ID,
			Name:         mb.Name,
			Role:         role,
			ParentRemote: parent,
			SortOrder:    mb.SortOrder,
			TotalCount:   mb.TotalEmails,
			UnreadCount:  mb.UnreadEmails,
		})
		if role != "" {
			if _, dup := roles[role]; !dup {
				roles[role] = mb.ID
			}
		}
	}
	m.mu.Lock()
	m.roles = roles
	m.mu.Unlock()
	return out, nil
}

// mailboxByRole returns the id of the (first) mailbox with a role, refreshing
// the cache once if it is not known yet.
func (m *Mail) mailboxByRole(ctx context.Context, role model.MailboxRole) (string, error) {
	m.mu.Lock()
	id, ok := m.roles[role]
	m.mu.Unlock()
	if ok && id != "" {
		return id, nil
	}
	if _, err := m.Mailboxes(ctx); err != nil {
		return "", err
	}
	m.mu.Lock()
	id, ok = m.roles[role]
	m.mu.Unlock()
	if !ok || id == "" {
		return "", fmt.Errorf("jmap: account has no mailbox with role %q", role)
	}
	return id, nil
}

// ---------------------------------------------------------------------------
// State

// mailState is the opaque delta token handed to the sync engine. One string
// carries both the Email and the Mailbox state:
//
//	{"email":"<Email state>","mailbox":"<Mailbox state>"}
//
// A bare (non-JSON) token is accepted as an Email state alone, so tokens
// written by an older build still work.
type mailState struct {
	Email   string `json:"email"`
	Mailbox string `json:"mailbox"`
}

func (s mailState) String() string {
	b, err := json.Marshal(s)
	if err != nil { // cannot happen for two strings
		return s.Email
	}
	return string(b)
}

func parseMailState(s string) mailState {
	s = strings.TrimSpace(s)
	if s == "" {
		return mailState{}
	}
	if strings.HasPrefix(s, "{") {
		var ms mailState
		if json.Unmarshal([]byte(s), &ms) == nil {
			return ms
		}
	}
	return mailState{Email: s}
}

// State returns the current combined Email+Mailbox delta state.
func (m *Mail) State(ctx context.Context) (string, error) {
	acct, err := m.AccountID(ctx)
	if err != nil {
		return "", err
	}
	// A /get with an empty id list is the cheapest way to read a type's state.
	resp, err := m.c.Request(ctx, m.using(), []Invocation{
		{Name: "Email/get", Args: map[string]any{"accountId": acct, "ids": []string{}}, ID: "e"},
		{Name: "Mailbox/get", Args: map[string]any{"accountId": acct, "ids": []string{}}, ID: "m"},
	})
	if err != nil {
		return "", err
	}
	var eg emailGetResponse
	if err := resp.DecodeAt(0, &eg); err != nil {
		return "", err
	}
	var mg mailboxGetResponse
	if err := resp.DecodeAt(1, &mg); err != nil {
		return "", err
	}
	return mailState{Email: eg.State, Mailbox: mg.State}.String(), nil
}

// ---------------------------------------------------------------------------
// Enumerate

// Sort directions recorded in an enumCursor.
const (
	sortAsc  = "asc"
	sortDesc = "desc"
)

// enumCursor is the opaque Enumerate cursor:
//
//	{"anchor":"<last Email id of the previous page>","n":<ids enumerated so far>,"sort":"desc"}
//
// Anchoring on the last id of the previous page (RFC 8620 §5.5: anchor +
// anchorOffset) is what makes paging safe against concurrent deletes. A plain
// numeric position shifts down by one for every message removed behind the
// cursor, silently skipping unenumerated mail.
//
// n is only the fallback: when the anchor itself has been destroyed the server
// answers "anchorNotFound", and resuming from position n is better than
// failing the whole enumeration.
//
// sort records the receivedAt direction the run was started in. A fresh
// enumeration goes newest first ("desc") so a multi-hour first sync archives
// this year's mail before 2005's. The marker exists because the direction
// cannot be changed mid-run: an anchor and a count only mean anything within
// one ordering, and re-sorting a half-finished backfill would re-enumerate one
// end of the mailbox and never reach the other. A cursor with no "sort" member
// was written by an older build that always sorted ascending, so that run
// keeps enumerating ascending until it finishes; only the next backfill (which
// starts from an empty cursor) switches to newest first.
type enumCursor struct {
	Anchor string `json:"anchor"`
	N      int    `json:"n"`
	Sort   string `json:"sort,omitempty"`
}

// ascending reports the direction this cursor enumerates in.
func (c enumCursor) ascending() bool { return c.Sort != sortDesc }

func (c enumCursor) String() string {
	b, err := json.Marshal(c)
	if err != nil { // cannot happen for a string and an int
		return ""
	}
	return string(b)
}

func parseEnumCursor(s string) (enumCursor, error) {
	s = strings.TrimSpace(s)
	if s == "" {
		// A new enumeration: newest first.
		return enumCursor{Sort: sortDesc}, nil
	}
	if !strings.HasPrefix(s, "{") {
		// A bare number is a cursor written by a much older build, persisted in
		// a half-finished backfill. Those runs sorted ascending, so this one
		// continues ascending; the next page picks up an anchor again.
		if n, err := strconv.Atoi(s); err == nil && n >= 0 {
			return enumCursor{N: n, Sort: sortAsc}, nil
		}
		return enumCursor{}, fmt.Errorf("jmap: bad enumerate cursor %q", s)
	}
	var c enumCursor
	if json.Unmarshal([]byte(s), &c) != nil || c.N < 0 {
		return enumCursor{}, fmt.Errorf("jmap: bad enumerate cursor %q", s)
	}
	switch c.Sort {
	case sortAsc, sortDesc:
	case "":
		// Written before the direction was recorded, i.e. by a build that
		// always sorted ascending. Finish that backfill the way it started.
		c.Sort = sortAsc
	default:
		return enumCursor{}, fmt.Errorf("jmap: bad enumerate cursor %q: unknown sort %q", s, c.Sort)
	}
	return c, nil
}

// Enumerate lists messages ordered by receivedAt, newest first, so a backfill
// archives recent mail before old mail. The cursor anchors on the last id of
// the previous page, so a restart resumes exactly where it stopped even if
// mail arrived or was deleted behind the cursor — which matters more in this
// direction than the other, since new mail lands at position 0 and shifts
// every remaining page along.
//
// A cursor left behind by a build that enumerated ascending keeps enumerating
// ascending to the end of that backfill; see enumCursor.
func (m *Mail) Enumerate(ctx context.Context, cursor string, limit int) ([]provider.Envelope, string, error) {
	acct, err := m.AccountID(ctx)
	if err != nil {
		return nil, "", err
	}
	cur, err := parseEnumCursor(cursor)
	if err != nil {
		return nil, "", err
	}
	s, err := m.c.Session(ctx)
	if err != nil {
		return nil, "", err
	}
	if limit <= 0 {
		limit = enumeratePageMax
	}
	limit = min(limit, enumeratePageMax, s.Core.MaxObjectsInGet)

	queryArgs := map[string]any{
		"accountId": acct,
		"sort":      []map[string]any{{"property": "receivedAt", "isAscending": cur.ascending()}},
		"limit":     limit,
	}
	switch {
	case cur.Anchor != "":
		// Start at the record after the anchor.
		queryArgs["anchor"] = cur.Anchor
		queryArgs["anchorOffset"] = 1
	default:
		queryArgs["position"] = cur.N
		if cur.N == 0 {
			// Only worth asking for on the first page; later pages fall back
			// to the "short page means last page" rule.
			queryArgs["calculateTotal"] = true
		}
	}

	q, g, err := m.queryPage(ctx, acct, queryArgs)
	if err != nil {
		if !IsMethodError(err, "anchorNotFound") {
			return nil, "", err
		}
		// The anchor was destroyed between pages. Fall back to a plain
		// position, one short of the count so far: the anchor's own removal
		// has shifted everything after it down by one, so that lands exactly
		// on the next unenumerated message. Positions count along the sort
		// order itself, so this holds in either direction. Further deletions
		// behind the cursor can still shift it (and mail arriving during a
		// descending run shifts it the other way), and re-delivering an
		// envelope is the harmless direction (upserts are idempotent) where
		// skipping one is not, so this errs towards overlap.
		pos := max(0, cur.N-1)
		m.c.log.Debug("jmap enumerate anchor is gone, falling back to position",
			"anchor", cur.Anchor, "position", pos)
		delete(queryArgs, "anchor")
		delete(queryArgs, "anchorOffset")
		queryArgs["position"] = pos
		q, g, err = m.queryPage(ctx, acct, queryArgs)
		if err != nil {
			return nil, "", err
		}
	}

	byID := make(map[string]emailObject, len(g.List))
	for _, e := range g.List {
		byID[e.ID] = e
	}
	// Preserve the query's order; Email/get makes no ordering promise.
	page := make([]provider.Envelope, 0, len(q.IDs))
	for _, id := range q.IDs {
		if e, ok := byID[id]; ok {
			page = append(page, e.envelope())
		}
	}

	if q.Total != nil {
		m.setTotal(*q.Total)
	}

	// The server may have applied a smaller limit than we asked for; compare
	// against what it actually used, or we would stop one page early.
	effLimit := limit
	if q.Limit != nil && *q.Limit > 0 {
		effLimit = *q.Limit
	}
	next := enumCursor{N: cur.N + len(q.IDs), Sort: cur.Sort}
	done := len(q.IDs) == 0 || len(q.IDs) < effLimit
	if q.Total != nil && next.N >= *q.Total {
		done = true
	}
	if done {
		return page, "", nil
	}
	next.Anchor = q.IDs[len(q.IDs)-1]
	return page, next.String(), nil
}

// Totaler is the optional interface a mail provider implements when it can
// state how many messages the account holds. provider.MailProvider.Enumerate
// has nowhere to return that, and the sync engine wants a denominator for
// backfill progress, so it asks for one separately:
//
//	if t, ok := mp.(interface{ Total(context.Context) (int, error) }); ok { … }
//
// A provider that cannot answer returns an error; the engine treats the total
// as unknown and reports progress without one.
type Totaler interface {
	Total(ctx context.Context) (int, error)
}

var _ Totaler = (*Mail)(nil)

// Total reports how many messages the account holds, via Email/query with
// limit 0 and calculateTotal.
//
// The first answer is cached — including the one Enumerate's first page
// already asks for, so a backfill that calls Total costs no extra round trip —
// and reused for the life of the *Mail. A backfill is meant to see a stable
// denominator; mail arriving while it runs must not make the bar go backwards.
func (m *Mail) Total(ctx context.Context) (int, error) {
	m.mu.Lock()
	n, ok := m.total, m.totalOK
	m.mu.Unlock()
	if ok {
		return n, nil
	}
	acct, err := m.AccountID(ctx)
	if err != nil {
		return 0, err
	}
	var q queryResponse
	if err := m.c.call(ctx, m.using(), "Email/query", map[string]any{
		"accountId":      acct,
		"limit":          0,
		"calculateTotal": true,
	}, &q); err != nil {
		return 0, err
	}
	if q.Total == nil {
		return 0, fmt.Errorf("jmap: Email/query did not return a total: %w", provider.ErrNotSupported)
	}
	m.setTotal(*q.Total)
	return *q.Total, nil
}

func (m *Mail) setTotal(n int) {
	if n < 0 {
		return
	}
	m.mu.Lock()
	if !m.totalOK {
		m.total, m.totalOK = n, true
	}
	m.mu.Unlock()
}

// queryPage runs one Email/query chained into an Email/get.
func (m *Mail) queryPage(ctx context.Context, acct string, queryArgs map[string]any) (queryResponse, emailGetResponse, error) {
	var (
		q queryResponse
		g emailGetResponse
	)
	resp, err := m.c.Request(ctx, m.using(), []Invocation{
		{Name: "Email/query", Args: queryArgs, ID: "q"},
		{Name: "Email/get", Args: map[string]any{
			"accountId":  acct,
			"#ids":       ResultRef("q", "Email/query", "/ids"),
			"properties": envelopeProperties,
		}, ID: "g"},
	})
	if err != nil {
		return q, g, err
	}
	if err := resp.DecodeAt(0, &q); err != nil {
		return q, g, err
	}
	if err := resp.DecodeAt(1, &g); err != nil {
		return q, g, err
	}
	return q, g, nil
}

// ---------------------------------------------------------------------------
// FetchRaw

// FetchRaw downloads full RFC 822 messages. In JMAP the Email's own blobId is
// the raw message, so this is one Email/get per chunk plus one blob download
// per message, run through a pool of fetchWorkers. fn is called serially.
func (m *Mail) FetchRaw(ctx context.Context, ids []string, fn func(provider.RawMessage) error) error {
	if len(ids) == 0 {
		return nil
	}
	acct, err := m.AccountID(ctx)
	if err != nil {
		return err
	}
	s, err := m.c.Session(ctx)
	if err != nil {
		return err
	}
	chunkSize := max(1, s.Core.MaxObjectsInGet)

	parent := ctx
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()

	work := make(chan emailObject)
	var (
		wg       sync.WaitGroup
		fnMu     sync.Mutex
		errMu    sync.Mutex
		firstErr error
	)
	fail := func(err error) {
		errMu.Lock()
		if firstErr == nil {
			firstErr = err
			cancel()
		}
		errMu.Unlock()
	}

	for range fetchWorkers {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for e := range work {
				raw, err := m.c.Download(ctx, acct, e.BlobID, "message.eml", "message/rfc822")
				if err != nil {
					if ctx.Err() != nil {
						return
					}
					if errors.Is(err, model.ErrNotFound) {
						// Deleted between the get and the download.
						m.c.log.Debug("jmap blob vanished", "email", e.ID, "blob", e.BlobID)
						continue
					}
					fail(err)
					return
				}
				fnMu.Lock()
				err = fn(provider.RawMessage{Envelope: e.envelope(), Raw: raw})
				fnMu.Unlock()
				if err != nil {
					fail(err)
					return
				}
			}
		}()
	}

feed:
	for chunk := range slices.Chunk(ids, chunkSize) {
		var got emailGetResponse
		err := m.c.call(ctx, m.using(), "Email/get", map[string]any{
			"accountId":  acct,
			"ids":        chunk,
			"properties": envelopeProperties,
		}, &got)
		if err != nil {
			fail(err)
			break
		}
		if len(got.NotFound) > 0 {
			m.c.log.Debug("jmap Email/get notFound", "count", len(got.NotFound))
		}
		for _, e := range got.List {
			if e.BlobID == "" {
				continue
			}
			select {
			case work <- e:
			case <-ctx.Done():
				break feed
			}
		}
	}
	close(work)
	wg.Wait()

	if firstErr != nil {
		return firstErr
	}
	return parent.Err()
}

// FetchEnvelopes reads the current flags, mailboxes and metadata of ids and
// hands each one to fn. It is not part of provider.MailProvider: the sync
// engine reaches for it through an optional interface during a reconcile, to
// refresh messages whose raw bytes it already has without downloading them
// again (the same contract gmail.Mail implements).
//
// It is Email/get with the envelope properties minus blobId, chunked by the
// session's maxObjectsInGet. Calls are serial — a reconcile walks the whole
// mailbox and there is no reason to spend the account's concurrency budget on
// it — and so is fn. Ids the server no longer knows come back in notFound and
// are skipped silently.
func (m *Mail) FetchEnvelopes(ctx context.Context, ids []string, fn func(provider.Envelope) error) error {
	if len(ids) == 0 {
		return nil
	}
	acct, err := m.AccountID(ctx)
	if err != nil {
		return err
	}
	s, err := m.c.Session(ctx)
	if err != nil {
		return err
	}
	for chunk := range slices.Chunk(ids, max(1, s.Core.MaxObjectsInGet)) {
		var got emailGetResponse
		if err := m.c.call(ctx, m.using(), "Email/get", map[string]any{
			"accountId":  acct,
			"ids":        chunk,
			"properties": envelopeOnlyProperties,
		}, &got); err != nil {
			return err
		}
		if len(got.NotFound) > 0 {
			m.c.log.Debug("jmap Email/get notFound", "count", len(got.NotFound), "of", "FetchEnvelopes")
		}
		for _, e := range got.List {
			if err := fn(e.envelope()); err != nil {
				return err
			}
		}
		if err := ctx.Err(); err != nil {
			return err
		}
	}
	return nil
}

// ---------------------------------------------------------------------------
// Changes

// maxChangesPerCall bounds one /changes round trip.
const maxChangesPerCall = 500

// changesLoopLimit guards against a server that never clears hasMoreChanges.
const changesLoopLimit = 1000

// Changes returns everything that changed since a state token produced by
// State or a previous Changes.
func (m *Mail) Changes(ctx context.Context, since string) (*provider.Changes, error) {
	acct, err := m.AccountID(ctx)
	if err != nil {
		return nil, err
	}
	st := parseMailState(since)
	if st.Email == "" {
		// Nothing to compute a delta from.
		return nil, provider.ErrStateExpired
	}

	created, updated, destroyed, emailState, err := m.c.collectChanges(ctx, m.using(), "Email/changes", acct, st.Email)
	if err != nil {
		if IsMethodError(err, "cannotCalculateChanges") {
			return nil, provider.ErrStateExpired
		}
		return nil, err
	}

	out := &provider.Changes{Removed: destroyed}

	// Mailbox delta. A failure here must not lose the mail delta, so an
	// expired mailbox state degrades to "resync the mailbox list".
	mailboxState := st.Mailbox
	if mailboxState == "" {
		var mg mailboxGetResponse
		if err := m.c.call(ctx, m.using(), "Mailbox/get",
			map[string]any{"accountId": acct, "ids": []string{}}, &mg); err != nil {
			return nil, err
		}
		mailboxState = mg.State
		out.MailboxesChanged = true
	} else {
		// A mailbox delta that cannot be computed (or cannot be paged to the
		// end) must not lose the mail delta: it degrades to "resync the
		// mailbox list" instead.
		mc, mu2, md, newMbState, err := m.c.collectChanges(ctx, m.using(), "Mailbox/changes", acct, mailboxState)
		switch {
		case err == nil:
			mailboxState = newMbState
			out.MailboxesChanged = len(mc)+len(mu2)+len(md) > 0
		case IsMethodError(err, "cannotCalculateChanges"), errors.Is(err, provider.ErrStateExpired):
			var mg mailboxGetResponse
			if err := m.c.call(ctx, m.using(), "Mailbox/get",
				map[string]any{"accountId": acct, "ids": []string{}}, &mg); err != nil {
				return nil, err
			}
			mailboxState = mg.State
			out.MailboxesChanged = true
		default:
			return nil, err
		}
	}

	// Coalesce: a destroyed id must not also appear as created/updated, and an
	// id that was both created and updated is simply new to us.
	gone := make(map[string]bool, len(destroyed))
	for _, id := range destroyed {
		gone[id] = true
	}
	isNew := make(map[string]bool, len(created))
	want := make([]string, 0, len(created)+len(updated))
	for _, id := range created {
		if gone[id] || isNew[id] {
			continue
		}
		isNew[id] = true
		want = append(want, id)
	}
	seenUpd := make(map[string]bool, len(updated))
	for _, id := range updated {
		if gone[id] || isNew[id] || seenUpd[id] {
			continue
		}
		seenUpd[id] = true
		want = append(want, id)
	}

	if len(want) > 0 {
		s, err := m.c.Session(ctx)
		if err != nil {
			return nil, err
		}
		for chunk := range slices.Chunk(want, max(1, s.Core.MaxObjectsInGet)) {
			var got emailGetResponse
			if err := m.c.call(ctx, m.using(), "Email/get", map[string]any{
				"accountId":  acct,
				"ids":        chunk,
				"properties": envelopeProperties,
			}, &got); err != nil {
				return nil, err
			}
			for _, e := range got.List {
				env := e.envelope()
				if isNew[e.ID] {
					out.Added = append(out.Added, env)
				} else {
					out.Updated = append(out.Updated, env)
				}
			}
			// Ids in notFound were destroyed after the /changes response was
			// generated. They are strictly newer than emailState, so the next
			// delta will report them as destroyed; dropping them here is safe.
			for _, id := range got.NotFound {
				m.c.log.Debug("jmap changed email no longer exists", "id", id)
			}
		}
	}

	out.NewState = mailState{Email: emailState, Mailbox: mailboxState}.String()
	return out, nil
}

// collectChanges loops a /changes method until hasMoreChanges is false.
//
// A server that reports more changes without advancing its state, or one that
// never converges, cannot be paged to completion. Returning what we have would
// hand the sync engine a token that claims to cover changes it never saw, so
// both cases report provider.ErrStateExpired instead: the caller re-lists.
func (c *Client) collectChanges(ctx context.Context, using []string, method, acct, since string) (created, updated, destroyed []string, newState string, err error) {
	newState = since
	for i := 0; ; i++ {
		if i >= changesLoopLimit {
			return nil, nil, nil, "", fmt.Errorf("jmap: %s did not converge after %d rounds: %w",
				method, i, provider.ErrStateExpired)
		}
		var cr changesResponse
		err = c.call(ctx, using, method, map[string]any{
			"accountId":  acct,
			"sinceState": newState,
			"maxChanges": maxChangesPerCall,
		}, &cr)
		if err != nil {
			return nil, nil, nil, "", err
		}
		created = append(created, cr.Created...)
		updated = append(updated, cr.Updated...)
		destroyed = append(destroyed, cr.Destroyed...)
		if !cr.HasMoreChanges {
			return created, updated, destroyed, cr.NewState, nil
		}
		if cr.NewState == "" || cr.NewState == newState {
			return nil, nil, nil, "", fmt.Errorf(
				"jmap: %s reports more changes but its state did not advance past %q: %w",
				method, newState, provider.ErrStateExpired)
		}
		newState = cr.NewState
	}
}

// ---------------------------------------------------------------------------
// Writes

// setEmails applies the same patch to every id, chunked by maxObjectsInSet.
func (m *Mail) setEmails(ctx context.Context, ids []string, patch map[string]any, what string) error {
	if len(ids) == 0 || len(patch) == 0 {
		return nil
	}
	acct, err := m.AccountID(ctx)
	if err != nil {
		return err
	}
	s, err := m.c.Session(ctx)
	if err != nil {
		return err
	}
	for chunk := range slices.Chunk(ids, max(1, s.Core.MaxObjectsInSet)) {
		update := make(map[string]any, len(chunk))
		for _, id := range chunk {
			update[id] = patch
		}
		var sr setResponse
		if err := m.c.call(ctx, m.using(), "Email/set", map[string]any{
			"accountId": acct,
			"update":    update,
		}, &sr); err != nil {
			return err
		}
		if err := sr.firstError(what); err != nil {
			return err
		}
	}
	return nil
}

// SetFlags adds the flags in set and removes those in clear.
//
// Note the inversion: model.Flags records "unread" while JMAP records $seen,
// so setting Unread removes $seen and clearing Unread adds it.
func (m *Mail) SetFlags(ctx context.Context, ids []string, set, clear model.Flags) error {
	patch := map[string]any{}
	if set.Unread {
		patch["keywords/"+kwSeen] = nil
	}
	if clear.Unread {
		patch["keywords/"+kwSeen] = true
	}
	for _, f := range []struct {
		kw         string
		set, clear bool
	}{
		{kwFlagged, set.Flagged, clear.Flagged},
		{kwDraft, set.Draft, clear.Draft},
		{kwAnswered, set.Answered, clear.Answered},
	} {
		if f.set {
			patch["keywords/"+f.kw] = true
		}
		if f.clear {
			patch["keywords/"+f.kw] = nil
		}
	}
	return m.setEmails(ctx, ids, patch, "SetFlags")
}

// SetMailboxes adds and removes mailbox memberships without touching the rest.
func (m *Mail) SetMailboxes(ctx context.Context, ids []string, add, remove []string) error {
	patch := map[string]any{}
	for _, id := range add {
		patch["mailboxIds/"+id] = true
	}
	for _, id := range remove {
		if _, dup := patch["mailboxIds/"+id]; dup {
			return fmt.Errorf("jmap: mailbox %s is in both add and remove", id)
		}
		patch["mailboxIds/"+id] = nil
	}
	return m.setEmails(ctx, ids, patch, "SetMailboxes")
}

// Trash moves messages into the trash mailbox, replacing all other mailbox
// memberships (that is what "move to trash" means in JMAP).
func (m *Mail) Trash(ctx context.Context, ids []string) error {
	trash, err := m.mailboxByRole(ctx, model.RoleTrash)
	if err != nil {
		return err
	}
	patch := map[string]any{"mailboxIds": map[string]bool{trash: true}}
	return m.setEmails(ctx, ids, patch, "Trash")
}

type importedEmail struct {
	ID       string `json:"id"`
	BlobID   string `json:"blobId"`
	ThreadID string `json:"threadId"`
	Size     int64  `json:"size"`
}

type importResponse struct {
	AccountID  string                   `json:"accountId"`
	OldState   *string                  `json:"oldState"`
	NewState   string                   `json:"newState"`
	Created    map[string]importedEmail `json:"created"`
	NotCreated map[string]SetError      `json:"notCreated"`
}

// importRaw uploads raw and imports it into one mailbox with the given keywords.
//
// The upload is idempotent (a blob is content-addressed and expires on its
// own) and stays retryable; the Email/import is not, and must not be retried
// on a 5xx — the server may well have created the message before failing.
func (m *Mail) importRaw(ctx context.Context, raw []byte, mailboxID string, keywords map[string]bool) (importedEmail, error) {
	acct, err := m.AccountID(ctx)
	if err != nil {
		return importedEmail{}, err
	}
	blobID, _, err := m.c.Upload(ctx, acct, "message/rfc822", raw)
	if err != nil {
		return importedEmail{}, err
	}
	const cid = "new"
	var ir importResponse
	if err := m.c.callNoRetry(ctx, m.using(), "Email/import", map[string]any{
		"accountId": acct,
		"emails": map[string]any{
			cid: map[string]any{
				"blobId":     blobID,
				"mailboxIds": map[string]bool{mailboxID: true},
				"keywords":   keywords,
				"receivedAt": time.Now().UTC().Format("2006-01-02T15:04:05Z"),
			},
		},
	}, &ir); err != nil {
		return importedEmail{}, err
	}
	if se, bad := ir.NotCreated[cid]; bad {
		return importedEmail{}, fmt.Errorf("jmap: Email/import rejected: %w", se)
	}
	em, ok := ir.Created[cid]
	if !ok || em.ID == "" {
		return importedEmail{}, errors.New("jmap: Email/import returned no created email")
	}
	return em, nil
}

// CreateDraft stores raw in the drafts mailbox.
func (m *Mail) CreateDraft(ctx context.Context, raw []byte) (string, error) {
	drafts, err := m.mailboxByRole(ctx, model.RoleDrafts)
	if err != nil {
		return "", err
	}
	em, err := m.importRaw(ctx, raw, drafts, map[string]bool{kwDraft: true, kwSeen: true})
	if err != nil {
		return "", err
	}
	return em.ID, nil
}

type identityObject struct {
	ID    string `json:"id"`
	Name  string `json:"name"`
	Email string `json:"email"`
}

type identityGetResponse struct {
	AccountID string           `json:"accountId"`
	State     string           `json:"state"`
	List      []identityObject `json:"list"`
}

// submissionAccountID resolves the account that owns Identity and
// EmailSubmission. RFC 8621 §7 puts those in the submission capability's
// primary account, which need not be the mail account; on Fastmail the two
// coincide, so a server that advertises no submission primary account falls
// back to the mail account rather than failing.
func (m *Mail) submissionAccountID(ctx context.Context) (string, error) {
	if id, err := m.c.PrimaryAccount(ctx, CapSubmission); err == nil && id != "" {
		return id, nil
	}
	return m.AccountID(ctx)
}

// identityID picks the Identity whose address matches the configured account
// email, falling back to the first identity the server offers.
func (m *Mail) identityID(ctx context.Context) (string, error) {
	m.mu.Lock()
	id := m.identity
	m.mu.Unlock()
	if id != "" {
		return id, nil
	}
	acct, err := m.submissionAccountID(ctx)
	if err != nil {
		return "", err
	}
	var ig identityGetResponse
	if err := m.c.call(ctx, []string{CapMail, CapSubmission}, "Identity/get",
		map[string]any{"accountId": acct, "ids": nil}, &ig); err != nil {
		return "", err
	}
	if len(ig.List) == 0 {
		return "", errors.New("jmap: account has no sending identity")
	}
	want := strings.ToLower(m.c.AccountEmail())
	chosen := ig.List[0].ID
	for _, idn := range ig.List {
		if want != "" && strings.EqualFold(idn.Email, want) {
			chosen = idn.ID
			break
		}
	}
	m.mu.Lock()
	m.identity = chosen
	m.mu.Unlock()
	return chosen, nil
}

// Send submits raw for delivery.
//
// threadID is ignored: JMAP threads messages by their References/In-Reply-To
// headers, which the caller has already put in raw.
//
// The flow is import-then-submit (RFC 8621 §7):
//
//  1. upload raw, Email/import it into Drafts with $draft
//  2. EmailSubmission/set create, with onSuccessUpdateEmail moving the message
//     into Sent and clearing $draft once the submission succeeds
//
// Two round trips rather than one, because Email/import creation ids are not
// guaranteed to be usable as "#creationId" references from a later call in the
// same request.
func (m *Mail) Send(ctx context.Context, raw []byte, threadID string) (string, error) {
	drafts, err := m.mailboxByRole(ctx, model.RoleDrafts)
	if err != nil {
		return "", err
	}
	sent, err := m.mailboxByRole(ctx, model.RoleSent)
	if err != nil {
		return "", err
	}
	identity, err := m.identityID(ctx)
	if err != nil {
		return "", err
	}
	// The submission is created in the identity's own account, which is the
	// mail account on any server that does not split the two.
	acct, err := m.submissionAccountID(ctx)
	if err != nil {
		return "", err
	}

	em, err := m.importRaw(ctx, raw, drafts, map[string]bool{kwDraft: true, kwSeen: true})
	if err != nil {
		return "", err
	}

	// Never retried: a 5xx from a submission says nothing about whether the
	// message was already handed to the MTA, and a retry would send it twice.
	const cid = "sub"
	var sr setResponse
	err = m.c.callNoRetry(ctx, []string{CapMail, CapSubmission}, "EmailSubmission/set", map[string]any{
		"accountId": acct,
		"create": map[string]any{
			cid: map[string]any{"identityId": identity, "emailId": em.ID},
		},
		"onSuccessUpdateEmail": map[string]any{
			"#" + cid: map[string]any{
				"mailboxIds":          map[string]bool{sent: true},
				"keywords/" + kwDraft: nil,
			},
		},
	}, &sr)
	if err != nil {
		return "", fmt.Errorf("jmap: submitting message (draft %s left in place): %w", em.ID, err)
	}
	if se, bad := sr.NotCreated[cid]; bad {
		return "", fmt.Errorf("jmap: submission rejected (draft %s left in place): %w", em.ID, se)
	}
	if _, ok := sr.Created[cid]; !ok {
		return "", fmt.Errorf("jmap: submission of %s produced no EmailSubmission", em.ID)
	}
	// The Email id survives the move to Sent.
	return em.ID, nil
}

// FetchAttachment downloads one attachment blob. ref is the JMAP blobId
// recorded in attachments.remote_ref.
func (m *Mail) FetchAttachment(ctx context.Context, messageID, ref string) ([]byte, error) {
	acct, err := m.AccountID(ctx)
	if err != nil {
		return nil, err
	}
	if ref == "" {
		return nil, fmt.Errorf("jmap: no blob id for attachment of %s: %w", messageID, model.ErrNotFound)
	}
	return m.c.Download(ctx, acct, ref, "attachment", "application/octet-stream")
}
