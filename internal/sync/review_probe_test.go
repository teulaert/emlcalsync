// Regression tests for the adversarial review in
// docs/reviews/2026-08-25-data-path.md. Every finding that was fixed asserts
// the fixed behaviour outright — these are no longer gated behind
// EMLCAL_REVIEW.
package sync

import (
	"context"
	"errors"
	stdsync "sync"
	"testing"
	"time"

	"github.com/lennert/emlcal/internal/config"
	"github.com/lennert/emlcal/internal/model"
	"github.com/lennert/emlcal/internal/provider"
	"github.com/lennert/emlcal/internal/store"
)

// rejectCal accepts every read but refuses every write, the way a provider
// does for a read-only (subscribed) calendar.
type rejectCal struct{ f *fakeCalendar }

func (c rejectCal) Calendars(ctx context.Context) ([]model.Calendar, error) {
	return c.f.Calendars(ctx)
}

func (c rejectCal) EventChanges(ctx context.Context, calendarRemote, since string) (*provider.EventChanges, error) {
	return c.f.EventChanges(ctx, calendarRemote, since)
}

func (c rejectCal) CreateEvent(ctx context.Context, calendarRemote string, ev *model.Event) (*model.Event, error) {
	return nil, errRejected
}

func (c rejectCal) UpdateEvent(ctx context.Context, ev *model.Event) (*model.Event, error) {
	return nil, errRejected
}

func (c rejectCal) DeleteEvent(ctx context.Context, calendarRemote, remoteID string) error {
	return errRejected
}

func (c rejectCal) Respond(ctx context.Context, calendarRemote, remoteID string, resp model.Participation) error {
	return errRejected
}

// errRejected is a permanent provider rejection (a 400/403), explicitly not an
// offline/transport error.
var errRejected = errors.New("fake: 403 permission denied")

// rejectMail delegates every read to the fake but refuses every write, the way
// a provider does for a label the account may not touch.
type rejectMail struct {
	*fakeMail
	sends int
}

func (r *rejectMail) SetFlags(ctx context.Context, ids []string, set, clear model.Flags) error {
	return errRejected
}

func (r *rejectMail) SetMailboxes(ctx context.Context, ids []string, add, remove []string) error {
	return errRejected
}

func (r *rejectMail) Trash(ctx context.Context, ids []string) error { return errRejected }

func (r *rejectMail) Send(ctx context.Context, raw []byte, threadID string) (string, error) {
	r.sends++
	return "", errRejected
}

