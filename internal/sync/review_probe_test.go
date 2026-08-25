package sync

import (
	"context"
	"errors"
	"os"
	stdsync "sync"
	"testing"

	"github.com/lennert/emlcal/internal/config"
	"github.com/lennert/emlcal/internal/model"
	"github.com/lennert/emlcal/internal/provider"
	"github.com/lennert/emlcal/internal/store"
)

// reviewFail reports a confirmed defect. Assertions only fail the build when
// EMLCAL_REVIEW=1 so `go test ./...` stays green for everyone else.
func reviewFail(t *testing.T, format string, args ...any) {
	t.Helper()
	if os.Getenv("EMLCAL_REVIEW") == "1" {
		t.Errorf(format, args...)
		return
	}
	t.Logf("[review] "+format, args...)
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

// TestReviewRejectedWriteLeavesIndexWrong: DESIGN §7.4 says a rejected write is
// reported to the caller; nothing puts the optimistically patched index back.
func TestReviewRejectedWriteLeavesIndexWrong(t *testing.T) {
	ctx := context.Background()
	h := newHarness(t)
	rej := &rejectMail{fakeMail: h.mail}
	h.fact.mail = rej

	h.mail.Add(&fakeMsg{id: "m1", raw: mailRaw(t, "one", "one"), flags: model.Flags{Unread: true}})
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

	// The index kept the optimistic patch.
	if got := h.message("m1").Flags; got.Flagged || !got.Unread {
		reviewFail(t, "rejected write left the index wrong: local flags = %+v, provider = %+v", got, remote)
	}

	// And a later delta does not repair it: the provider has nothing to report.
	h.sync(SyncOptions{Mail: true})
	if got := h.message("m1").Flags; got.Flagged || !got.Unread {
		reviewFail(t, "a later delta did not repair the index: local flags = %+v, provider = %+v", got, remote)
	}

	// The row is still pending, so `status` shows it and RetryOutbox re-runs it.
	item, err := h.st.GetOutbox(ctx, res.OutboxID)
	if err != nil {
		t.Fatal(err)
	}
	if item.DoneAt == nil {
		reviewFail(t, "rejected outbox row %d is still pending (attempts=%d, err=%q)",
			item.ID, item.Attempts, item.LastError)
	}
}

// TestReviewRejectedSendIsRetried: a permanently rejected send is re-executed
// by every RetryOutbox pass until maxOutboxAttempts.
func TestReviewRejectedSendIsRetried(t *testing.T) {
	ctx := context.Background()
	h := newHarness(t)
	rej := &rejectMail{fakeMail: h.mail}
	h.fact.mail = rej

	if _, err := h.eng.Apply(ctx, "work", Op{Kind: OpSend, Raw: mailRaw(t, "hello", "hi")}); !errors.Is(err, errRejected) {
		t.Fatalf("Apply err = %v, want the rejection", err)
	}
	if rej.sends != 1 {
		t.Fatalf("sends after Apply = %d, want 1", rej.sends)
	}

	// Clear the in-memory backoff before each pass, the way a restarted daemon
	// is: DESIGN says a fresh process retries everything once.
	for i := 0; i < 3; i++ {
		if _, err := h.eng.RetryOutbox(ctx, "work"); err == nil {
			t.Fatalf("RetryOutbox pass %d returned no error", i)
		}
		h.eng.retryMu.Lock()
		for k := range h.eng.retryAt {
			delete(h.eng.retryAt, k)
		}
		h.eng.retryMu.Unlock()
	}
	if rej.sends > 1 {
		reviewFail(t, "a permanently rejected send was executed %d times", rej.sends)
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
		reviewFail(t, "reconcile did not resurrect a message that came back")
	}
	if len(m.MailboxRemotes) == 0 {
		reviewFail(t, "reconcile resurrected m1 but left it in no mailbox: %v", m.MailboxRemotes)
	}
	inbox, err := h.st.ListMessages(ctx, store.MessageFilter{MailboxRole: string(model.RoleInbox)})
	if err != nil {
		t.Fatal(err)
	}
	if len(inbox) != 1 {
		reviewFail(t, "resurrected message is invisible in the inbox listing (%d messages)", len(inbox))
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
		reviewFail(t, "backfill re-enumerated m1 but left deleted_at set; it stays invisible")
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
	h.mail.Add(&fakeMsg{id: "big1", raw: raw, size: int64(len(raw))})
	h.sync(SyncOptions{Mail: true})

	m := h.message("big1")
	if m.RawComplete {
		t.Fatalf("precondition: oversize message was fetched in full during backfill")
	}

	// The provider reports it as added again (a label change on Gmail shows up
	// in history as messagesAdded for that label).
	h.mail.Add(&fakeMsg{id: "big1", raw: raw, size: int64(len(raw))})
	h.sync(SyncOptions{Mail: true})

	if h.message("big1").RawComplete {
		reviewFail(t, "delta downloaded a message above raw_max_size in full because a stub row existed")
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

// TestReviewFetchRawConcurrentCallback must be run with -race.
func TestReviewFetchRawConcurrentCallback(t *testing.T) {
	if os.Getenv("EMLCAL_REVIEW") != "1" {
		t.Skip("review-only: demonstrates an unsynchronised field under -race")
	}
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
		reviewFail(t, "indexed %d messages, want 300", n)
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
