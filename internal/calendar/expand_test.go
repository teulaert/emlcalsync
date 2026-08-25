package calendar

import (
	"testing"
	"time"

	"github.com/lennert/emlcal/internal/model"
)

func amsterdam(t *testing.T) *time.Location {
	t.Helper()
	loc, err := time.LoadLocation("Europe/Amsterdam")
	if err != nil {
		t.Skip("tzdata unavailable")
	}
	return loc
}

// starts renders occurrence start times as RFC 3339 in loc, for readable
// assertions.
func starts(occ []model.Occurrence, loc *time.Location) []string {
	out := make([]string, len(occ))
	for i, o := range occ {
		out[i] = o.Start.In(loc).Format(time.RFC3339)
	}
	return out
}

func equal(t *testing.T, got, want []string) {
	t.Helper()
	if len(got) != len(want) {
		t.Fatalf("got %d occurrences %v, want %d %v", len(got), got, len(want), want)
	}
	for i := range got {
		if got[i] != want[i] {
			t.Errorf("occurrence %d = %s, want %s", i, got[i], want[i])
		}
	}
}

func TestExpandSingleEvent(t *testing.T) {
	loc := amsterdam(t)
	ev := &model.Event{
		ID:       1,
		Timezone: "Europe/Amsterdam",
		Start:    time.Date(2026, 8, 25, 9, 0, 0, 0, loc),
		End:      time.Date(2026, 8, 25, 9, 30, 0, 0, loc),
	}

	// Inside the window.
	occ, err := Expand(ev, time.Date(2026, 8, 1, 0, 0, 0, 0, loc), time.Date(2026, 9, 1, 0, 0, 0, 0, loc))
	if err != nil {
		t.Fatal(err)
	}
	equal(t, starts(occ, loc), []string{"2026-08-25T09:00:00+02:00"})
	if occ[0].End.Sub(occ[0].Start) != 30*time.Minute {
		t.Errorf("duration = %v", occ[0].End.Sub(occ[0].Start))
	}
	if occ[0].EventID != 1 {
		t.Errorf("EventID = %d", occ[0].EventID)
	}

	// Outside the window.
	occ, err = Expand(ev, time.Date(2026, 9, 1, 0, 0, 0, 0, loc), time.Date(2026, 10, 1, 0, 0, 0, 0, loc))
	if err != nil {
		t.Fatal(err)
	}
	if len(occ) != 0 {
		t.Errorf("event outside the window returned %v", starts(occ, loc))
	}

	// Straddling the window start still counts: the meeting is in progress.
	occ, _ = Expand(ev,
		time.Date(2026, 8, 25, 9, 15, 0, 0, loc),
		time.Date(2026, 8, 25, 12, 0, 0, 0, loc))
	if len(occ) != 1 {
		t.Errorf("an in-progress event should overlap the window, got %v", starts(occ, loc))
	}
}

func TestExpandWeeklyAcrossDST(t *testing.T) {
	loc := amsterdam(t)
	// Clocks go back on 25 October 2026. A weekly 09:00 meeting must stay at
	// 09:00 wall clock, changing UTC offset rather than local time.
	ev := &model.Event{
		ID:       7,
		Timezone: "Europe/Amsterdam",
		Start:    time.Date(2026, 10, 13, 9, 0, 0, 0, loc),
		End:      time.Date(2026, 10, 13, 10, 0, 0, 0, loc),
		RRule:    "FREQ=WEEKLY;BYDAY=TU;COUNT=4",
	}
	occ, err := Expand(ev, time.Date(2026, 1, 1, 0, 0, 0, 0, loc), time.Date(2027, 1, 1, 0, 0, 0, 0, loc))
	if err != nil {
		t.Fatal(err)
	}
	equal(t, starts(occ, loc), []string{
		"2026-10-13T09:00:00+02:00",
		"2026-10-20T09:00:00+02:00",
		"2026-10-27T09:00:00+01:00", // after the transition
		"2026-11-03T09:00:00+01:00",
	})
	for _, o := range occ {
		if d := o.End.Sub(o.Start); d != time.Hour {
			t.Errorf("occurrence at %v has duration %v, want 1h", o.Start, d)
		}
	}
}

func TestExpandHonoursCount(t *testing.T) {
	loc := amsterdam(t)
	ev := &model.Event{
		Timezone: "Europe/Amsterdam",
		Start:    time.Date(2026, 3, 2, 8, 0, 0, 0, loc),
		End:      time.Date(2026, 3, 2, 8, 15, 0, 0, loc),
		RRule:    "FREQ=DAILY;COUNT=3",
	}
	occ, err := Expand(ev, time.Date(2026, 1, 1, 0, 0, 0, 0, loc), time.Date(2027, 1, 1, 0, 0, 0, 0, loc))
	if err != nil {
		t.Fatal(err)
	}
	equal(t, starts(occ, loc), []string{
		"2026-03-02T08:00:00+01:00",
		"2026-03-03T08:00:00+01:00",
		"2026-03-04T08:00:00+01:00",
	})
}

