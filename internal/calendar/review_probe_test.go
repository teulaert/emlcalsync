package calendar

import (
	"os"
	"testing"
	"time"

	"github.com/teulaert/emlcalsync/internal/model"
)

func reviewFail(t *testing.T, format string, args ...any) {
	t.Helper()
	if os.Getenv("EMLCAL_REVIEW") == "1" {
		t.Errorf(format, args...)
		return
	}
	t.Logf("[review] "+format, args...)
}

func ams(t *testing.T) *time.Location {
	t.Helper()
	loc, err := time.LoadLocation("Europe/Amsterdam")
	if err != nil {
		t.Skip("tzdata unavailable")
	}
	return loc
}

// TestReviewDSTSpringForward: a daily 02:30 meeting across the spring-forward
// day, when 02:30 local does not exist.
func TestReviewDSTSpringForward(t *testing.T) {
	loc := ams(t)
	// 2026: DST starts Sunday 29 March 02:00 -> 03:00 CEST.
	ev := &model.Event{
		ID: 1, UID: "dst-1", Timezone: "Europe/Amsterdam",
		Start: time.Date(2026, 3, 27, 2, 30, 0, 0, loc),
		End:   time.Date(2026, 3, 27, 3, 0, 0, 0, loc),
		RRule: "FREQ=DAILY;COUNT=5",
	}
	occ, err := Expand(ev, time.Date(2026, 3, 1, 0, 0, 0, 0, loc), time.Date(2026, 4, 10, 0, 0, 0, 0, loc))
	if err != nil {
		t.Fatalf("Expand: %v", err)
	}
	if len(occ) != 5 {
		reviewFail(t, "COUNT=5 daily produced %d occurrences", len(occ))
	}
	for _, o := range occ {
		s := o.Start.In(loc)
		y, m, d := s.Date()
		if y == 2026 && m == time.March && d == 29 {
			// The 02:30 wall time does not exist; Go normalises to 03:30 CEST.
			t.Logf("spring-forward instance: %s (offset %s)", s.Format(time.RFC3339), s.Format("-0700"))
			if s.Hour() != 3 || s.Minute() != 30 {
				reviewFail(t, "non-existent 02:30 on the spring-forward day became %s, want 03:30 CEST",
					s.Format(time.RFC3339))
			}
		}
		if o.End.Sub(o.Start) != 30*time.Minute {
			reviewFail(t, "occurrence %s lost its 30m duration: %s", s, o.End.Sub(o.Start))
		}
	}
	// Every other instance keeps 02:30 wall clock.
	for _, o := range occ {
		s := o.Start.In(loc)
		if s.Day() == 29 && s.Month() == time.March {
			continue
		}
		if s.Hour() != 2 || s.Minute() != 30 {
			reviewFail(t, "wall-clock time drifted across DST: %s", s.Format(time.RFC3339))
		}
	}
}

// TestReviewDSTFallBack: the autumn transition, where 02:30 happens twice.
func TestReviewDSTFallBack(t *testing.T) {
	loc := ams(t)
	// 2026: DST ends Sunday 25 October 03:00 -> 02:00 CET.
	ev := &model.Event{
		ID: 1, UID: "dst-2", Timezone: "Europe/Amsterdam",
		Start: time.Date(2026, 10, 23, 2, 30, 0, 0, loc),
		End:   time.Date(2026, 10, 23, 3, 0, 0, 0, loc),
		RRule: "FREQ=DAILY;COUNT=5",
	}
	occ, err := Expand(ev, time.Date(2026, 10, 1, 0, 0, 0, 0, loc), time.Date(2026, 11, 10, 0, 0, 0, 0, loc))
	if err != nil {
		t.Fatalf("Expand: %v", err)
	}
	if len(occ) != 5 {
		reviewFail(t, "COUNT=5 daily across fall-back produced %d occurrences", len(occ))
	}
	for _, o := range occ {
		s := o.Start.In(loc)
		if s.Hour() != 2 || s.Minute() != 30 {
			reviewFail(t, "wall-clock time drifted across the autumn change: %s", s.Format(time.RFC3339))
		}
	}
	// Starts must be strictly increasing (a repeated wall time must not collide).
	for i := 1; i < len(occ); i++ {
		if !occ[i].Start.After(occ[i-1].Start) {
			reviewFail(t, "occurrences are not strictly increasing: %s then %s",
				occ[i-1].Start.Format(time.RFC3339), occ[i].Start.Format(time.RFC3339))
		}
	}
}

