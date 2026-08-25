package gcal

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"net/url"
	"reflect"
	"strings"
	"sync"
	"testing"
	"time"

	calendarapi "google.golang.org/api/calendar/v3"

	"github.com/lennert/emlcal/internal/model"
	"github.com/lennert/emlcal/internal/provider"
)

// fakeCalendar is a stand-in for the Google Calendar API.
type fakeCalendar struct {
	t   *testing.T
	srv *httptest.Server

	mu sync.Mutex

	calendars []*calendarapi.CalendarListEntry
	// pages are returned in order by successive events.list calls.
	pages     []*calendarapi.Events
	pageIndex int
	events    map[string]*calendarapi.Event

	gone bool // events.list answers 410

	// observations
	listQueries   []url.Values
	insertQueries []url.Values
	patchQueries  []url.Values
	deleteQueries []url.Values
	lastPatch     *calendarapi.Event
	lastInsert    *calendarapi.Event
	patchedID     string
	deleted       []string
}

func newFakeCalendar(t *testing.T) *fakeCalendar {
	t.Helper()
	f := &fakeCalendar{t: t, events: map[string]*calendarapi.Event{}}

	mux := http.NewServeMux()
	mux.HandleFunc("GET /calendar/v3/users/me/calendarList", f.handleCalendarList)
	mux.HandleFunc("GET /calendar/v3/calendars/{cid}/events", f.handleEventsList)
	mux.HandleFunc("POST /calendar/v3/calendars/{cid}/events", f.handleInsert)
	mux.HandleFunc("GET /calendar/v3/calendars/{cid}/events/{eid}", f.handleGet)
	mux.HandleFunc("PATCH /calendar/v3/calendars/{cid}/events/{eid}", f.handlePatch)
	mux.HandleFunc("DELETE /calendar/v3/calendars/{cid}/events/{eid}", f.handleDelete)
	mux.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		f.t.Errorf("fake calendar: unexpected request %s %s", r.Method, r.URL.Path)
		http.Error(w, "unexpected", http.StatusBadRequest)
	})

	f.srv = httptest.NewServer(mux)
	t.Cleanup(f.srv.Close)
	return f
}

func (f *fakeCalendar) options() Options {
	return Options{
		HTTPClient: f.srv.Client(),
		Email:      "me@example.com",
		Endpoint:   f.srv.URL + "/calendar/v3/",
		Logger:     slog.New(slog.NewTextHandler(io.Discard, nil)),
	}
}

func (f *fakeCalendar) writeJSON(w http.ResponseWriter, v any) {
	w.Header().Set("Content-Type", "application/json")
	if err := json.NewEncoder(w).Encode(v); err != nil {
		f.t.Errorf("fake calendar: encode: %v", err)
	}
}

func apiError(w http.ResponseWriter, status int, reason, message string) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	fmt.Fprintf(w, `{"error":{"code":%d,"message":%q,"errors":[{"reason":%q,"message":%q}]}}`,
		status, message, reason, message)
}

func (f *fakeCalendar) handleCalendarList(w http.ResponseWriter, r *http.Request) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.writeJSON(w, &calendarapi.CalendarList{Items: f.calendars})
}

func (f *fakeCalendar) handleEventsList(w http.ResponseWriter, r *http.Request) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.listQueries = append(f.listQueries, r.URL.Query())
	if f.gone {
		apiError(w, http.StatusGone, "fullSyncRequired", "Sync token is no longer valid, a full sync is required.")
		return
	}
	if f.pageIndex >= len(f.pages) {
		f.writeJSON(w, &calendarapi.Events{NextSyncToken: "token-empty"})
		return
	}
	page := f.pages[f.pageIndex]
	f.pageIndex++
	f.writeJSON(w, page)
}

func (f *fakeCalendar) handleGet(w http.ResponseWriter, r *http.Request) {
	f.mu.Lock()
	defer f.mu.Unlock()
	ev, ok := f.events[r.PathValue("eid")]
	if !ok {
		apiError(w, http.StatusNotFound, "notFound", "event not found")
		return
	}
	f.writeJSON(w, ev)
}

