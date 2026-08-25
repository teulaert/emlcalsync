package jmap

import (
	"encoding/json"
	"errors"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/lennert/emlcal/internal/model"
	"github.com/lennert/emlcal/internal/provider"
)

func seedCalendars(f *fakeServer) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.calendars = []map[string]any{
		{
			"id": "cal-1", "name": "Personal", "color": "#3a87ad", "sortOrder": 1,
			"isDefault": true, "timeZone": "Europe/Amsterdam",
			"myRights": map[string]any{
				"mayReadFreeBusy": true, "mayReadItems": true,
				"mayWriteAll": true, "mayWriteOwn": true,
				"mayUpdatePrivate": true, "mayRSVP": true,
				"mayShare": true, "mayDelete": true,
			},
		},
		{
			"id": "cal-2", "name": "Team", "color": "#cc0000", "sortOrder": 2,
			"isDefault": false, "timeZone": nil,
			"myRights": map[string]any{"mayReadFreeBusy": true, "mayReadItems": true},
		},
		{
			"id": "cal-3", "name": "Shared", "sortOrder": 3, "isDefault": false,
			"myRights": map[string]any{"mayReadItems": true, "mayWriteOwn": true, "mayRSVP": true},
		},
	}
}

// standupEvent is a recurring, zoned, multi-participant event.
func standupEvent() map[string]any {
	var obj map[string]any
	json.Unmarshal([]byte(`{
	  "@type": "Event",
	  "id": "ev-standup",
	  "calendarIds": {"cal-1": true},
	  "uid": "uid-standup",
	  "title": "Standup",
	  "description": "Daily sync",
	  "start": "2026-03-02T09:30:00",
	  "duration": "PT15M",
	  "timeZone": "Europe/Amsterdam",
	  "status": "confirmed",
	  "updated": "2026-02-01T10:00:00Z",
	  "sequence": 3,
	  "freeBusyStatus": "busy",
	  "locations": {"loc-a": {"@type": "Location", "name": "Room 4"}},
	  "recurrenceRules": [{
	    "@type": "RecurrenceRule",
	    "frequency": "weekly",
	    "interval": 1,
	    "byDay": [{"@type": "NDay", "day": "mo"}, {"@type": "NDay", "day": "we"}],
	    "until": "2026-06-01T09:30:00"
	  }],
	  "participants": {
	    "org": {
	      "@type": "Participant", "name": "Boss", "email": "boss@example.com",
	      "sendTo": {"imip": "mailto:boss@example.com"},
	      "roles": {"owner": true, "attendee": true},
	      "participationStatus": "accepted"
	    },
	    "me": {
	      "@type": "Participant", "name": "Me",
	      "sendTo": {"imip": "mailto:me@example.com"},
	      "roles": {"attendee": true},
	      "participationStatus": "tentative",
	      "expectReply": true
	    }
	  },
	  "replyTo": {"imip": "mailto:boss@example.com"},
	  "alerts": {"a1": {"@type": "Alert", "trigger": {"@type": "OffsetTrigger", "offset": "-PT5M"}}}
	}`), &obj)
	return obj
}

func holidayEvent() map[string]any {
	var obj map[string]any
	json.Unmarshal([]byte(`{
	  "@type": "Event",
	  "id": "ev-holiday",
	  "calendarIds": {"cal-1": true},
	  "uid": "uid-holiday",
	  "title": "Bastille Day",
	  "start": "2026-07-14T00:00:00",
	  "duration": "P1D",
	  "showWithoutTime": true,
	  "status": "confirmed",
	  "updated": "2026-02-02T10:00:00Z"
	}`), &obj)
	return obj
}

func otherCalendarEvent() map[string]any {
	var obj map[string]any
	json.Unmarshal([]byte(`{
	  "@type": "Event",
	  "id": "ev-team",
	  "calendarIds": {"cal-2": true},
	  "uid": "uid-team",
	  "title": "Team offsite",
	  "start": "2026-04-01T10:00:00",
	  "duration": "PT2H",
	  "timeZone": "UTC",
	  "status": "tentative",
	  "updated": "2026-02-03T10:00:00Z"
	}`), &obj)
	return obj
}

func exceptionEvent() map[string]any {
	var obj map[string]any
	json.Unmarshal([]byte(`{
	  "@type": "Event",
	  "id": "ev-standup-exception",
	  "calendarIds": {"cal-1": true},
	  "uid": "uid-standup",
	  "title": "Standup (moved)",
	  "start": "2026-03-09T11:00:00",
	  "duration": "PT15M",
	  "timeZone": "Europe/Amsterdam",
	  "recurrenceId": "2026-03-09T09:30:00",
	  "recurrenceIdTimeZone": "Europe/Amsterdam",
	  "status": "cancelled",
	  "updated": "2026-02-04T10:00:00Z"
	}`), &obj)
	return obj
}