func TestExpandMonthlyByDay(t *testing.T) {
	loc := amsterdam(t)
	// Second Wednesday of every month.
	ev := &model.Event{
		Timezone: "Europe/Amsterdam",
		Start:    time.Date(2026, 1, 14, 10, 0, 0, 0, loc),
		End:      time.Date(2026, 1, 14, 11, 0, 0, 0, loc),
		RRule:    "FREQ=MONTHLY;BYDAY=2WE",
	}
	occ, err := Expand(ev, time.Date(2026, 1, 1, 0, 0, 0, 0, loc), time.Date(2026, 6, 1, 0, 0, 0, 0, loc))
	if err != nil {
		t.Fatal(err)
	}
	equal(t, starts(occ, loc), []string{
		"2026-01-14T10:00:00+01:00",
		"2026-02-11T10:00:00+01:00",
		"2026-03-11T10:00:00+01:00",
		"2026-04-08T10:00:00+02:00",
		"2026-05-13T10:00:00+02:00",
	})
}

func TestExpandUntil(t *testing.T) {
	loc := amsterdam(t)
	ev := &model.Event{
		Timezone: "Europe/Amsterdam",
		Start:    time.Date(2026, 8, 3, 9, 0, 0, 0, loc),
		End:      time.Date(2026, 8, 3, 9, 30, 0, 0, loc),
		RRule:    "RRULE:FREQ=WEEKLY;BYDAY=MO;UNTIL=20260824T070000Z",
	}
	occ, err := Expand(ev, time.Date(2026, 1, 1, 0, 0, 0, 0, loc), time.Date(2027, 1, 1, 0, 0, 0, 0, loc))
	if err != nil {
		t.Fatal(err)
	}
	// UNTIL is inclusive; 07:00Z is 09:00 local on 24 August.
	equal(t, starts(occ, loc), []string{
		"2026-08-03T09:00:00+02:00",
		"2026-08-10T09:00:00+02:00",
		"2026-08-17T09:00:00+02:00",
		"2026-08-24T09:00:00+02:00",
	})
}

func TestExpandWindowClipsSeries(t *testing.T) {
	loc := amsterdam(t)
	ev := &model.Event{
		Timezone: "Europe/Amsterdam",
		Start:    time.Date(2026, 1, 1, 12, 0, 0, 0, loc),
		End:      time.Date(2026, 1, 1, 13, 0, 0, 0, loc),
		RRule:    "FREQ=DAILY",
	}
	occ, err := Expand(ev, time.Date(2026, 3, 10, 0, 0, 0, 0, loc), time.Date(2026, 3, 13, 0, 0, 0, 0, loc))
	if err != nil {
		t.Fatal(err)
	}
	equal(t, starts(occ, loc), []string{
		"2026-03-10T12:00:00+01:00",
		"2026-03-11T12:00:00+01:00",
		"2026-03-12T12:00:00+01:00",
	})
}

func TestExpandCapsUnboundedSeries(t *testing.T) {
	loc := amsterdam(t)
	ev := &model.Event{
		Timezone: "Europe/Amsterdam",
		Start:    time.Date(2000, 1, 1, 0, 0, 0, 0, loc),
		End:      time.Date(2000, 1, 1, 0, 1, 0, 0, loc),
		RRule:    "FREQ=HOURLY",
	}
	occ, err := Expand(ev, time.Date(2000, 1, 1, 0, 0, 0, 0, loc), time.Date(2030, 1, 1, 0, 0, 0, 0, loc))
	if err != nil {
		t.Fatal(err)
	}
	if len(occ) != MaxOccurrences {
		t.Errorf("got %d occurrences, want the cap %d", len(occ), MaxOccurrences)
	}
}

func TestExpandAllDayMultiDay(t *testing.T) {
	loc := amsterdam(t)
	// Stored the way providers hand them over: UTC midnight, exclusive end.
	ev := &model.Event{
		AllDay:   true,
		Timezone: "Europe/Amsterdam",
		Start:    time.Date(2026, 8, 26, 0, 0, 0, 0, time.UTC),
		End:      time.Date(2026, 8, 29, 0, 0, 0, 0, time.UTC),
	}
	occ, err := Expand(ev, time.Date(2026, 8, 1, 0, 0, 0, 0, loc), time.Date(2026, 9, 1, 0, 0, 0, 0, loc))
	if err != nil {
		t.Fatal(err)
	}
	if len(occ) != 1 {
		t.Fatalf("got %d occurrences", len(occ))
	}
	// Anchored at local midnight, not 02:00 local.
	if got := occ[0].Start.In(loc).Format(time.RFC3339); got != "2026-08-26T00:00:00+02:00" {
		t.Errorf("start = %s, want local midnight", got)
	}
	if d := occ[0].End.Sub(occ[0].Start); d != 72*time.Hour {
		t.Errorf("duration = %v, want 72h", d)
	}
}

