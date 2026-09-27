package sync

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/teulaert/emlcalsync/internal/model"
	"github.com/teulaert/emlcalsync/internal/store"
)

// stale is a check row well in the past, planted so a test can tell "the pass
// moved it" from "it was already about now". The engine stamps time.Now(),
// and a test cannot wait a second between two syncs to see that advance.
var stale = time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)

func (h *harness) plantCheck(resource string) {
	h.t.Helper()
	if err := h.st.RecordSyncCheck(context.Background(), "work", resource, stale, nil); err != nil {
		h.t.Fatal(err)
	}
}

func (h *harness) check(resource string) *store.SyncCheck {
	h.t.Helper()
	c, err := h.st.GetSyncCheck(context.Background(), "work", resource)
	if err != nil {
		h.t.Fatal(err)
	}
	return c
}

func (h *harness) lastChange(resource string) time.Time {
	h.t.Helper()
	e, err := h.st.LastSyncChange(context.Background(), "work", resource)
	if err != nil {
		h.t.Fatal(err)
	}
	if e == nil {
		return time.Time{}
	}
	return e.Finished
}

// The complaint behind this test: a watch service that polls for hours and
// finds nothing looked, in `emlcal status`, exactly like one that had
// stopped, because only a pass that changed something was ever recorded.
func TestNoOpDeltaMovesTheCheckButNotTheChange(t *testing.T) {
	h := newHarness(t)
	h.seedMail(2)
	ctx := context.Background()
	if _, err := h.eng.SyncAccount(ctx, "work", SyncOptions{Mail: true}); err != nil {
		t.Fatal(err)
	}
	changed := h.lastChange(resourceMail)
	if changed.IsZero() {
		t.Fatal("the backfill was not logged as a change")
	}
	h.plantCheck(resourceMail)

	rep, err := h.eng.SyncAccount(ctx, "work", SyncOptions{Mail: true})
	if err != nil {
		t.Fatal(err)
	}
	if n := rep.Mail.Added + rep.Mail.Updated + rep.Mail.Removed; n != 0 {
		t.Fatalf("second pass applied %d changes, want a no-op", n)
	}

	c := h.check(resourceMail)
	if c == nil || !c.CheckedAt.After(stale) {
		t.Errorf("a no-op delta did not move checked_at: %+v", c)
	}
	if c != nil && c.Failed() {
		t.Errorf("a no-op delta was recorded as a failure: %+v", c)
	}
	if got := h.lastChange(resourceMail); !got.Equal(changed) {
		t.Errorf("a no-op delta moved the last change from %s to %s", changed, got)
	}
}

// One resource's pulse must not be taken for the other's: an event landing
// on the calendar says nothing about whether the mailbox was looked at.
func TestCalendarPassLeavesTheMailCheckAlone(t *testing.T) {
	h := newHarness(t)
	h.plantCheck(resourceMail)
	start := time.Date(2026, 9, 1, 9, 0, 0, 0, time.UTC)
	h.cal.Put("primary", model.Event{
		RemoteID: "e1", UID: "uid-1", Title: "Standup", Start: start, End: start.Add(time.Hour),
	})

	if _, err := h.eng.SyncAccount(context.Background(), "work", SyncOptions{Calendar: true}); err != nil {
		t.Fatal(err)
	}

	if c := h.check(resourceMail); c == nil || !c.CheckedAt.Equal(stale) {
		t.Errorf("a calendar pass moved the mail check: %+v", c)
	}
	if c := h.check(resourceCalendar); c == nil || !c.CheckedAt.After(stale) || c.Failed() {
		t.Errorf("the calendar pass was not recorded as a check: %+v", c)
	}
	if h.lastChange(resourceCalendar).IsZero() {
		t.Error("an added event was not logged as a calendar change")
	}
	if !h.lastChange(resourceMail).IsZero() {
		t.Error("a calendar change turned up as a mail change")
	}
}

// A pass that failed is not a check. The last good check stays on record and
// the failure is recorded beside it, so status can say both "last known good
// at" and "failing since".
func TestFailedPassIsRecordedApartFromTheLastGoodCheck(t *testing.T) {
	h := newHarness(t)
	h.fastWait()
	h.seedMail(1)
	h.plantCheck(resourceMail)
	h.mail.FailNextWith(errors.New("fake: 400 invalid request"))

	_, err := h.eng.SyncAccount(context.Background(), "work", SyncOptions{Mail: true})
	if err == nil {
		t.Fatal("SyncAccount succeeded, want the rejection")
	}

	c := h.check(resourceMail)
	if c == nil {
		t.Fatal("no check row after a failed pass")
	}
	if !c.CheckedAt.Equal(stale) {
		t.Errorf("a failed pass moved checked_at to %s", c.CheckedAt)
	}
	if !c.AttemptedAt.After(stale) || !c.Failed() || !strings.Contains(c.Error, "fake: 400 invalid request") {
		t.Errorf("the failure was not recorded: %+v", c)
	}

	// And the next good pass clears it.
	if _, err := h.eng.SyncAccount(context.Background(), "work", SyncOptions{Mail: true}); err != nil {
		t.Fatal(err)
	}
	if c := h.check(resourceMail); c.Failed() || !c.CheckedAt.After(stale) {
		t.Errorf("a good pass after a failure left: %+v", c)
	}
}

// Shutting the daemon down mid-pass is not the provider failing: an
// interrupted pass moves nothing, in either direction.
func TestInterruptedPassRecordsNothing(t *testing.T) {
	h := newHarness(t)
	h.seedMail(1)
	h.plantCheck(resourceMail)
	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	_, err := h.eng.SyncAccount(ctx, "work", SyncOptions{Mail: true})
	if err == nil {
		t.Fatal("SyncAccount under a cancelled context succeeded")
	}
	if c := h.check(resourceMail); c == nil || !c.CheckedAt.Equal(stale) || !c.AttemptedAt.Equal(stale) || c.Failed() {
		t.Errorf("a cancelled pass touched the check row: %+v", c)
	}
}
