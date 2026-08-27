package store

import (
	"context"
	"testing"

	"github.com/teulaert/emlcalsync/internal/model"
)

// threadFixture is a minimal indexable message; the threading tests only care
// about the Message-ID graph.
func threadFixture(acct, remote string) *model.Message {
	return &model.Message{
		AccountID: acct, RemoteID: remote,
		Subject: remote, Date: base, Received: base,
		From: addr("Someone", "someone@example.com"),
	}
}

// putThreaded indexes a message with no provider thread id, so the store has to
// stitch the thread from the Message-ID graph the way it does for IMAP.
func putThreaded(t *testing.T, s *Store, acct, remote, msgID, inReplyTo string, refs []string) string {
	t.Helper()
	ctx := context.Background()
	m := threadFixture(acct, remote)
	m.ThreadID = "" // the IMAP case: the provider supplies none
	m.MessageIDHeader = msgID
	m.InReplyTo = inReplyTo
	m.References = refs
	if _, err := s.UpsertMessage(ctx, m, nil); err != nil {
		t.Fatalf("UpsertMessage %s: %v", remote, err)
	}
	got, err := s.GetMessage(ctx, acct, remote)
	if err != nil {
		t.Fatalf("GetMessage %s: %v", remote, err)
	}
	return got.ThreadID
}

func TestThreadingJoinsAReplyToItsParent(t *testing.T) {
	s := newTestStore(t)
	seedAccount(t, s, "a")

	parent := putThreaded(t, s, "a", "r1", "root@x", "", nil)
	reply := putThreaded(t, s, "a", "r2", "reply@x", "root@x", []string{"root@x"})

	if parent != reply {
		t.Fatalf("reply landed in %q, parent in %q; want one thread", reply, parent)
	}
}

// The order messages arrive in is not ours to choose: folders are enumerated
// separately and a reply is frequently indexed before the message it answers.
// Both orders must converge on the same thread id.
func TestThreadingSurvivesOutOfOrderArrival(t *testing.T) {
	s := newTestStore(t)
	seedAccount(t, s, "a")

	reply := putThreaded(t, s, "a", "r1", "reply@x", "root@x", []string{"root@x"})
	parent := putThreaded(t, s, "a", "r2", "root@x", "", nil)

	if parent != reply {
		t.Fatalf("parent landed in %q, reply in %q; want one thread", parent, reply)
	}
}

// A message can prove that two threads were always one conversation. The
// hash-the-root approach cannot do this; the ref graph can.
func TestThreadingMergesTwoThreads(t *testing.T) {
	s := newTestStore(t)
	ctx := context.Background()
	seedAccount(t, s, "a")

	// A is the root. C names B, which nobody has seen, so it starts alone.
	a := putThreaded(t, s, "a", "rA", "a@x", "", nil)
	c := putThreaded(t, s, "a", "rC", "c@x", "b@x", []string{"b@x"})
	if a == c {
		t.Fatalf("A and C should not be threaded yet (a=%q c=%q)", a, c)
	}

	// B names A and is named by C: it joins the two. Which of the two ids wins
	// is deliberately not asserted -- the winner is the smaller of the pair, so
	// that the outcome does not depend on which message arrived last. What must
	// hold is that one thread survives and holds everything.
	putThreaded(t, s, "a", "rB", "b@x", "a@x", []string{"a@x"})

	winner := ""
	for _, remote := range []string{"rA", "rB", "rC"} {
		got, err := s.GetMessage(ctx, "a", remote)
		if err != nil {
			t.Fatalf("GetMessage %s: %v", remote, err)
		}
		if winner == "" {
			winner = got.ThreadID
		}
		if got.ThreadID != winner {
			t.Errorf("%s is in thread %q, want the merged %q", remote, got.ThreadID, winner)
		}
	}
	if winner != a && winner != c {
		t.Errorf("merged thread %q is neither of the two that merged (%q, %q)", winner, a, c)
	}

	th, _, err := s.GetThread(ctx, "a", winner, false)
	if err != nil {
		t.Fatalf("GetThread: %v", err)
	}
	if th.MessageCount != 3 {
		t.Errorf("merged thread holds %d messages, want 3", th.MessageCount)
	}
	// The loser's summary row must be gone, not left behind as a phantom.
	loser := a
	if winner == a {
		loser = c
	}
	if _, _, err := s.GetThread(ctx, "a", loser, false); err == nil {
		t.Errorf("thread %q survived the merge", loser)
	}
}

// Gmail and JMAP supply their own thread id. Nothing here may touch it.
func TestThreadingLeavesProviderThreadsAlone(t *testing.T) {
	s := newTestStore(t)
	ctx := context.Background()
	seedAccount(t, s, "a")

	m := threadFixture("a", "r1")
	m.ThreadID = "gmail-thread-1"
	m.MessageIDHeader = "root@x"
	if _, err := s.UpsertMessage(ctx, m, nil); err != nil {
		t.Fatalf("UpsertMessage: %v", err)
	}
	got, err := s.GetMessage(ctx, "a", "r1")
	if err != nil {
		t.Fatalf("GetMessage: %v", err)
	}
	if got.ThreadID != "gmail-thread-1" {
		t.Errorf("thread_id = %q, want the provider's own", got.ThreadID)
	}
}

