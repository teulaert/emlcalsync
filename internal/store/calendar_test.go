package store

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/teulaert/emlcalsync/internal/model"
)

func seedCalendars(t *testing.T, s *Store, accountID string) {
	t.Helper()
	if err := s.ReplaceCalendars(context.Background(), accountID, []model.Calendar{
		{RemoteID: "primary", Name: "Personal", Color: "#3366cc", Timezone: "Europe/Amsterdam",
			Primary: true, AccessRole: "owner"},
		{RemoteID: "team@group.calendar", Name: "Team", Timezone: "Europe/Amsterdam", AccessRole: "writer"},
	}); err != nil {
		t.Fatalf("ReplaceCalendars: %v", err)
	}
}

func TestReplaceCalendars(t *testing.T) {
	ctx := context.Background()
	s := newTestStore(t)
	seedAccount(t, s, "work")
	seedCalendars(t, s, "work")

	cals, err := s.ListCalendars(ctx, []string{"work"})
	if err != nil {
		t.Fatal(err)
	}
	if len(cals) != 2 {
		t.Fatalf("%d calendars, want 2", len(cals))
	}
	if !cals[0].Primary || cals[0].RemoteID != "primary" {
		t.Fatalf("primary calendar not first: %+v", cals[0])
	}
	if cals[0].Color != "#3366cc" || cals[0].Timezone != "Europe/Amsterdam" || cals[0].AccessRole != "owner" {
		t.Fatalf("calendar fields = %+v", cals[0])
	}
	if cals[0].ID == 0 {
		t.Fatal("calendar id not set")
	}

	// An event on a calendar that later disappears goes with it.
	ev := &model.Event{AccountID: "work", CalendarRemote: "team@group.calendar", RemoteID: "e1",
		Title: "Standup", Start: base, End: base.Add(30 * time.Minute), RawJSON: []byte(`{}`)}
	if _, err := s.UpsertEvent(ctx, ev); err != nil {
		t.Fatal(err)
	}
	if err := s.ReplaceOccurrences(ctx, ev.ID, []model.Occurrence{
		{EventID: ev.ID, Start: base, End: base.Add(30 * time.Minute)}}); err != nil {
		t.Fatal(err)
	}

	if err := s.ReplaceCalendars(ctx, "work", []model.Calendar{
		{RemoteID: "primary", Name: "Personal (renamed)", Primary: true, AccessRole: "owner"},
	}); err != nil {
		t.Fatal(err)
	}
	cals, _ = s.ListCalendars(ctx, []string{"work"})
	if len(cals) != 1 || cals[0].Name != "Personal (renamed)" {
		t.Fatalf("after replace: %+v", cals)
	}
	var n int
	if err := s.DB().QueryRowContext(ctx, `SELECT count(*) FROM events`).Scan(&n); err != nil {
		t.Fatal(err)
	}
	if n != 0 {
		t.Fatalf("%d events survived their calendar", n)
	}
	if err := s.DB().QueryRowContext(ctx, `SELECT count(*) FROM event_occurrences`).Scan(&n); err != nil {
		t.Fatal(err)
	}
	if n != 0 {
		t.Fatalf("%d occurrences survived their calendar", n)
	}

	if _, err := s.GetCalendarByRemote(ctx, "work", "gone"); !errors.Is(err, model.ErrNotFound) {
		t.Fatalf("GetCalendarByRemote(missing) = %v", err)
	}
}

func TestFindCalendar(t *testing.T) {
	ctx := context.Background()
	s := newTestStore(t)
	seedAccount(t, s, "work")
	seedCalendars(t, s, "work")

	for _, tc := range []struct{ query, want string }{
		{"", "primary"},
		{"primary", "primary"},
		{"team@group.calendar", "team@group.calendar"},
		{"Team", "team@group.calendar"},
		{"team", "team@group.calendar"},
		{"personal", "primary"},
	} {
		c, err := s.FindCalendar(ctx, "work", tc.query)
		if err != nil {
			t.Fatalf("FindCalendar(%q): %v", tc.query, err)
		}
		if c.RemoteID != tc.want {
			t.Errorf("FindCalendar(%q) = %s, want %s", tc.query, c.RemoteID, tc.want)
		}
	}
	if _, err := s.FindCalendar(ctx, "work", "nope"); !errors.Is(err, model.ErrNotFound) {
		t.Fatalf("FindCalendar(missing) = %v", err)
	}
}

