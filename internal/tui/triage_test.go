package tui

import (
	"context"
	"errors"
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

// fakeFactory hands the engine one in-memory mail provider and one calendar.
type fakeFactory struct {
	mail *fake.Mail
	cal  *fake.Calendar
}

func (f *fakeFactory) Mail(context.Context, config.Account) (provider.MailProvider, error) {
	return f.mail, nil
}

func (f *fakeFactory) Calendar(context.Context, config.Account) (provider.CalendarProvider, error) {
	return f.cal, nil
}

func (f *fakeFactory) Pusher(context.Context, config.Account) (provider.Pusher, bool, error) {
	return nil, false, nil
}

// newTriageDeps wires a real store and a real sync engine to a fake provider,
// so an action taken in the TUI goes down exactly the path `emlcal mail
// archive` takes.
func newTriageDeps(t *testing.T) (Deps, *fake.Mail) {
	t.Helper()
	d, mail, _ := newTriageDepsWithCalendar(t)
	return d, mail
}

// newTriageDepsWithCalendar is newTriageDeps with the fake calendar the
// engine writes to as well, for the actions that land there.
func newTriageDepsWithCalendar(t *testing.T) (Deps, *fake.Mail, *fake.Calendar) {
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
	cal := fake.NewCalendar()
	eng, err := sync.New(sync.Options{
		Store:     st,
		Blobs:     blobs,
		Config:    cfg,
		Providers: &fakeFactory{mail: mail, cal: cal},
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
	}, mail, cal
}

// addTriageMessage indexes one message and gives the fake provider the same
// one, ago before now, so a thread's order on screen is the order it was
// added in.
func addTriageMessage(t *testing.T, d Deps, mail *fake.Mail, remote, subject string, ago time.Duration) {
	t.Helper()
	addTriageMessageIn(t, d, mail, "INBOX", remote, subject, ago)
}

// addTriageMessageIn is addTriageMessage into a mailbox other than the inbox,
// for the triage actions that start from the archive or the trash.
func addTriageMessageIn(t *testing.T, d Deps, mail *fake.Mail, mailbox, remote, subject string, ago time.Duration) {
	t.Helper()
	when := testNow.Add(-ago)
	raw := []byte("From: anna@example.com\r\nSubject: " + subject + "\r\n\r\n" + subject + " body\r\n")
	mail.Add(fake.NewMsg(remote, raw).WithMailboxes(mailbox))
	m := &model.Message{
		AccountID:      "work",
		RemoteID:       remote,
		ThreadID:       "t-" + remote,
		Subject:        subject,
		From:           model.Address{Name: "anna", Email: "anna@example.com"},
		Date:           when,
		Received:       when,
		TextBody:       subject + " body",
		MailboxRemotes: []string{mailbox},
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
	// Undo is offered only after the archive landed, so the row had already
	// been committed; the reload undo starts is the load that lifts it out
	// of gone and puts it back on screen.
	if got := len(r.top().(*mailList).threads); got != 1 {
		t.Errorf("undo did not put the archived row back (%d rows)", got)
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

func TestRestoreFromTheListReachesTheProvider(t *testing.T) {
	d, mail := newTriageDeps(t)
	addTriageMessageIn(t, d, mail, "TRASH", "m1", "Rescue me", time.Hour)

	r := newTestRoot(t, d)
	send(t, r, "M") // inbox -> all, which shows trashed mail too
	if got := len(r.mail[0].(*mailList).threads); got != 1 {
		t.Fatalf("all view has %d threads, want 1", got)
	}

	send(t, r, "i")

	_, boxes, ok := mail.Lookup("m1")
	if !ok {
		t.Fatal("message vanished from the provider")
	}
	if contains(boxes, "TRASH") {
		t.Errorf("after restore the provider still has TRASH: %v", boxes)
	}
	if !contains(boxes, "INBOX") {
		t.Errorf("after restore the provider lacks INBOX: %v", boxes)
	}
	if r.undo == nil {
		t.Error("restore offered no undo")
	}
}

// A message trashed in the inbox is gone, but the all view shows the trash
// too: the row the screen dropped optimistically is still what the store has,
// and once the write lands the next load has to stop hiding it. Without that,
// the row is gone until the view changes -- and restoring, archiving and
// trashing from a view that still holds the message all end the same way.
func TestMailListAllViewKeepsItsRowAfterTheActionLands(t *testing.T) {
	for _, tc := range []struct {
		name    string
		mailbox string
		key     string
	}{
		{"trash", "INBOX", "d"},
		{"archive", "INBOX", "e"},
		{"restore", "TRASH", "i"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			d, mail := newTriageDeps(t)
			addTriageMessageIn(t, d, mail, tc.mailbox, "m1", tc.name, time.Hour)

			r := newTestRoot(t, d)
			send(t, r, "M") // inbox -> all, which shows trash and archive too
			send(t, r, tc.key)
			ml := r.top().(*mailList)
			if got := len(ml.threads); got != 0 {
				t.Fatalf("after %s the list holds %d rows, want the dropped row gone", tc.name, got)
			}

			drain(t, r, ml.reload())
			if got := len(ml.threads); got != 1 {
				t.Fatalf("%s left the all view with %d rows, want the thread still on screen", tc.name, got)
			}
		})
	}
}

// Archiving a thread out of the inbox does not make a reply that lands later
// invisible. The landed row is only hidden from loads that were already in
// flight against the unpatched index; a reply that makes the thread match the
// query again has to show up on the next load.
func TestMailListAReplyToAnArchivedThreadShowsUp(t *testing.T) {
	d, mail := newTriageDeps(t)
	addTriageMessage(t, d, mail, "m1", "Old question", 2*time.Hour)

	r := newTestRoot(t, d)
	send(t, r, "e")
	ml := r.top().(*mailList)
	if got := len(ml.threads); got != 0 {
		t.Fatalf("after archive the list holds %d rows, want none", got)
	}

	addTriageMessage(t, d, mail, "m2", "Re: Old question", time.Minute)
	inThread(t, d, "t-m1", "m2") // the reply lands in the thread that was archived

	drain(t, r, ml.reload())
	if got := len(ml.threads); got != 1 {
		t.Fatalf("the reply left the inbox with %d rows, want the thread back", got)
	}
}

// The result belongs to the screen that dropped the row, not to whatever is
// on top when it arrives. In the gap before a write answers the person can
// open a thread or switch to the calendar; the list still has a pending drop
// of its own to commit, and leaving it uncommitted keeps the row hidden until
// the view changes.
func TestMailListOrphanedDropLandsOnTheListThatDroppedIt(t *testing.T) {
	d, mail := newTriageDeps(t)
	addTriageMessage(t, d, mail, "m1", "Trash me", time.Hour)

	r := newTestRoot(t, d)
	send(t, r, "M") // all view: a trashed thread still belongs to it
	ml := r.top().(*mailList)

	cmd := r.triage("trash")
	r.push(newThreadView(d, "work", "t-m1", "Trash me", false))
	r.Update(cmd()) // the answer comes while the thread is on top
	settle(t, r)
	r.pop()

	drain(t, r, ml.reload())
	if got := len(ml.threads); got != 1 {
		t.Fatalf("the trashed row is still hidden after moving on and back (%d rows)", got)
	}
}

// A failed write puts the row back even when the screen that dropped it is no
// longer the top one. Otherwise the rollback leaves the message hidden until a
// view change, which is the state the person came back to.
func TestMailListOrphanedDropRestoresOnTheListWhenTheWriteFails(t *testing.T) {
	d := newTestDeps(t, "work")
	addMessage(t, d, "work", "w1", "t1", "Trash me", "anna", time.Hour, false)

	r := newTestRoot(t, d)
	ml := r.top().(*mailList)
	key := ml.dropSelected()
	r.push(newThreadView(d, "work", "t1", "Trash me", false))

	r.Update(applied{action: "trash", drop: ml, key: key, err: errors.New("rejected")})
	r.pop()

	drain(t, r, ml.reload())
	if got := len(ml.threads); got != 1 {
		t.Fatalf("the failed trash left the row hidden (%d rows)", got)
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

// Undoing a trash taken in the thread puts the message back where it was on
// screen. The commit that landed took it off removed; gone is what still hides
// it, and undo is what lifts it until the reload brings it back.
func TestTrashFromTheThreadUndoShowsTheMessageAgain(t *testing.T) {
	d, mail := newTriageDeps(t)
	addTriageMessage(t, d, mail, "m1", "Older", 2*time.Hour)
	addTriageMessage(t, d, mail, "m2", "Newer", time.Hour)
	inThread(t, d, "t-shared", "m1", "m2")

	r := newTestRoot(t, d)
	send(t, r, "enter")
	tv := r.top().(*threadView)
	send(t, r, "d")
	if len(tv.messages) != 1 {
		t.Fatalf("after trash the thread holds %d messages, want 1", len(tv.messages))
	}

	send(t, r, "z")
	if _, boxes, _ := mail.Lookup("m2"); !contains(boxes, "INBOX") {
		t.Errorf("after undo the message is not back in INBOX: %v", boxes)
	}
	if len(tv.messages) != 2 {
		t.Fatalf("after undo the thread holds %d messages, want both", len(tv.messages))
	}
}

// A deferred write the provider later rejects has already committed its drop.
// The engine rolled the index back, but the thread view still hides the
// message in gone: the settled error has to lift it before reloading, or the
// conversation stays one message short.
func TestThreadViewSettledFailureShowsTheMessageAgain(t *testing.T) {
	d, mail := newTriageDeps(t)
	addTriageMessage(t, d, mail, "m1", "Older", 2*time.Hour)
	addTriageMessage(t, d, mail, "m2", "Newer", time.Hour)
	inThread(t, d, "t-shared", "m1", "m2")

	r := newTestRoot(t, d)
	send(t, r, "enter")
	tv := r.top().(*threadView)
	cmd := r.triage("trash")
	res := cmd().(applied)
	r.Update(res) // the drop lands, so commit takes it off removed
	settle(t, r)
	if len(tv.messages) != 1 {
		t.Fatalf("after the drop the thread holds %d messages, want 1", len(tv.messages))
	}

	drain(t, r, r.onSettled(applied{
		action: "trash", drop: tv, key: res.key, settled: true, err: errors.New("rejected"),
	}))
	if len(tv.messages) != 2 {
		t.Fatalf("the rejected write left the thread at %d messages, want both back", len(tv.messages))
	}
}

// Two drops in one thread, results delivered out of order, and the first
// write rejected: the settled error has to lift the message it dropped. The
// last one that landed is not the one that failed, so unhide must go by the
// key carried in the result, not by whatever commit saw last.
func TestThreadViewSettledFailureUnhidesItsOwnMessage(t *testing.T) {
	d, mail := newTriageDeps(t)
	addTriageMessage(t, d, mail, "m1", "Oldest", 3*time.Hour)
	addTriageMessage(t, d, mail, "m2", "Middle", 2*time.Hour)
	addTriageMessage(t, d, mail, "m3", "Newest", time.Hour)
	inThread(t, d, "t-shared", "m1", "m2", "m3")

	r := newTestRoot(t, d)
	send(t, r, "enter")
	tv := r.top().(*threadView)
	if len(tv.messages) != 3 {
		t.Fatalf("thread has %d messages, want 3", len(tv.messages))
	}

	first := r.triage("trash")  // drops m3
	second := r.triage("trash") // drops m2, leaving m1
	resFirst := first().(applied)
	resSecond := second().(applied)
	if resFirst.key == resSecond.key {
		t.Fatalf("both drops use the key %q", resFirst.key)
	}
	// The second write answers first; the first answers after it.
	r.Update(resSecond)
	r.Update(resFirst)
	settle(t, r)
	if got := len(tv.messages); got != 1 || tv.messages[0].RemoteID != "m1" {
		t.Fatalf("after the drops the thread holds %+v, want only m1", tv.messages)
	}

	drain(t, r, r.onSettled(applied{
		action: "trash", drop: tv, key: resFirst.key, settled: true, err: errors.New("rejected"),
	}))
	if got := len(tv.messages); got != 2 ||
		tv.messages[0].RemoteID != "m3" || tv.messages[1].RemoteID != "m1" {
		t.Fatalf("the rejected write uncovered %+v, want m3 back and m2 still gone", tv.messages)
	}
}

// The thread view has the same out-of-order problem as the list: a failed
// result has to put back its own message, not whichever drop happened last.
func TestThreadViewOutOfOrderResultsRestoreTheRightMessage(t *testing.T) {
	d, mail := newTriageDeps(t)
	addTriageMessage(t, d, mail, "m1", "Oldest", 3*time.Hour)
	addTriageMessage(t, d, mail, "m2", "Middle", 2*time.Hour)
	addTriageMessage(t, d, mail, "m3", "Newest", time.Hour)
	inThread(t, d, "t-shared", "m1", "m2", "m3")

	r := newTestRoot(t, d)
	send(t, r, "enter")
	tv := r.top().(*threadView)

	first := tv.dropSelected()  // m3
	second := tv.dropSelected() // m2
	tv.commit(second)
	tv.restore(first)

	if got := len(tv.removed); got != 0 {
		t.Errorf("out-of-order results left %d drops pending", got)
	}
	if tv.gone[first] {
		t.Error("the rejected message is still hidden")
	}
	if !tv.gone[second] {
		t.Error("the message whose write landed was lifted out of gone")
	}
	if got := len(tv.messages); got != 2 ||
		tv.messages[0].RemoteID != "m3" || tv.messages[1].RemoteID != "m1" {
		t.Fatalf("the wrong message was restored: %+v", tv.messages)
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

// ⌫ and delete are what a hand off Mail.app or Thunderbird reaches for. They
// are the same trash d is -- undo included -- not a second, quieter one.
func TestTheDeleteKeysTrashLikeD(t *testing.T) {
	for _, k := range []string{"delete", "backspace"} {
		d, mail := newTriageDeps(t)
		addTriageMessage(t, d, mail, "m1", "Trash me", time.Hour)

		r := newTestRoot(t, d)
		send(t, r, k)

		_, boxes, ok := mail.Lookup("m1")
		if !ok {
			t.Fatalf("%s: message vanished from the provider", k)
		}
		if !contains(boxes, "TRASH") {
			t.Errorf("%s: after trash the provider lacks TRASH: %v", k, boxes)
		}
		if r.undo == nil {
			t.Errorf("%s: trash offered no undo", k)
		}
	}
}