// A message with no usable headers (an oversize stub, whose body was never
// parsed) still has to land somewhere.
func TestThreadingFallsBackToRemoteID(t *testing.T) {
	s := newTestStore(t)
	ctx := context.Background()
	seedAccount(t, s, "a")

	m := threadFixture("a", "r1")
	m.ThreadID = ""
	m.MessageIDHeader = ""
	if _, err := s.UpsertMessage(ctx, m, nil); err != nil {
		t.Fatalf("UpsertMessage: %v", err)
	}
	got, err := s.GetMessage(ctx, "a", "r1")
	if err != nil {
		t.Fatalf("GetMessage: %v", err)
	}
	if got.ThreadID != "r1" {
		t.Errorf("thread_id = %q, want the remote id as the fallback", got.ThreadID)
	}
}

// Re-indexing the same message must not fan its refs out or move its thread.
func TestThreadingIsStableAcrossReindex(t *testing.T) {
	s := newTestStore(t)
	seedAccount(t, s, "a")

	first := putThreaded(t, s, "a", "r1", "root@x", "", nil)
	second := putThreaded(t, s, "a", "r1", "root@x", "", nil)
	if first != second {
		t.Fatalf("thread id moved on re-index: %q -> %q", first, second)
	}
}

// ---------------------------------------------------------------------------
// RenameRemoteID

// A rename must carry everything the row owns with it. This is the whole point
// of renaming rather than letting the delta delete and re-fetch: an IMAP MOVE
// changes the uid, and re-downloading the body every time someone archives a
// message would be absurd.
func TestRenameRemoteIDKeepsEverythingAttached(t *testing.T) {
	s := newTestStore(t)
	ctx := context.Background()
	seedAccount(t, s, "a") // also creates mb-inbox and friends

	m := threadFixture("a", "old-id")
	m.Subject = "findable subject"
	m.TextBody = "a distinctive body"
	m.BlobSHA256 = "deadbeef"
	m.MailboxRemotes = []string{"mb-inbox"}
	m.MessageIDHeader = "root@x"
	before := putMessage(t, s, m, nil)
	// UpsertMessage works on a copy and writes back only ID/IndexedAt, so read
	// the thread it actually landed in rather than trusting the fixture.
	stored, err := s.GetMessage(ctx, "a", "old-id")
	if err != nil {
		t.Fatalf("GetMessage(old-id): %v", err)
	}

	moved, err := s.RenameRemoteID(ctx, "a", "old-id", "new-id")
	if err != nil {
		t.Fatalf("RenameRemoteID: %v", err)
	}
	if !moved {
		t.Fatal("RenameRemoteID reported no move")
	}

	if _, err := s.GetMessage(ctx, "a", "old-id"); err == nil {
		t.Error("the old id still resolves")
	}
	got, err := s.GetMessage(ctx, "a", "new-id")
	if err != nil {
		t.Fatalf("GetMessage(new-id): %v", err)
	}
	if got.ID != before {
		t.Errorf("row id changed: %d -> %d", before, got.ID)
	}
	if got.BlobSHA256 != "deadbeef" {
		t.Errorf("blob lost: %q", got.BlobSHA256)
	}
	if len(got.MailboxRemotes) != 1 || got.MailboxRemotes[0] != "mb-inbox" {
		t.Errorf("membership lost: %v", got.MailboxRemotes)
	}
	if got.ThreadID != stored.ThreadID {
		t.Errorf("thread changed: %q -> %q", stored.ThreadID, got.ThreadID)
	}

	// remote_id is not one of the columns messages_au fires on, so the
	// external-content FTS entry must still point at this row.
	hits, err := s.Search(ctx, "distinctive", MessageFilter{Accounts: []string{"a"}})
	if err != nil {
		t.Fatalf("Search: %v", err)
	}
	if len(hits) != 1 || hits[0].Message.RemoteID != "new-id" {
		t.Errorf("search after rename returned %d hits (%+v), want the renamed row", len(hits), hits)
	}
}

// A delta can discover the moved copy under its new id before the outbox gets
// to report the rename. Two rows for one message is exactly what the unique
// index exists to prevent, so the stale one goes.
func TestRenameRemoteIDOntoAnExistingRowDropsTheStaleOne(t *testing.T) {
	s := newTestStore(t)
	ctx := context.Background()
	seedAccount(t, s, "a")

	putMessage(t, s, threadFixture("a", "old-id"), nil)
	putMessage(t, s, threadFixture("a", "new-id"), nil)

	moved, err := s.RenameRemoteID(ctx, "a", "old-id", "new-id")
	if err != nil {
		t.Fatalf("RenameRemoteID: %v", err)
	}
	if moved {
		t.Error("reported a move onto an id that was already indexed")
	}
	if _, err := s.GetMessage(ctx, "a", "old-id"); err == nil {
		t.Error("the stale row survived")
	}
	if _, err := s.GetMessage(ctx, "a", "new-id"); err != nil {
		t.Errorf("the authoritative row was dropped: %v", err)
	}
}

// Renaming something we never indexed is routine, not an error: the message may
// predate the account, or have been skipped as oversize.
func TestRenameRemoteIDIgnoresUnknownIDs(t *testing.T) {
	s := newTestStore(t)
	seedAccount(t, s, "a")

	moved, err := s.RenameRemoteID(context.Background(), "a", "never-seen", "whatever")
	if err != nil {
		t.Fatalf("RenameRemoteID: %v", err)
	}
	if moved {
		t.Error("reported a move for an id that was never indexed")
	}
}
