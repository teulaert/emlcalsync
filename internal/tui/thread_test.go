package tui

import (
	"context"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/teulaert/emlcalsync/internal/model"
	"github.com/teulaert/emlcalsync/internal/provider/fake"
)

// openThreadView loads one thread the way the root would, at a size big enough
// to hold the whole conversation.
func openThreadView(t *testing.T, d Deps, expanded bool) *threadView {
	t.Helper()
	tv := newThreadView(d, "work", "t1", "Hello", expanded)
	s := pump(t, tv, tv.Init(), defaultKeys(), 80, 20)
	return s.(*threadView)
}

// view renders and strips the styling, so a test reads what the terminal shows.
func view(s screen, w, h int) string { return stripANSI(s.View(w, h)) }

func TestThreadOpensOnTheMessageTextNotAnIndex(t *testing.T) {
	d := newTestDeps(t, "work")
	addMessage(t, d, "work", "w1", "t1", "First", "anna", 2*time.Hour, false)
	addMessage(t, d, "work", "w2", "t1", "Second", "bob", time.Hour, false)

	tv := openThreadView(t, d, true)
	got := view(tv, 80, 20)
	for _, want := range []string{"First body", "Second body", "anna@example.com", "bob@example.com"} {
		if !strings.Contains(got, want) {
			t.Errorf("expanded thread is missing %q:\n%s", want, got)
		}
	}
	// The bodies go through StripQuotes, the same as the reader's.
	if strings.Contains(got, "quoted") {
		t.Errorf("expanded thread kept the quoted reply:\n%s", got)
	}
}

// The newest message is the one the thread was opened for, so it goes on top —
// the reverse of the order the store (and `emlcal mail thread`) hands back.
func TestThreadPutsTheNewestMessageOnTop(t *testing.T) {
	d := newTestDeps(t, "work")
	addMessage(t, d, "work", "w1", "t1", "First", "anna", 3*time.Hour, false)
	addMessage(t, d, "work", "w2", "t1", "Second", "bob", 2*time.Hour, false)
	addMessage(t, d, "work", "w3", "t1", "Third", "cara", time.Hour, false)

	for _, expanded := range []bool{true, false} {
		tv := openThreadView(t, d, expanded)
		got := view(tv, 80, 30)
		first, last := strings.Index(got, "Third"), strings.Index(got, "First")
		if first < 0 || last < 0 {
			t.Fatalf("expanded=%v: thread is missing messages:\n%s", expanded, got)
		}
		if first > last {
			t.Errorf("expanded=%v: the oldest message is above the newest:\n%s", expanded, got)
		}
	}
}

func TestThreadCollapsesToOneRowPerMessageAndBack(t *testing.T) {
	d := newTestDeps(t, "work")
	addMessage(t, d, "work", "w1", "t1", "First", "anna", 2*time.Hour, false)
	addMessage(t, d, "work", "w2", "t1", "Second", "bob", time.Hour, false)

	r := newTestRoot(t, d)
	send(t, r, "enter")
	tv := r.top().(*threadView)
	if !tv.expanded {
		t.Fatal("a thread should open expanded")
	}

	send(t, r, "t")
	if tv.expanded || r.threadExpanded {
		t.Fatal("t did not collapse the thread")
	}
	rows := strings.Split(view(tv, 80, 20), "\n")
	var filled int
	for _, l := range rows {
		if strings.TrimSpace(l) != "" {
			filled++
		}
	}
	if filled != 2 {
		t.Errorf("collapsed thread drew %d rows, want one per message:\n%s", filled, view(tv, 80, 20))
	}

	send(t, r, "t")
	if !tv.expanded || !r.threadExpanded {
		t.Error("t did not expand the thread again")
	}
}