func seedEvents(f *fakeServer, objs ...map[string]any) {
	f.mu.Lock()
	defer f.mu.Unlock()
	for _, o := range objs {
		f.events[o["id"].(string)] = o
	}
}

// ---------------------------------------------------------------------------

func TestCalendars(t *testing.T) {
	f := newFakeServer(t)
	seedCalendars(f)
	cal := f.client(t).Calendar()

	got, err := cal.Calendars(testCtx(t))
	if err != nil {
		t.Fatalf("Calendars: %v", err)
	}
	if len(got) != 3 {
		t.Fatalf("got %d calendars, want 3", len(got))
	}
	if got[0].RemoteID != "cal-1" || got[0].Name != "Personal" ||
		got[0].Color != "#3a87ad" || got[0].Timezone != "Europe/Amsterdam" {
		t.Errorf("calendar 1 = %+v", got[0])
	}
	if !got[0].Primary {
		t.Error("isDefault should map to Primary")
	}
	if got[1].Primary {
		t.Error("non-default calendar marked primary")
	}
	roles := []string{got[0].AccessRole, got[1].AccessRole, got[2].AccessRole}
	want := []string{"owner", "reader", "writer"}
	if !reflect.DeepEqual(roles, want) {
		t.Errorf("access roles = %v, want %v", roles, want)
	}
	if got[1].Timezone != "" {
		t.Errorf("null timeZone should map to empty string, got %q", got[1].Timezone)
	}
}

func TestEventChangesFullListing(t *testing.T) {
	f := newFakeServer(t)
	seedCalendars(f)
	seedEvents(f, standupEvent(), holidayEvent(), otherCalendarEvent(), exceptionEvent())
	cal := f.client(t).Calendar()

	ch, err := cal.EventChanges(testCtx(t), "cal-1", "")
	if err != nil {
		t.Fatalf("EventChanges: %v", err)
	}
	if ch.NewState != "cal-0" {
		t.Errorf("NewState = %q, want the state captured before the listing", ch.NewState)
	}
	if len(ch.Upserted) != 3 {
		t.Fatalf("got %d events, want the 3 in cal-1 (not ev-team)", len(ch.Upserted))
	}
	byID := map[string]model.Event{}
	for _, e := range ch.Upserted {
		byID[e.RemoteID] = e
	}
	if _, leaked := byID["ev-team"]; leaked {
		t.Error("an event from another calendar leaked into the listing")
	}

	// --- the recurring, zoned event ------------------------------------
	su := byID["ev-standup"]
	if su.CalendarRemote != "cal-1" || su.UID != "uid-standup" {
		t.Errorf("ids = %+v", su)
	}
	if su.Title != "Standup" || su.Description != "Daily sync" {
		t.Errorf("title/description = %q / %q", su.Title, su.Description)
	}
	if su.Location != "Room 4" {
		t.Errorf("location = %q", su.Location)
	}
	if su.Timezone != "Europe/Amsterdam" {
		t.Errorf("timezone = %q", su.Timezone)
	}
	ams, err := time.LoadLocation("Europe/Amsterdam")
	if err != nil {
		t.Skipf("no tzdata available: %v", err)
	}
	wantStart := time.Date(2026, 3, 2, 9, 30, 0, 0, ams)
	if !su.Start.Equal(wantStart) {
		t.Errorf("start = %s, want %s", su.Start, wantStart)
	}
	if !su.End.Equal(wantStart.Add(15 * time.Minute)) {
		t.Errorf("end = %s, want start+15m", su.End)
	}
	if su.AllDay {
		t.Error("a timed event must not be all-day")
	}
	// UNTIL is a local time in Amsterdam (CEST in June) rendered as UTC.
	if su.RRule != "FREQ=WEEKLY;UNTIL=20260601T073000Z;BYDAY=MO,WE" {
		t.Errorf("rrule = %q", su.RRule)
	}
	if su.Status != model.StatusConfirmed {
		t.Errorf("status = %q", su.Status)
	}
	if su.Organizer.Email != "boss@example.com" || su.Organizer.Name != "Boss" {
		t.Errorf("organizer = %+v", su.Organizer)
	}
	if len(su.Attendees) != 2 {
		t.Fatalf("attendees = %+v", su.Attendees)
	}
	// Participants are emitted in key order: "me" then "org".
	me := su.Attendees[0]
	if me.Email != "me@example.com" {
		t.Errorf("attendee 0 = %+v (sendTo imip should be used when email is absent)", me)
	}
	if !me.Self {
		t.Error("the account's own participant should be marked Self")
	}
	if me.Response != model.PartTentative {
		t.Errorf("my response on the attendee = %q", me.Response)
	}
	if su.Attendees[1].Response != model.PartAccepted {
		t.Errorf("organiser response = %q", su.Attendees[1].Response)
	}
	if su.MyResponse != model.PartTentative {
		t.Errorf("MyResponse = %q, want tentative", su.MyResponse)
	}
	if su.Updated.IsZero() {
		t.Error("updated not parsed")
	}
	// RawJSON must be the untouched server object, including things we do not
	// model (alerts, sequence).
	var raw map[string]any
	if err := json.Unmarshal(su.RawJSON, &raw); err != nil {
		t.Fatalf("RawJSON is not valid JSON: %v", err)
	}
	if _, ok := raw["alerts"]; !ok {
		t.Error("RawJSON lost the alerts property")
	}

	// --- the all-day event ---------------------------------------------
	hol := byID["ev-holiday"]
	if !hol.AllDay {
		t.Error("showWithoutTime should map to AllDay")
	}
	if !hol.Start.Equal(time.Date(2026, 7, 14, 0, 0, 0, 0, time.UTC)) {
		t.Errorf("all-day start = %s", hol.Start)
	}
	if !hol.End.Equal(time.Date(2026, 7, 15, 0, 0, 0, 0, time.UTC)) {
		t.Errorf("all-day end = %s, want +P1D", hol.End)
	}
	if hol.Timezone != "" {
		t.Errorf("all-day event should have no time zone, got %q", hol.Timezone)
	}
	if hol.MyResponse != model.PartNeedsAction {
		t.Errorf("MyResponse on an event with no participants = %q", hol.MyResponse)
	}

	// --- the exception instance ----------------------------------------
	ex := byID["ev-standup-exception"]
	if ex.RecurrenceID != "2026-03-09T09:30:00" {
		t.Errorf("recurrenceId = %q", ex.RecurrenceID)
	}
	if ex.Status != model.StatusCancelled {
		t.Errorf("status = %q", ex.Status)
	}
	if ex.UID != su.UID {
		t.Errorf("exception uid %q should match the master %q", ex.UID, su.UID)
	}
}

