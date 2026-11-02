package sync

import (
	"context"
	"encoding/json"
	"testing"
	"time"

	"github.com/teulaert/emlcalsync/internal/model"
	"github.com/teulaert/emlcalsync/internal/store"
)

func TestApplyOnlineExecutesImmediately(t *testing.T) {
	h := newHarness(t)
	h.mail.Add(&fakeMsg{id: "m1", raw: mailRaw(t, "one", "one"), flags: model.Flags{Unread: true}})
	h.sync(SyncOptions{Mail: true})

	op := Op{Kind: OpFlags, IDs: []string{"m1"}}
	op.Flags.Clear = model.Flags{Unread: true}
	op.Flags.Set = model.Flags{Flagged: true}

	res, err := h.eng.Apply(context.Background(), "work", op)
	if err != nil {
		t.Fatalf("Apply: %v", err)
	}
	if res.Queued {
		t.Fatal("Apply queued a write while the provider was reachable")
	}

	item, err := h.st.GetOutbox(context.Background(), res.OutboxID)
	if err != nil {
		t.Fatalf("GetOutbox: %v", err)
	}
	if item.DoneAt == nil {
		t.Fatalf("outbox row not marked done: %+v", item)
	}
	if got := h.message("m1").Flags; got.Unread || !got.Flagged {
		t.Fatalf("local flags = %+v, want read + flagged", got)
	}
	h.mail.mu.Lock()
	remote := h.mail.msgs["m1"].flags
	h.mail.mu.Unlock()
	if remote.Unread || !remote.Flagged {
		t.Fatalf("provider flags = %+v", remote)
	}
}

func TestApplyOfflineQueuesAndRetryDrains(t *testing.T) {
	h := newHarness(t)
	h.mail.Add(&fakeMsg{id: "m1", raw: mailRaw(t, "one", "one")})
	h.sync(SyncOptions{Mail: true})

	h.mail.FailNext(1)
	res, err := h.eng.Apply(context.Background(), "work", Op{Kind: OpArchive, IDs: []string{"m1"}})
	if err != nil {
		t.Fatalf("Apply: %v", err)
	}
	if !res.Queued {
		t.Fatal("offline write was not queued")
	}

	// The optimistic patch is visible immediately.
	got := h.message("m1").MailboxRemotes
	if contains(got, "INBOX") || !contains(got, "ARCHIVE") {
		t.Fatalf("local mailboxes = %v, want archived", got)
	}
	// ...and the provider has not been told yet.
	h.mail.mu.Lock()
	remote := append([]string(nil), h.mail.msgs["m1"].mailboxes...)
	h.mail.mu.Unlock()
	if !contains(remote, "INBOX") {
		t.Fatalf("provider mailboxes = %v, want the write to still be pending", remote)
	}

	pending, err := h.st.ListOutbox(context.Background(), true)
	if err != nil {
		t.Fatalf("ListOutbox: %v", err)
	}
	if len(pending) != 1 {
		t.Fatalf("%d pending rows, want 1", len(pending))
	}

	rep, err := h.eng.RetryOutbox(context.Background(), "")
	if err != nil {
		t.Fatalf("RetryOutbox: %v", err)
	}
	if rep.Done != 1 {
		t.Fatalf("retry report = %+v, want one done", rep)
	}
	h.mail.mu.Lock()
	remote = append([]string(nil), h.mail.msgs["m1"].mailboxes...)
	h.mail.mu.Unlock()
	if contains(remote, "INBOX") || !contains(remote, "ARCHIVE") {
		t.Fatalf("provider mailboxes = %v, want archived", remote)
	}
	pending, _ = h.st.ListOutbox(context.Background(), true)
	if len(pending) != 0 {
		t.Fatalf("%d rows still pending", len(pending))
	}
}

func TestApplyProviderRejectionIsNotQueued(t *testing.T) {
	h := newHarness(t)
	h.sync(SyncOptions{Mail: true})

	// An unknown op kind never reaches the provider; use a real rejection
	// instead: responding to an event that does not exist.
	_, err := h.eng.Apply(context.Background(), "work", Op{
		Kind:           OpEventRespond,
		CalendarRemote: "primary",
		IDs:            []string{"nope"},
		Response:       model.PartAccepted,
	})
	if err == nil {
		t.Fatal("expected the provider rejection to surface")
	}
	items, err := h.st.ListOutbox(context.Background(), false)
	if err != nil {
		t.Fatalf("ListOutbox: %v", err)
	}
	if len(items) != 1 || items[0].DoneAt != nil || items[0].Attempts == 0 {
		t.Fatalf("outbox = %+v, want one failed row", items)
	}
}

