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
		{RemoteID: "DRAFTS", Name: "Drafts", Role: model.RoleDrafts, SortOrder: 4},
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

// addTriageMessage indexes one message and gives the fake provider the same
// one, ago before now, so a thread's order on screen is the order it was
// added in.
func addTriageMessage(t *testing.T, d Deps, mail *fake.Mail, remote, subject string, ago time.Duration) {
	t.Helper()
	when := testNow.Add(-ago)
	raw := []byte("From: anna@example.com\r\nSubject: " + subject + "\r\n\r\n" + subject + " body\r\n")
	mail.Add(fake.NewMsg(remote, raw).WithMailboxes("INBOX"))
	m := &model.Message{
		AccountID:      "work",
		RemoteID:       remote,
		ThreadID:       "t-" + remote,
		Subject:        subject,
		From:           model.Address{Name: "anna", Email: "anna@example.com"},
		Date:           when,
		Received:       when,
		TextBody:       subject + " body",
		MailboxRemotes: []string{"INBOX"},
		IndexedAt:      testNow,
	}
	if _, err := d.Store.UpsertMessage(context.Background(), m, nil); err != nil {
		t.Fatalf("UpsertMessage: %v", err)
	}
}

// inThread re-files messages under one thread id, so a triage test can act on
// a conversation rather than a run of one-message threads.
func inThread(t *testing.T, d Deps, thread string, remotes ...string) {
	t.Helper()
	ctx := context.Background()
	for _, remote := range remotes {
		m, err := d.Store.GetMessage(ctx, "work", remote)
		if err != nil {
			t.Fatalf("GetMessage %s: %v", remote, err)
		}
		m.ThreadID = thread
		if _, err := d.Store.UpsertMessage(ctx, m, nil); err != nil {
			t.Fatalf("UpsertMessage %s: %v", remote, err)
		}
	}
}

func TestArchiveFromTheListReachesTheProviderAndUndoRestoresIt(t *testing.T) {
	d, mail := newTriageDeps(t)
	addTriageMessage(t, d, mail, "m1", "Archive me", time.Hour)

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
	addTriageMessage(t, d, mail, "m1", "Trash me", time.Hour)

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
	addTriageMessage(t, d, mail, "m1", "Star me", time.Hour)

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

// Trashing the message being read has to move on to the next one. Staying put
// leaves a message on screen that is no longer where the person put it.
func TestTrashFromTheReaderMovesToTheNextMessage(t *testing.T) {
	d, mail := newTriageDeps(t)
	addTriageMessage(t, d, mail, "m1", "Older", 2*time.Hour)
	addTriageMessage(t, d, mail, "m2", "Newer", time.Hour)
	inThread(t, d, "t-shared", "m1", "m2")

	r := newTestRoot(t, d)
	send(t, r, "enter") // the thread
	send(t, r, "enter") // the newest message, m2

	rd, ok := r.top().(*reader)
	if !ok {
		t.Fatalf("top = %T, want the reader", r.top())
	}
	if rd.remote != "m2" {
		t.Fatalf("reading %s, want the newest message m2", rd.remote)
	}

	send(t, r, "d")

	rd, ok = r.top().(*reader)
	if !ok {
		t.Fatalf("after trash top = %T, want to still be reading", r.top())
	}
	if rd.remote != "m1" {
		t.Errorf("after trash the reader shows %s, want the next message m1", rd.remote)
	}
	if _, boxes, _ := mail.Lookup("m2"); !contains(boxes, "TRASH") {
		t.Errorf("the trashed message is not in TRASH: %v", boxes)
	}
	if r.undo == nil {
		t.Error("trash from the reader offered no undo")
	}
}

// The last message of a thread has no next one: the reader and the thread both
// close, and the row goes with them.
func TestTrashFromTheReaderClosesAThreadWithNothingLeft(t *testing.T) {
	d, mail := newTriageDeps(t)
	addTriageMessage(t, d, mail, "m1", "Only one", time.Hour)

	r := newTestRoot(t, d)
	send(t, r, "enter")
	send(t, r, "enter")
	if _, ok := r.top().(*reader); !ok {
		t.Fatalf("top = %T, want the reader", r.top())
	}

	send(t, r, "d")

	ml, ok := r.top().(*mailList)
	if !ok {
		t.Fatalf("after trash top = %T, want to be back on the list", r.top())
	}
	if got := len(ml.threads); got != 0 {
		t.Errorf("the trashed thread is still in the list (%d rows)", got)
	}
	if _, boxes, _ := mail.Lookup("m1"); !contains(boxes, "TRASH") {
		t.Errorf("the trashed message is not in TRASH: %v", boxes)
	}
}

// The thread view is where a message is read now that threads open expanded,
// so trashing from there is the common case. The reload that lands a second
// later must not bring the message back: GetThread hands over the whole
// conversation whatever mailbox each message sits in.
func TestTrashFromTheThreadDropsTheMessageForGood(t *testing.T) {
	d, mail := newTriageDeps(t)
	addTriageMessage(t, d, mail, "m1", "Older", 2*time.Hour)
	addTriageMessage(t, d, mail, "m2", "Newer", time.Hour)
	inThread(t, d, "t-shared", "m1", "m2")

	r := newTestRoot(t, d)
	send(t, r, "enter")
	tv := r.top().(*threadView)
	if len(tv.messages) != 2 {
		t.Fatalf("thread has %d messages, want 2", len(tv.messages))
	}

	send(t, r, "d")

	if _, boxes, _ := mail.Lookup("m2"); !contains(boxes, "TRASH") {
		t.Errorf("the trashed message is not in TRASH: %v", boxes)
	}
	drain(t, r, tv.reload())
	if len(tv.messages) != 1 || tv.messages[0].RemoteID != "m1" {
		t.Errorf("after the reload the thread holds %d messages, want only m1", len(tv.messages))
	}
}

// Trashing the only message a thread has leaves nothing to look at, so the
// thread closes and the row goes with it.
func TestTrashFromTheThreadClosesItWhenNothingIsLeft(t *testing.T) {
	d, mail := newTriageDeps(t)
	addTriageMessage(t, d, mail, "m1", "Only one", time.Hour)

	r := newTestRoot(t, d)
	send(t, r, "enter")
	if _, ok := r.top().(*threadView); !ok {
		t.Fatalf("top = %T, want the thread", r.top())
	}

	send(t, r, "d")

	ml, ok := r.top().(*mailList)
	if !ok {
		t.Fatalf("after trash top = %T, want to be back on the list", r.top())
	}
	if got := len(ml.threads); got != 0 {
		t.Errorf("the trashed thread is still in the list (%d rows)", got)
	}
	if r.undo == nil {
		t.Fatal("trash from the thread offered no undo")
	}

	send(t, r, "z")
	if _, boxes, _ := mail.Lookup("m1"); !contains(boxes, "INBOX") {
		t.Errorf("after undo the provider lacks INBOX: %v", boxes)
	}
	if got := len(r.top().(*mailList).threads); got != 1 {
		t.Errorf("undo did not put the row back (%d rows)", got)
	}
}
