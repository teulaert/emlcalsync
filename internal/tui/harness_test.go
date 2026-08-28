package tui

import (
	"context"
	"io"
	"log/slog"
	"path/filepath"
	"testing"
	"time"

	tea "charm.land/bubbletea/v2"

	"github.com/teulaert/emlcalsync/internal/model"
	"github.com/teulaert/emlcalsync/internal/store"
)

// testNow is pinned so RelTime output is stable, matching what the cli and e2e
// harnesses do.
var testNow = time.Date(2026, 8, 25, 12, 0, 0, 0, time.UTC)

func newTestDeps(t *testing.T, accounts ...string) Deps {
	t.Helper()
	st, err := store.Open(filepath.Join(t.TempDir(), "index.db"))
	if err != nil {
		t.Fatalf("store.Open: %v", err)
	}
	st.SetLogger(slog.New(slog.NewTextHandler(io.Discard, &slog.HandlerOptions{Level: slog.LevelError})))
	t.Cleanup(func() { st.Close() })

	ctx := context.Background()
	for _, a := range accounts {
		if err := st.UpsertAccount(ctx, &model.Account{
			ID: a, Vendor: model.VendorFastmail, Email: a + "@example.com", CreatedAt: testNow,
		}); err != nil {
			t.Fatalf("UpsertAccount %s: %v", a, err)
		}
		if err := st.ReplaceMailboxes(ctx, a, []model.Mailbox{
			{RemoteID: "inbox", Name: "Inbox", Role: model.RoleInbox, SortOrder: 1},
			{RemoteID: "archive", Name: "Archive", Role: model.RoleArchive, SortOrder: 2},
			{RemoteID: "trash", Name: "Trash", Role: model.RoleTrash, SortOrder: 3},
			{RemoteID: "drafts", Name: "Drafts", Role: model.RoleDrafts, SortOrder: 4},
		}); err != nil {
			t.Fatalf("ReplaceMailboxes %s: %v", a, err)
		}
	}
	return Deps{
		Store:    st,
		Accounts: accounts,
		Loc:      time.UTC,
		Now:      func() time.Time { return testNow },
		Logger:   slog.New(slog.DiscardHandler),
	}
}

// addMessage indexes one message in the inbox.
func addMessage(t *testing.T, d Deps, account, remote, thread, subject, from string, ago time.Duration, unread bool) {
	t.Helper()
	when := testNow.Add(-ago)
	m := &model.Message{
		AccountID:      account,
		RemoteID:       remote,
		ThreadID:       thread,
		Subject:        subject,
		From:           model.Address{Name: from, Email: from + "@example.com"},
		Date:           when,
		Received:       when,
		Snippet:        subject + " body",
		TextBody:       subject + " body\n\nOn earlier, someone wrote:\n> quoted",
		Flags:          model.Flags{Unread: unread},
		MailboxRemotes: []string{"inbox"},
		IndexedAt:      testNow,
	}
	if _, err := d.Store.UpsertMessage(context.Background(), m, nil); err != nil {
		t.Fatalf("UpsertMessage %s: %v", remote, err)
	}
}

// addDraft indexes one unsent message in the drafts mailbox: from the account
// itself, addressed to someone else, the way a provider hands a draft over.
func addDraft(t *testing.T, d Deps, account, remote, thread, subject string, ago time.Duration) {
	t.Helper()
	when := testNow.Add(-ago)
	m := &model.Message{
		AccountID:      account,
		RemoteID:       remote,
		ThreadID:       thread,
		Subject:        subject,
		From:           model.Address{Name: account, Email: account + "@example.com"},
		To:             []model.Address{{Email: "anna@example.com"}},
		Date:           when,
		Received:       when,
		Snippet:        subject + " body",
		TextBody:       subject + " body",
		Flags:          model.Flags{Draft: true},
		MailboxRemotes: []string{"drafts"},
		IndexedAt:      testNow,
	}
	if _, err := d.Store.UpsertMessage(context.Background(), m, nil); err != nil {
		t.Fatalf("UpsertMessage %s: %v", remote, err)
	}
}

// pump runs a screen's command chain until it stops producing messages, so a
// test can assert on the state a load actually settled into.
func pump(t *testing.T, s screen, cmd tea.Cmd, k keymap, w, h int) screen {
	t.Helper()
	for i := 0; cmd != nil && i < 20; i++ {
		msg := cmd()
		if msg == nil {
			return s
		}
		if batch, ok := msg.(tea.BatchMsg); ok {
			var next tea.Cmd
			for _, c := range batch {
				if c == nil {
					continue
				}
				m := c()
				s, next = s.Update(m, k, w, h)
			}
			cmd = next
			continue
		}
		s, cmd = s.Update(msg, k, w, h)
	}
	return s
}

func keyPress(s string) tea.KeyPressMsg {
	switch s {
	case "enter":
		return tea.KeyPressMsg{Code: tea.KeyEnter}
	case "esc":
		return tea.KeyPressMsg{Code: tea.KeyEscape}
	case "backspace":
		return tea.KeyPressMsg{Code: tea.KeyBackspace}
	case "tab":
		return tea.KeyPressMsg{Code: tea.KeyTab}
	}
	return tea.KeyPressMsg{Code: rune(s[0]), Text: s}
}

// addEvent puts one calendar, one event and one occurrence in place.
func addEvent(t *testing.T, d Deps, account, calRemote, calName, remote, title string, start time.Time, dur time.Duration) {
	t.Helper()
	ctx := context.Background()
	cals, err := d.Store.ListCalendars(ctx, []string{account})
	if err != nil {
		t.Fatalf("ListCalendars: %v", err)
	}
	if len(cals) == 0 {
		if err := d.Store.ReplaceCalendars(ctx, account, []model.Calendar{
			{AccountID: account, RemoteID: calRemote, Name: calName, Primary: true, AccessRole: "owner"},
		}); err != nil {
			t.Fatalf("ReplaceCalendars: %v", err)
		}
	}
	cal, err := d.Store.GetCalendarByRemote(ctx, account, calRemote)
	if err != nil {
		t.Fatalf("GetCalendarByRemote: %v", err)
	}
	ev := &model.Event{
		CalendarID:     cal.ID,
		AccountID:      account,
		CalendarRemote: calRemote,
		RemoteID:       remote,
		UID:            remote,
		Title:          title,
		Start:          start,
		End:            start.Add(dur),
		Status:         model.StatusConfirmed,
		MyResponse:     model.PartNeedsAction,
		RawJSON:        []byte(`{}`),
		Updated:        testNow,
	}
	id, err := d.Store.UpsertEvent(ctx, ev)
	if err != nil {
		t.Fatalf("UpsertEvent: %v", err)
	}
	if err := d.Store.ReplaceOccurrences(ctx, id, []model.Occurrence{
		{EventID: id, Start: ev.Start, End: ev.End},
	}); err != nil {
		t.Fatalf("ReplaceOccurrences: %v", err)
	}
}

// moveOutOfInbox re-indexes a message into the trash, which is what the list
// sees once a trash has gone through and the daemon has re-synced.
func moveOutOfInbox(t *testing.T, d Deps, account, remote string) {
	t.Helper()
	ctx := context.Background()
	m, err := d.Store.GetMessage(ctx, account, remote)
	if err != nil {
		t.Fatalf("GetMessage %s: %v", remote, err)
	}
	m.MailboxRemotes = []string{"trash"}
	if _, err := d.Store.UpsertMessage(ctx, m, nil); err != nil {
		t.Fatalf("UpsertMessage %s: %v", remote, err)
	}
}