func TestEventChangesFilterFallback(t *testing.T) {
	f := newFakeServer(t)
	seedCalendars(f)
	seedEvents(f, standupEvent(), otherCalendarEvent())
	f.mu.Lock()
	f.calendarFilterPlural = true // reject {"inCalendar": id}, as older drafts do
	f.mu.Unlock()

	cal := f.client(t).Calendar()
	ch, err := cal.EventChanges(testCtx(t), "cal-1", "")
	if err != nil {
		t.Fatalf("EventChanges should fall back to inCalendars: %v", err)
	}
	if len(ch.Upserted) != 1 || ch.Upserted[0].RemoteID != "ev-standup" {
		t.Fatalf("upserted = %+v", ch.Upserted)
	}
	queries := f.captured("CalendarEvent/query")
	if len(queries) != 2 {
		t.Fatalf("expected a rejected query then a retry, got %d", len(queries))
	}
	if _, ok := queries[0]["filter"].(map[string]any)["inCalendar"]; !ok {
		t.Error("first attempt should use the current draft's inCalendar")
	}
	if _, ok := queries[1]["filter"].(map[string]any)["inCalendars"]; !ok {
		t.Error("retry should use the legacy inCalendars")
	}
	// The fallback must be remembered rather than re-probed each time.
	f.resetCalls()
	if _, err := cal.EventChanges(testCtx(t), "cal-1", ""); err != nil {
		t.Fatal(err)
	}
	if n := len(f.captured("CalendarEvent/query")); n != 1 {
		t.Errorf("second listing made %d queries, want 1", n)
	}
}

func TestEventChangesDelta(t *testing.T) {
	f := newFakeServer(t)
	seedCalendars(f)
	seedEvents(f, standupEvent(), holidayEvent(), otherCalendarEvent())
	f.eventChanges["cal-0"] = changeScript{
		Created: []string{"ev-holiday"}, Updated: []string{"ev-standup", "ev-team"},
		Destroyed: []string{"ev-old"}, NewState: "cal-1", HasMore: true,
	}
	f.eventChanges["cal-1"] = changeScript{NewState: "cal-2"}

	cal := f.client(t).Calendar()
	ch, err := cal.EventChanges(testCtx(t), "cal-1", "cal-0")
	if err != nil {
		t.Fatalf("EventChanges: %v", err)
	}
	if ch.NewState != "cal-2" {
		t.Errorf("NewState = %q, want the state after the hasMoreChanges loop", ch.NewState)
	}
	if len(ch.Upserted) != 2 {
		t.Fatalf("upserted = %d, want 2 (ev-team belongs to another calendar)", len(ch.Upserted))
	}
	// ev-team changed but is not in cal-1, so it is reported as removed here.
	wantRemoved := map[string]bool{"ev-old": true, "ev-team": true}
	if len(ch.Removed) != 2 {
		t.Fatalf("removed = %v", ch.Removed)
	}
	for _, id := range ch.Removed {
		if !wantRemoved[id] {
			t.Errorf("unexpected removal %q", id)
		}
	}
}

