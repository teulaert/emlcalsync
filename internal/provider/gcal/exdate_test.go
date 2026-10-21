package gcal

import (
	"context"
	"encoding/json"
	"strings"
	"testing"
	"time"

	calendarapi "google.golang.org/api/calendar/v3"

	"github.com/teulaert/emlcalsync/internal/calendar"
	"github.com/teulaert/emlcalsync/internal/model"
)

// sponsortje is the series as Google delivered it: a weekly meeting whose
// 14 and 21 October instances were deleted, which Google expressed as EXDATE
// lines on the master and nothing else. The start carries a +02:00 offset but
// declares UTC as its zone, and the exclusions are stamped in
// Europe/Amsterdam -- three zones in one object, each of which has to be
// read as written.
func sponsortje() *calendarapi.Event {
	return &calendarapi.Event{
		Id: "g7ifrv1337tr318er1n3aq3uc4", ICalUID: "g7ifrv1337tr318er1n3aq3uc4@google.com",
		Summary: "Overleg 't Sponsortje", Status: statusConfirmed,
		Start: &calendarapi.EventDateTime{DateTime: "2026-10-07T13:00:00+02:00", TimeZone: "UTC"},
		End:   &calendarapi.EventDateTime{DateTime: "2026-10-07T13:30:00+02:00", TimeZone: "UTC"},
		Recurrence: []string{
			"EXDATE;TZID=Europe/Amsterdam:20261014T130000,20261021T130000",
			"RRULE:FREQ=WEEKLY;BYDAY=WE",
		},
		Updated: "2026-10-13T15:33:01.165Z",
	}
}

// listOnce runs one full listing of a page holding items.
func listOnce(t *testing.T, calTZ string, items ...*calendarapi.Event) []model.Event {
	t.Helper()
	f := newFakeCalendar(t)
	f.pages = []*calendarapi.Events{{TimeZone: calTZ, NextSyncToken: "token-1", Items: items}}
	ch, err := newCal(t, f).EventChanges(context.Background(), "cal-1", "")
	if err != nil {
		t.Fatalf("EventChanges: %v", err)
	}
	if len(ch.Removed) != 0 {
		t.Fatalf("Removed = %v, want none", ch.Removed)
	}
	return ch.Upserted
}

// expanded runs the mapped events through the same expansion the sync engine
// uses, over [from, to), and returns the occurrence starts in UTC.
func expanded(t *testing.T, evs []model.Event, from, to time.Time) []time.Time {
	t.Helper()
	var master *model.Event
	var exceptions []model.Event
	for i := range evs {
		if evs[i].RecurrenceID != "" {
			exceptions = append(exceptions, evs[i])
		} else if master == nil {
			master = &evs[i]
		}
	}
	if master == nil {
		t.Fatal("no master among the mapped events")
	}
	occ, err := calendar.Expand(master, from, to)
	if err != nil {
		t.Fatalf("Expand: %v", err)
	}
	occ = calendar.ApplyExceptions(occ, exceptions)
	out := make([]time.Time, 0, len(occ))
	for _, o := range occ {
		out = append(out, o.Start.UTC())
	}
	return out
}

func wantStarts(t *testing.T, got []time.Time, want ...time.Time) {
	t.Helper()
	if len(got) != len(want) {
		t.Fatalf("occurrences = %v\nwant %v", got, want)
	}
	for i := range want {
		if !got[i].Equal(want[i]) {
			t.Fatalf("occurrence %d = %v, want %v\nall: %v", i, got[i], want[i], got)
		}
	}
}

func utc(y int, m time.Month, d, h, min int) time.Time {
	return time.Date(y, m, d, h, min, 0, 0, time.UTC)
}