// The mode is a preference, not a property of one thread: collapsing and going
// back to the list has to leave the next thread collapsed too.
func TestThreadModeSurvivesTheNextThread(t *testing.T) {
	d := newTestDeps(t, "work")
	addMessage(t, d, "work", "w1", "t1", "First", "anna", 2*time.Hour, false)
	addMessage(t, d, "work", "w2", "t2", "Other", "bob", time.Hour, false)

	r := newTestRoot(t, d)
	send(t, r, "enter")
	send(t, r, "t")
	send(t, r, "esc")
	send(t, r, "j")
	send(t, r, "enter")

	tv, ok := r.top().(*threadView)
	if !ok {
		t.Fatalf("top is %T, want *threadView", r.top())
	}
	if tv.expanded {
		t.Error("the next thread opened expanded again")
	}
}

// Reading starts where there is something to read: the newest unread message,
// even when a newer one has already been read.
func TestThreadStartsAtTheNewestUnreadMessage(t *testing.T) {
	d := newTestDeps(t, "work")
	addLongMessage(t, d, "w1", "First", "anna", 3*time.Hour, false)
	addLongMessage(t, d, "w2", "Second", "bob", 2*time.Hour, true)
	addLongMessage(t, d, "w3", "Third", "cara", time.Hour, false)

	// On screen: Third, Second, First — so the unread one is in the middle.
	tv := openThreadView(t, d, true)
	if tv.cursor != 1 {
		t.Errorf("cursor is on message %d, want the unread one (1)", tv.cursor)
	}
	got := view(tv, 80, 6)
	if tv.off != tv.startOf(1) {
		t.Errorf("offset %d does not sit at the unread message (%d)", tv.off, tv.startOf(1))
	}
	if !strings.Contains(got, "Second body") {
		t.Errorf("the unread message is not on screen:\n%s", got)
	}
	if strings.Contains(got, "Third body") {
		t.Errorf("the thread opened above the unread message:\n%s", got)
	}
}

// With everything read, the newest message is the one worth showing, and it is
// already at the top.
func TestThreadWithNothingUnreadStartsAtTheNewest(t *testing.T) {
	d := newTestDeps(t, "work")
	addMessage(t, d, "work", "w1", "t1", "First", "anna", 2*time.Hour, false)
	addMessage(t, d, "work", "w2", "t1", "Second", "bob", time.Hour, false)

	tv := openThreadView(t, d, true)
	if tv.cursor != 0 || tv.messages[0].Subject != "Second" {
		t.Errorf("cursor is on message %d (%q), want the newest one on top",
			tv.cursor, tv.messages[tv.cursor].Subject)
	}
}

func TestExpandedThreadMovesByMessageAndScrollsByLine(t *testing.T) {
	d := newTestDeps(t, "work")
	addLongMessage(t, d, "w1", "First", "anna", 2*time.Hour, false)
	addLongMessage(t, d, "w2", "Second", "bob", time.Hour, false)

	tv := openThreadView(t, d, true)
	k := defaultKeys()
	press := func(s string) { tv.Update(keyPress(s), k, 80, 10) }

	press("g")
	if tv.off != 0 || tv.cursor != 0 {
		t.Fatalf("g left off=%d cursor=%d, want the top of the first message", tv.off, tv.cursor)
	}
	press("J")
	if tv.off != 1 {
		t.Errorf("J scrolled to %d, want one line down", tv.off)
	}
	if tv.cursor != 0 {
		t.Errorf("one line down changed the current message to %d", tv.cursor)
	}
	press("j")
	if tv.cursor != 1 || tv.off != tv.startOf(1) {
		t.Errorf("j left off=%d cursor=%d, want the start of message 1 (%d)", tv.off, tv.cursor, tv.startOf(1))
	}
	// Message 1 is the older one: down the screen is back in time.
	if got := view(tv, 80, 10); !strings.Contains(got, "First body") {
		t.Errorf("j did not bring the next message into view:\n%s", got)
	}
	press("k")
	if tv.cursor != 0 || tv.off != 0 {
		t.Errorf("k left off=%d cursor=%d, want back on message 0", tv.off, tv.cursor)
	}
}

