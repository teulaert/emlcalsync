package tui

import (
	"context"
	"io"
	"log/slog"
	"path/filepath"
	"testing"
	"time"

	"github.com/teulaert/emlcalsync/internal/blob"
	"github.com/teulaert/emlcalsync/internal/config"
	"github.com/teulaert/emlcalsync/internal/model"
	"github.com/teulaert/emlcalsync/internal/provider"
	"github.com/teulaert/emlcalsync/internal/provider/fake"
	"github.com/teulaert/emlcalsync/internal/store"
	"github.com/teulaert/emlcalsync/internal/sync"
)

// fakeFactory hands the engine one in-memory mail provider.
type fakeFactory struct{ mail *fake.Mail }

func (f *fakeFactory) Mail(context.Context, config.Account) (provider.MailProvider, error) {
	return f.mail, nil
}

func (f *fakeFactory) Calendar(context.Context, config.Account) (provider.CalendarProvider, error) {
	return fake.NewCalendar(), nil
}

func (f *fakeFactory) Pusher(context.Context, config.Account) (provider.Pusher, bool, error) {
	return nil, false, nil
}

// newTriageDeps wires a real store and a real sync engine to a fake provider,
// so an action taken in the TUI goes down exactly the path `emlcal mail
// archive` takes.
func newTriageDeps(t *testing.T) (Deps, *fake.Mail) {
	t.Helper()
	dir := t.TempDir()
	st, err := store.Open(filepath.Join(dir, "index.db"))
	if err != nil {
		t.Fatalf("store.Open: %v", err)
	}
	st.SetLogger(slog.New(slog.NewTextHandler(io.Discard, &slog.HandlerOptions{Level: slog.LevelError})))
	t.Cleanup(func() { st.Close() })

	blobs, err := blob.Open(filepath.Join(dir, "blobs"))
	if err != nil {
		t.Fatalf("blob.Open: %v", err)
	}

	acct := config.NewAccount("work", "work@example.com", model.VendorFastmail)
	cfg := &config.Config{Accounts: []config.Account{acct}}

	ctx := context.Background()
	if err := st.UpsertAccount(ctx, &model.Account{
		ID: "work", Vendor: model.VendorFastmail, Email: "work@example.com", CreatedAt: testNow,
	}); err != nil {
		t.Fatalf("UpsertAccount: %v", err)
	}
	// Mirror the fake provider's mailbox ids so the engine's role lookups
	// resolve to something the provider recognises.
	if err := st.ReplaceMailboxes(ctx, "work", []model.Mailbox{
		{RemoteID: "INBOX", Name: "Inbox", Role: model.RoleInbox, SortOrder: 1},
		{RemoteID: "ARCHIVE", Name: "Archive", Role: model.RoleArchive, SortOrder: 2},
		{RemoteID: "TRASH", Name: "Trash", Role: model.RoleTrash, SortOrder: 3},
	}); err != nil {
		t.Fatalf("ReplaceMailboxes: %v", err)
	}

	mail := fake.NewMail()
	eng, err := sync.New(sync.Options{
		Store:     st,
		Blobs:     blobs,
		Config:    cfg,
		Providers: &fakeFactory{mail: mail},
		Logger:    slog.New(slog.DiscardHandler),
	})
	if err != nil {
		t.Fatalf("sync.New: %v", err)
	}

	return Deps{
		Store:    st,
		Engine:   eng,
		Config:   cfg,
		Accounts: []string{"work"},
		Loc:      time.UTC,
		Now:      func() time.Time { return testNow },
		Logger:   slog.New(slog.DiscardHandler),
	}, mail
}

func addTriageMessage(t *testing.T, d Deps, mail *fake.Mail, remote, subject string) {
	t.Helper()
	raw := []byte("From: anna@example.com\r\nSubject: " + subject + "\r\n\r\n" + subject + " body\r\n")
	mail.Add(fake.NewMsg(remote, raw).WithMailboxes("INBOX"))
	m := &model.Message{
		AccountID:      "work",
		RemoteID:       remote,
		ThreadID:       "t-" + remote,
		Subject:        subject,
		From:           model.Address{Name: "anna", Email: "anna@example.com"},
		Date:           testNow.Add(-time.Hour),
		Received:       testNow.Add(-time.Hour),
		TextBody:       subject + " body",
		MailboxRemotes: []string{"INBOX"},
		IndexedAt:      testNow,
	}
	if _, err := d.Store.UpsertMessage(context.Background(), m, nil); err != nil {
		t.Fatalf("UpsertMessage: %v", err)
	}
}

func TestArchiveFromTheListReachesTheProviderAndUndoRestoresIt(t *testing.T) {
	d, mail := newTriageDeps(t)
	addTriageMessage(t, d, mail, "m1", "Archive me")

	r := newTestRoot(t, d)
	if got := len(r.mail[0].(*mailList).threads); got != 1 {
		t.Fatalf("list has %d threads, want 1", got)
	}

	send(t, r, "e")

	_, boxes, ok := mail.Lookup("m1")
	if !ok {
		t.Fatal("message vanished from the provider")
	}
	if contains(boxes, "INBOX") {
		t.Errorf("after archive the provider still has INBOX: %v", boxes)
	}
	if !contains(boxes, "ARCHIVE") {
		t.Errorf("after archive the provider lacks ARCHIVE: %v", boxes)
	}
	if r.undo == nil {
		t.Fatal("archive offered no undo")
	}
	if got := len(r.mail[0].(*mailList).threads); got != 0 {
		t.Errorf("archived row is still in the list (%d rows)", got)
	}

	send(t, r, "z")

	_, boxes, _ = mail.Lookup("m1")
	if !contains(boxes, "INBOX") {
		t.Errorf("after undo the provider lacks INBOX: %v", boxes)
	}
	if contains(boxes, "ARCHIVE") {
		t.Errorf("after undo the provider still has ARCHIVE: %v", boxes)
	}
}

func TestTrashFromTheListReachesTheProvider(t *testing.T) {
	d, mail := newTriageDeps(t)
	addTriageMessage(t, d, mail, "m1", "Trash me")

	r := newTestRoot(t, d)
	send(t, r, "d")

	_, boxes, ok := mail.Lookup("m1")
	if !ok {
		t.Fatal("message vanished from the provider")
	}
	if !contains(boxes, "TRASH") {
		t.Errorf("after trash the provider lacks TRASH: %v", boxes)
	}
	if r.undo == nil {
		t.Error("trash offered no undo")
	}
}

func TestStarFromTheListReachesTheProvider(t *testing.T) {
	d, mail := newTriageDeps(t)
	addTriageMessage(t, d, mail, "m1", "Star me")

	r := newTestRoot(t, d)
	send(t, r, "s")

	flags, _, ok := mail.Lookup("m1")
	if !ok {
		t.Fatal("message vanished from the provider")
	}
	if !flags.Flagged {
		t.Error("star did not reach the provider")
	}

	send(t, r, "z")
	flags, _, _ = mail.Lookup("m1")
	if flags.Flagged {
		t.Error("undo did not clear the star")
	}
}

func contains(s []string, v string) bool {
	for _, x := range s {
		if x == v {
			return true
		}
	}
	return false
}
