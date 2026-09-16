package sync

import (
	"context"
	"testing"
	"time"

	"github.com/teulaert/emlcalsync/internal/model"
)

// baseTime is a stable anchor inside calendar.DefaultWindow(now).
func baseTime() time.Time {
	return time.Now().UTC().Add(48 * time.Hour).Truncate(time.Hour)
}

func (h *harness) occurrences() []struct {
	Event string
	Start time.Time
	Title string
} {
	h.t.Helper()
	from := time.Now().UTC().AddDate(0, 0, -2)
	to := time.Now().UTC().AddDate(0, 0, 60)
	rows, err := h.st.ListOccurrences(context.Background(), from, to, nil)
	if err != nil {
		h.t.Fatalf("ListOccurrences: %v", err)
	}
	out := make([]struct {
		Event string
		Start time.Time
		Title string
	}, 0, len(rows))
	for _, r := range rows {
		out = append(out, struct {
			Event string
			Start time.Time
			Title string
		}{r.EventRemoteID, r.Start, r.Title})
	}
	return out
}

func TestCalendarFullThenDeltaWithRecurrenceAndException(t *testing.T) {
	h := newHarness(t)
	start := baseTime()

	h.cal.Put("primary", model.Event{
		RemoteID: "r1", UID: "u1", Title: "Standup",
		Start: start, End: start.Add(30 * time.Minute),
		Timezone: "UTC", RRule: "FREQ=DAILY;COUNT=5",
		Status: model.StatusConfirmed,
	})

	rep := h.sync(SyncOptions{Calendar: true})
	if rep.Calendar == nil || rep.Calendar.Added != 1 {
		t.Fatalf("calendar report = %+v", rep.Calendar)
	}
	cals, err := h.st.ListCalendars(context.Background(), []string{"work"})
	if err != nil || len(cals) != 1 {
		t.Fatalf("calendars = %+v, %v", cals, err)
	}
	occ := h.occurrences()
	if len(occ) != 5 {
		t.Fatalf("got %d occurrences, want 5: %+v", len(occ), occ)
	}

	// Move the second instance an hour later, as an exception.
	second := start.Add(24 * time.Hour)
	h.cal.Put("primary", model.Event{
		RemoteID: "r1-ex", UID: "u1", Title: "Standup (moved)",
		RecurrenceID: second.Format(time.RFC3339),
		Start:        second.Add(time.Hour), End: second.Add(90 * time.Minute),
		Timezone: "UTC", Status: model.StatusConfirmed,
	})

	rep = h.sync(SyncOptions{Calendar: true})
	if rep.Calendar.Added != 1 {
		t.Fatalf("delta report = %+v, want the exception only", rep.Calendar)
	}

	occ = h.occurrences()
	if len(occ) != 5 {
		t.Fatalf("got %d occurrences after the exception, want 5: %+v", len(occ), occ)
	}
	var moved, atOriginal int
	for _, o := range occ {
		if o.Start.Equal(second.Add(time.Hour)) && o.Event == "r1-ex" {
			moved++
		}
		if o.Start.Equal(second) {
			atOriginal++
		}
	}
	if moved != 1 {
		t.Fatalf("moved instance not stored under the exception: %+v", occ)
	}
	if atOriginal != 0 {
		t.Fatalf("original slot still expanded: %+v", occ)
	}

	// Deleting the master clears its occurrences.
	h.cal.Drop("primary", "r1")
	rep = h.sync(SyncOptions{Calendar: true})
	if rep.Calendar.Removed != 1 {
		t.Fatalf("removal report = %+v", rep.Calendar)
	}
	for _, o := range h.occurrences() {
		if o.Event == "r1" {
			t.Fatalf("master occurrences survived deletion: %+v", o)
		}
	}
}

func TestCalendarStateExpiredFallsBackToFullListing(t *testing.T) {
	h := newHarness(t)
	start := baseTime()
	h.cal.Put("primary", model.Event{
		RemoteID: "r1", UID: "u1", Title: "Lunch",
		Start: start, End: start.Add(time.Hour), Timezone: "UTC",
		Status: model.StatusConfirmed,
	})
	h.sync(SyncOptions{Calendar: true})

	h.cal.Put("primary", model.Event{
		RemoteID: "r2", UID: "u2", Title: "Dinner",
		Start: start.Add(6 * time.Hour), End: start.Add(7 * time.Hour), Timezone: "UTC",
		Status: model.StatusConfirmed,
	})
	h.cal.InjectStateExpired("primary")

	rep := h.sync(SyncOptions{Calendar: true})
	if rep.Calendar.Kind != KindBackfill {
		t.Fatalf("kind = %q, want a full listing", rep.Calendar.Kind)
	}
	if rep.Calendar.Added != 2 {
		t.Fatalf("added = %d, want both events re-listed", rep.Calendar.Added)
	}
	if got := len(h.occurrences()); got != 2 {
		t.Fatalf("%d occurrences, want 2", got)
	}
}

