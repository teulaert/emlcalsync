package calendar

import (
	"testing"
	"time"
)

// at builds a time on 25 August 2026 (a Tuesday) in loc.
func at(loc *time.Location, day, hour, min int) time.Time {
	return time.Date(2026, 8, day, hour, min, 0, 0, loc)
}

func slotStrings(slots []Slot) []string {
	out := make([]string, len(slots))
	for i, s := range slots {
		out[i] = s.Start.Format("Mon 15:04") + "-" + s.End.Format("15:04")
	}
	return out
}

func wantSlots(t *testing.T, got []Slot, want []string) {
	t.Helper()
	gs := slotStrings(got)
	if len(gs) != len(want) {
		t.Fatalf("got %d slots %v, want %d %v", len(gs), gs, len(want), want)
	}
	for i := range gs {
		if gs[i] != want[i] {
			t.Errorf("slot %d = %s, want %s", i, gs[i], want[i])
		}
	}
}

func TestFreeSlotsNoWorkingHours(t *testing.T) {
	loc := amsterdam(t)
	busy := []Busy{
		{Start: at(loc, 25, 10, 0), End: at(loc, 25, 11, 0), Title: "Standup"},
		{Start: at(loc, 25, 14, 0), End: at(loc, 25, 15, 0), Title: "Review"},
	}
	got := FreeSlots(busy, at(loc, 25, 9, 0), at(loc, 25, 17, 0), 30*time.Minute, nil)
	wantSlots(t, got, []string{
		"Tue 09:00-10:00",
		"Tue 11:00-14:00",
		"Tue 15:00-17:00",
	})
}

func TestFreeSlotsMergesOverlappingBusy(t *testing.T) {
	loc := amsterdam(t)
	busy := []Busy{
		{Start: at(loc, 25, 14, 0), End: at(loc, 25, 15, 0)},
		{Start: at(loc, 25, 10, 0), End: at(loc, 25, 11, 0)},  // out of order
		{Start: at(loc, 25, 10, 30), End: at(loc, 25, 12, 0)}, // overlaps the previous
		{Start: at(loc, 25, 12, 0), End: at(loc, 25, 13, 0)},  // back to back
		{Start: at(loc, 25, 11, 0), End: at(loc, 25, 11, 0)},  // zero length, ignored
		{Start: at(loc, 25, 16, 0), End: at(loc, 25, 15, 0)},  // inverted, ignored
	}
	got := FreeSlots(busy, at(loc, 25, 9, 0), at(loc, 25, 17, 0), 15*time.Minute, nil)
	wantSlots(t, got, []string{
		"Tue 09:00-10:00",
		"Tue 13:00-14:00",
		"Tue 15:00-17:00",
	})
}

func TestFreeSlotsMinDuration(t *testing.T) {
	loc := amsterdam(t)
	busy := []Busy{
		{Start: at(loc, 25, 9, 0), End: at(loc, 25, 10, 0)},
		{Start: at(loc, 25, 10, 20), End: at(loc, 25, 12, 0)}, // leaves a 20m gap
	}
	// A 20-minute gap is too short for a 30-minute meeting.
	got := FreeSlots(busy, at(loc, 25, 9, 0), at(loc, 25, 13, 0), 30*time.Minute, nil)
	wantSlots(t, got, []string{"Tue 12:00-13:00"})

	// but fine for a 15-minute one.
	got = FreeSlots(busy, at(loc, 25, 9, 0), at(loc, 25, 13, 0), 15*time.Minute, nil)
	wantSlots(t, got, []string{"Tue 10:00-10:20", "Tue 12:00-13:00"})
}

func TestFreeSlotsWithWorkingHours(t *testing.T) {
	loc := amsterdam(t)
	hours := &WorkHours{Start: "09:00", End: "17:00", Location: loc}
	// Window spans Tue 25th through Thu 27th; a meeting fills Wednesday
	// morning and one runs outside working hours entirely.
	busy := []Busy{
		{Start: at(loc, 25, 20, 0), End: at(loc, 25, 22, 0)}, // evening, irrelevant
		{Start: at(loc, 26, 9, 0), End: at(loc, 26, 12, 0)},
	}
	got := FreeSlots(busy, at(loc, 25, 8, 0), at(loc, 27, 20, 0), time.Hour, hours)
	wantSlots(t, got, []string{
		"Tue 09:00-17:00",
		"Wed 12:00-17:00",
		"Thu 09:00-17:00",
	})
}

func TestFreeSlotsSkipsWeekends(t *testing.T) {
	loc := amsterdam(t)
	hours := &WorkHours{Start: "09:00", End: "17:00", Location: loc}
	// 28 August 2026 is a Friday; the window runs into the following Monday.
	got := FreeSlots(nil, at(loc, 28, 0, 0), at(loc, 32, 0, 0), time.Hour, hours)
	wantSlots(t, got, []string{
		"Fri 09:00-17:00",
		"Mon 09:00-17:00",
	})

	// An explicit weekday list is honoured, weekends included.
	hours.Weekdays = []time.Weekday{time.Saturday}
	wantSlots(t, FreeSlots(nil, at(loc, 28, 0, 0), at(loc, 32, 0, 0), time.Hour, hours),
		[]string{"Sat 09:00-17:00"})
}