// TestReviewUntilVsCount compares an UNTIL rule with the COUNT that should be
// equivalent, in a non-UTC zone.
func TestReviewUntilVsCount(t *testing.T) {
	loc := ams(t)
	base := &model.Event{
		ID: 1, UID: "u", Timezone: "Europe/Amsterdam",
		Start: time.Date(2026, 3, 25, 10, 0, 0, 0, loc),
		End:   time.Date(2026, 3, 25, 11, 0, 0, 0, loc),
	}
	from := time.Date(2026, 1, 1, 0, 0, 0, 0, loc)
	to := time.Date(2027, 1, 1, 0, 0, 0, 0, loc)

	count := *base
	count.RRule = "FREQ=DAILY;COUNT=10"
	byCount, err := Expand(&count, from, to)
	if err != nil {
		t.Fatal(err)
	}

	// UNTIL in the RFC 5545 UTC form: 3 April 2026 10:00 CEST = 08:00Z.
	utc := *base
	utc.RRule = "FREQ=DAILY;UNTIL=20260403T080000Z"
	byUntilUTC, err := Expand(&utc, from, to)
	if err != nil {
		t.Fatalf("UNTIL with Z: %v", err)
	}
	if len(byCount) != 10 {
		reviewFail(t, "COUNT=10 produced %d occurrences", len(byCount))
	}
	if len(byUntilUTC) != len(byCount) {
		reviewFail(t, "UNTIL=20260403T080000Z produced %d occurrences, COUNT=10 produced %d (last: %v vs %v)",
			len(byUntilUTC), len(byCount), lastStart(byUntilUTC), lastStart(byCount))
	}

	// UNTIL without a zone: read in the event's location per buildRule's doc.
	local := *base
	local.RRule = "FREQ=DAILY;UNTIL=20260403T100000"
	byUntilLocal, err := Expand(&local, from, to)
	if err != nil {
		reviewFail(t, "zone-less UNTIL rejected: %v", err)
	} else if len(byUntilLocal) != len(byCount) {
		reviewFail(t, "zone-less UNTIL produced %d occurrences, want %d (last: %v)",
			len(byUntilLocal), len(byCount), lastStart(byUntilLocal))
	}
}

func lastStart(o []model.Occurrence) string {
	if len(o) == 0 {
		return "<none>"
	}
	return o[len(o)-1].Start.Format(time.RFC3339)
}