func TestCalendarSelectionFiltersByName(t *testing.T) {
	h := newHarness(t)
	h.cal.cals = append(h.cal.cals, model.Calendar{RemoteID: "other", Name: "Birthdays"})
	h.cfg.Accounts[0].Calendars = []string{"Primary"}

	h.sync(SyncOptions{Calendar: true})
	cals, err := h.st.ListCalendars(context.Background(), []string{"work"})
	if err != nil {
		t.Fatalf("ListCalendars: %v", err)
	}
	if len(cals) != 1 || cals[0].RemoteID != "primary" {
		t.Fatalf("calendars = %+v, want only the primary", cals)
	}
}

func TestEventOpsRoundTrip(t *testing.T) {
	h := newHarness(t)
	h.sync(SyncOptions{Calendar: true})
	start := baseTime()

	ev := &model.Event{
		UID: "new-1", Title: "Coffee", CalendarRemote: "primary",
		Start: start, End: start.Add(30 * time.Minute), Timezone: "UTC",
		Status: model.StatusConfirmed,
	}
	res, err := h.eng.Apply(context.Background(), "work", Op{
		Kind: OpEventCreate, Event: ev, CalendarRemote: "primary",
	})
	if err != nil {
		t.Fatalf("Apply create: %v", err)
	}
	if res.RemoteID == "" {
		t.Fatalf("create result = %+v", res)
	}
	stored, err := h.st.GetEvent(context.Background(), "work", "primary", res.RemoteID)
	if err != nil {
		t.Fatalf("GetEvent: %v", err)
	}
	if stored.Title != "Coffee" {
		t.Fatalf("stored event = %+v", stored)
	}
	found := false
	for _, o := range h.occurrences() {
		if o.Event == res.RemoteID {
			found = true
		}
	}
	if !found {
		t.Fatal("created event was not expanded into an occurrence")
	}

	// Respond.
	if _, err := h.eng.Apply(context.Background(), "work", Op{
		Kind: OpEventRespond, CalendarRemote: "primary",
		IDs: []string{res.RemoteID}, Response: model.PartAccepted,
	}); err != nil {
		t.Fatalf("Apply respond: %v", err)
	}
	stored, _ = h.st.GetEvent(context.Background(), "work", "primary", res.RemoteID)
	if stored.MyResponse != model.PartAccepted {
		t.Fatalf("response = %q", stored.MyResponse)
	}

	// Delete.
	if _, err := h.eng.Apply(context.Background(), "work", Op{
		Kind: OpEventDelete, CalendarRemote: "primary", IDs: []string{res.RemoteID},
	}); err != nil {
		t.Fatalf("Apply delete: %v", err)
	}
	stored, _ = h.st.GetEvent(context.Background(), "work", "primary", res.RemoteID)
	if stored.DeletedAt == nil {
		t.Fatal("event not marked deleted locally")
	}
	for _, o := range h.occurrences() {
		if o.Event == res.RemoteID {
			t.Fatalf("occurrences survived the delete: %+v", o)
		}
	}
}

func TestReexpandAll(t *testing.T) {
	h := newHarness(t)
	start := baseTime()
	h.cal.Put("primary", model.Event{
		RemoteID: "r1", UID: "u1", Title: "Weekly",
		Start: start, End: start.Add(time.Hour), Timezone: "UTC",
		RRule: "FREQ=WEEKLY;COUNT=3", Status: model.StatusConfirmed,
	})
	h.sync(SyncOptions{Calendar: true})

	// Wipe the expansion and rebuild it.
	ev, err := h.st.GetEvent(context.Background(), "work", "primary", "r1")
	if err != nil {
		t.Fatalf("GetEvent: %v", err)
	}
	if err := h.st.ReplaceOccurrences(context.Background(), ev.ID, nil); err != nil {
		t.Fatalf("ReplaceOccurrences: %v", err)
	}
	if got := len(h.occurrences()); got != 0 {
		t.Fatalf("%d occurrences after the wipe", got)
	}
	if err := h.eng.ReexpandAll(context.Background()); err != nil {
		t.Fatalf("ReexpandAll: %v", err)
	}
	if got := len(h.occurrences()); got != 3 {
		t.Fatalf("%d occurrences after re-expansion, want 3", got)
	}
}

