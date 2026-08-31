package tui

import (
	"context"
	"testing"
	"time"

	"github.com/teulaert/emlcalsync/internal/model"
	"github.com/teulaert/emlcalsync/internal/provider"
	"github.com/teulaert/emlcalsync/internal/sync"
)

func TestGroupKeepsAccountsSeparateAndOrdered(t *testing.T) {
	got := group([]target{
		{account: "work", remote: "a"},
		{account: "home", remote: "b"},
		{account: "work", remote: "c"},
	})
	if len(got) != 2 {
		t.Fatalf("got %d groups, want 2", len(got))
	}
	if got[0].account != "work" || len(got[0].remotes) != 2 {
		t.Errorf("group 0 = %+v, want work with 2 ids", got[0])
	}
	if got[1].account != "home" || len(got[1].remotes) != 1 {
		t.Errorf("group 1 = %+v, want home with 1 id", got[1])
	}
}

func TestArchiveUndoPutsItBackInTheInbox(t *testing.T) {
	d := newTestDeps(t, "work")
	addMessage(t, d, "work", "w1", "t1", "Subject", "anna", time.Hour, false)
	ctx := context.Background()

	ts := []target{{account: "work", remote: "w1", mailboxes: []string{"inbox"}}}
	fwd, undo := archiveOps(ctx, d.Store, ts)

	if len(fwd) != 1 || fwd[0].op.Kind != sync.OpArchive {
		t.Fatalf("forward op = %+v, want one OpArchive", fwd)
	}
	if undo == nil || len(undo.ops) != 1 {
		t.Fatal("archive produced no undo")
	}
	u := undo.ops[0].op
	if u.Kind != sync.OpMailboxes {
		t.Errorf("undo kind = %v, want OpMailboxes", u.Kind)
	}
	if len(u.AddMailboxes) != 1 || u.AddMailboxes[0] != "inbox" {
		t.Errorf("undo adds %v, want [inbox]", u.AddMailboxes)
	}
	if len(u.RemoveMailboxes) != 1 || u.RemoveMailboxes[0] != "archive" {
		t.Errorf("undo removes %v, want [archive]", u.RemoveMailboxes)
	}
}

// Trash sets clearOthers, so the engine drops every other membership. The undo
// is only exact if the set was captured beforehand — this is the test that
// says so.
func TestTrashUndoReplaysTheWholeMailboxSet(t *testing.T) {
	d := newTestDeps(t, "work")
	ctx := context.Background()

	ts := []target{{
		account:   "work",
		remote:    "w1",
		mailboxes: []string{"inbox", "archive"},
	}}
	fwd, undo := trashOps(ctx, d.Store, ts)

	if len(fwd) != 1 || fwd[0].op.Kind != sync.OpTrash {
		t.Fatalf("forward op = %+v, want one OpTrash", fwd)
	}
	if undo == nil || len(undo.ops) != 1 {
		t.Fatal("trash produced no undo")
	}
	u := undo.ops[0].op
	if len(u.AddMailboxes) != 2 || u.AddMailboxes[0] != "inbox" || u.AddMailboxes[1] != "archive" {
		t.Errorf("undo adds %v, want both original mailboxes back", u.AddMailboxes)
	}
	if len(u.RemoveMailboxes) != 1 || u.RemoveMailboxes[0] != "trash" {
		t.Errorf("undo removes %v, want [trash]", u.RemoveMailboxes)
	}
}

// restoreOps' forward op does not know (or need to know) whether the message
// came from the archive or the trash -- OpRestore resolves that server-side,
// the same way OpArchive does. The undo is per-target and exact, the same way
// trashOps' is.
func TestRestoreUndoPutsItBackWhereItWas(t *testing.T) {
	d := newTestDeps(t, "work")
	ctx := context.Background()

	ts := []target{{account: "work", remote: "w1", mailboxes: []string{"trash"}}}
	fwd, undo := restoreOps(ctx, d.Store, ts)

	if len(fwd) != 1 || fwd[0].op.Kind != sync.OpRestore {
		t.Fatalf("forward op = %+v, want one OpRestore", fwd)
	}
	if undo == nil || len(undo.ops) != 1 {
		t.Fatal("restore produced no undo")
	}
	u := undo.ops[0].op
	if u.Kind != sync.OpMailboxes {
		t.Errorf("undo kind = %v, want OpMailboxes", u.Kind)
	}
	if len(u.AddMailboxes) != 1 || u.AddMailboxes[0] != "trash" {
		t.Errorf("undo adds %v, want [trash]", u.AddMailboxes)
	}
	if len(u.RemoveMailboxes) != 1 || u.RemoveMailboxes[0] != "inbox" {
		t.Errorf("undo removes %v, want [inbox]", u.RemoveMailboxes)
	}
}

func TestFlagUndoIsTheOppositeFlag(t *testing.T) {
	ts := []target{{account: "work", remote: "w1"}}

	fwd, undo := flagOps(ts, "unread", false) // mark read
	if !fwd[0].op.Flags.Clear.Unread {
		t.Error("marking read should clear the unread flag")
	}
	if !undo.ops[0].op.Flags.Set.Unread {
		t.Error("undo of mark-read should set the unread flag again")
	}

	fwd, undo = flagOps(ts, "flagged", true) // star
	if !fwd[0].op.Flags.Set.Flagged {
		t.Error("star should set the flagged flag")
	}
	if !undo.ops[0].op.Flags.Clear.Flagged {
		t.Error("undo of star should clear the flagged flag")
	}
}

func TestUndoExpiresAfterItsWindow(t *testing.T) {
	now := time.Now()
	u := &undoRecord{label: "archive", when: now}
	if !u.live(now.Add(time.Second)) {
		t.Error("undo should still be live one second in")
	}
	if u.live(now.Add(undoWindow + time.Second)) {
		t.Error("undo should have expired past its window")
	}
	var nilRec *undoRecord
	if nilRec.live(now) {
		t.Error("a nil undo record must not be live")
	}
}

// On IMAP a move mints a new uid, so ApplyResult reports renames and anything
// still holding the old id has to follow. Gmail and JMAP report none, which is
// why this is easy to get wrong and never notice.
func TestUndoRecordFollowsRenames(t *testing.T) {
	u := &undoRecord{
		label: "archive",
		ops: []accountOp{{
			account: "work",
			op:      sync.Op{Kind: sync.OpMailboxes, IDs: []string{"old-uid", "untouched"}},
		}},
	}
	u.rewrite(map[string]string{
		model.MessagePublicID("work", "old-uid"): model.MessagePublicID("work", "new-uid"),
	})
	got := u.ops[0].op.IDs
	if got[0] != "new-uid" {
		t.Errorf("renamed id = %q, want new-uid", got[0])
	}
	if got[1] != "untouched" {
		t.Errorf("unrenamed id = %q, want it left alone", got[1])
	}
}

func TestRespondOpCarriesTheCalendar(t *testing.T) {
	ops := respondOp("work", "cal-1", "ev-1", model.PartAccepted)
	if len(ops) != 1 {
		t.Fatalf("got %d ops, want 1", len(ops))
	}
	op := ops[0].op
	if op.Kind != sync.OpEventRespond {
		t.Errorf("kind = %v, want OpEventRespond", op.Kind)
	}
	if op.CalendarRemote != "cal-1" || op.Response != model.PartAccepted {
		t.Errorf("op = %+v, want cal-1 / accepted", op)
	}
}

// Guard the assumption the rename handling rests on.
var _ = provider.Rename{Old: "a", New: "b"}