func TestApplySendAndDraft(t *testing.T) {
	h := newHarness(t)
	h.sync(SyncOptions{Mail: true})

	raw := mailRaw(t, "hello", "hi there")
	res, err := h.eng.Apply(context.Background(), "work", Op{Kind: OpSend, Raw: raw, ThreadID: "t1"})
	if err != nil {
		t.Fatalf("Apply send: %v", err)
	}
	if res.RemoteID == "" || res.Queued {
		t.Fatalf("send result = %+v", res)
	}
	res, err = h.eng.Apply(context.Background(), "work", Op{Kind: OpDraft, Raw: raw})
	if err != nil {
		t.Fatalf("Apply draft: %v", err)
	}
	if res.RemoteID == "" {
		t.Fatalf("draft result = %+v", res)
	}
	h.mail.mu.Lock()
	defer h.mail.mu.Unlock()
	if len(h.mail.sentRaw) != 1 || len(h.mail.draftRaw) != 1 {
		t.Fatalf("provider saw %d sends, %d drafts", len(h.mail.sentRaw), len(h.mail.draftRaw))
	}
}

func TestRetryOutboxBacksOff(t *testing.T) {
	h := newHarness(t)
	h.mail.Add(&fakeMsg{id: "m1", raw: mailRaw(t, "one", "one")})
	h.sync(SyncOptions{Mail: true})

	h.mail.FailNext(2) // the Apply and the first retry both fail
	if _, err := h.eng.Apply(context.Background(), "work", Op{Kind: OpTrash, IDs: []string{"m1"}}); err != nil {
		t.Fatalf("Apply: %v", err)
	}
	rep, err := h.eng.RetryOutbox(context.Background(), "work")
	if err != nil {
		t.Fatalf("RetryOutbox: %v", err)
	}
	if rep.Failed != 1 {
		t.Fatalf("report = %+v, want one failure", rep)
	}
	// The failed row is now in backoff and must be skipped, not hammered.
	rep, err = h.eng.RetryOutbox(context.Background(), "work")
	if err != nil {
		t.Fatalf("RetryOutbox: %v", err)
	}
	if rep.Attempted != 0 || rep.Skipped != 1 {
		t.Fatalf("report = %+v, want the row skipped by the backoff", rep)
	}

	if d := backoff(1); d != time.Minute {
		t.Fatalf("backoff(1) = %v", d)
	}
	if d := backoff(3); d != 4*time.Minute {
		t.Fatalf("backoff(3) = %v", d)
	}
	if d := backoff(20); d != time.Hour {
		t.Fatalf("backoff(20) = %v, want the cap", d)
	}
}

// Gmail's messages.trash only swaps INBOX for TRASH; the message keeps every
// other label. The local patch used to clear them all, which the provider
// never agreed with, so the labels stayed gone on the machine that trashed.
func TestTrashOnGmailKeepsOtherLabelsLocally(t *testing.T) {
	h := newHarness(t)
	h.mail.Add(&fakeMsg{id: "m1", raw: mailRaw(t, "one", "one"), mailboxes: []string{"INBOX", "WORK"}})
	h.sync(SyncOptions{Mail: true})

	if _, err := h.eng.Apply(context.Background(), "work", Op{Kind: OpTrash, IDs: []string{"m1"}}); err != nil {
		t.Fatalf("Apply: %v", err)
	}
	got := h.message("m1").MailboxRemotes
	if !contains(got, "TRASH") || !contains(got, "WORK") || contains(got, "INBOX") {
		t.Fatalf("mailboxes = %v, want TRASH and WORK", got)
	}
	// The patch is what the provider ends up with, so the next pass agrees.
	h.sync(SyncOptions{Mail: true})
	if got := h.message("m1").MailboxRemotes; !contains(got, "TRASH") || !contains(got, "WORK") || len(got) != 2 {
		t.Fatalf("after sync, mailboxes = %v, want [TRASH WORK]", got)
	}
}

// A folder store moves a trashed message: it ends up in the trash only.
func TestTrashOnAFolderStoreClearsOtherMailboxesLocally(t *testing.T) {
	h := newHarness(t)
	h.cfg.Accounts[0].Mail.Backend = model.BackendJMAP
	h.mail.Add(&fakeMsg{id: "m1", raw: mailRaw(t, "one", "one"), mailboxes: []string{"INBOX", "WORK"}})
	h.sync(SyncOptions{Mail: true})

	if _, err := h.eng.Apply(context.Background(), "work", Op{Kind: OpTrash, IDs: []string{"m1"}}); err != nil {
		t.Fatalf("Apply: %v", err)
	}
	if got := h.message("m1").MailboxRemotes; len(got) != 1 || got[0] != "TRASH" {
		t.Fatalf("mailboxes = %v, want [TRASH]", got)
	}
}