func TestEventChangesStateExpired(t *testing.T) {
	f := newFakeServer(t)
	seedCalendars(f)
	f.eventChanges["cal-0"] = changeScript{ErrorType: "cannotCalculateChanges"}
	cal := f.client(t).Calendar()

	if _, err := cal.EventChanges(testCtx(t), "cal-1", "cal-0"); !errors.Is(err, provider.ErrStateExpired) {
		t.Fatalf("error = %v, want ErrStateExpired", err)
	}
}

func TestEventChangesNeedsCalendar(t *testing.T) {
	f := newFakeServer(t)
	cal := f.client(t).Calendar()
	if _, err := cal.EventChanges(testCtx(t), "", ""); err == nil {
		t.Fatal("expected an error without a calendar id")
	}
}

// ---------------------------------------------------------------------------
// Event writes

func TestCreateEvent(t *testing.T) {
	f := newFakeServer(t)
	seedCalendars(f)
	cal := f.client(t).Calendar()

	ev := &model.Event{
		Title:       "Lunch",
		Description: "with the team",
		Location:    "Cafe",
		Start:       time.Date(2026, 5, 4, 12, 0, 0, 0, time.UTC),
		End:         time.Date(2026, 5, 4, 13, 30, 0, 0, time.UTC),
		Timezone:    "UTC",
		RRule:       "FREQ=MONTHLY;BYDAY=1MO;COUNT=6",
		Status:      model.StatusConfirmed,
		Organizer:   model.Address{Name: "Me", Email: testEmail},
		Attendees: []model.Attendee{
			{Name: "You", Email: "you@example.com", Response: model.PartNeedsAction},
		},
	}
	got, err := cal.CreateEvent(testCtx(t), "cal-1", ev)
	if err != nil {
		t.Fatalf("CreateEvent: %v", err)
	}
	if got.RemoteID == "" {
		t.Fatal("created event has no remote id")
	}
	if got.Title != "Lunch" || got.Location != "Cafe" {
		t.Errorf("round-tripped event = %+v", got)
	}
	if got.RRule != "FREQ=MONTHLY;COUNT=6;BYDAY=1MO" {
		t.Errorf("rrule round trip = %q", got.RRule)
	}
	if !got.End.Equal(ev.End) {
		t.Errorf("end = %s, want %s", got.End, ev.End)
	}

	sets := f.captured("CalendarEvent/set")
	if len(sets) != 1 {
		t.Fatalf("got %d CalendarEvent/set calls", len(sets))
	}
	if sets[0]["sendSchedulingMessages"] != true {
		t.Error("an event with attendees should ask the server to send invitations")
	}
	create := sets[0]["create"].(map[string]any)["new"].(map[string]any)
	if create["start"] != "2026-05-04T12:00:00" {
		t.Errorf("start = %v", create["start"])
	}
	if create["duration"] != "PT1H30M" {
		t.Errorf("duration = %v", create["duration"])
	}
	if create["@type"] != "Event" {
		t.Errorf("@type = %v", create["@type"])
	}
	if cals, _ := create["calendarIds"].(map[string]any); cals["cal-1"] != true {
		t.Errorf("calendarIds = %v", create["calendarIds"])
	}
}

func TestCreateEventAllDay(t *testing.T) {
	f := newFakeServer(t)
	seedCalendars(f)
	cal := f.client(t).Calendar()

	ev := &model.Event{
		Title:  "Conference",
		Start:  time.Date(2026, 9, 1, 0, 0, 0, 0, time.UTC),
		End:    time.Date(2026, 9, 4, 0, 0, 0, 0, time.UTC),
		AllDay: true,
	}
	if _, err := cal.CreateEvent(testCtx(t), "cal-1", ev); err != nil {
		t.Fatalf("CreateEvent: %v", err)
	}
	create := f.captured("CalendarEvent/set")[0]["create"].(map[string]any)["new"].(map[string]any)
	if create["showWithoutTime"] != true {
		t.Error("all-day event should set showWithoutTime")
	}
	if _, hasTZ := create["timeZone"]; hasTZ {
		t.Error("all-day event must not carry a time zone")
	}
	if create["duration"] != "P3D" {
		t.Errorf("duration = %v, want P3D", create["duration"])
	}
	if f.captured("CalendarEvent/set")[0]["sendSchedulingMessages"] != nil {
		t.Error("an event without attendees should not trigger scheduling messages")
	}
}

