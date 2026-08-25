package gmail

import (
	"context"
	"fmt"
	"sort"
	"strconv"

	gmailapi "google.golang.org/api/gmail/v1"

	"github.com/lennert/emlcal/internal/provider"
)

// historyTypes are the record types we ask for. Gmail also has
// "labelAdded"/"labelRemoved" spellings in the docs; the API accepts these.
var historyTypes = []string{"messageAdded", "messageDeleted", "labelAdded", "labelRemoved"}

// change is the coalesced state of one message across the whole history
// window. Gmail's history is noisy: the same message shows up repeatedly, and
// a message can be added, labelled and deleted within one delta.
type change struct {
	threadID string
	added    bool
	deleted  bool
	labelled bool
}

// Changes replays users.history.list since the given historyId. Deletions win
// over everything else, so a message added and removed inside one window is
// reported only as removed.
func (m *Mail) Changes(ctx context.Context, since string) (*provider.Changes, error) {
	if since == "" {
		return nil, fmt.Errorf("gmail: no history state to delta from: %w", provider.ErrStateExpired)
	}
	start, err := strconv.ParseUint(since, 10, 64)
	if err != nil {
		return nil, fmt.Errorf("gmail: unusable history state %q: %w", since, provider.ErrStateExpired)
	}

	entries := make(map[string]*change)
	entry := func(msg *gmailapi.Message) *change {
		if msg == nil || msg.Id == "" {
			return nil
		}
		e, ok := entries[msg.Id]
		if !ok {
			e = &change{}
			entries[msg.Id] = e
		}
		if msg.ThreadId != "" {
			e.threadID = msg.ThreadId
		}
		return e
	}

	maxHistory := start
	mailboxesChanged := false
	pageToken := ""
	for {
		var resp *gmailapi.ListHistoryResponse
		err := m.do(ctx, "history.list", unitsHistoryList, func() error {
			call := m.svc.Users.History.List(me).
				StartHistoryId(start).
				HistoryTypes(historyTypes...).
				MaxResults(500).
				Context(ctx)
			if pageToken != "" {
				call = call.PageToken(pageToken)
			}
			var err error
			resp, err = call.Do()
			return err
		})
		if err != nil {
			if isNotFound(err) {
				// The startHistoryId is older than Gmail's retention window.
				return nil, fmt.Errorf("gmail history %s too old: %w", since, provider.ErrStateExpired)
			}
			return nil, err
		}

		for _, h := range resp.History {
			if h.Id > maxHistory {
				maxHistory = h.Id
			}
			for _, a := range h.MessagesAdded {
				if e := entry(a.Message); e != nil {
					e.added = true
				}
			}
			for _, d := range h.MessagesDeleted {
				if e := entry(d.Message); e != nil {
					e.deleted = true
				}
			}
			for _, la := range h.LabelsAdded {
				if e := entry(la.Message); e != nil {
					e.labelled = true
				}
				if m.unknownLabels(la.LabelIds) {
					mailboxesChanged = true
				}
			}
			for _, lr := range h.LabelsRemoved {
				if e := entry(lr.Message); e != nil {
					e.labelled = true
				}
				if m.unknownLabels(lr.LabelIds) {
					mailboxesChanged = true
				}
			}
		}
		if resp.HistoryId > maxHistory {
			maxHistory = resp.HistoryId
		}
		if resp.NextPageToken == "" {
			break
		}
		pageToken = resp.NextPageToken
	}

	out := &provider.Changes{MailboxesChanged: mailboxesChanged}

	var needLabels []string
	ids := make([]string, 0, len(entries))
	for id := range entries {
		ids = append(ids, id)
	}
	sort.Strings(ids) // deterministic output; history order carries no meaning
	for _, id := range ids {
		e := entries[id]
		switch {
		case e.deleted:
			out.Removed = append(out.Removed, id)
		case e.added:
			// The sync engine fetches these in full, so labels are irrelevant.
			out.Added = append(out.Added, provider.Envelope{RemoteID: id, ThreadID: e.threadID})
		case e.labelled:
			needLabels = append(needLabels, id)
		}
	}

	// Updated envelopes must carry the *current* labels, which history does
	// not give us reliably (records can be out of order and coalesced).
	if len(needLabels) > 0 {
		found, gone, err := m.fetchLabels(ctx, needLabels)
		if err != nil {
			return nil, err
		}
		for _, id := range needLabels {
			if msg, ok := found[id]; ok {
				out.Updated = append(out.Updated, envelopeOf(msg))
			}
		}
		out.Removed = append(out.Removed, gone...)
		sort.Strings(out.Removed)
	}

	if maxHistory <= start {
		// Nothing happened at all; re-read the profile so we never move the
		// state backwards or leave it unset.
		state, err := m.State(ctx)
		if err != nil {
			return nil, err
		}
		out.NewState = state
		return out, nil
	}
	out.NewState = strconv.FormatUint(maxHistory, 10)
	return out, nil
}

// unknownLabels reports whether any of ids is a label we have not seen in the
// last Mailboxes call — a decent hint that a label was created or renamed.
func (m *Mail) unknownLabels(ids []string) bool {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.knownLabels == nil {
		return false
	}
	for _, id := range ids {
		if _, ok := m.knownLabels[id]; !ok {
			return true
		}
	}
	return false
}