// An EXDATE on a Google master has nowhere to go in the flat event model, so
// it becomes what the CalDAV and JMAP providers already make of one: a
// cancelled instance, under the id Google itself gives that instance.
func TestExdateBecomesCancelledInstances(t *testing.T) {
	evs := listOnce(t, "Europe/Amsterdam", sponsortje())
	if len(evs) != 3 {
		t.Fatalf("mapped %d events, want the master and two cancelled instances: %+v", len(evs), evs)
	}
	master := evs[0]
	if master.RRule != "FREQ=WEEKLY;BYDAY=WE" || master.RecurrenceID != "" {
		t.Errorf("master = rrule %q, recurrence id %q", master.RRule, master.RecurrenceID)
	}

	want := []struct {
		id    string
		start time.Time
	}{
		{"g7ifrv1337tr318er1n3aq3uc4_20261014T110000Z", utc(2026, 10, 14, 11, 0)},
		{"g7ifrv1337tr318er1n3aq3uc4_20261021T110000Z", utc(2026, 10, 21, 11, 0)},
	}
	for i, w := range want {
		ex := evs[1+i]
		if ex.RemoteID != w.id {
			t.Errorf("exception %d id = %q, want %q (Google's own instance id)", i, ex.RemoteID, w.id)
		}
		if ex.Status != model.StatusCancelled {
			t.Errorf("exception %d status = %q, want cancelled", i, ex.Status)
		}
		if !ex.Start.Equal(w.start) || !ex.End.Equal(w.start.Add(30*time.Minute)) {
			t.Errorf("exception %d = %v..%v, want %v and the master's half hour", i, ex.Start, ex.End, w.start)
		}
		rid, err := time.Parse(time.RFC3339, ex.RecurrenceID)
		if err != nil || !rid.Equal(w.start) {
			t.Errorf("exception %d recurrence id = %q (%v), want the instant %v", i, ex.RecurrenceID, err, w.start)
		}
		if ex.UID != master.UID || ex.Title != master.Title || ex.CalendarRemote != "cal-1" {
			t.Errorf("exception %d does not carry the master's uid, title and calendar: %+v", i, ex)
		}
		if ex.Timezone != "Europe/Amsterdam" {
			t.Errorf("exception %d timezone = %q, want the EXDATE's TZID", i, ex.Timezone)
		}
		if !ex.Updated.Equal(master.Updated) {
			t.Errorf("exception %d updated = %v, want the master's %v", i, ex.Updated, master.Updated)
		}
		// Its raw object is what Google would have sent for the instance,
		// so nothing downstream can tell the two apart.
		var raw calendarapi.Event
		if err := json.Unmarshal(ex.RawJSON, &raw); err != nil {
			t.Fatalf("exception %d raw json: %v", i, err)
		}
		if raw.Id != w.id || raw.Status != statusCancelled || raw.RecurringEventId != "g7ifrv1337tr318er1n3aq3uc4" ||
			raw.OriginalStartTime == nil || raw.OriginalStartTime.DateTime == "" {
			t.Errorf("exception %d raw json = %s", i, ex.RawJSON)
		}
	}

	// Through the expander: 14 and 21 October are gone, the rest of the
	// autumn is there. The series is declared in UTC, so it stays at 11:00Z
	// across the end of summer time, which is what Google shows for it.
	got := expanded(t, evs, utc(2026, 10, 1, 0, 0), utc(2026, 11, 30, 0, 0))
	wantStarts(t, got,
		utc(2026, 10, 7, 11, 0), utc(2026, 10, 28, 11, 0),
		utc(2026, 11, 4, 11, 0), utc(2026, 11, 11, 11, 0), utc(2026, 11, 18, 11, 0), utc(2026, 11, 25, 11, 0))
}

// When Google also delivers the cancelled instance itself, the one made from
// the EXDATE has the same remote id, so the index holds one row, not two.
func TestExdateMatchesTheDeliveredCancellation(t *testing.T) {
	delivered := &calendarapi.Event{
		Id: "g7ifrv1337tr318er1n3aq3uc4_20261014T110000Z", Status: statusCancelled,
		RecurringEventId:  "g7ifrv1337tr318er1n3aq3uc4",
		OriginalStartTime: &calendarapi.EventDateTime{DateTime: "2026-10-14T13:00:00+02:00", TimeZone: "UTC"},
	}
	evs := listOnce(t, "Europe/Amsterdam", sponsortje(), delivered)
	ids := map[string]int{}
	for _, ev := range evs {
		ids[ev.RemoteID]++
	}
	if ids["g7ifrv1337tr318er1n3aq3uc4_20261014T110000Z"] != 2 {
		t.Errorf("ids = %v, want the EXDATE and the delivered instance to share one id", ids)
	}
	got := expanded(t, evs, utc(2026, 10, 1, 0, 0), utc(2026, 10, 31, 0, 0))
	wantStarts(t, got, utc(2026, 10, 7, 11, 0), utc(2026, 10, 28, 11, 0))
}

// Every form an EXDATE value takes resolves to the instant it names: a TZID
// across a DST change, a UTC stamp, a floating stamp read in the series'
// zone, and several values on one line. A value that cannot be read is
// skipped, not fatal, and never hides anything.
func TestExdateFormsAcrossDST(t *testing.T) {
	ev := &calendarapi.Event{
		Id: "wk", ICalUID: "wk@google.com", Summary: "Weekly", Status: statusConfirmed,
		Start: &calendarapi.EventDateTime{DateTime: "2026-10-07T13:00:00+02:00", TimeZone: "Europe/Amsterdam"},
		End:   &calendarapi.EventDateTime{DateTime: "2026-10-07T14:00:00+02:00", TimeZone: "Europe/Amsterdam"},
		Recurrence: []string{
			"RRULE:FREQ=WEEKLY;BYDAY=WE",
			"EXDATE;TZID=Europe/Amsterdam:20261104T130000", // CET by then: 12:00Z
			"EXDATE:20261111T120000Z",
			"EXDATE:20261125T130000,20261202T130000", // floating: the series' zone
			"EXDATE;TZID=Europe/Amsterdam:not-a-date",
			"EXDATE;TZID=Mars/Olympus_Mons:20261209T130000",
		},
	}
	evs := listOnce(t, "Europe/Amsterdam", ev)
	if len(evs) != 5 {
		t.Fatalf("mapped %d events, want the master and four cancelled instances: %+v", len(evs), evs)
	}
	for _, want := range []time.Time{
		utc(2026, 11, 4, 12, 0), utc(2026, 11, 11, 12, 0), utc(2026, 11, 25, 12, 0), utc(2026, 12, 2, 12, 0),
	} {
		found := false
		for _, ex := range evs[1:] {
			if ex.Start.Equal(want) {
				found = true
			}
		}
		if !found {
			t.Errorf("no cancelled instance at %v: %+v", want, evs[1:])
		}
	}
	got := expanded(t, evs, utc(2026, 10, 1, 0, 0), utc(2026, 12, 10, 0, 0))
	wantStarts(t, got,
		utc(2026, 10, 7, 11, 0), utc(2026, 10, 14, 11, 0), utc(2026, 10, 21, 11, 0),
		utc(2026, 10, 28, 12, 0), utc(2026, 11, 18, 12, 0), utc(2026, 12, 9, 12, 0))
}

