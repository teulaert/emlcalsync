package sync

import (
	"context"
	"testing"
	"time"

	"github.com/teulaert/emlcalsync/internal/model"
)

func (h *harness) waitWrites(t *testing.T) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if err := h.eng.WaitWrites(ctx); err != nil {
		t.Fatalf("WaitWrites: %v", err)
	}
}

func (h *harness) providerMailboxes(id string) []string {
	h.mail.mu.Lock()
	defer h.mail.mu.Unlock()
	return append([]string(nil), h.mail.msgs[id].mailboxes...)
}

// The point of ApplyLater: the index is patched and the row is durable before
// it returns, with the provider round trip still ahead of it.
func TestApplyLaterReturnsBeforeTheProviderIsTold(t *testing.T) {
	h := newHarness(t)
	h.mail.Add(&fakeMsg{id: "m1", raw: mailRaw(t, "one", "one")})
	h.sync(SyncOptions{Mail: true})

	release := h.mail.Gate()
	res, err := h.eng.ApplyLater(context.Background(), "work",
		Op{Kind: OpArchive, IDs: []string{"m1"}}, nil)
	if err != nil {
		t.Fatalf("ApplyLater: %v", err)
	}
	if res.OutboxID == 0 {
		t.Error("no outbox row, so the write was not made durable before returning")
	}
	if got := h.message("m1").MailboxRemotes; contains(got, "INBOX") || !contains(got, "ARCHIVE") {
		t.Errorf("local mailboxes = %v, want the archive patched in already", got)
	}
	if got := h.providerMailboxes("m1"); !contains(got, "INBOX") {
		t.Errorf("provider mailboxes = %v: the round trip was waited for after all", got)
	}

	release()
	h.waitWrites(t)

	if got := h.providerMailboxes("m1"); contains(got, "INBOX") || !contains(got, "ARCHIVE") {
		t.Errorf("provider mailboxes = %v, want archived once the write landed", got)
	}
	item, err := h.st.GetOutbox(context.Background(), res.OutboxID)
	if err != nil {
		t.Fatalf("GetOutbox: %v", err)
	}
	if item.DoneAt == nil {
		t.Errorf("outbox row not retired: %+v", item)
	}
}

// Two keystrokes about one message must reach the provider in the order they
// were pressed: the second is the answer, and a race would let the first win.
func TestApplyLaterKeepsWritesInOrder(t *testing.T) {
	h := newHarness(t)
	h.mail.Add(&fakeMsg{id: "m1", raw: mailRaw(t, "one", "one"), flags: model.Flags{Unread: true}})
	h.sync(SyncOptions{Mail: true})

	release := h.mail.Gate()
	read := Op{Kind: OpFlags, IDs: []string{"m1"}}
	read.Flags.Clear = model.Flags{Unread: true}
	unread := Op{Kind: OpFlags, IDs: []string{"m1"}}
	unread.Flags.Set = model.Flags{Unread: true}

	for _, op := range []Op{read, unread} {
		if _, err := h.eng.ApplyLater(context.Background(), "work", op, nil); err != nil {
			t.Fatalf("ApplyLater: %v", err)
		}
	}
	release()
	h.waitWrites(t)

	h.mail.mu.Lock()
	got := h.mail.msgs["m1"].flags
	h.mail.mu.Unlock()
	if !got.Unread {
		t.Errorf("provider flags = %+v, want the later mark-unread to have won", got)
	}
}

// A write whose answer the caller needs is not deferred, because the answer is
// the whole reason for making it.
func TestApplyLaterDoesNotDeferASend(t *testing.T) {
	h := newHarness(t)
	res, err := h.eng.ApplyLater(context.Background(), "work",
		Op{Kind: OpSend, Raw: mailRaw(t, "hello", "hello"), Recipients: []string{"a@example.com"}}, nil)
	if err != nil {
		t.Fatalf("ApplyLater: %v", err)
	}
	if res.RemoteID == "" {
		t.Error("a deferred send: there is no id to report and nothing to point the user at")
	}
}

// A rejection arrives after the caller has moved on, so it has to be reported
// through the callback -- and the optimistic patch rolled back with it.
func TestApplyLaterReportsARejection(t *testing.T) {
	h := newHarness(t)
	h.mail.Add(&fakeMsg{id: "m1", raw: mailRaw(t, "one", "one")})
	h.sync(SyncOptions{Mail: true})

	h.mail.FailNextWith(errRejected)
	got := make(chan error, 1)
	if _, err := h.eng.ApplyLater(context.Background(), "work",
		Op{Kind: OpArchive, IDs: []string{"m1"}},
		func(_ *ApplyResult, err error) { got <- err }); err != nil {
		t.Fatalf("ApplyLater: %v", err)
	}
	h.waitWrites(t)

	select {
	case err := <-got:
		if err == nil {
			t.Fatal("a rejected write reported success")
		}
	default:
		t.Fatal("a rejected write reported nothing at all")
	}
	if got := h.message("m1").MailboxRemotes; !contains(got, "INBOX") {
		t.Errorf("local mailboxes = %v, want the patch rolled back", got)
	}
}

// The bug this exists for: a message trashed here, and a sync pass that still
// has the state from before the write -- a pass already in flight, or a
// provider briefly inconsistent about its own mutation -- used to file the
// message back in the inbox, only for the next pass to take it out again. It
// reappeared and then left, which reads as the archive losing its mind.
func TestASyncPassDoesNotUndoAFreshLocalWrite(t *testing.T) {
	h := newHarness(t)
	h.mail.Add(&fakeMsg{id: "m1", raw: mailRaw(t, "one", "one")})
	h.sync(SyncOptions{Mail: true})

	if _, err := h.eng.Apply(context.Background(), "work",
		Op{Kind: OpTrash, IDs: []string{"m1"}}); err != nil {
		t.Fatalf("Apply: %v", err)
	}
	if got := h.message("m1").MailboxRemotes; !contains(got, "TRASH") {
		t.Fatalf("local mailboxes = %v, want trashed", got)
	}

	// The provider answers the next delta with the state from before the
	// write, which is what an in-flight pass or an eventually consistent read
	// amounts to.
	h.mail.Update("m1", model.Flags{}, []string{"INBOX"})
	h.sync(SyncOptions{Mail: true})

	got := h.message("m1").MailboxRemotes
	if contains(got, "INBOX") {
		t.Errorf("mailboxes = %v: a stale sync pass put the message back in the inbox", got)
	}
	if !contains(got, "TRASH") {
		t.Errorf("mailboxes = %v, want it still trashed", got)
	}
}