// The bug this replaced: the current message was read back off the scroll
// position, so j and k did nothing whenever the document could not scroll —
// which is every thread that fits on one screen, and every thread already
// scrolled to its end.
func TestExpandedThreadMovesEvenWhenItCannotScroll(t *testing.T) {
	d := newTestDeps(t, "work")
	addMessage(t, d, "work", "w1", "t1", "First", "anna", 3*time.Hour, false)
	addMessage(t, d, "work", "w2", "t1", "Second", "bob", 2*time.Hour, false)
	addMessage(t, d, "work", "w3", "t1", "Third", "cara", time.Hour, false)

	tv := openThreadView(t, d, true)
	k := defaultKeys()
	if got := view(tv, 80, 40); strings.Count(got, "\n")+1 != 40 {
		t.Fatalf("the whole thread should fit in the window:\n%s", got)
	}
	if tv.off != 0 {
		t.Fatalf("a thread that fits should not be scrolled (off=%d)", tv.off)
	}

	// Opening lands on the newest message, which is now the top one, so j has
	// to walk down through the older ones and k back up.
	for want := 1; want <= 2; want++ {
		tv.Update(keyPress("j"), k, 80, 40)
		if tv.cursor != want {
			t.Fatalf("j left the cursor on message %d, want %d", tv.cursor, want)
		}
	}
	for want := 1; want >= 0; want-- {
		tv.Update(keyPress("k"), k, 80, 40)
		if tv.cursor != want {
			t.Fatalf("k left the cursor on message %d, want %d", tv.cursor, want)
		}
	}
	// And the selection is visible: the current message's header is the one
	// drawn in reverse video.
	head := tv.headerText(&tv.messages[0], 80)
	if !strings.Contains(tv.View(80, 40), styleSelected.Render(head)) {
		t.Error("the current message's header is not highlighted")
	}
}

// A reload lands every couple of seconds while the daemon syncs. It must not
// yank the reader back to the top of the thread.
func TestThreadReloadKeepsTheReadingPosition(t *testing.T) {
	d := newTestDeps(t, "work")
	addLongMessage(t, d, "w1", "First", "anna", 2*time.Hour, false)
	addLongMessage(t, d, "w2", "Second", "bob", time.Hour, false)

	tv := openThreadView(t, d, true)
	k := defaultKeys()
	tv.Update(keyPress("g"), k, 80, 10)
	tv.Update(keyPress("j"), k, 80, 10)
	before := tv.off

	s := pump(t, tv, tv.reload(), k, 80, 10)
	tv = s.(*threadView)
	if tv.off != before {
		t.Errorf("a reload moved the view from line %d to %d", before, tv.off)
	}
}

// Having a message expanded under the cursor means its text is on screen, so
// it is read — and that has to reach the provider, not just the local flag.
func TestExpandedThreadMarksTheMessageRead(t *testing.T) {
	d, mail := newTriageDeps(t)
	addUnreadThreadMessage(t, d, mail, "m1", "Read me")

	r := newTestRoot(t, d)
	send(t, r, "enter")

	if _, ok := r.top().(*threadView); !ok {
		t.Fatalf("top is %T, want *threadView", r.top())
	}
	flags, _, ok := mail.Lookup("m1")
	if !ok {
		t.Fatal("message vanished from the provider")
	}
	if flags.Unread {
		t.Error("opening the thread expanded did not mark the message read")
	}
}

// The collapsed view is an index: a row is not a message you have read.
func TestCollapsedThreadLeavesTheUnreadFlagAlone(t *testing.T) {
	d, mail := newTriageDeps(t)
	addUnreadThreadMessage(t, d, mail, "m1", "Leave me")

	r := newTestRoot(t, d)
	r.threadExpanded = false
	send(t, r, "enter")

	flags, _, _ := mail.Lookup("m1")
	if !flags.Unread {
		t.Error("the collapsed thread marked the message read")
	}
}