func TestCreateEventSchedulingFallback(t *testing.T) {
	f := newFakeServer(t)
	seedCalendars(f)
	f.mu.Lock()
	f.rejectScheduling = true
	f.mu.Unlock()
	cal := f.client(t).Calendar()

	ev := &model.Event{
		Title:     "Sync",
		Start:     time.Date(2026, 5, 4, 12, 0, 0, 0, time.UTC),
		End:       time.Date(2026, 5, 4, 12, 30, 0, 0, time.UTC),
		Attendees: []model.Attendee{{Email: "you@example.com"}},
	}
	if _, err := cal.CreateEvent(testCtx(t), "cal-1", ev); err != nil {
		t.Fatalf("CreateEvent should retry without sendSchedulingMessages: %v", err)
	}
	sets := f.captured("CalendarEvent/set")
	if len(sets) != 2 {
		t.Fatalf("expected a rejected call then a retry, got %d", len(sets))
	}
	if _, ok := sets[1]["sendSchedulingMessages"]; ok {
		t.Error("the retry should omit sendSchedulingMessages")
	}
}

func TestUpdateEventPatchesOnlyWhatChanged(t *testing.T) {
	f := newFakeServer(t)
	seedCalendars(f)
	seedEvents(f, standupEvent())
	cal := f.client(t).Calendar()
	ctx := testCtx(t)

	ch, err := cal.EventChanges(ctx, "cal-1", "")
	if err != nil {
		t.Fatal(err)
	}
	ev := ch.Upserted[0]
	f.resetCalls()

	ev.Title = "Standup (renamed)"
	if _, err := cal.UpdateEvent(ctx, &ev); err != nil {
		t.Fatalf("UpdateEvent: %v", err)
	}
	sets := f.captured("CalendarEvent/set")
	if len(sets) != 1 {
		t.Fatalf("got %d CalendarEvent/set calls", len(sets))
	}
	patch := sets[0]["update"].(map[string]any)["ev-standup"].(map[string]any)
	if !reflect.DeepEqual(patch, map[string]any{"title": "Standup (renamed)"}) {
		t.Errorf("patch = %#v, want only the title", patch)
	}
	// Everything we do not model must survive.
	f.mu.Lock()
	stored := f.events["ev-standup"]
	f.mu.Unlock()
	if _, ok := stored["alerts"]; !ok {
		t.Error("the update dropped the server's alerts")
	}
	if parts, _ := stored["participants"].(map[string]any); len(parts) != 2 {
		t.Errorf("participants rewritten: %v", stored["participants"])
	}
}

func TestUpdateEventClearsLocation(t *testing.T) {
	f := newFakeServer(t)
	seedCalendars(f)
	seedEvents(f, standupEvent())
	cal := f.client(t).Calendar()
	ctx := testCtx(t)

	ch, _ := cal.EventChanges(ctx, "cal-1", "")
	ev := ch.Upserted[0]
	f.resetCalls()

	ev.Location = ""
	if _, err := cal.UpdateEvent(ctx, &ev); err != nil {
		t.Fatalf("UpdateEvent: %v", err)
	}
	patch := f.captured("CalendarEvent/set")[0]["update"].(map[string]any)["ev-standup"].(map[string]any)
	v, ok := patch["locations"]
	if !ok || v != nil {
		t.Errorf("patch = %#v, want locations: null", patch)
	}
}

func TestUpdateEventNoChangeSkipsWrite(t *testing.T) {
	f := newFakeServer(t)
	seedCalendars(f)
	seedEvents(f, standupEvent())
	cal := f.client(t).Calendar()
	ctx := testCtx(t)

	ch, _ := cal.EventChanges(ctx, "cal-1", "")
	ev := ch.Upserted[0]
	f.resetCalls()

	if _, err := cal.UpdateEvent(ctx, &ev); err != nil {
		t.Fatalf("UpdateEvent: %v", err)
	}
	if n := len(f.captured("CalendarEvent/set")); n != 0 {
		t.Errorf("an unchanged event produced %d writes", n)
	}
}

func TestDeleteEvent(t *testing.T) {
	f := newFakeServer(t)
	seedCalendars(f)
	seedEvents(f, standupEvent())
	cal := f.client(t).Calendar()
	ctx := testCtx(t)

	if err := cal.DeleteEvent(ctx, "cal-1", "ev-standup"); err != nil {
		t.Fatalf("DeleteEvent: %v", err)
	}
	f.mu.Lock()
	_, still := f.events["ev-standup"]
	f.mu.Unlock()
	if still {
		t.Error("event not deleted")
	}
	// Deleting again is a no-op, not an error.
	if err := cal.DeleteEvent(ctx, "cal-1", "ev-standup"); err != nil {
		t.Errorf("second delete = %v, want nil", err)
	}
}

func TestRespond(t *testing.T) {
	f := newFakeServer(t)
	seedCalendars(f)
	seedEvents(f, standupEvent())
	cal := f.client(t).Calendar()
	ctx := testCtx(t)

	if err := cal.Respond(ctx, "cal-1", "ev-standup", model.PartAccepted); err != nil {
		t.Fatalf("Respond: %v", err)
	}
	patch := f.captured("CalendarEvent/set")[0]["update"].(map[string]any)["ev-standup"].(map[string]any)
	want := map[string]any{"participants/me/participationStatus": "accepted"}
	if !reflect.DeepEqual(patch, want) {
		t.Errorf("patch = %#v, want %#v", patch, want)
	}
	f.mu.Lock()
	parts := f.events["ev-standup"]["participants"].(map[string]any)
	f.mu.Unlock()
	if got := parts["me"].(map[string]any)["participationStatus"]; got != "accepted" {
		t.Errorf("stored participationStatus = %v", got)
	}
}