func TestExpandAllDayRecurring(t *testing.T) {
	loc := amsterdam(t)
	ev := &model.Event{
		AllDay:   true,
		Timezone: "Europe/Amsterdam",
		Start:    time.Date(2026, 10, 22, 0, 0, 0, 0, time.UTC),
		End:      time.Date(2026, 10, 23, 0, 0, 0, 0, time.UTC),
		RRule:    "FREQ=WEEKLY;COUNT=3",
	}
	occ, err := Expand(ev, time.Date(2026, 10, 1, 0, 0, 0, 0, loc), time.Date(2026, 12, 1, 0, 0, 0, 0, loc))
	if err != nil {
		t.Fatal(err)
	}
	// Every instance is local midnight, including the one after the clocks
	// change on 25 October.
	equal(t, starts(occ, loc), []string{
		"2026-10-22T00:00:00+02:00",
		"2026-10-29T00:00:00+01:00",
		"2026-11-05T00:00:00+01:00",
	})
}

func TestExpandBadRule(t *testing.T) {
	ev := &model.Event{
		UID:      "x",
		Timezone: "UTC",
		Start:    time.Now(),
		End:      time.Now().Add(time.Hour),
		RRule:    "FREQ=NEVER",
	}
	if _, err := Expand(ev, time.Now().AddDate(-1, 0, 0), time.Now().AddDate(1, 0, 0)); err == nil {
		t.Error("a malformed RRULE should be an error, not silence")
	}
}

func TestExpandNilAndMissingTimezone(t *testing.T) {
	if occ, err := Expand(nil, time.Now(), time.Now()); err != nil || occ != nil {
		t.Errorf("Expand(nil) = %v, %v", occ, err)
	}
	// No Timezone: the zone the Start carries is used, not a hard-coded one.
	start := time.Date(2026, 8, 25, 9, 0, 0, 0, time.UTC)
	ev := &model.Event{Start: start, End: start.Add(time.Hour)}
	occ, err := Expand(ev, start.Add(-time.Hour), start.Add(2*time.Hour))
	if err != nil || len(occ) != 1 || !occ[0].Start.Equal(start) {
		t.Errorf("Expand without a timezone = %v, %v", occ, err)
	}
}

func TestApplyExceptionsMovedInstance(t *testing.T) {
	loc := amsterdam(t)
	ev := &model.Event{
		ID:       3,
		Timezone: "Europe/Amsterdam",
		Start:    time.Date(2026, 8, 3, 9, 0, 0, 0, loc),
		End:      time.Date(2026, 8, 3, 9, 30, 0, 0, loc),
		RRule:    "FREQ=WEEKLY;BYDAY=MO;COUNT=3",
	}
	occ, err := Expand(ev, time.Date(2026, 8, 1, 0, 0, 0, 0, loc), time.Date(2026, 9, 1, 0, 0, 0, 0, loc))
	if err != nil {
		t.Fatal(err)
	}

	// The 10 August instance moves to 14:00 the same day.
	moved := model.Event{
		ID:           31,
		Timezone:     "Europe/Amsterdam",
		RecurrenceID: "2026-08-10T09:00:00+02:00",
		Start:        time.Date(2026, 8, 10, 14, 0, 0, 0, loc),
		End:          time.Date(2026, 8, 10, 15, 0, 0, 0, loc),
		Status:       model.StatusConfirmed,
	}
	got := ApplyExceptions(occ, []model.Event{moved})
	equal(t, starts(got, loc), []string{
		"2026-08-03T09:00:00+02:00",
		"2026-08-10T14:00:00+02:00",
		"2026-08-17T09:00:00+02:00",
	})
	if got[1].EventID != 31 {
		t.Errorf("moved instance should carry the exception's id, got %d", got[1].EventID)
	}
	if d := got[1].End.Sub(got[1].Start); d != time.Hour {
		t.Errorf("moved instance duration = %v, want the exception's 1h", d)
	}
}