// ageFences moves every local-write fence an hour back, well past the
// store's localPatchWindow.
func ageFences(t *testing.T, h *harness) {
	t.Helper()
	if _, err := h.st.DB().ExecContext(context.Background(),
		`UPDATE messages SET local_patch_at = local_patch_at - 3600 WHERE local_patch_at IS NOT NULL`); err != nil {
		t.Fatalf("age fences: %v", err)
	}
}

// A write the outbox gives up on leaves its optimistic patch behind, and the
// provider, which never changed, has nothing to report about the message. The
// delta re-reads such a row once its fence has expired.
func TestDeltaRereadsAPatchTheProviderNeverConfirmed(t *testing.T) {
	h := newHarness(t)
	h.mail.Add(&fakeMsg{id: "m1", raw: mailRaw(t, "one", "one"), flags: model.Flags{Unread: true}})
	h.sync(SyncOptions{Mail: true})

	h.mail.FailNext(1)
	op := Op{Kind: OpFlags, IDs: []string{"m1"}}
	op.Flags.Clear = model.Flags{Unread: true}
	res, err := h.eng.Apply(context.Background(), "work", op)
	if err != nil || !res.Queued {
		t.Fatalf("Apply = %+v, %v; want queued", res, err)
	}
	// Exhaust the retry budget: the next pass retires the row.
	if _, err := h.st.DB().ExecContext(context.Background(),
		`UPDATE outbox SET attempts = ?`, maxOutboxAttempts); err != nil {
		t.Fatalf("exhaust attempts: %v", err)
	}
	if _, err := h.eng.RetryOutbox(context.Background(), "work"); err != nil {
		t.Fatalf("RetryOutbox: %v", err)
	}
	if h.message("m1").Flags.Unread {
		t.Fatal("precondition: the patch should still say read")
	}

	// Inside the window the fence still stands.
	h.sync(SyncOptions{Mail: true})
	if h.message("m1").Flags.Unread {
		t.Fatal("a live fence was overridden")
	}

	ageFences(t, h)
	h.sync(SyncOptions{Mail: true})
	if !h.message("m1").Flags.Unread {
		t.Fatal("the stale patch survived the delta; want the provider's unread back")
	}
	stale, err := h.st.StaleLocalPatches(context.Background(), "work", 10)
	if err != nil || len(stale) != 0 {
		t.Fatalf("StaleLocalPatches = %v, %v; want none left", stale, err)
	}
}

// A write that is still waiting in the outbox is what the user asked for; an
// expired fence must not hand the row back to the provider's older state.
func TestDeltaLeavesAQueuedWritesPatchAlone(t *testing.T) {
	h := newHarness(t)
	h.mail.Add(&fakeMsg{id: "m1", raw: mailRaw(t, "one", "one")})
	h.sync(SyncOptions{Mail: true})

	h.mail.FailNext(1)
	res, err := h.eng.Apply(context.Background(), "work", Op{Kind: OpArchive, IDs: []string{"m1"}})
	if err != nil || !res.Queued {
		t.Fatalf("Apply = %+v, %v; want queued", res, err)
	}
	ageFences(t, h)
	h.sync(SyncOptions{Mail: true})
	if got := h.message("m1").MailboxRemotes; contains(got, "INBOX") {
		t.Fatalf("mailboxes = %v; the queued archive was undone", got)
	}
}

// Moving a message to a mailbox it is already in produced add INBOX + remove
// INBOX, which Gmail rejects; the removal is dropped before anything is sent.
func TestMailboxesOpDropsARemovalItAlsoAdds(t *testing.T) {
	h := newHarness(t)
	h.mail.Add(&fakeMsg{id: "m1", raw: mailRaw(t, "one", "one"), mailboxes: []string{"INBOX"}})
	h.sync(SyncOptions{Mail: true})

	res, err := h.eng.Apply(context.Background(), "work", Op{Kind: OpMailboxes, IDs: []string{"m1"},
		AddMailboxes: []string{"INBOX"}, RemoveMailboxes: []string{"INBOX", "WORK"}})
	if err != nil {
		t.Fatalf("Apply: %v", err)
	}
	item, err := h.st.GetOutbox(context.Background(), res.OutboxID)
	if err != nil {
		t.Fatalf("GetOutbox: %v", err)
	}
	var stored Op
	if err := json.Unmarshal(item.Payload, &stored); err != nil {
		t.Fatalf("payload: %v", err)
	}
	if len(stored.RemoveMailboxes) != 1 || stored.RemoveMailboxes[0] != "WORK" {
		t.Fatalf("stored removals = %v, want [WORK]", stored.RemoveMailboxes)
	}
	if got := h.message("m1").MailboxRemotes; !contains(got, "INBOX") {
		t.Fatalf("mailboxes = %v, want INBOX kept", got)
	}
}