// TestReviewRejectedWriteLeavesIndexWrong: a write the provider rejects must
// leave neither an optimistic patch in the index nor a pending outbox row.
// Regression test for H1 + H2.
func TestReviewRejectedWriteLeavesIndexWrong(t *testing.T) {
	ctx := context.Background()
	h := newHarness(t)
	rej := &rejectMail{fakeMail: h.mail}
	h.fact.mail = rej

	h.mail.Add(&fakeMsg{id: "m1", raw: mailRaw(t, "one", "one"), flags: model.Flags{Unread: true}, mailboxes: []string{"INBOX"}})
	h.sync(SyncOptions{Mail: true})

	if got := h.message("m1").Flags; !got.Unread || got.Flagged {
		t.Fatalf("precondition: flags = %+v", got)
	}

	op := Op{Kind: OpFlags, IDs: []string{"m1"}}
	op.Flags.Set = model.Flags{Flagged: true}
	op.Flags.Clear = model.Flags{Unread: true}
	res, err := h.eng.Apply(ctx, "work", op)
	if !errors.Is(err, errRejected) {
		t.Fatalf("Apply err = %v, want the rejection", err)
	}
	if res.Queued {
		t.Fatal("a rejection must not be reported as queued")
	}

	// The provider still has the original flags.
	h.mail.mu.Lock()
	remote := h.mail.msgs["m1"].flags
	h.mail.mu.Unlock()
	if !remote.Unread || remote.Flagged {
		t.Fatalf("fake applied the write it claimed to reject: %+v", remote)
	}

	// ...and so does the index: the optimistic patch was rolled back.
	m := h.message("m1")
	if m.Flags.Flagged || !m.Flags.Unread {
		t.Fatalf("rejected write left the index wrong: local flags = %+v, provider = %+v", m.Flags, remote)
	}
	if len(m.MailboxRemotes) != 1 || m.MailboxRemotes[0] != "INBOX" {
		t.Fatalf("rollback lost the mailbox membership: %v", m.MailboxRemotes)
	}

	// A later delta cannot repair it — the provider has nothing to report —
	// which is exactly why the rollback has to happen at rejection time.
	h.sync(SyncOptions{Mail: true})
	if got := h.message("m1").Flags; got.Flagged || !got.Unread {
		t.Fatalf("index drifted after a later delta: local flags = %+v, provider = %+v", got, remote)
	}

	// The row is retired, not pending: `status` does not show it and
	// RetryOutbox does not re-run it.
	item, err := h.st.GetOutbox(ctx, res.OutboxID)
	if err != nil {
		t.Fatal(err)
	}
	if item.FailedAt == nil {
		t.Fatalf("rejected outbox row %d is still pending (attempts=%d, err=%q)",
			item.ID, item.Attempts, item.LastError)
	}
	if item.DoneAt != nil {
		t.Fatalf("rejected outbox row %d was marked done", item.ID)
	}
	if item.LastError == "" {
		t.Fatal("a retired row must keep the error that retired it")
	}
	pending, err := h.st.ListOutbox(ctx, true)
	if err != nil {
		t.Fatal(err)
	}
	if len(pending) != 0 {
		t.Fatalf("%d rows still pending", len(pending))
	}
	// ...but `outbox list --all` still shows it.
	all, err := h.st.ListOutbox(ctx, false)
	if err != nil {
		t.Fatal(err)
	}
	if len(all) != 1 {
		t.Fatalf("outbox list --all returned %d rows, want 1", len(all))
	}
}

// TestReviewRejectedSendIsRetried: a permanently rejected send is executed
// exactly once, however many retry passes run. Regression test for H2.
func TestReviewRejectedSendIsRetried(t *testing.T) {
	ctx := context.Background()
	h := newHarness(t)
	rej := &rejectMail{fakeMail: h.mail}
	h.fact.mail = rej

	res, err := h.eng.Apply(ctx, "work", Op{Kind: OpSend, Raw: mailRaw(t, "hello", "hi")})
	if !errors.Is(err, errRejected) {
		t.Fatalf("Apply err = %v, want the rejection", err)
	}
	if rej.sends != 1 {
		t.Fatalf("sends after Apply = %d, want 1", rej.sends)
	}
	if res.Queued {
		t.Fatal("a rejected send must not be reported as queued")
	}

	// Clear the in-memory backoff before each pass, the way a restarted daemon
	// is: DESIGN says a fresh process retries everything once.
	for i := 0; i < 3; i++ {
		rep, err := h.eng.RetryOutbox(ctx, "work")
		if err != nil {
			t.Fatalf("RetryOutbox pass %d: %v", i, err)
		}
		if rep.Attempted != 0 {
			t.Fatalf("RetryOutbox pass %d attempted %d writes, want 0", i, rep.Attempted)
		}
		h.eng.retryMu.Lock()
		for k := range h.eng.retryAt {
			delete(h.eng.retryAt, k)
		}
		h.eng.retryMu.Unlock()
	}
	if rej.sends != 1 {
		t.Fatalf("a permanently rejected send was executed %d times", rej.sends)
	}
	item, err := h.st.GetOutbox(ctx, res.OutboxID)
	if err != nil {
		t.Fatal(err)
	}
	if item.FailedAt == nil || item.DoneAt != nil {
		t.Fatalf("rejected send row = %+v, want permanently failed", item)
	}
}

