package sync

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/teulaert/emlcalsync/internal/model"
	"github.com/teulaert/emlcalsync/internal/provider"
)

// fastWait shrinks the ride-out backoff so a test that has to go through it
// takes milliseconds instead of the ten minutes a real outage would.
func (h *harness) fastWait() {
	h.eng.waitMin = time.Millisecond
	h.eng.waitMax = 5 * time.Millisecond
}

// seedMail adds n messages named m1..mn to the fake provider.
func (h *harness) seedMail(n int) {
	h.t.Helper()
	for i := 1; i <= n; i++ {
		id := fmt.Sprintf("m%d", i)
		h.mail.Add(&fakeMsg{id: id, raw: mailRaw(h.t, "subject "+id, "body "+id)})
	}
}

// A backfill that loses the network mid-way waits for it and picks up where it
// stopped: same backfill row, same state token, no message fetched twice.
func TestSyncWaitsOutAnOutageDuringBackfill(t *testing.T) {
	h := newHarness(t)
	h.fastWait()
	h.mail.SetPageSize(20)
	h.seedMail(60)

	// The second page of the enumeration finds the network gone.
	h.mail.OnEnumerate(func(call int) {
		if call == 2 {
			h.mail.FailNext(1)
		}
	})

	rep := h.sync(SyncOptions{Mail: true, WaitOffline: time.Minute})
	if rep.Mail.Kind != KindBackfill {
		t.Errorf("kind = %q, want %q (the kind the pass started with)", rep.Mail.Kind, KindBackfill)
	}
	if rep.Mail.Added != 60 {
		t.Errorf("added = %d, want 60", rep.Mail.Added)
	}
	if got := len(h.messages()); got != 60 {
		t.Fatalf("indexed %d messages, want 60", got)
	}

	bf, err := h.st.GetBackfill(context.Background(), "work", resourceMail)
	if err != nil {
		t.Fatalf("GetBackfill: %v", err)
	}
	if !bf.Finished() {
		t.Fatal("backfill not marked finished")
	}
	if bf.Done != 60 {
		t.Errorf("backfill done = %d, want 60", bf.Done)
	}

	// The retry resumed the interrupted backfill instead of starting a new
	// one: the sync log holds a resume, and no second backfill.
	entries, err := h.st.RecentSyncLog(context.Background(), "work", 20)
	if err != nil {
		t.Fatalf("RecentSyncLog: %v", err)
	}
	kinds := map[string]int{}
	for _, e := range entries {
		kinds[e.Kind]++
	}
	if kinds[KindResume] == 0 {
		t.Errorf("no resume in the sync log; kinds = %v", kinds)
	}
	if kinds[KindBackfill] != 1 {
		t.Errorf("backfill entries = %d, want exactly 1; kinds = %v", kinds[KindBackfill], kinds)
	}

	// The wait was visible while it happened.
	var offline []ProgressEvent
	for _, ev := range h.progress() {
		if ev.Phase == PhaseOffline {
			offline = append(offline, ev)
		}
	}
	if len(offline) == 0 {
		t.Fatal("no offline progress event was emitted")
	}
	if !strings.Contains(offline[0].Message, "waiting for network (retry in ") ||
		!strings.Contains(offline[0].Message, "left)") {
		t.Errorf("offline message = %q", offline[0].Message)
	}
}

// The whole budget can go by without the network coming back; then the pass
// fails with the provider's own error, which the CLI turns into exit 4.
func TestSyncGivesUpWhenTheBudgetRunsOut(t *testing.T) {
	h := newHarness(t)
	h.fastWait()
	h.mail.SetPageSize(20)
	h.seedMail(60)
	// The first page lands, then the network goes and stays away.
	h.mail.OnEnumerate(func(call int) {
		if call == 2 {
			h.mail.FailNext(1000)
		}
	})

	started := time.Now()
	_, err := h.eng.SyncAccount(context.Background(), "work",
		SyncOptions{Mail: true, WaitOffline: 30 * time.Millisecond})
	if err == nil {
		t.Fatal("SyncAccount succeeded, want the offline error")
	}
	if !provider.IsOffline(err) {
		t.Fatalf("err = %v, want an offline error", err)
	}
	if took := time.Since(started); took > 30*time.Second {
		t.Errorf("took %s; the budget must bound the wait", took)
	}
	// What did land before the outage is kept, so the next run resumes.
	if got := len(h.messages()); got != 20 {
		t.Errorf("indexed %d messages, want the 20 from the first page", got)
	}
}