func TestRespondNotAParticipant(t *testing.T) {
	f := newFakeServer(t)
	seedCalendars(f)
	seedEvents(f, holidayEvent())
	cal := f.client(t).Calendar()

	err := cal.Respond(testCtx(t), "cal-1", "ev-holiday", model.PartAccepted)
	if !errors.Is(err, model.ErrNotFound) {
		t.Fatalf("error = %v, want ErrNotFound", err)
	}
}

// ---------------------------------------------------------------------------
// Recurrence conversion

func TestRecurrenceToRRule(t *testing.T) {
	ams, err := time.LoadLocation("Europe/Amsterdam")
	if err != nil {
		t.Skipf("no tzdata: %v", err)
	}
	nth := func(n int) *int { return &n }

	tests := []struct {
		name   string
		rule   jsRecurrenceRule
		loc    *time.Location
		allDay bool
		want   string
	}{
		{
			name: "simple daily",
			rule: jsRecurrenceRule{Frequency: "daily"},
			want: "FREQ=DAILY",
		},
		{
			name: "interval and count",
			rule: jsRecurrenceRule{Frequency: "weekly", Interval: 2, Count: 10},
			want: "FREQ=WEEKLY;INTERVAL=2;COUNT=10",
		},
		{
			name: "interval 1 is the default and is omitted",
			rule: jsRecurrenceRule{Frequency: "weekly", Interval: 1},
			want: "FREQ=WEEKLY",
		},
		{
			name: "byDay with ordinals",
			rule: jsRecurrenceRule{
				Frequency: "monthly",
				ByDay:     []jsNDay{{Day: "su", NthOfPeriod: nth(-1)}, {Day: "mo", NthOfPeriod: nth(2)}},
			},
			want: "FREQ=MONTHLY;BYDAY=-1SU,2MO",
		},
		{
			name: "until in a zone becomes UTC",
			rule: jsRecurrenceRule{Frequency: "daily", Until: "2026-06-01T09:30:00"},
			loc:  ams,
			want: "FREQ=DAILY;UNTIL=20260601T073000Z",
		},
		{
			name:   "until on an all-day event stays a date",
			rule:   jsRecurrenceRule{Frequency: "yearly", Until: "2030-01-01T00:00:00"},
			allDay: true,
			want:   "FREQ=YEARLY;UNTIL=20300101",
		},
		{
			name: "monthly by month day and set position",
			rule: jsRecurrenceRule{
				Frequency:     "monthly",
				ByMonthDay:    []int{1, -1},
				BySetPosition: []int{1},
				ByMonth:       []string{"1", "7"},
			},
			want: "FREQ=MONTHLY;BYMONTHDAY=1,-1;BYMONTH=1,7;BYSETPOS=1",
		},
		{
			name: "week start",
			rule: jsRecurrenceRule{Frequency: "weekly", FirstDayOfWeek: "su"},
			want: "FREQ=WEEKLY;WKST=SU",
		},
		{
			name: "week start monday is the default and is omitted",
			rule: jsRecurrenceRule{Frequency: "weekly", FirstDayOfWeek: "mo"},
			want: "FREQ=WEEKLY",
		},
		{
			name: "time-of-day parts",
			rule: jsRecurrenceRule{
				Frequency: "hourly", ByHour: []int{9, 12}, ByMinute: []int{0, 30}, BySecond: []int{0},
			},
			want: "FREQ=HOURLY;BYHOUR=9,12;BYMINUTE=0,30;BYSECOND=0",
		},
		{
			name: "yearly by week and year day",
			rule: jsRecurrenceRule{Frequency: "yearly", ByWeekNo: []int{1, 53}, ByYearDay: []int{-1}},
			want: "FREQ=YEARLY;BYYEARDAY=-1;BYWEEKNO=1,53",
		},
		{
			name: "no frequency yields nothing",
			rule: jsRecurrenceRule{Interval: 3},
			want: "",
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			loc := tc.loc
			if loc == nil {
				loc = time.UTC
			}
			if got := recurrenceToRRule(tc.rule, loc, tc.allDay); got != tc.want {
				t.Errorf("got  %q\nwant %q", got, tc.want)
			}
		})
	}
}