// TestReviewOfflineSendIsNotResent: an offline failure from a send that may
// already have reached the server is retired too — re-running it would submit
// the message twice. Only a failure from before the request (no provider
// client) stays queued.
func TestReviewOfflineSendIsNotResent(t *testing.T) {
	ctx := context.Background()
	h := newHarness(t)
	h.mail.FailNext(1) // an offline error from Send itself
	res, err := h.eng.Apply(ctx, "work", Op{Kind: OpSend, Raw: mailRaw(t, "hello", "hi")})
	if err == nil {
		t.Fatal("Apply returned no error")
	}
	if res.Queued {
		t.Fatal("a send that may have gone out was queued for re-sending")
	}
	item, err := h.st.GetOutbox(ctx, res.OutboxID)
	if err != nil {
		t.Fatal(err)
	}
	if item.FailedAt == nil {
		t.Fatalf("offline send row = %+v, want permanently failed", item)
	}
	rep, err := h.eng.RetryOutbox(ctx, "work")
	if err != nil {
		t.Fatalf("RetryOutbox: %v", err)
	}
	if rep.Attempted != 0 {
		t.Fatalf("RetryOutbox attempted %d writes, want 0", rep.Attempted)
	}
	h.mail.mu.Lock()
	sent := len(h.mail.sentRaw)
	h.mail.mu.Unlock()
	if sent != 0 {
		t.Fatalf("provider saw %d sends", sent)
	}
}

// TestReviewRejectedEventCreateIsRolledBack: a rejected `cal add` must not
// leave its optimistic pending: event in the index.
func TestReviewRejectedEventCreateIsRolledBack(t *testing.T) {
	ctx := context.Background()
	h := newHarness(t)
	// The engine caches provider clients, so the rejecting one has to be in
	// place before the sync that materialises the calendar.
	h.fact.cal = rejectCal{f: h.cal}
	h.sync(SyncOptions{Calendar: true})

	ev := &model.Event{
		CalendarRemote: "primary", Title: "Standup",
		Start: time.Now().Add(time.Hour), End: time.Now().Add(2 * time.Hour),
	}
	res, err := h.eng.Apply(ctx, "work", Op{Kind: OpEventCreate, CalendarRemote: "primary", Event: ev})
	if err == nil {
		t.Fatal("Apply returned no error")
	}
	if res.Queued {
		t.Fatal("a rejected event create must not be queued")
	}
	if ev.RemoteID == "" {
		t.Fatal("expected a placeholder remote id")
	}
	if _, err := h.st.GetEvent(ctx, "work", "primary", ev.RemoteID); !errors.Is(err, model.ErrNotFound) {
		t.Fatalf("the optimistically created event survived the rejection: %v", err)
	}
	item, err := h.st.GetOutbox(ctx, res.OutboxID)
	if err != nil {
		t.Fatal(err)
	}
	if item.FailedAt == nil {
		t.Fatalf("rejected event row = %+v, want permanently failed", item)
	}
}

// TestReviewReconcileUndeleteLosesMailboxes: reconcile resurrects a message
// that reappeared on the server, but on a provider without FetchEnvelopes
// (JMAP/Fastmail) nothing restores the mailbox membership MarkDeleted cleared.
func TestReviewReconcileUndeleteLosesMailboxes(t *testing.T) {
	ctx := context.Background()
	h := newHarness(t)
	h.fact.mail = bareMail{f: h.mail} // no FetchEnvelopes, like the JMAP client

	h.mail.Add(&fakeMsg{id: "m1", raw: mailRaw(t, "one", "one"), mailboxes: []string{"INBOX"}})
	h.sync(SyncOptions{Mail: true})
	if got := h.message("m1").MailboxRemotes; len(got) != 1 || got[0] != "INBOX" {
		t.Fatalf("precondition: mailboxes = %v", got)
	}

	// The message disappears, we notice, then it comes back (a Gmail/JMAP
	// undelete, or a move out of and back into a synced folder).
	h.mail.Remove("m1")
	h.sync(SyncOptions{Mail: true})
	if h.message("m1").DeletedAt == nil {
		t.Fatal("precondition: message was not marked deleted")
	}
	h.mail.Add(&fakeMsg{id: "m1", raw: mailRaw(t, "one", "one"), mailboxes: []string{"INBOX"}})

	// A reconcile is what recovers after ErrStateExpired.
	h.mail.InjectStateExpired()
	rep := h.sync(SyncOptions{Mail: true})
	if rep.Mail.Kind != KindReconcile {
		t.Fatalf("expected a reconcile, got %q", rep.Mail.Kind)
	}

	m := h.message("m1")
	if m.DeletedAt != nil {
		t.Fatal("reconcile did not resurrect a message that came back")
	}
	if len(m.MailboxRemotes) == 0 {
		t.Fatalf("reconcile resurrected m1 but left it in no mailbox: %v", m.MailboxRemotes)
	}
	inbox, err := h.st.ListMessages(ctx, store.MessageFilter{MailboxRole: string(model.RoleInbox)})
	if err != nil {
		t.Fatal(err)
	}
	if len(inbox) != 1 {
		t.Fatalf("resurrected message is invisible in the inbox listing (%d messages)", len(inbox))
	}
}

