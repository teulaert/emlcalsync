package tui

import (
	"strings"
	"testing"
	"time"
)

// The unified list is the whole point of the application: one screen holding
// every account, ordered by time, with each row saying where it came from.
func TestMailListMergesAccountsInTimeOrder(t *testing.T) {
	d := newTestDeps(t, "work", "home")
	addMessage(t, d, "work", "w1", "tw1", "Q3 budget", "anna", 2*time.Hour, true)
	addMessage(t, d, "home", "h1", "th1", "Flight confirmation", "klm", 5*time.Hour, false)
	addMessage(t, d, "work", "w2", "tw2", "Contract review", "legal", 24*time.Hour, false)

	m := newMailList(d, d.Accounts)
	s := pump(t, m, m.Init(), defaultKeys(), 100, 24)
	ml := s.(*mailList)

	if len(ml.threads) != 3 {
		t.Fatalf("got %d threads, want 3 across both accounts", len(ml.threads))
	}
	wantOrder := []struct{ account, subject string }{
		{"work", "Q3 budget"},
		{"home", "Flight confirmation"},
		{"work", "Contract review"},
	}
	for i, w := range wantOrder {
		got := ml.threads[i]
		if got.AccountID != w.account || got.Subject != w.subject {
			t.Errorf("row %d = %s/%q, want %s/%q", i, got.AccountID, got.Subject, w.account, w.subject)
		}
	}

	// The account has to be visible, or a merged list is unreadable.
	view := ml.View(100, 20)
	for _, want := range []string{"work", "home", "Q3 budget", "Flight confirmation"} {
		if !strings.Contains(view, want) {
			t.Errorf("view is missing %q:\n%s", want, view)
		}
	}
}

func TestMailListUnreadIsMarked(t *testing.T) {
	d := newTestDeps(t, "work")
	addMessage(t, d, "work", "w1", "t1", "Unread one", "anna", time.Hour, true)
	addMessage(t, d, "work", "w2", "t2", "Read one", "bram", 2*time.Hour, false)

	m := newMailList(d, d.Accounts)
	s := pump(t, m, m.Init(), defaultKeys(), 100, 24)
	ml := s.(*mailList)

	if ml.threads[0].UnreadCount != 1 {
		t.Errorf("unread thread has UnreadCount %d, want 1", ml.threads[0].UnreadCount)
	}
	if ml.threads[1].UnreadCount != 0 {
		t.Errorf("read thread has UnreadCount %d, want 0", ml.threads[1].UnreadCount)
	}
	if !strings.Contains(ml.View(100, 20), "●") {
		t.Error("unread marker is missing from the view")
	}
}

func TestMailListNavigation(t *testing.T) {
	d := newTestDeps(t, "work")
	for i := 0; i < 5; i++ {
		addMessage(t, d, "work", "w"+string(rune('a'+i)), "t"+string(rune('a'+i)),
			"Subject "+string(rune('a'+i)), "sender", time.Duration(i)*time.Hour, false)
	}
	k := defaultKeys()
	m := newMailList(d, d.Accounts)
	s := pump(t, m, m.Init(), k, 100, 24)
	ml := s.(*mailList)

	if ml.cursor != 0 {
		t.Fatalf("cursor starts at %d, want 0", ml.cursor)
	}
	s, _ = ml.Update(keyPress("j"), k, 100, 24)
	ml = s.(*mailList)
	if ml.cursor != 1 {
		t.Errorf("after j cursor = %d, want 1", ml.cursor)
	}
	s, _ = ml.Update(keyPress("G"), k, 100, 24)
	ml = s.(*mailList)
	if ml.cursor != 4 {
		t.Errorf("after G cursor = %d, want 4", ml.cursor)
	}
	s, _ = ml.Update(keyPress("g"), k, 100, 24)
	ml = s.(*mailList)
	if ml.cursor != 0 {
		t.Errorf("after g cursor = %d, want 0", ml.cursor)
	}
	// k at the top must not run off the end of the slice.
	s, _ = ml.Update(keyPress("k"), k, 100, 24)
	if s.(*mailList).cursor != 0 {
		t.Errorf("k at the top moved the cursor to %d", s.(*mailList).cursor)
	}
}