func TestUpsertEvent(t *testing.T) {
	ctx := context.Background()
	s := newTestStore(t)
	seedAccount(t, s, "work")
	seedCalendars(t, s, "work")

	ev := &model.Event{
		AccountID: "work", CalendarRemote: "primary", RemoteID: "ev-1",
		UID:         "uid-1@example.com",
		Title:       "Design review",
		Description: "Go through the store API",
		Location:    "Room 3",
		Start:       base, End: base.Add(time.Hour),
		Timezone:  "Europe/Amsterdam",
		RRule:     "FREQ=WEEKLY;BYDAY=MO;COUNT=4",
		Status:    model.StatusConfirmed,
		Organizer: addr("Alice", "alice@example.com"),
		Attendees: []model.Attendee{
			{Name: "Bob", Email: "bob@example.com", Response: model.PartAccepted},
			{Email: "carol@example.com", Response: model.PartNeedsAction, Optional: true},
		},
		MyResponse: model.PartAccepted,
		RawJSON:    []byte(`{"id":"ev-1","etag":"x"}`),
		Updated:    base,
	}
	id, err := s.UpsertEvent(ctx, ev)
	if err != nil {
		t.Fatalf("UpsertEvent: %v", err)
	}
	if id == 0 || ev.ID != id || ev.CalendarID == 0 {
		t.Fatalf("ids not written back: %+v", ev)
	}

	got, err := s.GetEvent(ctx, "work", "primary", "ev-1")
	if err != nil {
		t.Fatalf("GetEvent: %v", err)
	}
	if got.Title != "Design review" || got.Location != "Room 3" || got.Description == "" {
		t.Errorf("event = %+v", got)
	}
	if !got.Start.Equal(base) || !got.End.Equal(base.Add(time.Hour)) {
		t.Errorf("times = %v..%v", got.Start, got.End)
	}
	if got.RRule != "FREQ=WEEKLY;BYDAY=MO;COUNT=4" {
		t.Errorf("RRule = %q", got.RRule)
	}
	if got.Organizer.Email != "alice@example.com" || got.Organizer.Name != "Alice" {
		t.Errorf("Organizer = %+v", got.Organizer)
	}
	if len(got.Attendees) != 2 || got.Attendees[0].Response != model.PartAccepted || !got.Attendees[1].Optional {
		t.Errorf("Attendees = %+v", got.Attendees)
	}
	if got.MyResponse != model.PartAccepted || got.Status != model.StatusConfirmed {
		t.Errorf("status/response = %s/%s", got.Status, got.MyResponse)
	}
	if string(got.RawJSON) != `{"id":"ev-1","etag":"x"}` {
		t.Errorf("RawJSON = %s", got.RawJSON)
	}
	if got.AccountID != "work" || got.CalendarRemote != "primary" {
		t.Errorf("denormalised calendar fields = %+v", got)
	}
	if got.PublicID() != "work:c:primary:ev-1" {
		t.Errorf("PublicID = %s", got.PublicID())
	}

	// Update in place.
	ev.Title = "Design review (moved)"
	ev.Start = base.Add(2 * time.Hour)
	ev.MyResponse = model.PartTentative
	if id2, err := s.UpsertEvent(ctx, ev); err != nil || id2 != id {
		t.Fatalf("second UpsertEvent = %d %v", id2, err)
	}
	got, _ = s.GetEvent(ctx, "work", "primary", "ev-1")
	if got.Title != "Design review (moved)" || !got.Start.Equal(base.Add(2*time.Hour)) {
		t.Fatalf("update did not apply: %+v", got)
	}
	if got.MyResponse != model.PartTentative {
		t.Fatalf("MyResponse = %s", got.MyResponse)
	}

	byID, err := s.GetEventByID(ctx, id)
	if err != nil || byID.RemoteID != "ev-1" {
		t.Fatalf("GetEventByID = %+v %v", byID, err)
	}

	if _, err := s.GetEvent(ctx, "work", "primary", "missing"); !errors.Is(err, model.ErrNotFound) {
		t.Fatalf("GetEvent(missing) = %v", err)
	}
	// An event for an unknown calendar is a not-found, not a broken row.
	if _, err := s.UpsertEvent(ctx, &model.Event{
		AccountID: "work", CalendarRemote: "no-such-calendar", RemoteID: "x", RawJSON: []byte(`{}`),
	}); !errors.Is(err, model.ErrNotFound) {
		t.Fatalf("UpsertEvent(unknown calendar) = %v, want ErrNotFound", err)
	}
	if _, err := s.UpsertEvent(ctx, &model.Event{RemoteID: "x"}); err == nil {
		t.Fatal("UpsertEvent without calendar coordinates should fail")
	}
}