// TestReviewBackfillDoesNotUndelete: an interrupted backfill that re-enumerates
// a locally-deleted message leaves it deleted.
func TestReviewBackfillDoesNotUndelete(t *testing.T) {
	h := newHarness(t)
	h.mail.Add(&fakeMsg{id: "m1", raw: mailRaw(t, "one", "one")})
	h.sync(SyncOptions{Mail: true})

	// Mark it locally deleted, then run a fresh backfill (state cleared, the
	// way a first run after a crash between SetBackfill and SetState behaves).
	if _, err := h.st.MarkDeleted(context.Background(), "work", []string{"m1"}); err != nil {
		h.t.Fatal(err)
	}
	if err := h.st.ClearState(context.Background(), "work", resourceMail); err != nil {
		h.t.Fatal(err)
	}
	if err := h.st.ClearBackfill(context.Background(), "work", resourceMail); err != nil {
		h.t.Fatal(err)
	}
	rep := h.sync(SyncOptions{Mail: true})
	if rep.Mail.Kind != KindBackfill {
		h.t.Fatalf("expected a backfill, got %q", rep.Mail.Kind)
	}
	if h.message("m1").DeletedAt != nil {
		t.Fatal("backfill re-enumerated m1 but left deleted_at set; it stays invisible")
	}
}

// TestReviewOversizeRefetchedWhenRowExists: a message above raw_max_size is
// downloaded in full whenever the provider re-reports it as added and any row
// already exists.
func TestReviewOversizeRefetchedWhenRowExists(t *testing.T) {
	h := newHarness(t)
	limit := config.Size(1000)
	h.cfg.Accounts[0].RawMaxSize = &limit

	big := make([]byte, 4000)
	for i := range big {
		big[i] = 'x'
	}
	raw := append(mailRaw(t, "big", ""), big...)
	h.mail.Add(&fakeMsg{id: "big1", raw: raw, size: int64(len(raw)), mailboxes: []string{"INBOX"}})
	h.sync(SyncOptions{Mail: true})

	m := h.message("big1")
	if m.RawComplete {
		t.Fatalf("precondition: oversize message was fetched in full during backfill")
	}

	// The provider reports it as added again (a label change on Gmail shows up
	// in history as messagesAdded for that label).
	h.mail.Add(&fakeMsg{id: "big1", raw: raw, size: int64(len(raw)), mailboxes: []string{"INBOX", "WORK"}})
	h.sync(SyncOptions{Mail: true})

	m = h.message("big1")
	if m.RawComplete {
		t.Fatal("delta downloaded a message above raw_max_size in full because a stub row existed")
	}
	// The point of not fetching is that the cheap state is refreshed instead,
	// not that the row is left stale.
	if !contains(m.MailboxRemotes, "WORK") {
		t.Fatalf("the stub's mailboxes were not refreshed: %v", m.MailboxRemotes)
	}
	if !contains(m.MailboxRemotes, "INBOX") {
		t.Fatalf("refreshing the stub dropped a mailbox it was in: %v", m.MailboxRemotes)
	}
}

// gmailish reports "added" the way the Gmail history API does: an id and a
// thread, with no size, flags or labels. The oversize guard cannot read a size
// from that, so the existing stub row is what has to stop the re-fetch.
type gmailish struct{ *fakeMail }