// Restore is the inverse of archive/trash: unlike OpTrash it does not clear
// every other membership, so a mailbox the message picked up along the way
// (WORK here) survives the round trip.
func TestRestorePutsItBackInTheInboxWithoutClearingOtherMailboxes(t *testing.T) {
	h := newHarness(t)
	h.mail.Add(&fakeMsg{id: "m1", raw: mailRaw(t, "one", "one"), mailboxes: []string{"TRASH", "WORK"}})
	h.sync(SyncOptions{Mail: true})

	if _, err := h.eng.Apply(context.Background(), "work", Op{Kind: OpRestore, IDs: []string{"m1"}}); err != nil {
		t.Fatalf("Apply: %v", err)
	}
	got := h.message("m1").MailboxRemotes
	if !contains(got, "INBOX") {
		t.Errorf("mailboxes = %v, want INBOX", got)
	}
	if contains(got, "TRASH") {
		t.Errorf("mailboxes = %v, want TRASH gone", got)
	}
	if !contains(got, "WORK") {
		t.Errorf("mailboxes = %v, want WORK kept", got)
	}
}

// The row a process is pushing is pending in the table, and the daemon's
// outbox pass runs in another process with no memory of who is doing what.
// Before the lease, that pass executed the row as well -- which is how one
// message went out twice on 2026-10-02 -- so here the daemon is a second
// engine over the same store, and it has to find the row taken.
func TestOutboxPassLeavesAnInFlightSendAlone(t *testing.T) {
	h := newHarness(t)
	daemon := h.newEngine()
	release := h.mail.Gate()

	type outcome struct {
		res *ApplyResult
		err error
	}
	done := make(chan outcome, 1)
	go func() {
		res, err := h.eng.Apply(context.Background(), "work",
			Op{Kind: OpSend, Raw: mailRaw(t, "once", "only once")})
		done <- outcome{res, err}
	}()

	// Wait for the row to be committed; the push behind it is held at the gate.
	var row store.OutboxItem
	deadline := time.Now().Add(5 * time.Second)
	for {
		items, err := h.st.ListOutbox(context.Background(), true)
		if err != nil {
			t.Fatalf("ListOutbox: %v", err)
		}
		if len(items) == 1 {
			row = items[0]
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("the send never reached the outbox")
		}
		time.Sleep(10 * time.Millisecond)
	}
	if !row.InFlight(time.Now()) {
		t.Fatalf("row = %+v, want it leased by the process pushing it", row)
	}

	rep, err := daemon.RetryOutbox(context.Background(), "")
	if err != nil {
		t.Fatalf("RetryOutbox: %v", err)
	}
	if rep.Attempted != 0 || rep.Skipped != 1 {
		t.Fatalf("daemon pass = %+v, want the in-flight row skipped, not executed", rep)
	}

	release()
	out := <-done
	if out.err != nil {
		t.Fatalf("Apply: %v", out.err)
	}
	if n := len(h.mail.Sent()); n != 1 {
		t.Fatalf("the provider received %d sends, want exactly 1", n)
	}
	it, err := h.st.GetOutbox(context.Background(), out.res.OutboxID)
	if err != nil {
		t.Fatal(err)
	}
	if it.DoneAt == nil {
		t.Fatalf("row = %+v, want done", it)
	}
}

// A lease that has run out belongs to nobody: the holder crashed, or its
// context ran out inside the lease, as leaseCtx guarantees. The outbox pass
// then takes the row over, which is the crash-safety the outbox exists for.
func TestOutboxPassTakesOverAnExpiredLease(t *testing.T) {
	h := newHarness(t)
	ctx := context.Background()
	id, err := h.st.EnqueueOutbox(ctx, "work", string(OpSend),
		mustJSON(t, Op{Kind: OpSend, Raw: mailRaw(t, "late", "picked up later")}))
	if err != nil {
		t.Fatal(err)
	}
	if ok, err := h.st.LeaseOutbox(ctx, id, time.Now().Add(-time.Second)); err != nil || !ok {
		t.Fatalf("LeaseOutbox = %v, %v", ok, err)
	}

	rep, err := h.eng.RetryOutbox(ctx, "")
	if err != nil {
		t.Fatalf("RetryOutbox: %v", err)
	}
	if rep.Done != 1 {
		t.Fatalf("pass = %+v, want the expired row executed", rep)
	}
	if n := len(h.mail.Sent()); n != 1 {
		t.Fatalf("the provider received %d sends, want 1", n)
	}
}

func mustJSON(t *testing.T, v any) []byte {
	t.Helper()
	b, err := json.Marshal(v)
	if err != nil {
		t.Fatal(err)
	}
	return b
}
