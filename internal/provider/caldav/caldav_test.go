package caldav

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"strings"
	"testing"
	"time"

	"github.com/teulaert/emlcalsync/internal/calendar"
	"github.com/teulaert/emlcalsync/internal/model"
	"github.com/teulaert/emlcalsync/internal/provider"
	"github.com/teulaert/emlcalsync/internal/provider/caldav/caldavfake"
)

const (
	testEmail = "me@example.com"
	testPass  = "app-password"
)

func quietLogger() *slog.Logger {
	return slog.New(slog.NewTextHandler(io.Discard, &slog.HandlerOptions{Level: slog.LevelError}))
}

// newFixture starts a fake server with one "Calendar" collection and returns
// the provider, the server and the calendar path.
func newFixture(t *testing.T) (*Calendar, *caldavfake.Server, string) {
	t.Helper()
	srv := caldavfake.New()
	t.Cleanup(srv.Close)
	srv.User, srv.Password = testEmail, testPass
	home := srv.HomePath(testEmail)
	calPath := srv.AddCalendar(caldavfake.Calendar{
		Path: home + "Default/", Name: "Calendar", Color: "#3a429cff", Timezone: "Europe/Amsterdam",
	})
	c := newProvider(t, srv)
	return c, srv, calPath
}

func newProvider(t *testing.T, srv *caldavfake.Server) *Calendar {
	t.Helper()
	c, err := New(Options{
		Email: testEmail, Password: testPass,
		BaseURL: srv.BaseURL(), Logger: quietLogger(),
	})
	if err != nil {
		t.Fatal(err)
	}
	return c
}

// ics joins lines with CRLF, the line ending RFC 5545 requires.
func ics(lines ...string) string { return strings.Join(lines, "\r\n") + "\r\n" }

func simpleEvent(uid, summary, start, end string) string {
	return ics(
		"BEGIN:VCALENDAR", "VERSION:2.0", "PRODID:-//test//EN",
		"BEGIN:VEVENT",
		"UID:"+uid,
		"DTSTAMP:20260801T120000Z",
		"SUMMARY:"+summary,
		"DTSTART:"+start,
		"DTEND:"+end,
		"END:VEVENT", "END:VCALENDAR",
	)
}

// ---------------------------------------------------------------------------
// discovery