func TestOccurrencesWindowQuery(t *testing.T) {
	ctx := context.Background()
	s := newTestStore(t)
	seedAccount(t, s, "work")
	seedAccount(t, s, "personal")
	seedCalendars(t, s, "work")
	seedCalendars(t, s, "personal")

	day := 24 * time.Hour
	weekly := &model.Event{
		AccountID: "work", CalendarRemote: "primary", RemoteID: "weekly",
		Title: "Weekly sync", Start: base, End: base.Add(time.Hour),
		RRule: "FREQ=WEEKLY;COUNT=3", Status: model.StatusConfirmed,
		MyResponse: model.PartAccepted, Organizer: addr("Alice", "alice@example.com"),
		Timezone: "Europe/Amsterdam", RawJSON: []byte(`{}`), Updated: base,
	}
	if _, err := s.UpsertEvent(ctx, weekly); err != nil {
		t.Fatal(err)
	}
	if err := s.ReplaceOccurrences(ctx, weekly.ID, []model.Occurrence{
		{EventID: weekly.ID, Start: base, End: base.Add(time.Hour)},
		{EventID: weekly.ID, Start: base.Add(7 * day), End: base.Add(7*day + time.Hour)},
		{EventID: weekly.ID, Start: base.Add(14 * day), End: base.Add(14*day + time.Hour)},
	}); err != nil {
		t.Fatal(err)
	}

	allDay := &model.Event{
		AccountID: "work", CalendarRemote: "team@group.calendar", RemoteID: "offsite",
		Title: "Offsite", Start: base.Add(3 * day), End: base.Add(5 * day),
		AllDay: true, RawJSON: []byte(`{}`), Updated: base,
	}
	if _, err := s.UpsertEvent(ctx, allDay); err != nil {
		t.Fatal(err)
	}
	if err := s.ReplaceOccurrences(ctx, allDay.ID, []model.Occurrence{
		{EventID: allDay.ID, Start: base.Add(3 * day), End: base.Add(5 * day)}}); err != nil {
		t.Fatal(err)
	}

	other := &model.Event{
		AccountID: "personal", CalendarRemote: "primary", RemoteID: "dentist",
		Title: "Dentist", Start: base.Add(2 * day), End: base.Add(2*day + time.Hour),
		RawJSON: []byte(`{}`), Updated: base,
	}
	if _, err := s.UpsertEvent(ctx, other); err != nil {
		t.Fatal(err)
	}
	if err := s.ReplaceOccurrences(ctx, other.ID, []model.Occurrence{
		{EventID: other.ID, Start: base.Add(2 * day), End: base.Add(2*day + time.Hour)}}); err != nil {
		t.Fatal(err)
	}

	// A one-week agenda from base: weekly #1, dentist, offsite.
	occs, err := s.ListOccurrences(ctx, base.Add(-time.Minute), base.Add(7*day), nil)
	if err != nil {
		t.Fatalf("ListOccurrences: %v", err)
	}
	var titles []string
	for _, o := range occs {
		titles = append(titles, o.Title)
	}
	if !equalStrings(titles, []string{"Weekly sync", "Dentist", "Offsite"}) {
		t.Fatalf("agenda = %v", titles)
	}
	if !occs[0].Start.Equal(base) || !occs[0].End.Equal(base.Add(time.Hour)) {
		t.Errorf("occurrence times = %v..%v", occs[0].Start, occs[0].End)
	}
	if !occs[0].Recurring {
		t.Error("recurring flag not set on an RRULE event")
	}
	if occs[0].AccountID != "work" || occs[0].CalendarRemote != "primary" || occs[0].CalendarName != "Personal" {
		t.Errorf("calendar summary = %+v", occs[0])
	}
	if occs[0].EventRemoteID != "weekly" || occs[0].PublicID() != "work:c:primary:weekly" {
		t.Errorf("event identity = %+v", occs[0])
	}
	if occs[0].MyResponse != model.PartAccepted || occs[0].Organizer.Email != "alice@example.com" {
		t.Errorf("response/organizer = %+v", occs[0])
	}
	if !occs[2].AllDay {
		t.Error("all-day flag lost")
	}

	// An event that merely overlaps the window is included.
	overlap, err := s.ListOccurrences(ctx, base.Add(4*day), base.Add(4*day+time.Hour), nil)
	if err != nil {
		t.Fatal(err)
	}
	if len(overlap) != 1 || overlap[0].Title != "Offsite" {
		t.Fatalf("overlap query = %+v", overlap)
	}

	// Restricting to one calendar.
	cal, err := s.GetCalendarByRemote(ctx, "work", "primary")
	if err != nil {
		t.Fatal(err)
	}
	only, err := s.ListOccurrences(ctx, base.Add(-time.Minute), base.Add(30*day), []int64{cal.ID})
	if err != nil {
		t.Fatal(err)
	}
	if len(only) != 3 {
		t.Fatalf("calendar-scoped agenda = %d occurrences, want 3", len(only))
	}
	for _, o := range only {
		if o.CalendarID != cal.ID {
			t.Fatalf("occurrence from the wrong calendar: %+v", o)
		}
	}

	// Empty window.
	if none, err := s.ListOccurrences(ctx, base.Add(100*day), base.Add(101*day), nil); err != nil || len(none) != 0 {
		t.Fatalf("empty window = %d %v", len(none), err)
	}

	// ReplaceOccurrences is a replacement, not an append.
	if err := s.ReplaceOccurrences(ctx, weekly.ID, []model.Occurrence{
		{EventID: weekly.ID, Start: base, End: base.Add(time.Hour)}}); err != nil {
		t.Fatal(err)
	}
	if n, err := s.CountOccurrences(ctx, weekly.ID); err != nil || n != 1 {
		t.Fatalf("CountOccurrences = %d %v, want 1", n, err)
	}

	first, last, err := s.OccurrenceWindow(ctx, nil)
	if err != nil {
		t.Fatal(err)
	}
	if !first.Equal(base) || !last.Equal(base.Add(3*day)) {
		t.Fatalf("OccurrenceWindow = %v..%v", first, last)
	}

	// Recurring-event listing for the expander.
	rec, err := s.ListRecurringEvents(ctx, []string{"work"})
	if err != nil {
		t.Fatal(err)
	}
	if len(rec) != 1 || rec[0].RemoteID != "weekly" {
		t.Fatalf("ListRecurringEvents = %+v", rec)
	}
	if all, err := s.ListRecurringEvents(ctx, nil); err != nil || len(all) != 1 {
		t.Fatalf("ListRecurringEvents(all) = %d %v", len(all), err)
	}

	// Deleting an event drops it from the agenda but keeps the row.
	if err := s.MarkEventDeleted(ctx, other.CalendarID, "dentist"); err != nil {
		t.Fatalf("MarkEventDeleted: %v", err)
	}
	occs, _ = s.ListOccurrences(ctx, base.Add(-time.Minute), base.Add(7*day), nil)
	for _, o := range occs {
		if o.Title == "Dentist" {
			t.Fatal("deleted event still in the agenda")
		}
	}
	gone, err := s.GetEvent(ctx, "personal", "primary", "dentist")
	if err != nil {
		t.Fatalf("deleted event should still be readable: %v", err)
	}
	if gone.DeletedAt == nil {
		t.Error("DeletedAt not set")
	}
	if n, _ := s.CountOccurrences(ctx, other.ID); n != 0 {
		t.Errorf("occurrences of a deleted event = %d", n)
	}
	// Deleting something already gone is not an error.
	if err := s.MarkEventDeleted(ctx, other.CalendarID, "never-existed"); err != nil {
		t.Fatalf("MarkEventDeleted(missing) = %v", err)
	}
}