func (g gmailish) Changes(ctx context.Context, since string) (*provider.Changes, error) {
	ch, err := g.fakeMail.Changes(ctx, since)
	if err != nil {
		return nil, err
	}
	for i := range ch.Added {
		ch.Added[i] = provider.Envelope{
			RemoteID: ch.Added[i].RemoteID,
			ThreadID: ch.Added[i].ThreadID,
		}
	}
	return ch, nil
}

// TestReviewOversizeRefetchedWithoutSizeHint is the same finding on a provider
// whose change list carries no size: the stub row must still stop the fetch,
// and the flags/mailboxes come from a cheap envelope fetch instead.
func TestReviewOversizeRefetchedWithoutSizeHint(t *testing.T) {
	h := newHarness(t)
	limit := config.Size(1000)
	h.cfg.Accounts[0].RawMaxSize = &limit
	h.fact.mail = gmailish{fakeMail: h.mail}

	big := make([]byte, 4000)
	for i := range big {
		big[i] = 'x'
	}
	raw := append(mailRaw(t, "big", ""), big...)
	h.mail.Add(&fakeMsg{id: "big1", raw: raw, size: int64(len(raw)), mailboxes: []string{"INBOX"}})
	h.sync(SyncOptions{Mail: true})
	if h.message("big1").RawComplete {
		t.Fatal("precondition: oversize message was fetched in full during backfill")
	}

	h.mail.Add(&fakeMsg{id: "big1", raw: raw, size: int64(len(raw)), mailboxes: []string{"INBOX", "WORK"}})
	h.sync(SyncOptions{Mail: true})

	m := h.message("big1")
	if m.RawComplete {
		t.Fatal("a size-less added record pulled a message above raw_max_size in full")
	}
	if !contains(m.MailboxRemotes, "WORK") || !contains(m.MailboxRemotes, "INBOX") {
		t.Fatalf("the stub's mailboxes were not refreshed: %v", m.MailboxRemotes)
	}
}

var _ = provider.ErrStateExpired

// concurrentMail calls the FetchRaw callback from several goroutines at once.
// provider.MailProvider does not document that fn is called serially, and both
// shipped providers happen to serialise it — this shows what happens if one
// does not.
type concurrentMail struct{ *fakeMail }

func (c concurrentMail) FetchRaw(ctx context.Context, ids []string, fn func(provider.RawMessage) error) error {
	var wg stdsync.WaitGroup
	var errMu stdsync.Mutex
	var first error
	for _, id := range ids {
		c.fakeMail.mu.Lock()
		m := c.fakeMail.msgs[id]
		c.fakeMail.mu.Unlock()
		if m == nil || m.gone {
			continue
		}
		wg.Add(1)
		go func(m *fakeMsg) {
			defer wg.Done()
			err := fn(provider.RawMessage{
				Envelope: provider.Envelope{
					RemoteID: m.id, ThreadID: m.thread, Received: m.received,
					Size: m.size, Flags: m.flags, Mailboxes: m.mailboxes,
				},
				Raw: m.raw,
			})
			if err != nil {
				errMu.Lock()
				if first == nil {
					first = err
				}
				errMu.Unlock()
			}
		}(m)
	}
	wg.Wait()
	return first
}

// TestReviewFetchRawConcurrentCallback drives a provider that breaks the
// (now documented) serial-callback contract; under -race it proves the engine
// state the callback path touches is guarded anyway.
func TestReviewFetchRawConcurrentCallback(t *testing.T) {
	h := newHarness(t)
	h.fact.mail = concurrentMail{fakeMail: h.mail}
	for i := 0; i < 300; i++ {
		h.mail.Add(&fakeMsg{
			id:        "c" + itoa(i),
			raw:       mailRaw(t, "subject "+itoa(i), "body"),
			mailboxes: []string{"INBOX", "UNKNOWN-" + itoa(i%7)},
		})
	}
	h.sync(SyncOptions{Mail: true})
	if n := len(h.messages()); n != 300 {
		t.Fatalf("indexed %d messages, want 300", n)
	}
}

func itoa(n int) string {
	if n == 0 {
		return "0"
	}
	var b []byte
	for n > 0 {
		b = append([]byte{byte('0' + n%10)}, b...)
		n /= 10
	}
	return string(b)
}