func TestMailListSearchCapturesTypingAndFinds(t *testing.T) {
	d := newTestDeps(t, "work")
	addMessage(t, d, "work", "w1", "t1", "Invoice for August", "boekhouding", time.Hour, false)
	addMessage(t, d, "work", "w2", "t2", "Lunch on Friday", "anna", 2*time.Hour, false)

	k := defaultKeys()
	m := newMailList(d, d.Accounts)
	s := pump(t, m, m.Init(), k, 100, 24)
	ml := s.(*mailList)

	s, _ = ml.Update(keyPress("/"), k, 100, 24)
	ml = s.(*mailList)
	if !ml.searching {
		t.Fatal("/ did not open the search prompt")
	}
	// While searching, letters are text, not commands: "d" must not trash.
	for _, c := range []string{"i", "n", "v", "o", "i", "c", "e"} {
		s, _ = ml.Update(keyPress(c), k, 100, 24)
		ml = s.(*mailList)
	}
	if ml.input != "invoice" {
		t.Fatalf("search input = %q, want %q", ml.input, "invoice")
	}

	s2, cmd := ml.Update(keyPress("enter"), k, 100, 24)
	ml = pump(t, s2, cmd, k, 100, 24).(*mailList)
	if ml.searching {
		t.Error("enter did not close the search prompt")
	}
	if ml.query != "invoice" {
		t.Fatalf("query = %q, want %q", ml.query, "invoice")
	}
	if len(ml.threads) != 1 || ml.threads[0].Subject != "Invoice for August" {
		t.Errorf("search returned %+v, want just the invoice thread", ml.threads)
	}
}

func TestMailListMailboxCycleNarrowsToArchive(t *testing.T) {
	d := newTestDeps(t, "work")
	addMessage(t, d, "work", "w1", "t1", "In the inbox", "anna", time.Hour, false)

	k := defaultKeys()
	m := newMailList(d, d.Accounts)
	s := pump(t, m, m.Init(), k, 100, 24)
	ml := s.(*mailList)
	if len(ml.threads) != 1 {
		t.Fatalf("inbox has %d threads, want 1", len(ml.threads))
	}
	// inbox → all → flagged: nothing is flagged, so the list empties.
	s2, cmd := ml.Update(keyPress("M"), k, 100, 24) // all
	ml = pump(t, s2, cmd, k, 100, 24).(*mailList)
	if len(ml.threads) != 1 {
		t.Fatalf("all has %d threads, want 1", len(ml.threads))
	}
	s2, cmd = ml.Update(keyPress("M"), k, 100, 24) // flagged
	ml = pump(t, s2, cmd, k, 100, 24).(*mailList)
	if len(ml.threads) != 0 {
		t.Errorf("flagged has %d threads, want 0", len(ml.threads))
	}
}

func TestMailListDropAndRestore(t *testing.T) {
	d := newTestDeps(t, "work")
	addMessage(t, d, "work", "w1", "t1", "First", "anna", time.Hour, false)
	addMessage(t, d, "work", "w2", "t2", "Second", "bram", 2*time.Hour, false)

	m := newMailList(d, d.Accounts)
	ml := pump(t, m, m.Init(), defaultKeys(), 100, 24).(*mailList)

	ml.dropSelected()
	if len(ml.threads) != 1 || ml.threads[0].Subject != "Second" {
		t.Fatalf("after drop: %+v", ml.threads)
	}
	ml.restore()
	if len(ml.threads) != 2 || ml.threads[0].Subject != "First" {
		t.Fatalf("after restore: %+v", ml.threads)
	}
	if ml.cursor != 0 {
		t.Errorf("restore left the cursor at %d, want 0", ml.cursor)
	}
}

// Every row must occupy the full width, or the columns of adjacent rows do not
// line up. This is asserted on cells, not runes, because a subject full of
// emoji is the case that breaks a rune-counted layout.
func TestMailListRowsAreExactlyTheFullWidth(t *testing.T) {
	d := newTestDeps(t, "fastmail", "ryde")
	addMessage(t, d, "fastmail", "w1", "t1", "Short", "anna", time.Hour, false)
	addMessage(t, d, "ryde", "w2", "t2",
		"A considerably longer subject line that will have to be truncated somewhere", "bram", 2*time.Hour, true)
	addMessage(t, d, "fastmail", "w3", "t3", "Emoji 🔨 in the subject 🏆", "carla", 3*time.Hour, false)

	m := newMailList(d, d.Accounts)
	ml := pump(t, m, m.Init(), defaultKeys(), 120, 24).(*mailList)

	for _, w := range []int{60, 100, 120} {
		view := ml.View(w, 10)
		for i, line := range strings.Split(view, "\n") {
			plain := stripANSI(line)
			if plain == "" {
				continue // filler below the last row
			}
			if got := cellWidth(plain); got != w {
				t.Errorf("width %d: row %d is %d cells wide: %q", w, i, got, plain)
			}
		}
	}
}