func TestApplyExceptionsCancelledInstance(t *testing.T) {
	loc := amsterdam(t)
	ev := &model.Event{
		ID:       3,
		Timezone: "Europe/Amsterdam",
		Start:    time.Date(2026, 8, 3, 9, 0, 0, 0, loc),
		End:      time.Date(2026, 8, 3, 9, 30, 0, 0, loc),
		RRule:    "FREQ=WEEKLY;BYDAY=MO;COUNT=3",
	}
	occ, _ := Expand(ev, time.Date(2026, 8, 1, 0, 0, 0, 0, loc), time.Date(2026, 9, 1, 0, 0, 0, 0, loc))

	cancelled := model.Event{
		Timezone:     "Europe/Amsterdam",
		RecurrenceID: "2026-08-10T09:00:00", // JSCalendar LocalDateTime, no zone
		Start:        time.Date(2026, 8, 10, 9, 0, 0, 0, loc),
		End:          time.Date(2026, 8, 10, 9, 30, 0, 0, loc),
		Status:       model.StatusCancelled,
	}
	got := ApplyExceptions(occ, []model.Event{cancelled})
	equal(t, starts(got, loc), []string{
		"2026-08-03T09:00:00+02:00",
		"2026-08-17T09:00:00+02:00",
	})
}

func TestApplyExceptionsCompactRecurrenceID(t *testing.T) {
	loc := amsterdam(t)
	occ := []model.Occurrence{
		{EventID: 1, Start: time.Date(2026, 8, 3, 9, 0, 0, 0, loc), End: time.Date(2026, 8, 3, 9, 30, 0, 0, loc)},
		{EventID: 1, Start: time.Date(2026, 8, 10, 9, 0, 0, 0, loc), End: time.Date(2026, 8, 10, 9, 30, 0, 0, loc)},
	}
	// iCalendar's compact UTC form: 07:00Z is 09:00 in Amsterdam.
	ex := model.Event{Timezone: "Europe/Amsterdam", RecurrenceID: "20260810T070000Z", Status: model.StatusCancelled}
	got := ApplyExceptions(occ, []model.Event{ex})
	equal(t, starts(got, loc), []string{"2026-08-03T09:00:00+02:00"})
}

func TestApplyExceptionsIgnoresJunk(t *testing.T) {
	loc := amsterdam(t)
	occ := []model.Occurrence{
		{EventID: 1, Start: time.Date(2026, 8, 3, 9, 0, 0, 0, loc), End: time.Date(2026, 8, 3, 9, 30, 0, 0, loc)},
	}
	// An exception with no usable recurrence id is neither removed nor added.
	got := ApplyExceptions(occ, []model.Event{{RecurrenceID: "sometime"}})
	equal(t, starts(got, loc), []string{"2026-08-03T09:00:00+02:00"})

	// And with no exceptions at all the input is returned untouched.
	if got := ApplyExceptions(occ, nil); len(got) != 1 {
		t.Errorf("ApplyExceptions with no exceptions changed the series: %v", got)
	}
}

func TestApplyExceptionsSortsResult(t *testing.T) {
	loc := amsterdam(t)
	occ := []model.Occurrence{
		{EventID: 1, Start: time.Date(2026, 8, 3, 9, 0, 0, 0, loc), End: time.Date(2026, 8, 3, 10, 0, 0, 0, loc)},
		{EventID: 1, Start: time.Date(2026, 8, 10, 9, 0, 0, 0, loc), End: time.Date(2026, 8, 10, 10, 0, 0, 0, loc)},
		{EventID: 1, Start: time.Date(2026, 8, 17, 9, 0, 0, 0, loc), End: time.Date(2026, 8, 17, 10, 0, 0, 0, loc)},
	}
	// The last instance moves earlier than the second: order must be fixed up.
	moved := model.Event{
		ID: 9, Timezone: "Europe/Amsterdam",
		RecurrenceID: "2026-08-17T09:00:00+02:00",
		Start:        time.Date(2026, 8, 5, 9, 0, 0, 0, loc),
		End:          time.Date(2026, 8, 5, 10, 0, 0, 0, loc),
	}
	equal(t, starts(ApplyExceptions(occ, []model.Event{moved}), loc), []string{
		"2026-08-03T09:00:00+02:00",
		"2026-08-05T09:00:00+02:00",
		"2026-08-10T09:00:00+02:00",
	})
}

func TestDefaultWindow(t *testing.T) {
	now := time.Date(2026, 8, 25, 12, 0, 0, 0, time.UTC)
	from, to := DefaultWindow(now)
	if got := from.Format("2006-01-02"); got != "2025-08-25" {
		t.Errorf("from = %s, want a year back", got)
	}
	if got := to.Format("2006-01-02"); got != "2028-08-25" {
		t.Errorf("to = %s, want two years ahead", got)
	}
}