// TestReviewAllDayMultiDayBoundary: a 3-day all-day event straddling the ends
// of the window must still be returned.
func TestReviewAllDayMultiDayBoundary(t *testing.T) {
	loc := ams(t)
	from := time.Date(2026, 8, 10, 0, 0, 0, 0, loc)
	to := time.Date(2026, 8, 20, 0, 0, 0, 0, loc)

	for _, tc := range []struct {
		name       string
		start, end time.Time
		want       bool
	}{
		{"straddles the start", time.Date(2026, 8, 8, 0, 0, 0, 0, loc), time.Date(2026, 8, 12, 0, 0, 0, 0, loc), true},
		{"straddles the end", time.Date(2026, 8, 18, 0, 0, 0, 0, loc), time.Date(2026, 8, 22, 0, 0, 0, 0, loc), true},
		{"ends exactly at from", time.Date(2026, 8, 8, 0, 0, 0, 0, loc), from, false},
		{"starts exactly at to", to, time.Date(2026, 8, 22, 0, 0, 0, 0, loc), false},
		{"ends one second after from", time.Date(2026, 8, 8, 0, 0, 0, 0, loc), from.Add(time.Second), true},
		{"single day inside", time.Date(2026, 8, 15, 0, 0, 0, 0, loc), time.Date(2026, 8, 16, 0, 0, 0, 0, loc), true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			ev := &model.Event{ID: 1, UID: "ad", AllDay: true, Timezone: "Europe/Amsterdam", Start: tc.start, End: tc.end}
			occ, err := Expand(ev, from, to)
			if err != nil {
				t.Fatal(err)
			}
			if got := len(occ) > 0; got != tc.want {
				reviewFail(t, "all-day %s..%s in window %s..%s: returned=%v want=%v",
					tc.start.Format("2006-01-02"), tc.end.Format("2006-01-02"),
					from.Format("2006-01-02"), to.Format("2006-01-02"), got, tc.want)
			}
		})
	}
}

// TestReviewExceptionMovedOutsideWindow: an instance dragged past the window
// end must vanish from the window, not reappear at its new time.
func TestReviewExceptionMovedOutsideWindow(t *testing.T) {
	loc := ams(t)
	from := time.Date(2026, 8, 1, 0, 0, 0, 0, loc)
	to := time.Date(2026, 8, 31, 0, 0, 0, 0, loc)

	master := &model.Event{
		ID: 1, UID: "series", Timezone: "Europe/Amsterdam",
		Start: time.Date(2026, 8, 3, 9, 0, 0, 0, loc),
		End:   time.Date(2026, 8, 3, 10, 0, 0, 0, loc),
		RRule: "FREQ=WEEKLY;COUNT=4",
	}
	occ, err := Expand(master, from, to)
	if err != nil {
		t.Fatal(err)
	}
	if len(occ) != 4 {
		t.Fatalf("precondition: %d occurrences", len(occ))
	}

	// Move the 17 August instance to 15 December, well outside the window.
	moved := model.Event{
		ID: 2, UID: "series", Timezone: "Europe/Amsterdam",
		RecurrenceID: "2026-08-17T09:00:00",
		Start:        time.Date(2026, 12, 15, 9, 0, 0, 0, loc),
		End:          time.Date(2026, 12, 15, 10, 0, 0, 0, loc),
	}
	got := ApplyExceptions(occ, []model.Event{moved})

	for _, o := range got {
		if o.Start.In(loc).Day() == 17 && o.Start.In(loc).Month() == time.August {
			reviewFail(t, "the moved instance is still at its original time: %s", o.Start.Format(time.RFC3339))
		}
	}
	// The moved instance is expected to appear at its new December time even
	// though that is outside the expansion window: it is a real occurrence and
	// event_occurrences is queried by range anyway.
	var moved15Dec bool
	for _, o := range got {
		if o.Start.In(loc).Month() == time.December && o.Start.In(loc).Day() == 15 {
			moved15Dec = true
			if o.EventID != 2 {
				reviewFail(t, "moved instance attributed to event %d, want the exception row (2)", o.EventID)
			}
		}
	}
	if !moved15Dec {
		reviewFail(t, "the exception's new time was dropped entirely: %v", got)
	}
	if len(got) != 4 {
		reviewFail(t, "ApplyExceptions returned %d occurrences, want 4 (3 in place + 1 moved)", len(got))
	}

	// A cancelled exception must simply remove the instance.
	cancelled := moved
	cancelled.Status = model.StatusCancelled
	got2 := ApplyExceptions(occ, []model.Event{cancelled})
	if len(got2) != 3 {
		reviewFail(t, "cancelled exception left %d occurrences, want 3", len(got2))
	}
}