// A reload arrives every couple of seconds, because the sync daemon commits
// that often and every commit re-queries the visible screen. It must not move
// the selection: the cursor belongs to the person, not to the query.
func TestMailListReloadKeepsTheCursorOnItsThread(t *testing.T) {
	d := newTestDeps(t, "work")
	for i := 0; i < 5; i++ {
		addMessage(t, d, "work", "w"+string(rune('a'+i)), "t"+string(rune('a'+i)),
			"Subject "+string(rune('a'+i)), "sender", time.Duration(i)*time.Hour, false)
	}
	k := defaultKeys()
	m := newMailList(d, d.Accounts)
	s := pump(t, m, m.Init(), k, 100, 24)
	ml := s.(*mailList)

	for i := 0; i < 3; i++ {
		s, _ = ml.Update(keyPress("j"), k, 100, 24)
		ml = s.(*mailList)
	}
	want := ml.selected().ThreadID

	s = pump(t, ml, ml.reload(), k, 100, 24)
	ml = s.(*mailList)

	if got := ml.selected(); got == nil || got.ThreadID != want {
		t.Fatalf("after reload the cursor is on %v at row %d, want thread %s", got, ml.cursor, want)
	}
}

// Trashing a row and having the cursor jump to the top makes deleting a run of
// mail impossible: every d would have to be followed by re-navigating. The
// cursor holds its position, so it lands on whatever moved up into the gap.
func TestMailListCursorHoldsItsPlaceWhenTheThreadIsGone(t *testing.T) {
	d := newTestDeps(t, "work")
	for i := 0; i < 5; i++ {
		addMessage(t, d, "work", "w"+string(rune('a'+i)), "t"+string(rune('a'+i)),
			"Subject "+string(rune('a'+i)), "sender", time.Duration(i)*time.Hour, false)
	}
	k := defaultKeys()
	m := newMailList(d, d.Accounts)
	s := pump(t, m, m.Init(), k, 100, 24)
	ml := s.(*mailList)

	for i := 0; i < 2; i++ {
		s, _ = ml.Update(keyPress("j"), k, 100, 24)
		ml = s.(*mailList)
	}
	next := ml.threads[3].ThreadID // the row that will move up into the gap

	// What the root does on d: drop the row, then the daemon's commit reloads
	// the list without it.
	ml.dropSelected()
	moveOutOfInbox(t, d, "work", "wc")
	s = pump(t, ml, ml.reload(), k, 100, 24)
	ml = s.(*mailList)

	if len(ml.threads) != 4 {
		t.Fatalf("got %d threads after the trash, want 4", len(ml.threads))
	}
	if got := ml.selected(); got == nil || got.ThreadID != next {
		t.Fatalf("cursor is on %v at row %d, want thread %s at row 2", got, ml.cursor, next)
	}
}

// Changing what the list shows is the one case where the top is right.
func TestMailListMailboxSwitchStartsAtTheTop(t *testing.T) {
	d := newTestDeps(t, "work")
	for i := 0; i < 5; i++ {
		addMessage(t, d, "work", "w"+string(rune('a'+i)), "t"+string(rune('a'+i)),
			"Subject "+string(rune('a'+i)), "sender", time.Duration(i)*time.Hour, false)
	}
	k := defaultKeys()
	m := newMailList(d, d.Accounts)
	s := pump(t, m, m.Init(), k, 100, 24)
	ml := s.(*mailList)

	for i := 0; i < 3; i++ {
		s, _ = ml.Update(keyPress("j"), k, 100, 24)
		ml = s.(*mailList)
	}
	s, cmd := ml.Update(keyPress("M"), k, 100, 24) // inbox → all
	ml = pump(t, s, cmd, k, 100, 24).(*mailList)

	if ml.cursor != 0 || ml.top != 0 {
		t.Fatalf("mailbox switch left the cursor at %d/%d, want 0/0", ml.cursor, ml.top)
	}
}