func TestEventExceptionsAndUpdatedSince(t *testing.T) {
	ctx := context.Background()
	s := newTestStore(t)
	seedAccount(t, s, "work")
	seedCalendars(t, s, "work")

	master := &model.Event{
		AccountID: "work", CalendarRemote: "primary", RemoteID: "series",
		Title: "Series", Start: base, End: base.Add(time.Hour),
		RRule: "FREQ=DAILY;COUNT=5", RawJSON: []byte(`{}`), Updated: base,
	}
	if _, err := s.UpsertEvent(ctx, master); err != nil {
		t.Fatal(err)
	}
	exception := &model.Event{
		AccountID: "work", CalendarRemote: "primary", RemoteID: "series_20260803",
		Title: "Series (moved)", Start: base.Add(50 * time.Hour), End: base.Add(51 * time.Hour),
		RecurrenceID: "2026-08-03T12:00:00Z", RawJSON: []byte(`{}`),
		Updated: base.Add(time.Hour),
	}
	if _, err := s.UpsertEvent(ctx, exception); err != nil {
		t.Fatal(err)
	}

	cal, err := s.GetCalendarByRemote(ctx, "work", "primary")
	if err != nil {
		t.Fatal(err)
	}
	exs, err := s.ListEventExceptions(ctx, cal.ID)
	if err != nil {
		t.Fatal(err)
	}
	if len(exs) != 1 || exs[0].RemoteID != "series_20260803" {
		t.Fatalf("ListEventExceptions = %+v", exs)
	}
	if exs[0].RecurrenceID != "2026-08-03T12:00:00Z" {
		t.Fatalf("RecurrenceID = %q", exs[0].RecurrenceID)
	}

	changed, err := s.ListEventsUpdatedSince(ctx, []string{"work"}, base.Add(30*time.Minute))
	if err != nil {
		t.Fatal(err)
	}
	if len(changed) != 1 || changed[0].RemoteID != "series_20260803" {
		t.Fatalf("ListEventsUpdatedSince = %+v", changed)
	}
	if all, err := s.ListEventsUpdatedSince(ctx, nil, base); err != nil || len(all) != 2 {
		t.Fatalf("ListEventsUpdatedSince(all) = %d %v", len(all), err)
	}
}