// An outage with nothing in flight is not worth waiting for: `emlcal sync` on
// a machine with no network at all still fails fast (DESIGN.md §12), however
// generous the budget is.
func TestSyncFailsFastWhenNothingIsInFlight(t *testing.T) {
	h := newHarness(t)
	h.eng.waitMin, h.eng.waitMax = time.Minute, time.Minute // any wait would hang the test
	h.seedMail(3)
	h.mail.FailNext(1000)

	started := time.Now()
	_, err := h.eng.SyncAccount(context.Background(), "work",
		SyncOptions{Mail: true, WaitOffline: time.Hour})
	if err == nil {
		t.Fatal("SyncAccount succeeded, want the offline error")
	}
	if !provider.IsOffline(err) {
		t.Fatalf("err = %v, want an offline error", err)
	}
	if took := time.Since(started); took > 30*time.Second {
		t.Errorf("took %s; an outage with nothing in flight must not be waited out", took)
	}
	for _, ev := range h.progress() {
		if ev.Phase == PhaseOffline {
			t.Errorf("waited for a network no work depended on: %+v", ev)
		}
	}
}

// A half-finished backfill is in flight even before this pass has done
// anything, so `emlcal sync` after a reboot waits for the network rather than
// abandoning the resume.
func TestSyncWaitsForAnInterruptedBackfill(t *testing.T) {
	h := newHarness(t)
	h.fastWait()
	h.mail.SetPageSize(20)
	h.seedMail(60)
	h.mail.OnEnumerate(func(call int) {
		if call == 2 {
			h.mail.FailNext(1000)
		}
	})
	// First run: one page in, then it gives up with the budget spent.
	if _, err := h.eng.SyncAccount(context.Background(), "work",
		SyncOptions{Mail: true, WaitOffline: 20 * time.Millisecond}); err == nil {
		t.Fatal("the first pass should have failed offline")
	}

	// Second run, still offline at the first call: there is a backfill to
	// finish, so it waits — and finishes when the network returns.
	h.mail.OnEnumerate(nil)
	h.mail.FailNext(1)
	rep, err := h.eng.SyncAccount(context.Background(), "work",
		SyncOptions{Mail: true, WaitOffline: time.Minute})
	if err != nil {
		t.Fatalf("second pass: %v", err)
	}
	if rep.Mail.Kind != KindResume {
		t.Errorf("kind = %q, want %q", rep.Mail.Kind, KindResume)
	}
	if got := len(h.messages()); got != 60 {
		t.Errorf("indexed %d messages, want 60", got)
	}
}

// --wait-offline 0 is the old behaviour: fail on the first transport error,
// without waiting at all.
func TestSyncWithoutWaitFailsImmediately(t *testing.T) {
	h := newHarness(t)
	h.fastWait()
	h.seedMail(3)
	h.mail.FailNext(1)

	started := time.Now()
	_, err := h.eng.SyncAccount(context.Background(), "work", SyncOptions{Mail: true})
	if err == nil {
		t.Fatal("SyncAccount succeeded, want the offline error")
	}
	if !provider.IsOffline(err) {
		t.Fatalf("err = %v, want an offline error", err)
	}
	if took := time.Since(started); took > time.Second {
		t.Errorf("took %s; a zero budget must not wait", took)
	}
	for _, ev := range h.progress() {
		if ev.Phase == PhaseOffline {
			t.Fatalf("emitted an offline wait event with a zero budget: %+v", ev)
		}
	}
	// Nothing was indexed and no state was advanced, so the next run retries.
	if got := len(h.messages()); got != 0 {
		t.Errorf("indexed %d messages after a failed first pass, want 0", got)
	}
}