// An all-day series excludes whole days, under Google's date-only instance id.
func TestExdateAllDay(t *testing.T) {
	ev := &calendarapi.Event{
		Id: "day", ICalUID: "day@google.com", Summary: "Office day", Status: statusConfirmed,
		Start:      &calendarapi.EventDateTime{Date: "2026-10-07"},
		End:        &calendarapi.EventDateTime{Date: "2026-10-08"},
		Recurrence: []string{"RRULE:FREQ=WEEKLY;BYDAY=WE", "EXDATE;VALUE=DATE:20261014"},
	}
	evs := listOnce(t, "Europe/Amsterdam", ev)
	if len(evs) != 2 {
		t.Fatalf("mapped %d events, want the master and one cancelled day: %+v", len(evs), evs)
	}
	ex := evs[1]
	if ex.RemoteID != "day_20261014" || !ex.AllDay {
		t.Errorf("exception = id %q all-day %v, want day_20261014 and all-day", ex.RemoteID, ex.AllDay)
	}
	loc, _ := time.LoadLocation("Europe/Amsterdam")
	if want := time.Date(2026, 10, 14, 0, 0, 0, 0, loc); !ex.Start.Equal(want) || !ex.End.Equal(want.AddDate(0, 0, 1)) {
		t.Errorf("exception = %v..%v, want the whole of 14 October in the calendar's zone", ex.Start, ex.End)
	}
	got := expanded(t, evs, utc(2026, 10, 1, 0, 0), utc(2026, 10, 31, 0, 0))
	if len(got) != 3 {
		t.Fatalf("occurrences = %v, want three Wednesdays", got)
	}
	for _, o := range got {
		if o.In(loc).Day() == 14 {
			t.Errorf("14 October is still there: %v", got)
		}
	}
}

// A patch that touches a series has to carry the server's EXDATE lines back
// to it: recurrence is replaced whole, so an RRULE on its own would quietly
// restore every meeting that had been deleted from the series.
func TestUpdateEventKeepsExclusions(t *testing.T) {
	f := newFakeCalendar(t)
	c := newCal(t, f)
	stored := &calendarapi.Event{
		Id: "ev-r", Summary: "Weekly sync", Status: statusConfirmed,
		Recurrence: []string{"RRULE:FREQ=WEEKLY;BYDAY=WE", "EXDATE;TZID=Europe/Amsterdam:20261014T130000"},
	}
	f.events["ev-r"] = stored
	raw, _ := json.Marshal(stored)

	// Same rule, new title: both lines come back untouched.
	if _, err := c.UpdateEvent(context.Background(), &model.Event{
		CalendarRemote: "cal-1", RemoteID: "ev-r", Title: "Weekly sync (renamed)",
		RRule: "FREQ=WEEKLY;BYDAY=WE", RawJSON: raw,
	}); err != nil {
		t.Fatalf("UpdateEvent: %v", err)
	}
	f.mu.Lock()
	got := strings.Join(f.events["ev-r"].Recurrence, "\n")
	f.mu.Unlock()
	if got != "RRULE:FREQ=WEEKLY;BYDAY=WE\nEXDATE;TZID=Europe/Amsterdam:20261014T130000" {
		t.Errorf("recurrence after a rename:\n%s", got)
	}

	// A new rule keeps the exclusions too: an EXDATE the new rule never
	// generates is harmless, one dropped brings a deleted meeting back.
	if _, err := c.UpdateEvent(context.Background(), &model.Event{
		CalendarRemote: "cal-1", RemoteID: "ev-r", RRule: "FREQ=DAILY;COUNT=9", RawJSON: raw,
	}); err != nil {
		t.Fatalf("UpdateEvent: %v", err)
	}
	f.mu.Lock()
	got = strings.Join(f.events["ev-r"].Recurrence, "\n")
	f.mu.Unlock()
	if got != "RRULE:FREQ=DAILY;COUNT=9\nEXDATE;TZID=Europe/Amsterdam:20261014T130000" {
		t.Errorf("recurrence after a rule change:\n%s", got)
	}
}