func TestFindEventsByUID(t *testing.T) {
	ctx := context.Background()
	s := newTestStore(t)
	seedAccount(t, s, "work")
	seedCalendars(t, s, "work")
	seedAccount(t, s, "home")
	if err := s.ReplaceCalendars(ctx, "home", []model.Calendar{
		{RemoteID: "primary", Name: "Home", Primary: true, AccessRole: "owner"},
	}); err != nil {
		t.Fatal(err)
	}
	base := time.Date(2026, 9, 2, 8, 0, 0, 0, time.UTC)
	put := func(account, cal, remote, uid, rid string, deleted bool) {
		t.Helper()
		ev := &model.Event{AccountID: account, CalendarRemote: cal, RemoteID: remote, UID: uid,
			Title: "Momentum FO", Start: base, End: base.Add(45 * time.Minute), RecurrenceID: rid,
			RawJSON: []byte(`{}`)}
		if deleted {
			d := base
			ev.DeletedAt = &d
		}
		if _, err := s.UpsertEvent(ctx, ev); err != nil {
			t.Fatal(err)
		}
	}
	// The same invitation filed on two accounts, plus an instance of it, a
	// deleted copy, and an unrelated event.
	put("home", "primary", "h1", "uid-1", "", false)
	put("work", "primary", "w1", "uid-1", "", false)
	put("work", "primary", "w1;20260909T080000", "uid-1", "2026-09-09T08:00:00Z", false)
	put("work", "team@group.calendar", "gone", "uid-1", "", true)
	put("work", "primary", "w2", "uid-2", "", false)

	evs, err := s.FindEventsByUID(ctx, nil, "uid-1")
	if err != nil {
		t.Fatal(err)
	}
	var got []string
	for _, e := range evs {
		got = append(got, e.PublicID())
	}
	want := []string{"home:c:primary:h1", "work:c:primary:w1"}
	if len(got) != len(want) || got[0] != want[0] || got[1] != want[1] {
		t.Errorf("all accounts: %v, want %v", got, want)
	}

	evs, err = s.FindEventsByUID(ctx, []string{"work"}, "uid-1")
	if err != nil {
		t.Fatal(err)
	}
	if len(evs) != 1 || evs[0].RemoteID != "w1" {
		t.Errorf("work only: %+v", evs)
	}
	if evs, _ := s.FindEventsByUID(ctx, nil, ""); len(evs) != 0 {
		t.Errorf("an empty uid matched %d events", len(evs))
	}
}
