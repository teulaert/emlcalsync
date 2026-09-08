package store

import (
	"context"
	"testing"

	"github.com/teulaert/emlcalsync/internal/model"
)

// mailboxesOf reads a message's membership back through the ordinary getter.
func mailboxesOf(t *testing.T, s *Store, account, remote string) []string {
	t.Helper()
	m, err := s.GetMessage(context.Background(), account, remote)
	if err != nil {
		t.Fatalf("GetMessage %s: %v", remote, err)
	}
	return m.MailboxRemotes
}

func seedFenced(t *testing.T, s *Store) {
	t.Helper()
	seedAccount(t, s, "work")
	putMessage(t, s, &model.Message{
		AccountID: "work", RemoteID: "m1", ThreadID: "t1", Received: base,
		Flags: model.Flags{Unread: true}, MailboxRemotes: []string{"mb-inbox"},
	}, nil)
	if err := s.PatchMessageState(context.Background(), "work", "m1",
		model.Flags{}, []string{"mb-archive"}); err != nil {
		t.Fatalf("PatchMessageState: %v", err)
	}
}

// The whole point: a delta carrying the state from before the local write must
// not put the message back in the inbox, because the pass after that would
// take it out again and the row would flicker.
func TestAFencedRowKeepsWhatTheLocalWriteSaid(t *testing.T) {
	ctx := context.Background()
	s := newTestStore(t)
	seedFenced(t, s)

	applied, err := s.ApplyRemoteState(ctx, "work", "m1",
		model.Flags{Unread: true}, []string{"mb-inbox"})
	if err != nil {
		t.Fatalf("ApplyRemoteState: %v", err)
	}
	if applied {
		t.Error("a stale provider state was applied over a local write")
	}
	if got := mailboxesOf(t, s, "work", "m1"); len(got) != 1 || got[0] != "mb-archive" {
		t.Errorf("mailboxes = %v, want [mb-archive]", got)
	}
	m, err := s.GetMessage(ctx, "work", "m1")
	if err != nil {
		t.Fatal(err)
	}
	if m.Flags.Unread {
		t.Error("the fenced row lost the read flag the local write set")
	}
}

// An upsert is the other way a delta writes: it re-fetches the whole message.
func TestAFencedRowSurvivesAReFetch(t *testing.T) {
	s := newTestStore(t)
	seedFenced(t, s)

	putMessage(t, s, &model.Message{
		AccountID: "work", RemoteID: "m1", ThreadID: "t1", Received: base,
		Flags: model.Flags{Unread: true}, MailboxRemotes: []string{"mb-inbox"},
	}, nil)

	if got := mailboxesOf(t, s, "work", "m1"); len(got) != 1 || got[0] != "mb-archive" {
		t.Errorf("mailboxes = %v, want [mb-archive]", got)
	}
}

// Once the provider agrees, the fence has nothing left to protect and comes
// down -- otherwise it would go on holding out against a state that is no
// longer stale, and a change made elsewhere would be ignored for two minutes.
func TestTheFenceComesDownWhenTheProviderCatchesUp(t *testing.T) {
	ctx := context.Background()
	s := newTestStore(t)
	seedFenced(t, s)

	applied, err := s.ApplyRemoteState(ctx, "work", "m1", model.Flags{}, []string{"mb-archive"})
	if err != nil {
		t.Fatalf("ApplyRemoteState: %v", err)
	}
	if !applied {
		t.Fatal("the provider agreeing with the local write was still refused")
	}
	// The fence is down, so what the provider says next is taken as it comes.
	applied, err = s.ApplyRemoteState(ctx, "work", "m1", model.Flags{}, []string{"mb-inbox"})
	if err != nil {
		t.Fatalf("ApplyRemoteState: %v", err)
	}
	if !applied {
		t.Error("the fence outlived the state it was protecting")
	}
	if got := mailboxesOf(t, s, "work", "m1"); len(got) != 1 || got[0] != "mb-inbox" {
		t.Errorf("mailboxes = %v, want [mb-inbox]", got)
	}
}

// A write the provider rejected never happened, so the fence must go with the
// rollback -- what the server has is the truth again.
func TestClearLocalPatchLetsTheProviderThroughAgain(t *testing.T) {
	ctx := context.Background()
	s := newTestStore(t)
	seedFenced(t, s)

	if err := s.ClearLocalPatch(ctx, "work", "m1"); err != nil {
		t.Fatalf("ClearLocalPatch: %v", err)
	}
	applied, err := s.ApplyRemoteState(ctx, "work", "m1",
		model.Flags{Unread: true}, []string{"mb-inbox"})
	if err != nil {
		t.Fatalf("ApplyRemoteState: %v", err)
	}
	if !applied {
		t.Fatal("the fence outlived the write it stood for")
	}
	if got := mailboxesOf(t, s, "work", "m1"); len(got) != 1 || got[0] != "mb-inbox" {
		t.Errorf("mailboxes = %v, want [mb-inbox]", got)
	}
}

// A message nothing local has touched is written the moment the provider says
// so, which is every message but the handful a triage run just moved.
func TestAnUnfencedRowTakesTheProviderAtItsWord(t *testing.T) {
	ctx := context.Background()
	s := newTestStore(t)
	seedAccount(t, s, "work")
	putMessage(t, s, &model.Message{
		AccountID: "work", RemoteID: "m1", ThreadID: "t1", Received: base,
		MailboxRemotes: []string{"mb-inbox"},
	}, nil)

	applied, err := s.ApplyRemoteState(ctx, "work", "m1",
		model.Flags{Flagged: true}, []string{"mb-archive"})
	if err != nil {
		t.Fatalf("ApplyRemoteState: %v", err)
	}
	if !applied {
		t.Fatal("an untouched row refused the provider")
	}
	if got := mailboxesOf(t, s, "work", "m1"); len(got) != 1 || got[0] != "mb-archive" {
		t.Errorf("mailboxes = %v, want [mb-archive]", got)
	}
}