func TestCalendarsDiscovery(t *testing.T) {
	srv := caldavfake.New()
	t.Cleanup(srv.Close)
	srv.User, srv.Password = testEmail, testPass
	home := srv.HomePath(testEmail)
	srv.AddCalendar(caldavfake.Calendar{
		Path: home + "Default/", Name: "Calendar", Color: "#3a429cff", Timezone: "Europe/Amsterdam",
	})
	srv.AddCalendar(caldavfake.Calendar{
		Path: home + "shared/", Name: "Team", Color: "#ff0000",
		Privileges: []string{"read"},
	})
	srv.AddCalendar(caldavfake.Calendar{
		Path: home + "todo/", Name: "Reminders", Components: []string{"VTODO"},
	})
	c := newProvider(t, srv)

	cals, err := c.Calendars(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if len(cals) != 2 {
		t.Fatalf("got %d calendars, want 2 (the VTODO collection must be skipped): %+v", len(cals), cals)
	}
	got := cals[0]
	if got.RemoteID != home+"Default/" || got.Name != "Calendar" {
		t.Errorf("first calendar = %+v", got)
	}
	if got.Color != "#3a429c" {
		t.Errorf("color = %q, want the alpha channel trimmed", got.Color)
	}
	if got.Timezone != "Europe/Amsterdam" {
		t.Errorf("timezone = %q", got.Timezone)
	}
	if !got.Primary {
		t.Errorf("the calendar named Calendar must be primary: %+v", got)
	}
	if got.AccessRole != "owner" {
		t.Errorf("access role = %q, want owner", got.AccessRole)
	}
	if cals[1].AccessRole != "reader" || cals[1].Primary {
		t.Errorf("shared calendar = %+v, want a non-primary reader", cals[1])
	}
}

func TestCalendarsFallbackPathWithoutPrincipal(t *testing.T) {
	srv := caldavfake.New()
	t.Cleanup(srv.Close)
	srv.User, srv.Password = testEmail, testPass
	srv.NoPrincipal = true
	srv.AddCalendar(caldavfake.Calendar{Path: srv.HomePath(testEmail) + "Default/", Name: "Calendar"})
	c := newProvider(t, srv)

	cals, err := c.Calendars(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if len(cals) != 1 || cals[0].Name != "Calendar" {
		t.Fatalf("discovery did not fall back to /dav/calendars/user/<email>/: %+v", cals)
	}
}

func TestCalendarsAuthError(t *testing.T) {
	srv := caldavfake.New()
	t.Cleanup(srv.Close)
	srv.User, srv.Password = testEmail, "the-right-one"
	srv.AddCalendar(caldavfake.Calendar{Path: srv.HomePath(testEmail) + "Default/", Name: "Calendar"})

	c, err := New(Options{Email: testEmail, Password: "wrong", BaseURL: srv.BaseURL(), Logger: quietLogger()})
	if err != nil {
		t.Fatal(err)
	}
	_, err = c.Calendars(context.Background())
	if err == nil {
		t.Fatal("Calendars with a bad app password must fail")
	}
	if !IsAuth(err) {
		t.Fatalf("error = %v, want an *AuthError", err)
	}
	if !strings.Contains(err.Error(), "app password") {
		t.Errorf("the error should point at the app password: %v", err)
	}
}

func TestNewValidatesOptions(t *testing.T) {
	if _, err := New(Options{Password: "x"}); err == nil {
		t.Error("New without an email must fail")
	}
	if _, err := New(Options{Email: testEmail}); err == nil {
		t.Error("New without a password must fail")
	}
}

// ---------------------------------------------------------------------------
// delta sync

func TestInitialSyncAndDelta(t *testing.T) {
	c, srv, calPath := newFixture(t)
	ctx := context.Background()
	srv.Put(calPath, "a.ics", simpleEvent("uid-a", "Standup", "20260901T090000Z", "20260901T091500Z"))
	srv.Put(calPath, "b.ics", simpleEvent("uid-b", "Lunch", "20260901T120000Z", "20260901T130000Z"))

	ch, err := c.EventChanges(ctx, calPath, "")
	if err != nil {
		t.Fatal(err)
	}
	if len(ch.Upserted) != 2 || len(ch.Removed) != 0 {
		t.Fatalf("initial sync = %d upserted, %d removed", len(ch.Upserted), len(ch.Removed))
	}
	if ch.NewState == "" {
		t.Fatal("initial sync returned no sync token")
	}
	first := ch.Upserted[0]
	if first.RemoteID != calPath+"a.ics" || first.UID != "uid-a" || first.Title != "Standup" {
		t.Fatalf("mapped event = %+v", first)
	}
	if !first.Start.Equal(time.Date(2026, 9, 1, 9, 0, 0, 0, time.UTC)) {
		t.Errorf("start = %v", first.Start)
	}
	if first.CalendarRemote != calPath {
		t.Errorf("calendar remote = %q", first.CalendarRemote)
	}
	raw, ok := decodeRaw(first.RawJSON)
	if !ok || raw.Href != calPath+"a.ics" || raw.ETag == "" || !strings.Contains(raw.ICS, "SUMMARY:Standup") {
		t.Errorf("RawJSON did not keep the ics/href/etag: %s", first.RawJSON)
	}

	// A delta reports only what moved.
	srv.Put(calPath, "a.ics", simpleEvent("uid-a", "Standup (moved)", "20260901T100000Z", "20260901T101500Z"))
	srv.Delete(calPath + "b.ics")

	ch2, err := c.EventChanges(ctx, calPath, ch.NewState)
	if err != nil {
		t.Fatal(err)
	}
	if len(ch2.Upserted) != 1 || ch2.Upserted[0].Title != "Standup (moved)" {
		t.Fatalf("delta upserted = %+v", ch2.Upserted)
	}
	if len(ch2.Removed) != 1 || ch2.Removed[0] != calPath+"b.ics" {
		t.Fatalf("delta removed = %v, want the deleted href", ch2.Removed)
	}
	if ch2.NewState == ch.NewState {
		t.Error("the delta must return a fresh sync token")
	}

	// An empty delta is not an error and still advances nothing.
	ch3, err := c.EventChanges(ctx, calPath, ch2.NewState)
	if err != nil {
		t.Fatal(err)
	}
	if len(ch3.Upserted) != 0 || len(ch3.Removed) != 0 {
		t.Fatalf("idle delta = %+v", ch3)
	}
}

func TestExpiredSyncTokenIsStateExpired(t *testing.T) {
	c, srv, calPath := newFixture(t)
	ctx := context.Background()
	srv.Put(calPath, "a.ics", simpleEvent("uid-a", "Standup", "20260901T090000Z", "20260901T091500Z"))

	ch, err := c.EventChanges(ctx, calPath, "")
	if err != nil {
		t.Fatal(err)
	}
	srv.ExpireTokens()

	_, err = c.EventChanges(ctx, calPath, ch.NewState)
	if !errors.Is(err, provider.ErrStateExpired) {
		t.Fatalf("error = %v, want provider.ErrStateExpired", err)
	}
	// The caller's recovery is a full listing, which must still work.
	full, err := c.EventChanges(ctx, calPath, "")
	if err != nil {
		t.Fatal(err)
	}
	if len(full.Upserted) != 1 {
		t.Fatalf("re-listing after expiry = %+v", full.Upserted)
	}
}

func TestSyncWithoutTokenIsRefused(t *testing.T) {
	srv := caldavfake.New()
	t.Cleanup(srv.Close)
	srv.User, srv.Password = testEmail, testPass
	calPath := srv.AddCalendar(caldavfake.Calendar{
		Path: srv.HomePath(testEmail) + "Default/", Name: "Calendar", NoSyncToken: true,
	})
	c := newProvider(t, srv)
	srv.Put(calPath, "a.ics", simpleEvent("uid-a", "Standup", "20260901T090000Z", "20260901T091500Z"))

	_, err := c.EventChanges(context.Background(), calPath, "")
	if err == nil || !strings.Contains(err.Error(), "no sync-token") {
		t.Fatalf("error = %v, want a refusal to persist an empty token", err)
	}
}

func TestOfflineIsTagged(t *testing.T) {
	srv := caldavfake.New()
	srv.User, srv.Password = testEmail, testPass
	calPath := srv.AddCalendar(caldavfake.Calendar{Path: srv.HomePath(testEmail) + "Default/", Name: "Calendar"})
	c := newProvider(t, srv)
	srv.Close() // nothing is listening any more

	_, err := c.EventChanges(context.Background(), calPath, "")
	if !provider.IsOffline(err) {
		t.Fatalf("error = %v, want it wrapped with model.ErrOffline", err)
	}
	if !provider.IsPreRequestFailure(err) {
		t.Errorf("a refused dial must be reported as a pre-request failure: %v", err)
	}
}

// ---------------------------------------------------------------------------
// mapping

func TestRecurringWithExceptionAndExdate(t *testing.T) {
	c, srv, calPath := newFixture(t)
	object := ics(
		"BEGIN:VCALENDAR", "VERSION:2.0", "PRODID:-//test//EN",
		"BEGIN:VEVENT",
		"UID:weekly",
		"DTSTAMP:20260801T120000Z",
		"SUMMARY:Weekly sync",
		"DTSTART;TZID=Europe/Amsterdam:20260901T090000",
		"DTEND;TZID=Europe/Amsterdam:20260901T100000",
		"RRULE:FREQ=WEEKLY;COUNT=5",
		"EXDATE;TZID=Europe/Amsterdam:20260915T090000",
		"END:VEVENT",
		"BEGIN:VEVENT",
		"UID:weekly",
		"DTSTAMP:20260801T120000Z",
		"RECURRENCE-ID;TZID=Europe/Amsterdam:20260908T090000",
		"SUMMARY:Weekly sync (moved)",
		"DTSTART;TZID=Europe/Amsterdam:20260908T140000",
		"DTEND;TZID=Europe/Amsterdam:20260908T150000",
		"END:VEVENT",
		"END:VCALENDAR",
	)
	srv.Put(calPath, "weekly.ics", object)

	ch, err := c.EventChanges(context.Background(), calPath, "")
	if err != nil {
		t.Fatal(err)
	}
	if len(ch.Upserted) != 3 {
		t.Fatalf("want master + moved instance + excluded instance, got %d: %+v", len(ch.Upserted), ch.Upserted)
	}
	master, moved, excluded := ch.Upserted[0], ch.Upserted[1], ch.Upserted[2]

	if master.RRule != "FREQ=WEEKLY;COUNT=5" || master.RecurrenceID != "" {
		t.Errorf("master = %+v", master)
	}
	if master.Timezone != "Europe/Amsterdam" {
		t.Errorf("master timezone = %q", master.Timezone)
	}
	if master.RemoteID != calPath+"weekly.ics" {
		t.Errorf("the master must own the object href, got %q", master.RemoteID)
	}

	if moved.RecurrenceID != "2026-09-08T09:00:00+02:00" {
		t.Errorf("exception recurrence id = %q", moved.RecurrenceID)
	}
	if moved.RRule != "" {
		t.Errorf("an exception instance must carry no RRULE: %q", moved.RRule)
	}
	if !strings.HasPrefix(moved.RemoteID, calPath+"weekly.ics;") {
		t.Errorf("exception remote id = %q", moved.RemoteID)
	}
	if strings.Contains(strings.TrimPrefix(moved.RemoteID, calPath), "::") {
		t.Errorf("an exception remote id must stay free of ':': %q", moved.RemoteID)
	}
	if excluded.Status != model.StatusCancelled || excluded.RecurrenceID != "2026-09-15T09:00:00+02:00" {
		t.Errorf("EXDATE did not become a cancelled exception: %+v", excluded)
	}

	// The recurrence ids must be exactly what calendar.ApplyExceptions matches.
	occ, err := calendar.Expand(&master, time.Date(2026, 9, 1, 0, 0, 0, 0, time.UTC),
		time.Date(2026, 10, 15, 0, 0, 0, 0, time.UTC))
	if err != nil {
		t.Fatal(err)
	}
	if len(occ) != 5 {
		t.Fatalf("expanded %d occurrences, want 5", len(occ))
	}
	final := calendar.ApplyExceptions(occ, []model.Event{moved, excluded})
	if len(final) != 4 {
		t.Fatalf("after exceptions: %d occurrences, want 4 (one moved, one dropped): %+v", len(final), final)
	}
	loc, err := time.LoadLocation("Europe/Amsterdam")
	if err != nil {
		t.Skip("no tzdata for Europe/Amsterdam")
	}
	want := time.Date(2026, 9, 8, 14, 0, 0, 0, loc)
	found := false
	for _, o := range final {
		if o.Start.Equal(want) {
			found = true
		}
		if o.Start.Equal(time.Date(2026, 9, 15, 9, 0, 0, 0, loc)) {
			t.Errorf("the EXDATE occurrence survived: %v", o.Start)
		}
	}
	if !found {
		t.Errorf("the moved instance is not at its new time %v: %+v", want, final)
	}
}

func TestAllDayEvent(t *testing.T) {
	c, srv, calPath := newFixture(t)
	srv.Put(calPath, "holiday.ics", ics(
		"BEGIN:VCALENDAR", "VERSION:2.0", "PRODID:-//test//EN",
		"BEGIN:VEVENT",
		"UID:holiday",
		"DTSTAMP:20260801T120000Z",
		"SUMMARY:Public holiday",
		"DTSTART;VALUE=DATE:20261225",
		"DTEND;VALUE=DATE:20261226",
		"END:VEVENT", "END:VCALENDAR",
	))
	ch, err := c.EventChanges(context.Background(), calPath, "")
	if err != nil {
		t.Fatal(err)
	}
	ev := ch.Upserted[0]
	if !ev.AllDay {
		t.Fatalf("event is not marked all-day: %+v", ev)
	}
	if !ev.Start.Equal(time.Date(2026, 12, 25, 0, 0, 0, 0, time.UTC)) {
		t.Errorf("start = %v", ev.Start)
	}
	if !ev.End.Equal(time.Date(2026, 12, 26, 0, 0, 0, 0, time.UTC)) {
		t.Errorf("end = %v, want the exclusive next day", ev.End)
	}
}

func TestAllDayWithoutDtendGetsAWholeDay(t *testing.T) {
	c, srv, calPath := newFixture(t)
	srv.Put(calPath, "d.ics", ics(
		"BEGIN:VCALENDAR", "VERSION:2.0", "PRODID:-//test//EN",
		"BEGIN:VEVENT", "UID:d", "DTSTAMP:20260801T120000Z",
		"SUMMARY:Day off", "DTSTART;VALUE=DATE:20261225",
		"END:VEVENT", "END:VCALENDAR",
	))
	ch, err := c.EventChanges(context.Background(), calPath, "")
	if err != nil {
		t.Fatal(err)
	}
	ev := ch.Upserted[0]
	if !ev.End.Equal(ev.Start.AddDate(0, 0, 1)) {
		t.Errorf("end = %v, want start+1d", ev.End)
	}
}

func TestDurationInsteadOfDtend(t *testing.T) {
	c, srv, calPath := newFixture(t)
	srv.Put(calPath, "dur.ics", ics(
		"BEGIN:VCALENDAR", "VERSION:2.0", "PRODID:-//test//EN",
		"BEGIN:VEVENT", "UID:dur", "DTSTAMP:20260801T120000Z",
		"SUMMARY:Call", "DTSTART:20260901T090000Z", "DURATION:PT45M",
		"END:VEVENT", "END:VCALENDAR",
	))
	ch, err := c.EventChanges(context.Background(), calPath, "")
	if err != nil {
		t.Fatal(err)
	}
	ev := ch.Upserted[0]
	if ev.End.Sub(ev.Start) != 45*time.Minute {
		t.Errorf("duration = %v, want 45m", ev.End.Sub(ev.Start))
	}
}

func TestAttendeesAndMyResponse(t *testing.T) {
	c, srv, calPath := newFixture(t)
	srv.Put(calPath, "meeting.ics", ics(
		"BEGIN:VCALENDAR", "VERSION:2.0", "PRODID:-//test//EN",
		"BEGIN:VEVENT",
		"UID:meeting", "DTSTAMP:20260801T120000Z",
		"SUMMARY:Review", "LOCATION:Room 3", "DESCRIPTION:Bring notes",
		"STATUS:TENTATIVE",
		"DTSTART:20260901T090000Z", "DTEND:20260901T100000Z",
		"ORGANIZER;CN=Alice:mailto:alice@example.com",
		"ATTENDEE;CN=Alice;ROLE=CHAIR;PARTSTAT=ACCEPTED:mailto:alice@example.com",
		"ATTENDEE;CN=Me;ROLE=REQ-PARTICIPANT;PARTSTAT=TENTATIVE:mailto:"+testEmail,
		"ATTENDEE;CN=Bob;ROLE=OPT-PARTICIPANT;PARTSTAT=NEEDS-ACTION:mailto:bob@example.com",
		"END:VEVENT", "END:VCALENDAR",
	))
	ch, err := c.EventChanges(context.Background(), calPath, "")
	if err != nil {
		t.Fatal(err)
	}
	ev := ch.Upserted[0]
	if ev.Status != model.StatusTentative {
		t.Errorf("status = %q", ev.Status)
	}
	if ev.Location != "Room 3" || ev.Description != "Bring notes" {
		t.Errorf("location/description = %q / %q", ev.Location, ev.Description)
	}
	if ev.Organizer.Email != "alice@example.com" || ev.Organizer.Name != "Alice" {
		t.Errorf("organizer = %+v", ev.Organizer)
	}
	if len(ev.Attendees) != 3 {
		t.Fatalf("attendees = %+v", ev.Attendees)
	}
	if ev.MyResponse != model.PartTentative {
		t.Errorf("MyResponse = %q, want tentative", ev.MyResponse)
	}
	var me, bob model.Attendee
	for _, a := range ev.Attendees {
		switch a.Email {
		case testEmail:
			me = a
		case "bob@example.com":
			bob = a
		}
	}
	if !me.Self || me.Response != model.PartTentative {
		t.Errorf("own attendee = %+v", me)
	}
	if !bob.Optional || bob.Response != model.PartNeedsAction {
		t.Errorf("optional attendee = %+v", bob)
	}
}

func TestUnparseableObjectIsSkippedNotFatal(t *testing.T) {
	c, srv, calPath := newFixture(t)
	srv.Put(calPath, "good.ics", simpleEvent("good", "Fine", "20260901T090000Z", "20260901T100000Z"))
	srv.Put(calPath, "bad.ics", "this is not iCalendar at all")

	ch, err := c.EventChanges(context.Background(), calPath, "")
	if err != nil {
		t.Fatalf("one broken object must not fail the calendar: %v", err)
	}
	if len(ch.Upserted) != 1 || ch.Upserted[0].UID != "good" {
		t.Fatalf("upserted = %+v", ch.Upserted)
	}
}

// ---------------------------------------------------------------------------
// writes

func TestCreateEvent(t *testing.T) {
	c, srv, calPath := newFixture(t)
	ctx := context.Background()

	ev := &model.Event{
		Title:       "Coffee",
		Description: "catch up",
		Location:    "Cafe",
		Start:       time.Date(2026, 9, 3, 10, 0, 0, 0, time.UTC),
		End:         time.Date(2026, 9, 3, 11, 0, 0, 0, time.UTC),
		Status:      model.StatusConfirmed,
		Organizer:   model.Address{Name: "Me", Email: testEmail},
		Attendees: []model.Attendee{
			{Name: "Me", Email: testEmail, Response: model.PartAccepted, Self: true},
			{Name: "Bob", Email: "bob@example.com", Optional: true},
		},
	}
	created, err := c.CreateEvent(ctx, calPath, ev)
	if err != nil {
		t.Fatal(err)
	}
	if created.RemoteID == "" || !strings.HasPrefix(created.RemoteID, calPath) {
		t.Fatalf("created remote id = %q", created.RemoteID)
	}
	if created.UID == "" || created.Title != "Coffee" {
		t.Errorf("created = %+v", created)
	}

	req, ok := srv.LastRequest("PUT")
	if !ok {
		t.Fatal("no PUT reached the server")
	}
	if req.Header.Get("If-None-Match") != "*" {
		t.Errorf("create must be conditional on the object not existing, headers: %v", req.Header)
	}
	if ct := req.Header.Get("Content-Type"); !strings.HasPrefix(ct, "text/calendar") {
		t.Errorf("content type = %q", ct)
	}
	for _, want := range []string{
		"BEGIN:VEVENT", "SUMMARY:Coffee", "LOCATION:Cafe",
		"DTSTART:20260903T100000Z", "DTEND:20260903T110000Z",
		"SEQUENCE:0", "STATUS:CONFIRMED",
		"ORGANIZER;CN=Me:mailto:" + testEmail,
		"ROLE=OPT-PARTICIPANT", "PARTSTAT=ACCEPTED",
	} {
		if !strings.Contains(req.Body, want) {
			t.Errorf("PUT body is missing %q:\n%s", want, req.Body)
		}
	}
	if !strings.Contains(req.Body, "DTSTAMP:") {
		t.Errorf("PUT body has no DTSTAMP:\n%s", req.Body)
	}

	// It comes back on the next sync as a normal event.
	ch, err := c.EventChanges(ctx, calPath, "")
	if err != nil {
		t.Fatal(err)
	}
	if len(ch.Upserted) != 1 || ch.Upserted[0].Title != "Coffee" {
		t.Fatalf("created event did not come back: %+v", ch.Upserted)
	}
}

func TestCreateEventWithTimezone(t *testing.T) {
	c, _, calPath := newFixture(t)
	loc, err := time.LoadLocation("Europe/Amsterdam")
	if err != nil {
		t.Skip("no tzdata")
	}
	created, err := c.CreateEvent(context.Background(), calPath, &model.Event{
		Title:    "Lunch",
		Start:    time.Date(2026, 9, 3, 12, 0, 0, 0, loc),
		End:      time.Date(2026, 9, 3, 13, 0, 0, 0, loc),
		Timezone: "Europe/Amsterdam",
	})
	if err != nil {
		t.Fatal(err)
	}
	if created.Timezone != "Europe/Amsterdam" {
		t.Errorf("timezone did not survive the round trip: %+v", created)
	}
	if !created.Start.Equal(time.Date(2026, 9, 3, 12, 0, 0, 0, loc)) {
		t.Errorf("start = %v", created.Start)
	}
}

func TestUpdateEventPreservesUnknownProperties(t *testing.T) {
	c, srv, calPath := newFixture(t)
	ctx := context.Background()
	srv.Put(calPath, "e.ics", ics(
		"BEGIN:VCALENDAR", "VERSION:2.0", "PRODID:-//other//EN",
		"BEGIN:VEVENT",
		"UID:e", "DTSTAMP:20260801T120000Z", "SEQUENCE:2",
		"SUMMARY:Old title", "DTSTART:20260901T090000Z", "DTEND:20260901T100000Z",
		"CATEGORIES:work",
		"BEGIN:VALARM", "ACTION:DISPLAY", "TRIGGER:-PT15M", "DESCRIPTION:Reminder", "END:VALARM",
		"END:VEVENT", "END:VCALENDAR",
	))
	ch, err := c.EventChanges(ctx, calPath, "")
	if err != nil {
		t.Fatal(err)
	}
	ev := ch.Upserted[0]
	ev.Title = "New title"

	updated, err := c.UpdateEvent(ctx, &ev)
	if err != nil {
		t.Fatal(err)
	}
	if updated.Title != "New title" {
		t.Errorf("updated title = %q", updated.Title)
	}
	req, _ := srv.LastRequest("PUT")
	if got := req.Header.Get("If-Match"); got == "" {
		t.Error("update must be conditional on the ETag we last saw")
	}
	for _, want := range []string{"SUMMARY:New title", "SEQUENCE:3", "CATEGORIES:work", "BEGIN:VALARM"} {
		if !strings.Contains(req.Body, want) {
			t.Errorf("PUT body is missing %q:\n%s", want, req.Body)
		}
	}
	if strings.Contains(req.Body, "SUMMARY:Old title") {
		t.Errorf("the old title survived:\n%s", req.Body)
	}
}

func TestUpdateEventKeepsOtherVEventsInTheObject(t *testing.T) {
	c, srv, calPath := newFixture(t)
	ctx := context.Background()
	srv.Put(calPath, "series.ics", ics(
		"BEGIN:VCALENDAR", "VERSION:2.0", "PRODID:-//other//EN",
		"BEGIN:VEVENT", "UID:s", "DTSTAMP:20260801T120000Z", "SUMMARY:Series",
		"DTSTART:20260901T090000Z", "DTEND:20260901T100000Z", "RRULE:FREQ=WEEKLY;COUNT=3",
		"END:VEVENT",
		"BEGIN:VEVENT", "UID:s", "DTSTAMP:20260801T120000Z",
		"RECURRENCE-ID:20260908T090000Z", "SUMMARY:Series (moved)",
		"DTSTART:20260908T140000Z", "DTEND:20260908T150000Z",
		"END:VEVENT", "END:VCALENDAR",
	))
	ch, err := c.EventChanges(ctx, calPath, "")
	if err != nil {
		t.Fatal(err)
	}
	master := ch.Upserted[0]
	master.Title = "Series renamed"
	if _, err := c.UpdateEvent(ctx, &master); err != nil {
		t.Fatal(err)
	}
	req, _ := srv.LastRequest("PUT")
	if !strings.Contains(req.Body, "RECURRENCE-ID:20260908T090000Z") ||
		!strings.Contains(req.Body, "SUMMARY:Series (moved)") {
		t.Errorf("the exception VEVENT was dropped:\n%s", req.Body)
	}
	if !strings.Contains(req.Body, "SUMMARY:Series renamed") {
		t.Errorf("the master was not updated:\n%s", req.Body)
	}
}

func TestUpdateEventRefusesAnOccurrence(t *testing.T) {
	c, _, calPath := newFixture(t)
	_, err := c.UpdateEvent(context.Background(), &model.Event{
		CalendarRemote: calPath, RemoteID: calPath + "s.ics;20260908T090000",
	})
	if err == nil || !strings.Contains(err.Error(), "one occurrence") {
		t.Fatalf("error = %v, want a refusal to write a single occurrence", err)
	}
}

func TestUpdateEventReportsAConflict(t *testing.T) {
	c, srv, calPath := newFixture(t)
	ctx := context.Background()
	srv.Put(calPath, "e.ics", simpleEvent("e", "Title", "20260901T090000Z", "20260901T100000Z"))
	ch, err := c.EventChanges(ctx, calPath, "")
	if err != nil {
		t.Fatal(err)
	}
	ev := ch.Upserted[0]
	// Somebody else edits the object after our last sync.
	srv.Put(calPath, "e.ics", simpleEvent("e", "Changed elsewhere", "20260901T090000Z", "20260901T100000Z"))

	ev.Title = "Mine"
	if _, err := c.UpdateEvent(ctx, &ev); err == nil {
		t.Fatal("a stale ETag must not silently overwrite the server's copy")
	} else if !strings.Contains(err.Error(), "changed on the server") {
		t.Fatalf("error = %v", err)
	}
	if got, _ := srv.Get(calPath + "e.ics"); !strings.Contains(got, "Changed elsewhere") {
		t.Errorf("the server's copy was overwritten:\n%s", got)
	}
}

func TestRespondSetsPartStat(t *testing.T) {
	c, srv, calPath := newFixture(t)
	ctx := context.Background()
	href := calPath + "invite.ics"
	srv.Put(calPath, "invite.ics", ics(
		"BEGIN:VCALENDAR", "VERSION:2.0", "PRODID:-//test//EN",
		"BEGIN:VEVENT", "UID:invite", "DTSTAMP:20260801T120000Z", "SUMMARY:Invite",
		"DTSTART:20260901T090000Z", "DTEND:20260901T100000Z",
		"ORGANIZER;CN=Alice:mailto:alice@example.com",
		"ATTENDEE;CN=Alice;PARTSTAT=ACCEPTED:mailto:alice@example.com",
		"ATTENDEE;CN=Me;PARTSTAT=NEEDS-ACTION:mailto:"+testEmail,
		"END:VEVENT", "END:VCALENDAR",
	))
	if err := c.Respond(ctx, calPath, href, model.PartAccepted); err != nil {
		t.Fatal(err)
	}
	req, ok := srv.LastRequest("PUT")
	if !ok {
		t.Fatal("Respond sent no PUT")
	}
	if req.Header.Get("If-Match") == "" {
		t.Error("Respond must be conditional on the ETag it read")
	}
	stored, _ := srv.Get(href)
	if !strings.Contains(stored, "PARTSTAT=ACCEPTED:mailto:"+testEmail) {
		t.Errorf("own PARTSTAT was not set:\n%s", stored)
	}
	if !strings.Contains(stored, "PARTSTAT=ACCEPTED:mailto:alice@example.com") {
		t.Errorf("another attendee's PARTSTAT was changed:\n%s", stored)
	}

	ch, err := c.EventChanges(ctx, calPath, "")
	if err != nil {
		t.Fatal(err)
	}
	if ch.Upserted[0].MyResponse != model.PartAccepted {
		t.Errorf("MyResponse after the reply = %q", ch.Upserted[0].MyResponse)
	}
}

func TestRespondWithoutOwnAttendee(t *testing.T) {
	c, srv, calPath := newFixture(t)
	href := calPath + "solo.ics"
	srv.Put(calPath, "solo.ics", simpleEvent("solo", "Alone", "20260901T090000Z", "20260901T100000Z"))
	err := c.Respond(context.Background(), calPath, href, model.PartAccepted)
	if err == nil || !strings.Contains(err.Error(), "not an attendee") {
		t.Fatalf("error = %v", err)
	}
}

func TestRespondRejectsUnknownStatus(t *testing.T) {
	c, _, calPath := newFixture(t)
	err := c.Respond(context.Background(), calPath, calPath+"x.ics", model.Participation("maybe"))
	if err == nil || !strings.Contains(err.Error(), "unknown participation") {
		t.Fatalf("error = %v", err)
	}
}

func TestDeleteEvent(t *testing.T) {
	c, srv, calPath := newFixture(t)
	ctx := context.Background()
	href := calPath + "gone.ics"
	srv.Put(calPath, "gone.ics", simpleEvent("gone", "Bye", "20260901T090000Z", "20260901T100000Z"))

	if err := c.DeleteEvent(ctx, calPath, href); err != nil {
		t.Fatal(err)
	}
	if _, ok := srv.Get(href); ok {
		t.Error("the object is still there")
	}
	// Deleting again is a no-op: the outbox retries.
	if err := c.DeleteEvent(ctx, calPath, href); err != nil {
		t.Fatalf("deleting a missing object must not be an error: %v", err)
	}
}

func TestDeleteEventRefusesAnOccurrence(t *testing.T) {
	c, _, calPath := newFixture(t)
	err := c.DeleteEvent(context.Background(), calPath, calPath+"s.ics;20260908T090000")
	if err == nil || !strings.Contains(err.Error(), "one occurrence") {
		t.Fatalf("error = %v", err)
	}
}

func TestMultigetIsBatched(t *testing.T) {
	srv := caldavfake.New()
	t.Cleanup(srv.Close)
	srv.User, srv.Password = testEmail, testPass
	calPath := srv.AddCalendar(caldavfake.Calendar{Path: srv.HomePath(testEmail) + "Default/", Name: "Calendar"})
	c, err := New(Options{
		Email: testEmail, Password: testPass, BaseURL: srv.BaseURL(),
		Logger: quietLogger(), BatchSize: 2,
	})
	if err != nil {
		t.Fatal(err)
	}
	for _, n := range []string{"a", "b", "c", "d", "e"} {
		srv.Put(calPath, n+".ics", simpleEvent(n, n, "20260901T090000Z", "20260901T100000Z"))
	}
	ch, err := c.EventChanges(context.Background(), calPath, "")
	if err != nil {
		t.Fatal(err)
	}
	if len(ch.Upserted) != 5 {
		t.Fatalf("upserted = %d, want 5", len(ch.Upserted))
	}
	multigets := 0
	for _, r := range srv.Requests() {
		if r.Method == "REPORT" && strings.Contains(r.Body, "calendar-multiget") {
			multigets++
		}
	}
	if multigets != 3 {
		t.Errorf("multiget requests = %d, want 3 batches of at most 2", multigets)
	}
}

func TestCalendarsFollowsTheDiscoveredHomeSet(t *testing.T) {
	srv := caldavfake.New()
	t.Cleanup(srv.Close)
	srv.User, srv.Password = testEmail, testPass
	// A home that the conventional /dav/calendars/user/<email>/ path would
	// never guess, so a pass here proves discovery was actually followed.
	srv.AddCalendar(caldavfake.Calendar{Path: "/dav/elsewhere/mycals/Work/", Name: "Work"})
	c := newProvider(t, srv)

	cals, err := c.Calendars(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if len(cals) != 1 || cals[0].RemoteID != "/dav/elsewhere/mycals/Work/" {
		t.Fatalf("calendars = %+v, want the collection under the discovered home set", cals)
	}
	var paths []string
	for _, r := range srv.Requests() {
		if r.Method == "PROPFIND" {
			paths = append(paths, r.Path)
		}
	}
	if len(paths) != 3 || paths[1] != srv.PrincipalPath() || paths[2] != "/dav/elsewhere/mycals/" {
		t.Errorf("PROPFIND chain = %v, want root -> principal -> calendar home", paths)
	}
}
