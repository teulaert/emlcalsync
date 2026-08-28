package tui

import (
	"strings"
	"testing"
	"time"

	tea "charm.land/bubbletea/v2"
)

func newTestRoot(t *testing.T, d Deps) *root {
	t.Helper()
	r := newRoot(d)
	r.w, r.h = 100, 24
	drain(t, r, r.mail[0].Init())
	drain(t, r, r.cal[0].Init())
	return r
}

// drain feeds a command's messages back into the root the way the program loop
// would.
func drain(t *testing.T, r *root, cmd tea.Cmd) {
	t.Helper()
	for i := 0; cmd != nil && i < 20; i++ {
		msg := cmd()
		if msg == nil {
			return
		}
		if batch, ok := msg.(tea.BatchMsg); ok {
			for _, c := range batch {
				drain(t, r, c)
			}
			return
		}
		_, cmd = r.Update(msg)
	}
}

func send(t *testing.T, r *root, key string) {
	t.Helper()
	_, cmd := r.Update(keyPress(key))
	drain(t, r, cmd)
}

func TestRootSwitchesBetweenMailAndCalendar(t *testing.T) {
	d := newTestDeps(t, "work")
	addMessage(t, d, "work", "w1", "t1", "Hello", "anna", time.Hour, false)
	addEvent(t, d, "work", "cal-w", "Work", "e1", "Standup", testNow.Add(time.Hour), time.Hour)

	r := newTestRoot(t, d)
	if r.onCal {
		t.Fatal("should start on mail")
	}
	if _, ok := r.top().(*mailList); !ok {
		t.Errorf("top = %T, want the mail list", r.top())
	}

	send(t, r, "2")
	if _, ok := r.top().(*agenda); !r.onCal || !ok {
		t.Errorf("2 did not switch to calendar (onCal=%v, top=%T)", r.onCal, r.top())
	}
	send(t, r, "tab")
	if r.onCal {
		t.Error("tab did not switch back to mail")
	}
	send(t, r, "1")
	if r.onCal {
		t.Error("1 did not select mail")
	}
}

func TestRootPushesAndPopsTheStack(t *testing.T) {
	d := newTestDeps(t, "work")
	addMessage(t, d, "work", "w1", "t1", "Hello", "anna", time.Hour, false)

	r := newTestRoot(t, d)
	if len(r.mail) != 1 {
		t.Fatalf("mail stack starts %d deep, want 1", len(r.mail))
	}
	send(t, r, "enter")
	if len(r.mail) != 2 {
		t.Fatalf("enter did not open the thread (depth %d)", len(r.mail))
	}
	if _, ok := r.top().(*threadView); !ok {
		t.Fatalf("top is %T, want *threadView", r.top())
	}
	send(t, r, "enter")
	if _, ok := r.top().(*reader); !ok {
		t.Fatalf("top is %T, want *reader", r.top())
	}
	send(t, r, "esc")
	if _, ok := r.top().(*threadView); !ok {
		t.Fatalf("after esc top is %T, want *threadView", r.top())
	}
	send(t, r, "q")
	if len(r.mail) != 1 {
		t.Fatalf("q did not pop back to the list (depth %d)", len(r.mail))
	}
	// q at the root quits rather than popping.
	_, cmd := r.Update(keyPress("q"))
	if cmd == nil {
		t.Fatal("q at the top level did not quit")
	}
	if !r.quitting {
		t.Error("root did not mark itself quitting")
	}
}

// The tabs are the only thing telling someone in their inbox that a calendar
// exists at all, so they are drawn on every screen, in both stacks.
func TestRootHeaderAlwaysShowsBothTabs(t *testing.T) {
	d := newTestDeps(t, "work")
	addMessage(t, d, "work", "w1", "t1", "Hello", "anna", time.Hour, false)
	addEvent(t, d, "work", "cal-w", "Work", "e1", "Standup", testNow.Add(time.Hour), time.Hour)
	r := newTestRoot(t, d)

	for _, step := range []struct{ key, where string }{
		{"", "the mail list"},
		{"enter", "an open thread"},
		{"2", "the agenda"},
		{"enter", "an open event"},
	} {
		if step.key != "" {
			send(t, r, step.key)
		}
		head := strings.SplitN(r.render(), "\n", 2)[0]
		if !strings.Contains(head, "1 mail") || !strings.Contains(head, "2 calendar") {
			t.Errorf("header on %s is missing a tab: %q", step.where, head)
		}
	}
}