// TestReviewFreeSlotsCrossMidnight covers a busy block spanning midnight, both
// with and without working hours.
func TestReviewFreeSlotsCrossMidnight(t *testing.T) {
	loc := ams(t)
	from := time.Date(2026, 8, 24, 0, 0, 0, 0, loc) // Monday
	to := time.Date(2026, 8, 26, 0, 0, 0, 0, loc)

	busy := []Busy{{
		Start: time.Date(2026, 8, 24, 22, 0, 0, 0, loc),
		End:   time.Date(2026, 8, 25, 2, 0, 0, 0, loc),
		Title: "overnight deploy",
	}}

	// No working hours: one continuous search range.
	slots := FreeSlots(busy, from, to, 30*time.Minute, nil)
	for _, s := range slots {
		if s.Start.Before(busy[0].End) && s.End.After(busy[0].Start) {
			reviewFail(t, "free slot %s..%s overlaps the overnight busy block %s..%s",
				s.Start.Format(time.RFC3339), s.End.Format(time.RFC3339),
				busy[0].Start.Format(time.RFC3339), busy[0].End.Format(time.RFC3339))
		}
	}
	if len(slots) != 2 {
		reviewFail(t, "an overnight busy block split the range into %d slots, want 2: %v", len(slots), fmtSlots(slots))
	}

	// With working hours the block only eats into Tuesday morning if hours
	// start before 02:00; 09:00-18:00 must leave both days fully free.
	hours := &WorkHours{Start: "09:00", End: "18:00", Location: loc}
	slots = FreeSlots(busy, from, to, 30*time.Minute, hours)
	if len(slots) != 2 {
		reviewFail(t, "09:00-18:00 over two weekdays gave %d slots, want 2: %v", len(slots), fmtSlots(slots))
	}
	for _, s := range slots {
		if s.Start.In(loc).Hour() != 9 || s.End.In(loc).Hour() != 18 {
			reviewFail(t, "working-hours slot is not 09:00-18:00: %s..%s",
				s.Start.Format(time.RFC3339), s.End.Format(time.RFC3339))
		}
		if s.Start.In(loc).Day() != s.End.In(loc).Day() {
			reviewFail(t, "a working-hours slot spans a night: %s..%s",
				s.Start.Format(time.RFC3339), s.End.Format(time.RFC3339))
		}
	}
}

func fmtSlots(s []Slot) []string {
	out := make([]string, 0, len(s))
	for _, x := range s {
		out = append(out, x.Start.Format(time.RFC3339)+".."+x.End.Format(time.RFC3339))
	}
	return out
}

// TestReviewFreeSlotsDSTDay: the working window on a clock-change day.
func TestReviewFreeSlotsDSTDay(t *testing.T) {
	loc := ams(t)
	from := time.Date(2026, 10, 25, 0, 0, 0, 0, loc) // fall-back Sunday
	to := time.Date(2026, 10, 27, 0, 0, 0, 0, loc)
	hours := &WorkHours{Start: "09:00", End: "18:00", Location: loc,
		Weekdays: []time.Weekday{time.Sunday, time.Monday}}

	slots := FreeSlots(nil, from, to, time.Minute, hours)
	if len(slots) != 2 {
		reviewFail(t, "two qualifying days gave %d slots: %v", len(slots), fmtSlots(slots))
		return
	}
	for _, s := range slots {
		if s.Start.In(loc).Hour() != 9 || s.End.In(loc).Hour() != 18 {
			reviewFail(t, "clock-change day window is not 09:00-18:00 wall clock: %s..%s",
				s.Start.Format(time.RFC3339), s.End.Format(time.RFC3339))
		}
	}
	// The clock change is at 03:00, before the working window, so 09:00-18:00
	// is a normal nine hours on both days.
	for _, s := range slots {
		if d := s.Duration(); d != 9*time.Hour {
			reviewFail(t, "working window %s..%s lasted %s, want 9h",
				s.Start.Format(time.RFC3339), s.End.Format(time.RFC3339), d)
		}
	}
}