// A draft is mail you have not sent yet, and until now the only way to reach
// one in the TUI was the "all" view, where it sat unlabelled among mail that
// had actually been sent or received. M now cycles through a drafts view.
func TestMailListDraftsView(t *testing.T) {
	d := newTestDeps(t, "work")
	addMessage(t, d, "work", "w1", "t1", "In the inbox", "anna", time.Hour, false)
	addDraft(t, d, "work", "w2", "t2", "Half written", 2*time.Hour)

	k := defaultKeys()
	m := newMailList(d, d.Accounts)
	ml := pump(t, m, m.Init(), k, 100, 24).(*mailList)
	if len(ml.threads) != 1 || ml.threads[0].Subject != "In the inbox" {
		t.Fatalf("inbox = %+v, want just the inbox thread", ml.threads)
	}

	// inbox → all → flagged → drafts.
	for i := 0; i < 3; i++ {
		s, cmd := ml.Update(keyPress("M"), k, 100, 24)
		ml = pump(t, s, cmd, k, 100, 24).(*mailList)
	}
	if got := mailboxCycle[ml.mailbox].label; got != "drafts" {
		t.Fatalf("three M presses land on %q, want drafts", got)
	}
	if len(ml.threads) != 1 || ml.threads[0].Subject != "Half written" {
		t.Fatalf("drafts = %+v, want just the draft", ml.threads)
	}
	if !strings.Contains(ml.Title(), "drafts") {
		t.Errorf("title = %q, does not say which mailbox it is showing", ml.Title())
	}
	if !strings.Contains(stripANSI(ml.View(100, 20)), "Half written") {
		t.Errorf("the drafts view does not show the draft:\n%s", ml.View(100, 20))
	}
}

// Trashed and spam mail is in the archive and always was in the "all" view,
// mixed into everything else. The cycle now ends on the two views that isolate
// it, so "what did I throw away" is a keypress rather than a search.
func TestMailListTrashAndSpamViews(t *testing.T) {
	d := newTestDeps(t, "work")
	addMessage(t, d, "work", "w1", "t1", "In the inbox", "anna", time.Hour, false)
	addMessageIn(t, d, "trash", "work", "w2", "t2", "Thrown away", "bram", 2*time.Hour, false)
	addMessageIn(t, d, "spam", "work", "w3", "t3", "Buy pills", "spammer", 3*time.Hour, false)

	k := defaultKeys()
	m := newMailList(d, d.Accounts)
	ml := pump(t, m, m.Init(), k, 100, 24).(*mailList)

	// Every view but "all" holds exactly the mail that belongs in it; "all"
	// holds the lot, trash and spam included, which is what it did before and
	// why the two views below are worth having.
	want := map[string]string{
		"inbox":   "In the inbox",
		"flagged": "",
		"drafts":  "",
		"sent":    "",
		"archive": "",
		"trash":   "Thrown away",
		"spam":    "Buy pills",
	}
	seen := map[string]bool{}
	for range mailboxCycle {
		label := mailboxCycle[ml.mailbox].label
		seen[label] = true
		if label == "all" {
			if len(ml.threads) != 3 {
				t.Errorf("all has %d threads, want 3 (inbox, trash and spam)", len(ml.threads))
			}
		} else if subject := want[label]; subject == "" {
			if len(ml.threads) != 0 {
				t.Errorf("%s has %d threads, want 0: %+v", label, len(ml.threads), ml.threads)
			}
		} else if len(ml.threads) != 1 || ml.threads[0].Subject != subject {
			t.Errorf("%s = %+v, want just %q", label, ml.threads, subject)
		}
		s, cmd := ml.Update(keyPress("M"), k, 100, 24)
		ml = pump(t, s, cmd, k, 100, 24).(*mailList)
	}
	for _, label := range []string{"trash", "spam"} {
		if !seen[label] {
			t.Errorf("M never reaches the %s view", label)
		}
	}
	// One full turn of the cycle comes back to where it started.
	if got := mailboxCycle[ml.mailbox].label; got != "inbox" {
		t.Errorf("the cycle wrapped to %q, want inbox", got)
	}
}