func TestRRuleToRecurrence(t *testing.T) {
	tests := []struct {
		rrule string
		check func(*testing.T, jsRecurrenceRule)
	}{
		{"FREQ=DAILY", func(t *testing.T, r jsRecurrenceRule) {
			if r.Frequency != "daily" || r.Type != "RecurrenceRule" {
				t.Errorf("%+v", r)
			}
		}},
		{"RRULE:FREQ=WEEKLY;INTERVAL=3;WKST=SU", func(t *testing.T, r jsRecurrenceRule) {
			if r.Frequency != "weekly" || r.Interval != 3 || r.FirstDayOfWeek != "su" {
				t.Errorf("%+v", r)
			}
		}},
		{"FREQ=MONTHLY;BYDAY=-1FR,MO", func(t *testing.T, r jsRecurrenceRule) {
			if len(r.ByDay) != 2 {
				t.Fatalf("%+v", r)
			}
			if r.ByDay[0].Day != "fr" || r.ByDay[0].NthOfPeriod == nil || *r.ByDay[0].NthOfPeriod != -1 {
				t.Errorf("byDay[0] = %+v", r.ByDay[0])
			}
			if r.ByDay[1].Day != "mo" || r.ByDay[1].NthOfPeriod != nil {
				t.Errorf("byDay[1] = %+v", r.ByDay[1])
			}
		}},
		{"FREQ=DAILY;UNTIL=20260601T073000Z", func(t *testing.T, r jsRecurrenceRule) {
			if r.Until != "2026-06-01T07:30:00" {
				t.Errorf("until = %q", r.Until)
			}
		}},
		{"FREQ=YEARLY;UNTIL=20300101", func(t *testing.T, r jsRecurrenceRule) {
			if r.Until != "2030-01-01T00:00:00" {
				t.Errorf("until = %q", r.Until)
			}
		}},
		{"FREQ=MONTHLY;BYMONTHDAY=1,-1;BYSETPOS=2;BYMONTH=3", func(t *testing.T, r jsRecurrenceRule) {
			if !reflect.DeepEqual(r.ByMonthDay, []int{1, -1}) ||
				!reflect.DeepEqual(r.BySetPosition, []int{2}) ||
				!reflect.DeepEqual(r.ByMonth, []string{"3"}) {
				t.Errorf("%+v", r)
			}
		}},
		{"FREQ=DAILY;X-VENDOR=whatever", func(t *testing.T, r jsRecurrenceRule) {
			if r.Frequency != "daily" {
				t.Errorf("unknown parts should be ignored: %+v", r)
			}
		}},
	}
	for _, tc := range tests {
		t.Run(tc.rrule, func(t *testing.T) {
			r, err := rruleToRecurrence(tc.rrule, time.UTC)
			if err != nil {
				t.Fatalf("rruleToRecurrence: %v", err)
			}
			tc.check(t, r)
		})
	}

	for _, bad := range []string{"", "INTERVAL=2", "FREQ=DAILY;BYDAY=XX", "FREQ=DAILY;COUNT=many"} {
		if _, err := rruleToRecurrence(bad, time.UTC); err == nil {
			t.Errorf("rruleToRecurrence(%q) should have failed", bad)
		}
	}
}

func TestRecurrenceRoundTrip(t *testing.T) {
	for _, rrule := range []string{
		"FREQ=DAILY",
		"FREQ=WEEKLY;INTERVAL=2;BYDAY=MO,WE,FR",
		"FREQ=MONTHLY;COUNT=12;BYDAY=1MO",
		"FREQ=DAILY;UNTIL=20260601T073000Z",
		"FREQ=YEARLY;BYMONTH=3;BYMONTHDAY=15",
		"FREQ=MONTHLY;BYDAY=MO,TU,WE,TH,FR;BYSETPOS=-1",
		"FREQ=WEEKLY;WKST=SU",
	} {
		t.Run(rrule, func(t *testing.T) {
			r, err := rruleToRecurrence(rrule, time.UTC)
			if err != nil {
				t.Fatalf("parse: %v", err)
			}
			if got := recurrenceToRRule(r, time.UTC, false); got != rrule {
				t.Errorf("round trip: got %q, want %q", got, rrule)
			}
		})
	}
}

// ---------------------------------------------------------------------------
// Durations and local date-times

func TestParseISODuration(t *testing.T) {
	base := time.Date(2026, 1, 31, 10, 0, 0, 0, time.UTC)
	tests := []struct {
		in   string
		want time.Time
	}{
		{"PT15M", base.Add(15 * time.Minute)},
		{"PT1H30M", base.Add(90 * time.Minute)},
		{"P1D", base.AddDate(0, 0, 1)},
		{"P1W", base.AddDate(0, 0, 7)},
		{"P1M", base.AddDate(0, 1, 0)}, // calendar month, not 30 days
		{"P1Y2M3DT4H5M6S", base.AddDate(1, 2, 3).Add(4*time.Hour + 5*time.Minute + 6*time.Second)},
		{"PT0S", base},
		{"-PT1H", base.Add(-time.Hour)},
	}
	for _, tc := range tests {
		t.Run(tc.in, func(t *testing.T) {
			d, err := parseISODuration(tc.in)
			if err != nil {
				t.Fatalf("parseISODuration: %v", err)
			}
			if got := d.addTo(base); !got.Equal(tc.want) {
				t.Errorf("got %s, want %s", got, tc.want)
			}
		})
	}
	for _, bad := range []string{"", "1H", "PT", "PTXH", "P1"} {
		if _, err := parseISODuration(bad); err == nil {
			t.Errorf("parseISODuration(%q) should have failed", bad)
		}
	}
}