// A recurring event created here has to be expanded before the call returns.
// The series is expanded by UID, and the UID is the provider's -- CalDAV
// mints one when the caller supplies none, Google answers with its own -- so
// a master indexed under the UID we sent has no occurrences at all until some
// later full sync notices.
func TestEventCreateRecurringIsExpandedUnderTheProvidersUID(t *testing.T) {
	h := newHarness(t)
	h.sync(SyncOptions{Calendar: true})
	start := baseTime()

	// No UID: the provider assigns one, which is the case that used to be
	// silently unexpandable.
	ev := &model.Event{
		Title: "Weekly sync", CalendarRemote: "primary",
		Start: start, End: start.Add(30 * time.Minute), Timezone: "UTC",
		Status: model.StatusConfirmed,
		RRule:  "FREQ=WEEKLY;COUNT=3",
	}
	res, err := h.eng.Apply(context.Background(), "work", Op{
		Kind: OpEventCreate, Event: ev, CalendarRemote: "primary",
	})
	if err != nil {
		t.Fatalf("Apply create: %v", err)
	}

	stored, err := h.st.GetEvent(context.Background(), "work", "primary", res.RemoteID)
	if err != nil {
		t.Fatalf("GetEvent: %v", err)
	}
	if stored.UID == "" {
		t.Fatal("the stored master has no UID, so nothing can expand it")
	}
	provider, ok := h.cal.event("primary", res.RemoteID)
	if !ok {
		t.Fatal("the event never reached the provider")
	}
	if stored.UID != provider.UID {
		t.Errorf("index holds uid %q, provider holds %q", stored.UID, provider.UID)
	}
	if stored.RRule != "FREQ=WEEKLY;COUNT=3" {
		t.Errorf("stored rrule = %q", stored.RRule)
	}

	var n int
	for _, o := range h.occurrences() {
		if o.Event == res.RemoteID {
			n++
		}
	}
	if n != 3 {
		t.Fatalf("the new series has %d occurrences, want 3 straight after the create", n)
	}
}

// Clearing the rule on an update collapses the series back to one occurrence,
// in the index as well as on the provider.
func TestEventUpdateClearingRRuleCollapsesTheSeries(t *testing.T) {
	h := newHarness(t)
	h.sync(SyncOptions{Calendar: true})
	start := baseTime()

	ev := &model.Event{
		Title: "Weekly sync", CalendarRemote: "primary",
		Start: start, End: start.Add(30 * time.Minute), Timezone: "UTC",
		Status: model.StatusConfirmed, RRule: "FREQ=WEEKLY;COUNT=3",
	}
	res, err := h.eng.Apply(context.Background(), "work", Op{
		Kind: OpEventCreate, Event: ev, CalendarRemote: "primary",
	})
	if err != nil {
		t.Fatalf("Apply create: %v", err)
	}
	stored, err := h.st.GetEvent(context.Background(), "work", "primary", res.RemoteID)
	if err != nil {
		t.Fatalf("GetEvent: %v", err)
	}

	cleared := *stored
	cleared.RRule = ""
	if _, err := h.eng.Apply(context.Background(), "work", Op{
		Kind: OpEventUpdate, Event: &cleared, CalendarRemote: "primary",
	}); err != nil {
		t.Fatalf("Apply update: %v", err)
	}

	after, err := h.st.GetEvent(context.Background(), "work", "primary", res.RemoteID)
	if err != nil {
		t.Fatalf("GetEvent: %v", err)
	}
	if after.RRule != "" {
		t.Errorf("index still holds rrule %q", after.RRule)
	}
	if got, ok := h.cal.event("primary", res.RemoteID); !ok {
		t.Fatal("the event vanished from the provider")
	} else if got.RRule != "" {
		t.Errorf("the provider still holds rrule %q", got.RRule)
	}
	var n int
	for _, o := range h.occurrences() {
		if o.Event == res.RemoteID {
			n++
		}
	}
	if n != 1 {
		t.Errorf("the cleared event has %d occurrences, want 1", n)
	}
}