func TestFreeSlotsClipsToWindow(t *testing.T) {
	loc := amsterdam(t)
	hours := &WorkHours{Start: "09:00", End: "17:00", Location: loc}
	// The window starts mid-morning and ends mid-afternoon.
	got := FreeSlots(nil, at(loc, 25, 11, 30), at(loc, 25, 15, 0), 30*time.Minute, hours)
	wantSlots(t, got, []string{"Tue 11:30-15:00"})
}

func TestFreeSlotsAcrossDST(t *testing.T) {
	loc := amsterdam(t)
	hours := &WorkHours{Start: "09:00", End: "17:00", Location: loc}
	// 25 October 2026 is the autumn transition (a Sunday); the working days
	// either side must still be 09:00–17:00 wall clock.
	from := time.Date(2026, 10, 23, 0, 0, 0, 0, loc) // Friday
	to := time.Date(2026, 10, 27, 0, 0, 0, 0, loc)   // Tuesday 00:00
	wantSlots(t, FreeSlots(nil, from, to, time.Hour, hours), []string{
		"Fri 09:00-17:00",
		"Mon 09:00-17:00",
	})
}

func TestFreeSlotsEdgeCases(t *testing.T) {
	loc := amsterdam(t)
	if got := FreeSlots(nil, at(loc, 25, 12, 0), at(loc, 25, 12, 0), time.Hour, nil); got != nil {
		t.Errorf("an empty window should have no slots, got %v", slotStrings(got))
	}
	if got := FreeSlots(nil, at(loc, 25, 15, 0), at(loc, 25, 9, 0), time.Hour, nil); got != nil {
		t.Errorf("an inverted window should have no slots, got %v", slotStrings(got))
	}
	// Fully booked.
	busy := []Busy{{Start: at(loc, 25, 9, 0), End: at(loc, 25, 17, 0)}}
	if got := FreeSlots(busy, at(loc, 25, 9, 0), at(loc, 25, 17, 0), time.Minute, nil); got != nil {
		t.Errorf("a full day should have no slots, got %v", slotStrings(got))
	}
	// Busy extending past the window on both sides.
	busy = []Busy{{Start: at(loc, 24, 0, 0), End: at(loc, 26, 0, 0)}}
	if got := FreeSlots(busy, at(loc, 25, 9, 0), at(loc, 25, 17, 0), time.Minute, nil); got != nil {
		t.Errorf("got %v, want nothing", slotStrings(got))
	}
	// Unusable working hours (end before start) yield nothing rather than
	// silently dropping the restriction.
	bad := &WorkHours{Start: "18:00", End: "09:00", Location: loc}
	if got := FreeSlots(nil, at(loc, 25, 0, 0), at(loc, 26, 0, 0), time.Hour, bad); got != nil {
		t.Errorf("inverted working hours gave %v", slotStrings(got))
	}
}

func TestSlotDuration(t *testing.T) {
	loc := amsterdam(t)
	s := Slot{Start: at(loc, 25, 9, 0), End: at(loc, 25, 10, 30)}
	if s.Duration() != 90*time.Minute {
		t.Errorf("Duration = %v", s.Duration())
	}
}

func TestParseWorkHours(t *testing.T) {
	loc := amsterdam(t)
	h, err := ParseWorkHours("09:00-18:00", loc)
	if err != nil || h == nil || h.Start != "09:00" || h.End != "18:00" || h.Location != loc {
		t.Fatalf("ParseWorkHours = %+v, %v", h, err)
	}
	if h, err := ParseWorkHours("", loc); err != nil || h != nil {
		t.Errorf("empty --hours should mean no restriction, got %+v, %v", h, err)
	}
	// The en dash people paste from calendars works too.
	if _, err := ParseWorkHours("09:00–18:00", loc); err != nil {
		t.Errorf("en dash rejected: %v", err)
	}
	for _, bad := range []string{"09:00", "9-25:00", "nine-five"} {
		if _, err := ParseWorkHours(bad, loc); err == nil {
			t.Errorf("ParseWorkHours(%q) should fail", bad)
		}
	}
}

func TestDefaultWorkHours(t *testing.T) {
	loc := amsterdam(t)
	h := DefaultWorkHours(loc)
	if h.Start != "09:00" || h.End != "18:00" {
		t.Errorf("DefaultWorkHours = %+v", h)
	}
	if !h.countsDay(time.Wednesday) || h.countsDay(time.Sunday) {
		t.Error("default working week should be Mon-Fri")
	}
}

func TestFreeSlotsWorkingHoursOnTransitionDay(t *testing.T) {
	loc := amsterdam(t)
	// 25 October 2026 is the 25-hour autumn day. The clocks change at 03:00,
	// so a 09:00–17:00 working day on it is still eight hours of real time —
	// which is only true if the window is built from wall-clock times.
	hours := &WorkHours{Start: "09:00", End: "17:00", Weekdays: []time.Weekday{time.Sunday}, Location: loc}
	got := FreeSlots(nil,
		time.Date(2026, 10, 25, 0, 0, 0, 0, loc),
		time.Date(2026, 10, 26, 0, 0, 0, 0, loc),
		time.Hour, hours)
	wantSlots(t, got, []string{"Sun 09:00-17:00"})
	if d := got[0].Duration(); d != 8*time.Hour {
		t.Errorf("duration = %v; the clocks change at 03:00, so 09:00-17:00 is still 8h", d)
	}
}