func TestFormatISODuration(t *testing.T) {
	tests := []struct {
		d      time.Duration
		allDay bool
		want   string
	}{
		{15 * time.Minute, false, "PT15M"},
		{90 * time.Minute, false, "PT1H30M"},
		{24 * time.Hour, false, "P1D"},
		{25*time.Hour + 30*time.Second, false, "P1DT1H30S"},
		{24 * time.Hour, true, "P1D"},
		{72 * time.Hour, true, "P3D"},
		{0, false, "PT0S"},
		{-time.Hour, false, "PT0S"},
	}
	for _, tc := range tests {
		if got := formatISODuration(tc.d, tc.allDay); got != tc.want {
			t.Errorf("formatISODuration(%v, %v) = %q, want %q", tc.d, tc.allDay, got, tc.want)
		}
	}
}

func TestParseLocalDateTime(t *testing.T) {
	utc, err := parseLocalDateTime("2026-03-02T09:30:00", time.UTC)
	if err != nil {
		t.Fatal(err)
	}
	if !utc.Equal(time.Date(2026, 3, 2, 9, 30, 0, 0, time.UTC)) {
		t.Errorf("got %s", utc)
	}
	if _, err := parseLocalDateTime("", time.UTC); err == nil {
		t.Error("empty value should fail")
	}
	if _, err := parseLocalDateTime("not a time", time.UTC); err == nil {
		t.Error("garbage should fail")
	}
	// A date-only value is accepted (some servers send one for all-day events).
	if d, err := parseLocalDateTime("2026-07-14", time.UTC); err != nil {
		t.Errorf("date-only: %v", err)
	} else if d.Day() != 14 {
		t.Errorf("date-only parsed as %s", d)
	}
}

func TestLoadLocationFallsBackToUTC(t *testing.T) {
	if loc := loadLocation(nil, nil); loc != time.UTC {
		t.Errorf("nil zone = %v, want UTC", loc)
	}
	bogus := "Mars/Olympus_Mons"
	if loc := loadLocation(&bogus, nil); loc != time.UTC {
		t.Errorf("unknown zone = %v, want UTC", loc)
	}
}

func TestEscapeJSONPointer(t *testing.T) {
	if got := escapeJSONPointer("a/b~c"); got != "a~1b~0c" {
		t.Errorf("got %q", got)
	}
}

func TestAccessRoleFromRights(t *testing.T) {
	if got := (*calendarRights)(nil).accessRole(); got != "owner" {
		t.Errorf("absent rights = %q, want owner", got)
	}
	yes := true
	if got := (&calendarRights{MayReadItems: true, MayAdmin: &yes}).accessRole(); got != "owner" {
		t.Errorf("mayAdmin = %q", got)
	}
	if got := (&calendarRights{MayReadItems: true, MayModifyAll: &yes}).accessRole(); got != "writer" {
		t.Errorf("legacy mayModifyAll = %q", got)
	}
	if got := (&calendarRights{MayReadFreeBusy: true}).accessRole(); got != "" {
		t.Errorf("free/busy only = %q, want empty", got)
	}
}

func TestParticipationMapping(t *testing.T) {
	cases := map[string]model.Participation{
		"accepted":     model.PartAccepted,
		"declined":     model.PartDeclined,
		"tentative":    model.PartTentative,
		"needs-action": model.PartNeedsAction,
		"":             model.PartNeedsAction,
		"delegated":    model.PartNeedsAction,
	}
	for in, want := range cases {
		if got := participationFromJS(in); got != want {
			t.Errorf("participationFromJS(%q) = %q, want %q", in, got, want)
		}
	}
	if got := participationToJS(model.PartDeclined); got != "declined" {
		t.Errorf("participationToJS = %q", got)
	}
	if got := participationToJS(model.Participation("nonsense")); got != "needs-action" {
		t.Errorf("unknown participation should default to needs-action, got %q", got)
	}
}

func TestStatusMapping(t *testing.T) {
	if statusFromJS("") != model.StatusConfirmed {
		t.Error("absent status should default to confirmed")
	}
	if statusFromJS("CANCELLED") != model.StatusCancelled {
		t.Error("status should be case-insensitive")
	}
}

func TestCalendarRequiresCapability(t *testing.T) {
	// Sanity check that the calendar provider announces the right URN.
	cal := (&Client{}).Calendar()
	if !strings.Contains(strings.Join(cal.using(), ","), CapCalendars) {
		t.Errorf("using = %v", cal.using())
	}
}