// A provider rejection is not an outage: it fails at once however long the
// budget is.
func TestSyncDoesNotWaitOutARejection(t *testing.T) {
	h := newHarness(t)
	h.fastWait()
	h.seedMail(3)
	h.mail.FailNextWith(errors.New("fake: 400 invalid request"))

	started := time.Now()
	_, err := h.eng.SyncAccount(context.Background(), "work",
		SyncOptions{Mail: true, WaitOffline: time.Hour})
	if err == nil {
		t.Fatal("SyncAccount succeeded, want the rejection")
	}
	if provider.IsOffline(err) {
		t.Fatalf("err = %v, want a non-transport error", err)
	}
	if took := time.Since(started); took > time.Second {
		t.Errorf("took %s; a rejection must not be waited out", took)
	}
}

// rideOut is the shared helper: the daemon and the one-shot sync both go
// through it, so its contract is worth pinning down on its own.
func TestRideOutRetriesUntilTheBudgetIsSpent(t *testing.T) {
	h := newHarness(t)
	h.fastWait()

	calls := 0
	err := h.eng.rideOut(context.Background(), h.eng.oneShotWait(50*time.Millisecond),
		"work", resourceMail, func(context.Context) error {
			calls++
			return errOffline("mail")
		})
	if err == nil {
		t.Fatal("rideOut returned nil, want the last failure")
	}
	if calls < 2 {
		t.Errorf("attempted %d times, want at least 2", calls)
	}

	calls = 0
	err = h.eng.rideOut(context.Background(), h.eng.oneShotWait(time.Minute),
		"work", resourceMail, func(context.Context) error {
			calls++
			if calls < 3 {
				return errOffline("mail")
			}
			return nil
		})
	if err != nil {
		t.Fatalf("rideOut = %v, want nil once the network is back", err)
	}
	if calls != 3 {
		t.Errorf("attempted %d times, want 3", calls)
	}
}

// A cancelled context ends the wait rather than sleeping out the budget.
func TestRideOutStopsOnContextCancel(t *testing.T) {
	h := newHarness(t)
	h.eng.waitMin, h.eng.waitMax = time.Minute, time.Minute

	ctx, cancel := context.WithCancel(context.Background())
	go func() {
		time.Sleep(10 * time.Millisecond)
		cancel()
	}()
	started := time.Now()
	err := h.eng.rideOut(ctx, h.eng.oneShotWait(time.Hour), "work", resourceMail,
		func(context.Context) error { return errOffline("mail") })
	if err == nil {
		t.Fatal("rideOut returned nil after cancellation")
	}
	if took := time.Since(started); took > 30*time.Second {
		t.Errorf("took %s; cancellation must interrupt the wait", took)
	}
}

// ---------------------------------------------------------------------------
// Per-account toggles

func TestSyncSkipsDisabledMail(t *testing.T) {
	h := newHarness(t)
	h.cfg.Accounts[0].Mail = false
	h.seedMail(3)

	rep := h.sync(SyncOptions{})
	if rep.Mail == nil || rep.Mail.Kind != KindDisabled {
		t.Fatalf("mail report = %+v, want kind %q", rep.Mail, KindDisabled)
	}
	if rep.Mail.Added != 0 {
		t.Errorf("added = %d, want 0", rep.Mail.Added)
	}
	if got := len(h.messages()); got != 0 {
		t.Errorf("indexed %d messages for a mail-disabled account", got)
	}
	if h.state() != "" {
		t.Errorf("mail state = %q, want none", h.state())
	}
	// The calendar half still ran.
	if rep.Calendar == nil {
		t.Error("calendar was not synced")
	}
}

