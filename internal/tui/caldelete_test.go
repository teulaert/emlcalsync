package tui

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/teulaert/emlcalsync/internal/model"
	"github.com/teulaert/emlcalsync/internal/provider/fake"
	"github.com/teulaert/emlcalsync/internal/sync"
)

// openCalendar puts one event on the fake calendar, syncs it in and leaves the
// root on the agenda with that row selected.
func openCalendar(t *testing.T, title string) (*root, Deps, *fake.Calendar) {
	t.Helper()
	d, _, cal := newTriageDepsWithCalendar(t)
	start := time.Date(2026, 8, 27, 9, 0, 0, 0, time.UTC)
	cal.Put("primary", model.Event{
		RemoteID: "ev-mine", UID: "uid-mine", Title: title,
		Start: start, End: start.Add(time.Hour), Status: model.StatusConfirmed,
	})
	if _, err := d.Engine.SyncAccount(context.Background(), "work", sync.SyncOptions{}); err != nil {
		t.Fatalf("sync: %v", err)
	}
	r := newTestRoot(t, d)
	send(t, r, "2") // over to the calendar
	a, ok := r.top().(*agenda)
	if !ok {
		t.Fatalf("top screen is %T, want the agenda", r.top())
	}
	if len(a.occs) != 1 {
		t.Fatalf("agenda has %d occurrences, want the one event", len(a.occs))
	}
	return r, d, cal
}

// gone reports that the event is off both the provider and the index, which
// together is what "deleted" has to mean: a delete that only patched the index
// comes back on the next sync.
//
// The index keeps a tombstone rather than dropping the row -- GetEvent still
// answers with it -- so what is asserted there is the deleted_at the agenda
// reads, not the row's absence.
func gone(t *testing.T, d Deps, cal *fake.Calendar) bool {
	t.Helper()
	if len(cal.Events("primary")) != 0 {
		return false
	}
	ev, err := d.Store.GetEvent(context.Background(), "work", "primary", "ev-mine")
	if errors.Is(err, model.ErrNotFound) {
		return true
	}
	if err != nil {
		t.Fatalf("GetEvent: %v", err)
	}
	return ev.DeletedAt != nil
}

func TestDeleteOnTheAgendaDeletesTheEvent(t *testing.T) {
	r, d, cal := openCalendar(t, "Afspraak met Gert")

	send(t, r, "d")

	if !gone(t, d, cal) {
		t.Errorf("after d the event is still there: provider %+v", cal.Events("primary"))
	}
	a, ok := r.top().(*agenda)
	if !ok {
		t.Fatalf("d left %T on top, want the agenda", r.top())
	}
	if len(a.occs) != 0 {
		t.Errorf("the agenda still shows %d occurrences", len(a.occs))
	}
	if !strings.Contains(r.status, "delete") {
		t.Errorf("status = %q, want it to say the event was deleted", r.status)
	}
}

// The mail trash on this key is reversible and says so. A calendar delete is
// not, and must not borrow the offer: pressing z after it would otherwise undo
// whatever mail action came before, which is not what the line promised.
func TestDeletingAnEventOffersNoUndo(t *testing.T) {
	r, _, _ := openCalendar(t, "Afspraak met Gert")

	send(t, r, "d")

	if r.undo != nil {
		t.Errorf("a calendar delete left an undo record: %+v", r.undo)
	}
	if strings.Contains(r.status, "undo") {
		t.Errorf("status = %q, want no offer to undo", r.status)
	}
}

// Deleting what is being read closes the screen too: a detail view of an event
// that no longer exists is only somewhere to press d a second time.
func TestDeleteOnTheEventViewClosesIt(t *testing.T) {
	r, d, cal := openCalendar(t, "Afspraak met Gert")
	send(t, r, "enter")
	ev, ok := r.top().(*eventView)
	if !ok {
		t.Fatalf("enter opened %T, want the event view", r.top())
	}
	if ev.remote != "ev-mine" {
		t.Fatalf("event view is on %q", ev.remote)
	}

	send(t, r, "d")

	if !gone(t, d, cal) {
		t.Errorf("after d the event is still there: provider %+v", cal.Events("primary"))
	}
	a, ok := r.top().(*agenda)
	if !ok {
		t.Fatalf("d left %T on top, want to be back on the agenda", r.top())
	}
	if len(a.occs) != 0 {
		t.Errorf("the agenda still shows %d occurrences", len(a.occs))
	}
}

// d on a day header -- the one row the agenda has that is not an event -- is
// swallowed rather than falling through to the mail trash underneath.
func TestDeleteOnADayHeaderDoesNothing(t *testing.T) {
	r, d, cal := openCalendar(t, "Afspraak met Gert")
	a := r.top().(*agenda)
	a.cursor = 0
	if a.lines[0].header == "" {
		t.Fatalf("line 0 is not a day header: %+v", a.lines[0])
	}

	send(t, r, "d")

	if gone(t, d, cal) {
		t.Error("d on a day header deleted an event")
	}
	if len(r.top().(*agenda).occs) != 1 {
		t.Error("the agenda lost its row")
	}
}

// The mail side of the same key is untouched.
func TestTrashStillTrashesOnTheMailStack(t *testing.T) {
	d, mail, _ := newTriageDepsWithCalendar(t)
	addTriageMessage(t, d, mail, "m-1", "Invoice 4021", time.Hour)
	r := newTestRoot(t, d)
	if got := len(r.mail[0].(*mailList).threads); got != 1 {
		t.Fatalf("list has %d threads, want the one message", got)
	}

	send(t, r, "d")

	if !strings.Contains(r.status, "trash") {
		t.Errorf("status = %q, want the mail trash", r.status)
	}
	if r.undo == nil {
		t.Error("the mail trash stopped offering an undo")
	}
}