func (f *fakeCalendar) handleInsert(w http.ResponseWriter, r *http.Request) {
	var ev calendarapi.Event
	if err := json.NewDecoder(r.Body).Decode(&ev); err != nil {
		apiError(w, http.StatusBadRequest, "invalid", err.Error())
		return
	}
	f.mu.Lock()
	f.lastInsert = &ev
	f.insertQueries = append(f.insertQueries, r.URL.Query())
	f.mu.Unlock()
	stored := ev
	stored.Id = "created-1"
	stored.ICalUID = "created-1@google.com"
	stored.Status = statusConfirmed
	stored.Updated = "2026-08-25T10:00:00Z"
	f.writeJSON(w, &stored)
}

func (f *fakeCalendar) handlePatch(w http.ResponseWriter, r *http.Request) {
	var patch calendarapi.Event
	if err := json.NewDecoder(r.Body).Decode(&patch); err != nil {
		apiError(w, http.StatusBadRequest, "invalid", err.Error())
		return
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	f.lastPatch = &patch
	f.patchQueries = append(f.patchQueries, r.URL.Query())
	f.patchedID = r.PathValue("eid")

	base, ok := f.events[f.patchedID]
	if !ok {
		apiError(w, http.StatusNotFound, "notFound", "event not found")
		return
	}
	merged := *base
	if patch.Summary != "" {
		merged.Summary = patch.Summary
	}
	if patch.Attendees != nil {
		merged.Attendees = patch.Attendees
	}
	if patch.Start != nil {
		merged.Start = patch.Start
	}
	f.events[f.patchedID] = &merged
	f.writeJSON(w, &merged)
}

func (f *fakeCalendar) handleDelete(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("eid")
	f.mu.Lock()
	defer f.mu.Unlock()
	f.deleteQueries = append(f.deleteQueries, r.URL.Query())
	if _, ok := f.events[id]; !ok {
		apiError(w, http.StatusNotFound, "notFound", "event not found")
		return
	}
	delete(f.events, id)
	f.deleted = append(f.deleted, id)
	w.WriteHeader(http.StatusNoContent)
}

func newCal(t *testing.T, f *fakeCalendar) *Calendar {
	t.Helper()
	c, err := New(context.Background(), f.options())
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	return c
}

// ---------------------------------------------------------------------------

func TestNewRequiresHTTPClient(t *testing.T) {
	if _, err := New(context.Background(), Options{}); err == nil {
		t.Fatal("New without an HTTP client succeeded, want an error")
	}
}

func TestCalendars(t *testing.T) {
	f := newFakeCalendar(t)
	f.calendars = []*calendarapi.CalendarListEntry{
		{Id: "primary@example.com", Summary: "me@example.com", BackgroundColor: "#9fe1e7",
			TimeZone: "Europe/Amsterdam", Primary: true, AccessRole: "owner"},
		{Id: "team@example.com", Summary: "Team", SummaryOverride: "Team (shared)",
			TimeZone: "UTC", AccessRole: "reader"},
		{Id: "dead@example.com", Summary: "Removed", Deleted: true},
	}
	c := newCal(t, f)
	cals, err := c.Calendars(context.Background())
	if err != nil {
		t.Fatalf("Calendars: %v", err)
	}
	if len(cals) != 2 {
		t.Fatalf("got %d calendars, want 2 (deleted ones are dropped)", len(cals))
	}
	want := model.Calendar{
		RemoteID: "primary@example.com", Name: "me@example.com", Color: "#9fe1e7",
		Timezone: "Europe/Amsterdam", Primary: true, AccessRole: "owner",
	}
	if cals[0] != want {
		t.Errorf("calendar[0] = %+v, want %+v", cals[0], want)
	}
	if cals[1].Name != "Team (shared)" {
		t.Errorf("calendar[1].Name = %q, want the summaryOverride", cals[1].Name)
	}
}

func TestEventChangesSyncTokenFlow(t *testing.T) {
	f := newFakeCalendar(t)
	f.pages = []*calendarapi.Events{
		{
			TimeZone:      "Europe/Amsterdam",
			NextPageToken: "page2",
			Items: []*calendarapi.Event{{
				Id: "e1", ICalUID: "e1@g", Summary: "One", Status: statusConfirmed,
				Start:   &calendarapi.EventDateTime{DateTime: "2026-08-25T10:00:00+02:00"},
				End:     &calendarapi.EventDateTime{DateTime: "2026-08-25T11:00:00+02:00"},
				Updated: "2026-08-20T09:00:00Z",
			}},
		},
		{
			TimeZone:      "Europe/Amsterdam",
			NextSyncToken: "token-2",
			Items: []*calendarapi.Event{{
				Id: "e2", ICalUID: "e2@g", Summary: "Two", Status: statusConfirmed,
				Start: &calendarapi.EventDateTime{DateTime: "2026-08-26T10:00:00+02:00"},
				End:   &calendarapi.EventDateTime{DateTime: "2026-08-26T11:00:00+02:00"},
			}},
		},
	}
	c := newCal(t, f)
	ch, err := c.EventChanges(context.Background(), "cal-1", "")
	if err != nil {
		t.Fatalf("EventChanges: %v", err)
	}
	if len(ch.Upserted) != 2 {
		t.Fatalf("upserted %d events, want 2 across both pages", len(ch.Upserted))
	}
	if ch.NewState != "token-2" {
		t.Errorf("NewState = %q, want token-2", ch.NewState)
	}

	f.mu.Lock()
	q := f.listQueries
	f.mu.Unlock()
	if len(q) != 2 {
		t.Fatalf("events.list called %d times, want 2", len(q))
	}
	if q[0].Get("singleEvents") != "false" || q[0].Get("showDeleted") != "true" {
		t.Errorf("first list query = %v, want singleEvents=false&showDeleted=true", q[0])
	}
	if q[0].Get("maxResults") != "2500" {
		t.Errorf("maxResults = %q, want 2500", q[0].Get("maxResults"))
	}
	if q[0].Get("syncToken") != "" {
		t.Errorf("first list sent syncToken %q, want none", q[0].Get("syncToken"))
	}
	if q[1].Get("pageToken") != "page2" {
		t.Errorf("second list pageToken = %q, want page2", q[1].Get("pageToken"))
	}

	// An incremental call must send the stored token.
	f.mu.Lock()
	f.pages = []*calendarapi.Events{{TimeZone: "UTC", NextSyncToken: "token-3"}}
	f.pageIndex = 0
	f.listQueries = nil
	f.mu.Unlock()

	ch, err = c.EventChanges(context.Background(), "cal-1", "token-2")
	if err != nil {
		t.Fatalf("EventChanges (incremental): %v", err)
	}
	if ch.NewState != "token-3" {
		t.Errorf("NewState = %q, want token-3", ch.NewState)
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	if got := f.listQueries[0].Get("syncToken"); got != "token-2" {
		t.Errorf("incremental list syncToken = %q, want token-2", got)
	}
}

func TestEventChangesStateExpired(t *testing.T) {
	f := newFakeCalendar(t)
	f.gone = true
	c := newCal(t, f)
	_, err := c.EventChanges(context.Background(), "cal-1", "stale-token")
	if !errors.Is(err, provider.ErrStateExpired) {
		t.Fatalf("EventChanges with a 410 = %v, want provider.ErrStateExpired", err)
	}
}

func TestEventMappingRecurrenceAndTimezone(t *testing.T) {
	f := newFakeCalendar(t)
	f.pages = []*calendarapi.Events{{
		TimeZone:      "Europe/Amsterdam",
		NextSyncToken: "token-1",
		Items: []*calendarapi.Event{
			{ // recurring master
				Id: "master", ICalUID: "master@g", Summary: "Standup",
				Description: "daily", Location: "Room 1", Status: statusConfirmed,
				Start:      &calendarapi.EventDateTime{DateTime: "2026-08-25T09:00:00+02:00", TimeZone: "Europe/Amsterdam"},
				End:        &calendarapi.EventDateTime{DateTime: "2026-08-25T09:15:00+02:00", TimeZone: "Europe/Amsterdam"},
				Recurrence: []string{"RRULE:FREQ=WEEKLY;BYDAY=MO,TU", "EXDATE;TZID=Europe/Amsterdam:20260901T090000"},
				Organizer:  &calendarapi.EventOrganizer{DisplayName: "Boss", Email: "boss@example.com"},
				Attendees: []*calendarapi.EventAttendee{
					{Email: "boss@example.com", DisplayName: "Boss", ResponseStatus: "accepted", Organizer: true},
					{Email: "me@example.com", ResponseStatus: "tentative", Self: true},
					{Email: "opt@example.com", ResponseStatus: "needsAction", Optional: true},
				},
				Updated: "2026-08-20T09:00:00Z",
			},
			{ // moved instance
				Id: "master_20260826T070000Z", ICalUID: "master@g", Summary: "Standup",
				Status:            statusConfirmed,
				RecurringEventId:  "master",
				OriginalStartTime: &calendarapi.EventDateTime{DateTime: "2026-08-26T09:00:00+02:00", TimeZone: "Europe/Amsterdam"},
				Start:             &calendarapi.EventDateTime{DateTime: "2026-08-26T10:00:00+02:00", TimeZone: "Europe/Amsterdam"},
				End:               &calendarapi.EventDateTime{DateTime: "2026-08-26T10:15:00+02:00", TimeZone: "Europe/Amsterdam"},
			},
			{ // cancelled instance: an exception, not a deletion
				Id: "master_20260827T070000Z", Status: statusCancelled,
				RecurringEventId:  "master",
				OriginalStartTime: &calendarapi.EventDateTime{DateTime: "2026-08-27T09:00:00+02:00", TimeZone: "Europe/Amsterdam"},
			},
			{ // cancelled master: a real deletion
				Id: "dead", Status: statusCancelled,
			},
		},
	}}

	c := newCal(t, f)
	ch, err := c.EventChanges(context.Background(), "cal-1", "")
	if err != nil {
		t.Fatalf("EventChanges: %v", err)
	}
	if !reflect.DeepEqual(ch.Removed, []string{"dead"}) {
		t.Errorf("Removed = %v, want [dead]", ch.Removed)
	}
	if len(ch.Upserted) != 3 {
		t.Fatalf("upserted %d events, want 3", len(ch.Upserted))
	}

	master := ch.Upserted[0]
	if master.RRule != "FREQ=WEEKLY;BYDAY=MO,TU" {
		t.Errorf("RRule = %q, want the RRULE line without its prefix", master.RRule)
	}
	if master.UID != "master@g" {
		t.Errorf("UID = %q, want the iCalUID", master.UID)
	}
	if master.Timezone != "Europe/Amsterdam" {
		t.Errorf("Timezone = %q", master.Timezone)
	}
	if master.CalendarRemote != "cal-1" {
		t.Errorf("CalendarRemote = %q, want cal-1", master.CalendarRemote)
	}
	if loc, err := time.LoadLocation("Europe/Amsterdam"); err == nil {
		want := time.Date(2026, 8, 25, 9, 0, 0, 0, loc)
		if !master.Start.Equal(want) {
			t.Errorf("Start = %v, want %v", master.Start, want)
		}
		if got, _ := master.Start.Zone(); got != "CEST" {
			t.Errorf("Start zone = %q, want CEST (the calendar's zone)", got)
		}
	}
	if master.MyResponse != model.PartTentative {
		t.Errorf("MyResponse = %q, want tentative", master.MyResponse)
	}
	if len(master.Attendees) != 3 || !master.Attendees[1].Self ||
		master.Attendees[0].Response != model.PartAccepted ||
		master.Attendees[2].Response != model.PartNeedsAction ||
		!master.Attendees[2].Optional {
		t.Errorf("attendees mapped wrong: %+v", master.Attendees)
	}
	if master.Organizer.Email != "boss@example.com" || master.Organizer.Name != "Boss" {
		t.Errorf("organizer = %+v", master.Organizer)
	}
	if master.Status != model.StatusConfirmed {
		t.Errorf("status = %q, want confirmed", master.Status)
	}
	// EXDATE is not modelled but must survive in RawJSON.
	var rawBack calendarapi.Event
	if err := json.Unmarshal(master.RawJSON, &rawBack); err != nil {
		t.Fatalf("RawJSON is not valid event JSON: %v", err)
	}
	if len(rawBack.Recurrence) != 2 {
		t.Errorf("RawJSON recurrence = %v, want both RRULE and EXDATE", rawBack.Recurrence)
	}
	if master.Updated.IsZero() {
		t.Error("Updated was not parsed")
	}

	moved := ch.Upserted[1]
	if moved.RecurrenceID != "2026-08-26T09:00:00+02:00" {
		t.Errorf("RecurrenceID = %q, want the original start in RFC3339", moved.RecurrenceID)
	}

	cancelled := ch.Upserted[2]
	if cancelled.Status != model.StatusCancelled {
		t.Errorf("cancelled instance status = %q, want cancelled", cancelled.Status)
	}
	if cancelled.RecurrenceID == "" {
		t.Error("cancelled instance has no RecurrenceID")
	}
}

func TestAllDayMapping(t *testing.T) {
	f := newFakeCalendar(t)
	f.pages = []*calendarapi.Events{{
		TimeZone:      "Europe/Amsterdam",
		NextSyncToken: "token-1",
		Items: []*calendarapi.Event{{
			Id: "holiday", ICalUID: "h@g", Summary: "Holiday", Status: statusConfirmed,
			Start: &calendarapi.EventDateTime{Date: "2026-12-25"},
			End:   &calendarapi.EventDateTime{Date: "2026-12-26"},
		}},
	}}
	c := newCal(t, f)
	ch, err := c.EventChanges(context.Background(), "cal-1", "")
	if err != nil {
		t.Fatalf("EventChanges: %v", err)
	}
	ev := ch.Upserted[0]
	if !ev.AllDay {
		t.Fatal("AllDay = false, want true")
	}
	loc, err := time.LoadLocation("Europe/Amsterdam")
	if err != nil {
		t.Skip("no tzdata available")
	}
	wantStart := time.Date(2026, 12, 25, 0, 0, 0, 0, loc)
	if !ev.Start.Equal(wantStart) {
		t.Errorf("Start = %v, want local midnight %v", ev.Start, wantStart)
	}
	if h, m, s := ev.Start.Clock(); h|m|s != 0 {
		t.Errorf("Start clock = %02d:%02d:%02d, want midnight", h, m, s)
	}
	// Google's all-day end is exclusive; it is kept as-is.
	if !ev.End.Equal(time.Date(2026, 12, 26, 0, 0, 0, 0, loc)) {
		t.Errorf("End = %v, want the exclusive end date at local midnight", ev.End)
	}
}

func TestRespondPatchesSelfAttendee(t *testing.T) {
	f := newFakeCalendar(t)
	f.events["ev-1"] = &calendarapi.Event{
		Id: "ev-1", Summary: "Lunch", Status: statusConfirmed,
		Start: &calendarapi.EventDateTime{DateTime: "2026-08-25T12:00:00Z"},
		End:   &calendarapi.EventDateTime{DateTime: "2026-08-25T13:00:00Z"},
		Attendees: []*calendarapi.EventAttendee{
			{Email: "boss@example.com", ResponseStatus: "accepted"},
			{Email: "me@example.com", ResponseStatus: "needsAction", Self: true},
		},
	}
	c := newCal(t, f)
	if err := c.Respond(context.Background(), "cal-1", "ev-1", model.PartDeclined); err != nil {
		t.Fatalf("Respond: %v", err)
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.patchedID != "ev-1" {
		t.Errorf("patched %q, want ev-1", f.patchedID)
	}
	if len(f.lastPatch.Attendees) != 2 {
		t.Fatalf("patch carried %d attendees, want the full list", len(f.lastPatch.Attendees))
	}
	if f.lastPatch.Attendees[0].ResponseStatus != "accepted" {
		t.Errorf("other attendee's response was changed to %q", f.lastPatch.Attendees[0].ResponseStatus)
	}
	if f.lastPatch.Attendees[1].ResponseStatus != "declined" {
		t.Errorf("self response = %q, want declined", f.lastPatch.Attendees[1].ResponseStatus)
	}
	if f.lastPatch.Summary != "" {
		t.Errorf("patch also sent summary %q; it must only touch attendees", f.lastPatch.Summary)
	}
}

func TestRespondWhenNotAnAttendee(t *testing.T) {
	f := newFakeCalendar(t)
	f.events["ev-1"] = &calendarapi.Event{Id: "ev-1", Attendees: []*calendarapi.EventAttendee{
		{Email: "someone@example.com", ResponseStatus: "accepted"},
	}}
	c := newCal(t, f)
	if err := c.Respond(context.Background(), "cal-1", "ev-1", model.PartAccepted); err == nil {
		t.Fatal("Respond on an event we do not attend succeeded, want an error")
	}
	if err := c.Respond(context.Background(), "cal-1", "missing", model.PartAccepted); !errors.Is(err, model.ErrNotFound) {
		t.Fatalf("Respond on a missing event = %v, want model.ErrNotFound", err)
	}
}

func TestCreateUpdateDelete(t *testing.T) {
	f := newFakeCalendar(t)
	c := newCal(t, f)
	ctx := context.Background()
	loc, err := time.LoadLocation("Europe/Amsterdam")
	if err != nil {
		loc = time.UTC
	}

	created, err := c.CreateEvent(ctx, "cal-1", &model.Event{
		Title:     "Design review",
		Location:  "Room 2",
		Start:     time.Date(2026, 9, 1, 10, 0, 0, 0, loc),
		End:       time.Date(2026, 9, 1, 11, 0, 0, 0, loc),
		Timezone:  loc.String(),
		RRule:     "FREQ=WEEKLY",
		Attendees: []model.Attendee{{Email: "boss@example.com", Response: model.PartNeedsAction}},
	})
	if err != nil {
		t.Fatalf("CreateEvent: %v", err)
	}
	if created.RemoteID != "created-1" || created.UID != "created-1@google.com" {
		t.Errorf("created event = %+v, want the server's ids", created)
	}
	f.mu.Lock()
	ins := f.lastInsert
	f.mu.Unlock()
	if ins.Summary != "Design review" || ins.Location != "Room 2" {
		t.Errorf("insert body = %+v", ins)
	}
	if !reflect.DeepEqual(ins.Recurrence, []string{"RRULE:FREQ=WEEKLY"}) {
		t.Errorf("insert recurrence = %v, want the RRULE line", ins.Recurrence)
	}
	if ins.Start.TimeZone != loc.String() {
		t.Errorf("insert start timezone = %q, want %q", ins.Start.TimeZone, loc)
	}

	f.mu.Lock()
	f.events["ev-9"] = &calendarapi.Event{
		Id: "ev-9", Summary: "Old", Status: statusConfirmed,
		Start: &calendarapi.EventDateTime{DateTime: "2026-09-01T10:00:00Z"},
		End:   &calendarapi.EventDateTime{DateTime: "2026-09-01T11:00:00Z"},
	}
	f.mu.Unlock()

	updated, err := c.UpdateEvent(ctx, &model.Event{
		CalendarRemote: "cal-1", RemoteID: "ev-9", Title: "New title",
	})
	if err != nil {
		t.Fatalf("UpdateEvent: %v", err)
	}
	if updated.Title != "New title" {
		t.Errorf("updated title = %q", updated.Title)
	}
	f.mu.Lock()
	patch := f.lastPatch
	f.mu.Unlock()
	if patch.Summary != "New title" {
		t.Errorf("patch summary = %q", patch.Summary)
	}
	if patch.Start != nil || patch.End != nil || patch.Attendees != nil || patch.Location != "" {
		t.Errorf("patch sent fields that were not set on the model event: %+v", patch)
	}

	if err := c.DeleteEvent(ctx, "cal-1", "ev-9"); err != nil {
		t.Fatalf("DeleteEvent: %v", err)
	}
	// Deleting twice is a no-op, not an error: the outbox may retry.
	if err := c.DeleteEvent(ctx, "cal-1", "ev-9"); err != nil {
		t.Fatalf("DeleteEvent (again): %v", err)
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	if !reflect.DeepEqual(f.deleted, []string{"ev-9"}) {
		t.Errorf("deleted = %v, want [ev-9]", f.deleted)
	}
}

func TestUpdateEventMissing(t *testing.T) {
	f := newFakeCalendar(t)
	c := newCal(t, f)
	_, err := c.UpdateEvent(context.Background(), &model.Event{
		CalendarRemote: "cal-1", RemoteID: "nope", Title: "x",
	})
	if !errors.Is(err, model.ErrNotFound) {
		t.Fatalf("UpdateEvent on a missing event = %v, want model.ErrNotFound", err)
	}
}

func TestOfflineWrapping(t *testing.T) {
	f := newFakeCalendar(t)
	c := newCal(t, f)
	f.srv.Close()

	if _, err := c.Calendars(context.Background()); !errors.Is(err, model.ErrOffline) {
		t.Errorf("Calendars while offline = %v, want model.ErrOffline", err)
	}
	_, err := c.EventChanges(context.Background(), "cal-1", "")
	if !errors.Is(err, model.ErrOffline) || !provider.IsOffline(err) {
		t.Errorf("EventChanges while offline = %v, want model.ErrOffline", err)
	}
}

func TestContextCancellation(t *testing.T) {
	f := newFakeCalendar(t)
	c := newCal(t, f)
	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	if _, err := c.Calendars(ctx); !errors.Is(err, context.Canceled) {
		t.Errorf("Calendars with a cancelled ctx = %v, want context.Canceled", err)
	}
	if _, err := c.EventChanges(ctx, "cal-1", ""); !errors.Is(err, context.Canceled) {
		t.Errorf("EventChanges with a cancelled ctx = %v, want context.Canceled", err)
	}
}

// ---------------------------------------------------------------------------
// sendUpdates

func lastQuery(t *testing.T, qs []url.Values) url.Values {
	t.Helper()
	if len(qs) == 0 {
		t.Fatal("no request was recorded")
	}
	return qs[len(qs)-1]
}

// Guests must be told when they are invited, when the meeting moves and when
// it is called off: every write that touches an event with other attendees
// carries sendUpdates=all.
func TestSendUpdatesOnWrites(t *testing.T) {
	guests := []model.Attendee{
		{Email: "me@example.com", Self: true, Response: model.PartAccepted},
		{Email: "guest@example.com", Response: model.PartNeedsAction},
	}
	start := time.Date(2026, 9, 1, 10, 0, 0, 0, time.UTC)

	t.Run("insert with guests", func(t *testing.T) {
		f := newFakeCalendar(t)
		c := newCal(t, f)
		ev := &model.Event{Title: "Kickoff", Start: start, End: start.Add(time.Hour), Attendees: guests}
		if _, err := c.CreateEvent(context.Background(), "cal-1", ev); err != nil {
			t.Fatalf("CreateEvent: %v", err)
		}
		f.mu.Lock()
		defer f.mu.Unlock()
		if got := lastQuery(t, f.insertQueries).Get("sendUpdates"); got != "all" {
			t.Errorf("events.insert sendUpdates = %q, want all", got)
		}
	})

	t.Run("insert without guests", func(t *testing.T) {
		f := newFakeCalendar(t)
		c := newCal(t, f)
		ev := &model.Event{
			Title: "Gym", Start: start, End: start.Add(time.Hour),
			Attendees: []model.Attendee{{Email: "me@example.com", Self: true}},
		}
		if _, err := c.CreateEvent(context.Background(), "cal-1", ev); err != nil {
			t.Fatalf("CreateEvent: %v", err)
		}
		f.mu.Lock()
		defer f.mu.Unlock()
		if q := lastQuery(t, f.insertQueries); q.Has("sendUpdates") {
			t.Errorf("events.insert sent sendUpdates=%q for a private event, want it omitted",
				q.Get("sendUpdates"))
		}
	})

	t.Run("patch with guests", func(t *testing.T) {
		f := newFakeCalendar(t)
		f.events["ev-1"] = &calendarapi.Event{Id: "ev-1", Summary: "Kickoff", Status: statusConfirmed}
		c := newCal(t, f)
		ev := &model.Event{
			CalendarRemote: "cal-1", RemoteID: "ev-1", Title: "Kickoff (moved)",
			Start: start, End: start.Add(time.Hour), Attendees: guests,
		}
		if _, err := c.UpdateEvent(context.Background(), ev); err != nil {
			t.Fatalf("UpdateEvent: %v", err)
		}
		f.mu.Lock()
		defer f.mu.Unlock()
		if got := lastQuery(t, f.patchQueries).Get("sendUpdates"); got != "all" {
			t.Errorf("events.patch sendUpdates = %q, want all", got)
		}
	})

	t.Run("patch without guests", func(t *testing.T) {
		f := newFakeCalendar(t)
		f.events["ev-1"] = &calendarapi.Event{Id: "ev-1", Summary: "Gym", Status: statusConfirmed}
		c := newCal(t, f)
		ev := &model.Event{CalendarRemote: "cal-1", RemoteID: "ev-1", Title: "Gym (later)"}
		if _, err := c.UpdateEvent(context.Background(), ev); err != nil {
			t.Fatalf("UpdateEvent: %v", err)
		}
		f.mu.Lock()
		defer f.mu.Unlock()
		if q := lastQuery(t, f.patchQueries); q.Has("sendUpdates") {
			t.Errorf("events.patch sent sendUpdates=%q for a private event, want it omitted",
				q.Get("sendUpdates"))
		}
	})

	// DeleteEvent is handed nothing but an id, so it cannot inspect the guest
	// list; it always announces, which is a no-op for a private event.
	t.Run("delete", func(t *testing.T) {
		f := newFakeCalendar(t)
		f.events["ev-1"] = &calendarapi.Event{Id: "ev-1", Status: statusConfirmed}
		c := newCal(t, f)
		if err := c.DeleteEvent(context.Background(), "cal-1", "ev-1"); err != nil {
			t.Fatalf("DeleteEvent: %v", err)
		}
		f.mu.Lock()
		defer f.mu.Unlock()
		if got := lastQuery(t, f.deleteQueries).Get("sendUpdates"); got != "all" {
			t.Errorf("events.delete sendUpdates = %q, want all", got)
		}
	})

	t.Run("respond", func(t *testing.T) {
		f := newFakeCalendar(t)
		f.events["ev-1"] = &calendarapi.Event{
			Id: "ev-1", Summary: "Kickoff", Status: statusConfirmed,
			Attendees: []*calendarapi.EventAttendee{
				{Email: "boss@example.com", ResponseStatus: "accepted"},
				{Email: "me@example.com", Self: true, ResponseStatus: "needsAction"},
			},
		}
		c := newCal(t, f)
		if err := c.Respond(context.Background(), "cal-1", "ev-1", model.PartAccepted); err != nil {
			t.Fatalf("Respond: %v", err)
		}
		f.mu.Lock()
		defer f.mu.Unlock()
		if got := lastQuery(t, f.patchQueries).Get("sendUpdates"); got != "all" {
			t.Errorf("Respond's events.patch sendUpdates = %q, want all", got)
		}
	})
}

// A final page with neither token would otherwise persist an empty sync state,
// quietly turning every later delta into a full listing.
func TestEventChangesRejectsMissingSyncToken(t *testing.T) {
	f := newFakeCalendar(t)
	f.pages = []*calendarapi.Events{{
		TimeZone: "UTC",
		Items: []*calendarapi.Event{{
			Id: "e1", ICalUID: "e1@g", Summary: "One", Status: statusConfirmed,
			Start: &calendarapi.EventDateTime{DateTime: "2026-08-25T10:00:00Z"},
			End:   &calendarapi.EventDateTime{DateTime: "2026-08-25T11:00:00Z"},
		}},
		// no NextPageToken, no NextSyncToken
	}}
	c := newCal(t, f)
	ch, err := c.EventChanges(context.Background(), "cal-1", "token-1")
	if err == nil {
		t.Fatalf("EventChanges returned NewState %q and no error, want an error", ch.NewState)
	}
	if ch != nil {
		t.Errorf("EventChanges returned %+v alongside the error, want nil", ch)
	}
	if !strings.Contains(err.Error(), "nextSyncToken") {
		t.Errorf("error = %v, want it to name the missing nextSyncToken", err)
	}
}

// Google delivers a cancelled instance stripped down to id, status,
// recurringEventId and originalStartTime. It must still map to an event the
// occurrence matcher can place, and must never reach the index with a zero
// (1970) start.
func TestCancelledInstanceWithoutTimes(t *testing.T) {
	f := newFakeCalendar(t)
	f.pages = []*calendarapi.Events{{
		TimeZone:      "Europe/Amsterdam",
		NextSyncToken: "token-1",
		Items: []*calendarapi.Event{{
			Id:                "master_20260827T070000Z",
			Status:            statusCancelled,
			RecurringEventId:  "master",
			OriginalStartTime: &calendarapi.EventDateTime{DateTime: "2026-08-27T09:00:00+02:00", TimeZone: "Europe/Amsterdam"},
		}},
	}}
	c := newCal(t, f)
	ch, err := c.EventChanges(context.Background(), "cal-1", "")
	if err != nil {
		t.Fatalf("EventChanges: %v", err)
	}
	if len(ch.Removed) != 0 {
		t.Errorf("Removed = %v, want none: a cancelled instance is an exception, not a deletion", ch.Removed)
	}
	if len(ch.Upserted) != 1 {
		t.Fatalf("upserted %d events, want 1", len(ch.Upserted))
	}
	ev := ch.Upserted[0]
	if ev.Status != model.StatusCancelled {
		t.Errorf("Status = %q, want cancelled", ev.Status)
	}
	if ev.RecurrenceID != "2026-08-27T09:00:00+02:00" {
		t.Errorf("RecurrenceID = %q, want the original start in RFC3339", ev.RecurrenceID)
	}
	if ev.Start.IsZero() {
		t.Fatal("Start is the zero time; it would be stored as start_utc=0 (1970)")
	}
	if ev.End.IsZero() {
		t.Error("End is the zero time; it would be stored as end_utc=0 (1970)")
	}
	want, err := time.Parse(time.RFC3339, "2026-08-27T09:00:00+02:00")
	if err != nil {
		t.Fatal(err)
	}
	if !ev.Start.Equal(want) {
		t.Errorf("Start = %v, want the original start %v", ev.Start, want)
	}
	if !ev.End.Equal(want) {
		t.Errorf("End = %v, want the original start %v", ev.End, want)
	}
	if ev.Timezone != "Europe/Amsterdam" {
		t.Errorf("Timezone = %q, want the original start's zone", ev.Timezone)
	}
}