func TestSyncSkipsDisabledCalendar(t *testing.T) {
	h := newHarness(t)
	h.cfg.Accounts[0].Calendar = false
	h.seedMail(2)
	start := baseTime()
	h.cal.Put("primary", model.Event{
		RemoteID: "r1", UID: "u1", Title: "Standup",
		Start: start, End: start.Add(30 * time.Minute),
		Timezone: "UTC", Status: model.StatusConfirmed,
	})

	rep := h.sync(SyncOptions{})
	if rep.Calendar == nil || rep.Calendar.Kind != KindDisabled {
		t.Fatalf("calendar report = %+v, want kind %q", rep.Calendar, KindDisabled)
	}
	if got := len(h.messages()); got != 2 {
		t.Errorf("indexed %d messages, want 2 (mail is still on)", got)
	}
	if occ := h.occurrences(); len(occ) != 0 {
		t.Errorf("stored %d occurrences for a calendar-disabled account", len(occ))
	}
	cals, err := h.st.ListCalendars(context.Background(), []string{"work"})
	if err != nil {
		t.Fatalf("ListCalendars: %v", err)
	}
	if len(cals) != 0 {
		t.Errorf("stored %d calendars for a calendar-disabled account", len(cals))
	}
}

// ---------------------------------------------------------------------------
// Progress

// A backfill reports its size and its speed, not just a counter.
func TestBackfillProgressCarriesTotalAndRate(t *testing.T) {
	h := newHarness(t)
	h.mail.SetPageSize(50)
	h.seedMail(120)

	h.sync(SyncOptions{Mail: true})

	var withTotal, withRate int
	var last ProgressEvent
	for _, ev := range h.progress() {
		if ev.Resource != resourceMail || ev.Phase != KindBackfill {
			continue
		}
		if ev.Total == 120 {
			withTotal++
		}
		if strings.Contains(ev.Message, "/s") {
			withRate++
			last = ev
		}
	}
	if withTotal == 0 {
		t.Fatalf("no progress event carried the total; events = %+v", h.progress())
	}
	if withRate == 0 {
		t.Fatalf("no progress event carried a rate; events = %+v", h.progress())
	}
	if !strings.Contains(last.Message, "/120") {
		t.Errorf("message %q does not show the denominator", last.Message)
	}

	// The hint is persisted, which is what `status` renders a percentage from.
	bf, err := h.st.GetBackfill(context.Background(), "work", resourceMail)
	if err != nil {
		t.Fatalf("GetBackfill: %v", err)
	}
	if bf.TotalHint != 120 {
		t.Errorf("total_hint = %d, want 120", bf.TotalHint)
	}
}

// A provider that cannot count is not a problem: the run just has no total.
func TestBackfillWithoutATotalHint(t *testing.T) {
	h := newHarness(t)
	h.mail.noTotal = true
	h.seedMail(5)

	h.sync(SyncOptions{Mail: true})
	if got := len(h.messages()); got != 5 {
		t.Fatalf("indexed %d messages, want 5", got)
	}
	bf, err := h.st.GetBackfill(context.Background(), "work", resourceMail)
	if err != nil {
		t.Fatalf("GetBackfill: %v", err)
	}
	if bf.TotalHint != 0 {
		t.Errorf("total_hint = %d, want 0", bf.TotalHint)
	}
	for _, ev := range h.progress() {
		if ev.Total != 0 {
			t.Errorf("event carries a total the provider never gave: %+v", ev)
		}
	}
}

func TestHumanCountAndShortDur(t *testing.T) {
	for _, tc := range []struct{ in, want string }{
		{"0", "0"}, {"999", "999"}, {"1234", "1234"}, {"52000", "52000"},
		{"1234567", "1234567"}, {"-1234", "-1234"},
	} {
		var n int
		fmt.Sscanf(tc.in, "%d", &n)
		if got := humanCount(n); got != tc.want {
			t.Errorf("humanCount(%d) = %q, want %q", n, got, tc.want)
		}
	}
	for _, tc := range []struct {
		in   time.Duration
		want string
	}{
		{0, "0s"},
		{30 * time.Second, "30s"},
		{90 * time.Second, "2m"},
		{18 * time.Minute, "18m"},
		{78 * time.Minute, "1h18m"},
		{2 * time.Hour, "2h"},
	} {
		if got := shortDur(tc.in); got != tc.want {
			t.Errorf("shortDur(%s) = %q, want %q", tc.in, got, tc.want)
		}
	}
}