func TestRootHelpOverlayOpensAndAnyKeyCloses(t *testing.T) {
	d := newTestDeps(t, "work")
	r := newTestRoot(t, d)

	send(t, r, "?")
	if !r.showHelp {
		t.Fatal("? did not open help")
	}
	view := r.render()
	for _, want := range []string{"archive", "trash", "undo"} {
		if !strings.Contains(view, want) {
			t.Errorf("help is missing %q:\n%s", want, view)
		}
	}
	send(t, r, "j")
	if r.showHelp {
		t.Error("a key press did not close help")
	}
}

func TestRootUndoWithNothingToUndoSaysSo(t *testing.T) {
	d := newTestDeps(t, "work")
	r := newTestRoot(t, d)
	send(t, r, "z")
	if !strings.Contains(r.status, "nothing to undo") {
		t.Errorf("status = %q, want it to mention nothing to undo", r.status)
	}
}

func TestRootRendersTitleBodyAndStatus(t *testing.T) {
	d := newTestDeps(t, "work")
	addMessage(t, d, "work", "w1", "t1", "Renderable", "anna", time.Hour, false)
	r := newTestRoot(t, d)

	view := r.render()
	if !strings.Contains(view, "Renderable") {
		t.Errorf("body is missing the message:\n%s", view)
	}
	if !strings.Contains(view, "? help") {
		t.Errorf("status line is missing the help hint:\n%s", view)
	}
	if n := strings.Count(view, "\n") + 1; n != r.h {
		t.Errorf("render produced %d lines, want exactly %d", n, r.h)
	}
}

// A zero size arrives before the first WindowSizeMsg; painting then would
// scribble at whatever size the terminal happens to be.
func TestRootRendersNothingBeforeItKnowsTheSize(t *testing.T) {
	d := newTestDeps(t, "work")
	r := newRoot(d)
	if got := r.render(); got != "" {
		t.Errorf("render before sizing = %q, want empty", got)
	}
	_, _ = r.Update(tea.WindowSizeMsg{Width: 80, Height: 20})
	if r.w != 80 || r.h != 20 {
		t.Errorf("size = %dx%d, want 80x20", r.w, r.h)
	}
}

// Every screen has to fill exactly the height it is handed. A screen that
// comes up short leaves the previous frame's rows on screen; one that
// overshoots scrolls the terminal.
func TestEveryScreenFillsItsHeightExactly(t *testing.T) {
	d := newTestDeps(t, "work")
	addMessage(t, d, "work", "w1", "t1", "Hello", "anna", time.Hour, false)
	addEvent(t, d, "work", "cal-w", "Work", "e1", "Standup", testNow.Add(time.Hour), time.Hour)
	k := defaultKeys()

	screens := map[string]screen{
		"mailList":   newMailList(d, d.Accounts),
		"threadView": newThreadView(d, "work", "t1", "Hello", true),
		"reader":     newReader(d, "work", "w1"),
		"agenda":     newAgenda(d),
		"eventView":  newEventView(d, "work", "cal-w", "Work", "e1"),
	}
	for name, s := range screens {
		t.Run(name, func(t *testing.T) {
			loaded := pump(t, s, s.Init(), k, 100, 20)
			for _, h := range []int{5, 20, 60} {
				got := loaded.View(100, h)
				if n := strings.Count(got, "\n") + 1; n != h {
					t.Errorf("View(100, %d) produced %d lines, want %d", h, n, h)
				}
			}
		})
	}
}

// Both stacks are loaded at startup, so a result for the screen that is not on
// top still has to reach it. Routing only to the visible screen left the
// calendar permanently empty, which no unit test on the agenda alone catches.
func TestBackgroundScreenStillReceivesItsLoad(t *testing.T) {
	d := newTestDeps(t, "work")
	addMessage(t, d, "work", "w1", "t1", "Hello", "anna", time.Hour, false)
	addEvent(t, d, "work", "cal-w", "Work", "e1", "Standup", testNow.Add(time.Hour), time.Hour)

	r := newTestRoot(t, d)
	// Still on mail; the agenda loaded in the background.
	if r.onCal {
		t.Fatal("should still be on mail")
	}
	ag := r.cal[0].(*agenda)
	if len(ag.occs) != 1 {
		t.Fatalf("agenda has %d occurrences while off screen, want 1", len(ag.occs))
	}
	send(t, r, "2")
	if !strings.Contains(r.render(), "Standup") {
		t.Errorf("switching to the calendar showed no events:\n%s", r.render())
	}
}