// addLongMessage indexes a message in thread t1 whose body is taller than any
// window these tests use, so scrolling has somewhere to go.
func addLongMessage(t *testing.T, d Deps, remote, subject, from string, ago time.Duration, unread bool) {
	t.Helper()
	body := subject + " body"
	for i := 1; i <= 30; i++ {
		body += "\n" + subject + " line " + strconv.Itoa(i)
	}
	when := testNow.Add(-ago)
	m := &model.Message{
		AccountID:      "work",
		RemoteID:       remote,
		ThreadID:       "t1",
		Subject:        subject,
		From:           model.Address{Name: from, Email: from + "@example.com"},
		Date:           when,
		Received:       when,
		Snippet:        subject + " body",
		TextBody:       body,
		Flags:          model.Flags{Unread: unread},
		MailboxRemotes: []string{"inbox"},
		IndexedAt:      testNow,
	}
	if _, err := d.Store.UpsertMessage(context.Background(), m, nil); err != nil {
		t.Fatalf("UpsertMessage %s: %v", remote, err)
	}
}

// Marking a message unread by hand in the expanded view has to stick: the very
// next keystroke would otherwise hand it straight back to the auto-mark-read.
func TestExpandedThreadRespectsAManualMarkUnread(t *testing.T) {
	d, mail := newTriageDeps(t)
	addUnreadThreadMessage(t, d, mail, "m1", "Keep me unread")

	r := newTestRoot(t, d)
	send(t, r, "enter") // opening it expanded marks it read
	send(t, r, "m")     // ... and this puts it back

	if flags, _, _ := mail.Lookup("m1"); !flags.Unread {
		t.Fatal("m did not mark the message unread again")
	}
	send(t, r, "j")
	if flags, _, _ := mail.Lookup("m1"); !flags.Unread {
		t.Error("scrolling undid the manual mark-unread")
	}
}

func addUnreadThreadMessage(t *testing.T, d Deps, mail *fake.Mail, remote, subject string) {
	t.Helper()
	raw := []byte("From: anna@example.com\r\nSubject: " + subject + "\r\n\r\n" + subject + " body\r\n")
	mail.Add(fake.NewMsg(remote, raw).WithMailboxes("INBOX").WithFlags(model.Flags{Unread: true}))
	m := &model.Message{
		AccountID:      "work",
		RemoteID:       remote,
		ThreadID:       "t-" + remote,
		Subject:        subject,
		From:           model.Address{Name: "anna", Email: "anna@example.com"},
		Date:           testNow.Add(-time.Hour),
		Received:       testNow.Add(-time.Hour),
		TextBody:       subject + " body",
		Flags:          model.Flags{Unread: true},
		MailboxRemotes: []string{"INBOX"},
		IndexedAt:      testNow,
	}
	if _, err := d.Store.UpsertMessage(context.Background(), m, nil); err != nil {
		t.Fatalf("UpsertMessage: %v", err)
	}
}

// An unsent draft reply sits in the thread it belongs to, next to mail that
// really was sent. Nothing in the row said so before, so it read as sent.
func TestThreadMarksADraft(t *testing.T) {
	d := newTestDeps(t, "work")
	addMessage(t, d, "work", "w1", "t1", "First", "anna", 2*time.Hour, false)
	addDraft(t, d, "work", "w2", "t1", "My reply", time.Hour)

	// Compact marks the draft in the mark column, expanded in the flag run of
	// the message header; both put it on the draft's own row and nowhere else.
	for _, tc := range []struct {
		expanded bool
		want     string
	}{
		{false, "D work"},
		{true, "· D"},
	} {
		tv := openThreadView(t, d, tc.expanded)
		got := view(tv, 80, 30)
		if !strings.Contains(got, tc.want) {
			t.Errorf("expanded=%v: no %q draft marker:\n%s", tc.expanded, tc.want, got)
		}
		if n := strings.Count(got, tc.want); n != 1 {
			t.Errorf("expanded=%v: %q appears %d times, want 1 (the draft):\n%s",
				tc.expanded, tc.want, n, got)
		}
	}
}
