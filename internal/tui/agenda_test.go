package tui

import (
	"strings"
	"testing"
	"time"
)

func TestAgendaMergesCalendarsAndGroupsByDay(t *testing.T) {
	d := newTestDeps(t, "work", "home")
	day1 := testNow.Add(2 * time.Hour)
	day2 := testNow.Add(26 * time.Hour)
	addEvent(t, d, "work", "cal-w", "Work", "e1", "Standup", day1, 30*time.Minute)
	addEvent(t, d, "home", "cal-h", "Home", "e2", "Dentist", day1.Add(time.Hour), time.Hour)
	addEvent(t, d, "work", "cal-w", "Work", "e3", "Design review", day2, time.Hour)

	a := newAgenda(d)
	s := pump(t, a, a.Init(), defaultKeys(), 100, 24)
	ag := s.(*agenda)

	if len(ag.occs) != 3 {
		t.Fatalf("got %d occurrences across both accounts, want 3", len(ag.occs))
	}
	// Two days means two headers plus three rows.
	headers := 0
	for _, l := range ag.lines {
		if l.header != "" {
			headers++
		}
	}
	if headers != 2 {
		t.Errorf("got %d day headers, want 2", headers)
	}
	if len(ag.lines) != 5 {
		t.Errorf("got %d lines, want 5 (2 headers + 3 events)", len(ag.lines))
	}

	view := ag.View(100, 20)
	for _, want := range []string{"Standup", "Dentist", "Design review", "Work", "Home"} {
		if !strings.Contains(view, want) {
			t.Errorf("agenda view is missing %q:\n%s", want, view)
		}
	}
}

// The cursor must never land on a day header, or Enter would open nothing.
func TestAgendaCursorSkipsDayHeaders(t *testing.T) {
	d := newTestDeps(t, "work")
	addEvent(t, d, "work", "cal-w", "Work", "e1", "Today", testNow.Add(time.Hour), time.Hour)
	addEvent(t, d, "work", "cal-w", "Work", "e2", "Tomorrow", testNow.Add(26*time.Hour), time.Hour)

	k := defaultKeys()
	a := newAgenda(d)
	ag := pump(t, a, a.Init(), k, 100, 24).(*agenda)

	if ag.lines[ag.cursor].header != "" {
		t.Fatalf("cursor started on a header at %d", ag.cursor)
	}
	if got := ag.selectedOcc(); got == nil || got.Title != "Today" {
		t.Fatalf("selected = %v, want Today", got)
	}
	// Moving down crosses the second day's header.
	s, _ := ag.Update(keyPress("j"), k, 100, 24)
	ag = s.(*agenda)
	if ag.lines[ag.cursor].header != "" {
		t.Fatalf("cursor landed on a header at %d", ag.cursor)
	}
	if got := ag.selectedOcc(); got == nil || got.Title != "Tomorrow" {
		t.Fatalf("selected = %v, want Tomorrow", got)
	}
	// And back up again.
	s, _ = ag.Update(keyPress("k"), k, 100, 24)
	ag = s.(*agenda)
	if got := ag.selectedOcc(); got == nil || got.Title != "Today" {
		t.Fatalf("after k selected = %v, want Today", got)
	}
}

func TestAgendaPagesForward(t *testing.T) {
	d := newTestDeps(t, "work")
	k := defaultKeys()
	a := newAgenda(d)
	ag := pump(t, a, a.Init(), k, 100, 24).(*agenda)
	from := ag.from

	s, cmd := ag.Update(keyPress("]"), k, 100, 24)
	ag = pump(t, s, cmd, k, 100, 24).(*agenda)
	if !ag.from.After(from) {
		t.Errorf("] did not move the window forward: %s → %s", from, ag.from)
	}
	if ag.to.Sub(ag.from) != agendaDays*24*time.Hour {
		t.Errorf("window is %s wide, want %d days", ag.to.Sub(ag.from), agendaDays)
	}
}
