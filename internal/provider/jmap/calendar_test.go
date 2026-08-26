package jmap

import (
	"encoding/json"
	"errors"
	"reflect"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/teulaert/emlcalsync/internal/calendar"
	"github.com/teulaert/emlcalsync/internal/model"
	"github.com/teulaert/emlcalsync/internal/provider"
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

// weeklyWithOverridesEvent is a recurring master carrying per-occurrence
// changes inside recurrenceOverrides (RFC 8984 §4.7.3): one moved occurrence,
// one excluded, and one patched through a nested path.
func weeklyWithOverridesEvent() map[string]any {
	var obj map[string]any
	json.Unmarshal([]byte(`{
	  "@type": "Event",
	  "id": "ev-weekly",
	  "calendarIds": {"cal-1": true},
	  "uid": "uid-weekly",
	  "title": "Weekly",
	  "start": "2026-03-02T09:00:00",
	  "duration": "PT1H",
	  "timeZone": "Europe/Amsterdam",
	  "status": "confirmed",
	  "updated": "2026-02-01T10:00:00Z",
	  "locations": {"loc-a": {"@type": "Location", "name": "Room 4"}},
	  "recurrenceRules": [{
	    "@type": "RecurrenceRule",
	    "frequency": "weekly",
	    "byDay": [{"@type": "NDay", "day": "mo"}]
	  }],
	  "alerts": {"a1": {"@type": "Alert", "trigger": {"@type": "OffsetTrigger", "offset": "-PT5M"}}},
	  "recurrenceOverrides": {
	    "2026-03-09T09:00:00": {
	      "start": "2026-03-09T11:00:00",
	      "title": "Weekly (moved)",
	      "duration": "PT30M"
	    },
	    "2026-03-16T09:00:00": {"excluded": true},
	    "2026-03-23T09:00:00": {
	      "status": "tentative",
	      "locations/loc-a/name": "Room 9"
	    }
	  }
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

// TestVendorCalendarCapability: a server that ships calendars under a vendor
// URN (Fastmail's own predates the draft's spelling) must still work — the URN
// is discovered from the session and sent in "using".
func TestVendorCalendarCapability(t *testing.T) {
	const vendorURN = "https://www.fastmail.com/dev/calendars"
	f := newFakeServer(t)
	f.calendarURN = vendorURN
	seedCalendars(f)
	seedEvents(f, standupEvent())

	cal := f.client(t).Calendar()
	ctx := testCtx(t)

	cals, err := cal.Calendars(ctx)
	if err != nil {
		t.Fatalf("Calendars with a vendor capability URN: %v", err)
	}
	if len(cals) != 3 {
		t.Fatalf("got %d calendars", len(cals))
	}
	if using := f.capturedUsing("Calendar/get"); !slices.Contains(using, vendorURN) {
		t.Errorf("Calendar/get using = %v, want the vendor URN", using)
	}
	if urn, acct, err := f.client(t).CalendarCapability(ctx); err != nil ||
		urn != vendorURN || acct != testAccount {
		t.Errorf("CalendarCapability = %q, %q, %v", urn, acct, err)
	}

	// The rest of the surface must use the same URN, not the standard one.
	if _, err := cal.EventChanges(ctx, "cal-1", ""); err != nil {
		t.Fatalf("EventChanges with a vendor capability URN: %v", err)
	}
	for _, m := range []string{"CalendarEvent/query", "CalendarEvent/get"} {
		if using := f.capturedUsing(m); !slices.Contains(using, vendorURN) {
			t.Errorf("%s using = %v, want the vendor URN", m, using)
		}
	}
}

// TestCalendarCapabilityFromAccountCapabilities: no primaryAccounts entry for
// any calendars URN, so the account that lists it must be picked instead.
func TestCalendarCapabilityFromAccountCapabilities(t *testing.T) {
	const vendorURN = "https://www.fastmail.com/dev/calendars"
	f := newFakeServer(t)
	f.calendarURN = vendorURN
	f.calendarPrimary = "-" // drop it from primaryAccounts entirely
	seedCalendars(f)

	c := f.client(t)
	urn, acct, err := c.CalendarCapability(testCtx(t))
	if err != nil {
		t.Fatalf("CalendarCapability: %v", err)
	}
	if urn != vendorURN || acct != testAccount {
		t.Errorf("CalendarCapability = %q, %q, want the vendor URN on %s", urn, acct, testAccount)
	}
	if _, err := c.Calendar().Calendars(testCtx(t)); err != nil {
		t.Fatalf("Calendars: %v", err)
	}
}

// TestCalendarCapabilityMissing: a token without any calendars scope must fail
// with a message naming the standard URN.
func TestCalendarCapabilityMissing(t *testing.T) {
	f := newFakeServer(t)
	f.calendarURN = "-" // no calendars capability at all
	f.calendarPrimary = "-"

	_, _, err := f.client(t).CalendarCapability(testCtx(t))
	if err == nil || !strings.Contains(err.Error(), CapCalendars) {
		t.Fatalf("CalendarCapability error = %v, want one naming %s", err, CapCalendars)
	}
}

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
	// Normalised to an RFC 3339 instant in the master's zone, the way the
	// Google provider writes it and calendar.ApplyExceptions matches it.
	if ex.RecurrenceID != "2026-03-09T09:30:00+01:00" {
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

// TestParticipantKeyIsAValidJSCalendarID: RFC 8984 Ids are URL-safe base64
// characters only, so a raw mail address cannot be one.
func TestParticipantKeyIsAValidJSCalendarID(t *testing.T) {
	key := participantKey("Alice.Smith+tag@example.com")
	if len(key) != 22 {
		t.Errorf("key %q is %d characters, want 22", key, len(key))
	}
	if strings.HasPrefix(key, "-") {
		t.Errorf("key %q starts with '-', which RFC 8984 §1.4.1 forbids", key)
	}
	for _, r := range key {
		ok := r >= 'A' && r <= 'Z' || r >= 'a' && r <= 'z' || r >= '0' && r <= '9' ||
			r == '-' || r == '_'
		if !ok {
			t.Fatalf("key %q contains %q, outside the Id alphabet", key, r)
		}
	}
	// Stable, and insensitive to case and surrounding space.
	if got := participantKey("  ALICE.smith+TAG@Example.COM "); got != key {
		t.Errorf("key is not canonical: %q vs %q", got, key)
	}
	if participantKey("bob@example.com") == key {
		t.Error("different addresses share a key")
	}
}

// TestCreateEventParticipantKeys: the keys written to the server are hashes,
// and reading the event back still matches participants by address.
func TestCreateEventParticipantKeys(t *testing.T) {
	f := newFakeServer(t)
	seedCalendars(f)
	cal := f.client(t).Calendar()

	ev := &model.Event{
		Title:     "Lunch",
		Start:     time.Date(2026, 5, 4, 12, 0, 0, 0, time.UTC),
		End:       time.Date(2026, 5, 4, 13, 0, 0, 0, time.UTC),
		Timezone:  "UTC",
		Organizer: model.Address{Name: "Me", Email: testEmail},
		Attendees: []model.Attendee{
			{Name: "You", Email: "You@Example.com", Response: model.PartAccepted},
		},
	}
	got, err := cal.CreateEvent(testCtx(t), "cal-1", ev)
	if err != nil {
		t.Fatalf("CreateEvent: %v", err)
	}

	create := f.captured("CalendarEvent/set")[0]["create"].(map[string]any)["new"].(map[string]any)
	parts, _ := create["participants"].(map[string]any)
	if len(parts) != 2 {
		t.Fatalf("participants = %v", parts)
	}
	for _, want := range []string{participantKey(testEmail), participantKey("you@example.com")} {
		if _, ok := parts[want]; !ok {
			t.Errorf("participant key %q missing from %v", want, sortedMapKeys(parts))
		}
	}
	for k := range parts {
		if strings.Contains(k, "@") {
			t.Errorf("participant key %q is a mail address, not a JSCalendar Id", k)
		}
	}

	// The address is what reads match on, so the round trip is unaffected.
	if got.Organizer.Email != testEmail {
		t.Errorf("organizer = %+v", got.Organizer)
	}
	if len(got.Attendees) != 1 {
		t.Fatalf("attendees = %+v", got.Attendees)
	}
	var you *model.Attendee
	for i := range got.Attendees {
		if strings.EqualFold(got.Attendees[i].Email, "you@example.com") {
			you = &got.Attendees[i]
		}
	}
	if you == nil || you.Response != model.PartAccepted {
		t.Errorf("attendee round trip = %+v", got.Attendees)
	}

	// RSVP patches the key the server holds, whatever it is.
	if err := cal.Respond(testCtx(t), "cal-1", got.RemoteID, model.PartDeclined); err != nil {
		t.Fatalf("Respond: %v", err)
	}
	back, err := cal.fetchModelEvent(testCtx(t), testAccount, "cal-1", got.RemoteID)
	if err != nil {
		t.Fatal(err)
	}
	if back.MyResponse != model.PartDeclined {
		t.Errorf("MyResponse after Respond = %q", back.MyResponse)
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
			want: "FREQ=MONTHLY;BYMONTH=1,7;BYMONTHDAY=1,-1;BYSETPOS=1",
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
			want: "FREQ=YEARLY;BYWEEKNO=1,53;BYYEARDAY=-1",
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

// ---------------------------------------------------------------------------
// recurrenceOverrides

func TestEventChangesExpandsRecurrenceOverrides(t *testing.T) {
	f := newFakeServer(t)
	seedCalendars(f)
	seedEvents(f, weeklyWithOverridesEvent())
	cal := f.client(t).Calendar()

	ch, err := cal.EventChanges(testCtx(t), "cal-1", "")
	if err != nil {
		t.Fatalf("EventChanges: %v", err)
	}
	if len(ch.Upserted) != 4 {
		t.Fatalf("got %d events, want the master plus 3 overrides: %+v", len(ch.Upserted), ch.Upserted)
	}
	byID := map[string]model.Event{}
	for _, ev := range ch.Upserted {
		byID[ev.RemoteID] = ev
	}

	// --- the master ------------------------------------------------------
	master, ok := byID["ev-weekly"]
	if !ok {
		t.Fatalf("no master event in %v", sortedMapKeys(byID))
	}
	if master.RecurrenceID != "" {
		t.Errorf("the master must not carry a recurrence id, got %q", master.RecurrenceID)
	}
	if master.RRule != "FREQ=WEEKLY;BYDAY=MO" {
		t.Errorf("master rrule = %q", master.RRule)
	}
	var rawMaster map[string]any
	if err := json.Unmarshal(master.RawJSON, &rawMaster); err != nil {
		t.Fatal(err)
	}
	if _, ok := rawMaster["recurrenceOverrides"]; !ok {
		t.Error("the master's RawJSON lost recurrenceOverrides")
	}

	// --- the moved occurrence -------------------------------------------
	moved, ok := byID["ev-weekly;20260309T090000"]
	if !ok {
		t.Fatalf("no exception for the moved occurrence in %v", sortedMapKeys(byID))
	}
	if moved.RecurrenceID != "2026-03-09T09:00:00+01:00" {
		t.Errorf("moved recurrenceId = %q", moved.RecurrenceID)
	}
	if moved.UID != master.UID {
		t.Errorf("exception uid %q should match the master %q", moved.UID, master.UID)
	}
	if moved.RRule != "" {
		t.Errorf("an exception instance must not carry an RRULE, got %q", moved.RRule)
	}
	if moved.Title != "Weekly (moved)" {
		t.Errorf("moved title = %q", moved.Title)
	}
	loc, err := time.LoadLocation("Europe/Amsterdam")
	if err != nil {
		t.Skipf("no tzdata: %v", err)
	}
	if want := time.Date(2026, 3, 9, 11, 0, 0, 0, loc); !moved.Start.Equal(want) {
		t.Errorf("moved start = %s, want %s", moved.Start, want)
	}
	if want := time.Date(2026, 3, 9, 11, 30, 0, 0, loc); !moved.End.Equal(want) {
		t.Errorf("moved end = %s, want %s (the override's own duration)", moved.End, want)
	}
	if moved.Status != model.StatusConfirmed {
		t.Errorf("moved status = %q", moved.Status)
	}
	// Properties the override does not touch come from the master.
	if moved.Location != "Room 4" {
		t.Errorf("moved location = %q, want the master's", moved.Location)
	}

	// --- the excluded occurrence ----------------------------------------
	gone, ok := byID["ev-weekly;20260316T090000"]
	if !ok {
		t.Fatalf("no exception for the excluded occurrence in %v", sortedMapKeys(byID))
	}
	if gone.Status != model.StatusCancelled {
		t.Errorf("excluded occurrence status = %q, want cancelled", gone.Status)
	}
	if gone.RecurrenceID != "2026-03-16T09:00:00+01:00" {
		t.Errorf("excluded recurrenceId = %q", gone.RecurrenceID)
	}

	// --- the nested patch ------------------------------------------------
	patched, ok := byID["ev-weekly;20260323T090000"]
	if !ok {
		t.Fatalf("no exception for the patched occurrence in %v", sortedMapKeys(byID))
	}
	if patched.Location != "Room 9" {
		t.Errorf("patched location = %q, want the override's nested path to have applied", patched.Location)
	}
	if patched.Status != model.StatusTentative {
		t.Errorf("patched status = %q", patched.Status)
	}
	if want := time.Date(2026, 3, 23, 9, 0, 0, 0, loc); !patched.Start.Equal(want) {
		t.Errorf("patched start = %s, want the occurrence time %s", patched.Start, want)
	}
}

// TestRecurrenceOverridesExpandThroughApplyExceptions is the end-to-end check:
// the events this provider hands over must make internal/calendar produce the
// right occurrences.
func TestRecurrenceOverridesExpandThroughApplyExceptions(t *testing.T) {
	loc, err := time.LoadLocation("Europe/Amsterdam")
	if err != nil {
		t.Skipf("no tzdata: %v", err)
	}
	f := newFakeServer(t)
	seedCalendars(f)
	seedEvents(f, weeklyWithOverridesEvent())

	ch, err := f.client(t).Calendar().EventChanges(testCtx(t), "cal-1", "")
	if err != nil {
		t.Fatalf("EventChanges: %v", err)
	}
	var master model.Event
	var exceptions []model.Event
	for _, ev := range ch.Upserted {
		if ev.RecurrenceID == "" {
			master = ev
			continue
		}
		exceptions = append(exceptions, ev)
	}

	from := time.Date(2026, 3, 1, 0, 0, 0, 0, loc)
	to := time.Date(2026, 3, 31, 0, 0, 0, 0, loc)
	occ, err := calendar.Expand(&master, from, to)
	if err != nil {
		t.Fatalf("Expand: %v", err)
	}
	occ = calendar.ApplyExceptions(occ, exceptions)

	var got []string
	for _, o := range occ {
		got = append(got, o.Start.In(loc).Format(time.RFC3339))
	}
	want := []string{
		"2026-03-02T09:00:00+01:00",
		"2026-03-09T11:00:00+01:00", // moved
		// 2026-03-16 is excluded
		"2026-03-23T09:00:00+01:00",
		"2026-03-30T09:00:00+02:00", // after the DST switch
	}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("occurrences =\n%v\nwant\n%v", got, want)
	}
}

// TestUpdateEventKeepsRecurrenceOverrides: an ordinary edit of the master must
// not rewrite (or drop) the per-occurrence overrides.
func TestUpdateEventKeepsRecurrenceOverrides(t *testing.T) {
	f := newFakeServer(t)
	seedCalendars(f)
	seedEvents(f, weeklyWithOverridesEvent())
	cal := f.client(t).Calendar()
	ctx := testCtx(t)

	ch, err := cal.EventChanges(ctx, "cal-1", "")
	if err != nil {
		t.Fatal(err)
	}
	var master model.Event
	for _, ev := range ch.Upserted {
		if ev.RemoteID == "ev-weekly" {
			master = ev
		}
	}
	master.Title = "Weekly sync"
	if _, err := cal.UpdateEvent(ctx, &master); err != nil {
		t.Fatalf("UpdateEvent: %v", err)
	}

	sets := f.captured("CalendarEvent/set")
	if len(sets) != 1 {
		t.Fatalf("got %d CalendarEvent/set calls", len(sets))
	}
	patch := sets[0]["update"].(map[string]any)["ev-weekly"].(map[string]any)
	if patch["title"] != "Weekly sync" {
		t.Errorf("patch title = %v", patch["title"])
	}
	for k := range patch {
		if strings.HasPrefix(k, "recurrenceOverrides") {
			t.Errorf("the patch touches %q; overrides are not ours to rewrite", k)
		}
	}
	f.mu.Lock()
	stored, _ := f.events["ev-weekly"]["recurrenceOverrides"].(map[string]any)
	f.mu.Unlock()
	if len(stored) != 3 {
		t.Errorf("the server now holds %d overrides, want 3", len(stored))
	}
}

// TestCreateEventCarriesRecurrenceOverrides: re-creating an event we read from
// this provider keeps its overrides, which a create has no base to patch into.
func TestCreateEventCarriesRecurrenceOverrides(t *testing.T) {
	f := newFakeServer(t)
	seedCalendars(f)
	seedEvents(f, weeklyWithOverridesEvent())
	cal := f.client(t).Calendar()
	ctx := testCtx(t)

	ch, err := cal.EventChanges(ctx, "cal-1", "")
	if err != nil {
		t.Fatal(err)
	}
	var master model.Event
	for _, ev := range ch.Upserted {
		if ev.RemoteID == "ev-weekly" {
			master = ev
		}
	}
	master.RemoteID = ""
	if _, err := cal.CreateEvent(ctx, "cal-1", &master); err != nil {
		t.Fatalf("CreateEvent: %v", err)
	}
	create := f.captured("CalendarEvent/set")[0]["create"].(map[string]any)["new"].(map[string]any)
	ovr, _ := create["recurrenceOverrides"].(map[string]any)
	if len(ovr) != 3 {
		t.Fatalf("created event carries %d overrides, want 3: %v", len(ovr), create["recurrenceOverrides"])
	}
	if excl, _ := ovr["2026-03-16T09:00:00"].(map[string]any); excl["excluded"] != true {
		t.Errorf("the excluded override did not survive: %v", ovr["2026-03-16T09:00:00"])
	}
}

// TestWritesRefuseOverrideInstances: the ids this package invents for override
// instances mean nothing to the server, so a write against one is refused
// rather than sent.
func TestWritesRefuseOverrideInstances(t *testing.T) {
	f := newFakeServer(t)
	seedCalendars(f)
	seedEvents(f, weeklyWithOverridesEvent())
	cal := f.client(t).Calendar()
	ctx := testCtx(t)

	ev := &model.Event{
		RemoteID:       "ev-weekly;20260309T090000",
		CalendarRemote: "cal-1",
		Title:          "nope",
		Start:          time.Date(2026, 3, 9, 11, 0, 0, 0, time.UTC),
	}
	if _, err := cal.UpdateEvent(ctx, ev); err == nil {
		t.Error("UpdateEvent on an override instance should be refused")
	}
	if err := cal.DeleteEvent(ctx, "cal-1", ev.RemoteID); err == nil {
		t.Error("DeleteEvent on an override instance should be refused")
	}
	if n := len(f.captured("CalendarEvent/set")); n != 0 {
		t.Errorf("%d writes reached the server", n)
	}
}
