package imap

import (
	"strings"
	"testing"

	imapv2 "github.com/emersion/go-imap/v2"

	"github.com/teulaert/emlcalsync/internal/model"
	"github.com/teulaert/emlcalsync/internal/testutil/imapfake"
)

// A move mints a new uid, so the write has to report where the message went.
// Without that the engine would see the old id vanish and the new one appear,
// and re-download a message it already has.
func TestMoveReportsTheNewID(t *testing.T) {
	m, srv := dial(t)
	ctx := testCtx(t)
	srv.Mail("INBOX", "moving house", "hello")

	id := enumerateAll(t, m, 10)[0].RemoteID
	renames, err := m.SetMailboxesRemap(ctx, []string{id}, []string{"Archive"}, []string{"INBOX"})
	if err != nil {
		t.Fatalf("SetMailboxesRemap: %v", err)
	}
	if len(renames) != 1 {
		t.Fatalf("renames = %+v, want one", renames)
	}
	if renames[0].Old != id {
		t.Errorf("rename is from %q, want %q", renames[0].Old, id)
	}
	newRef, err := parseRef(renames[0].New)
	if err != nil {
		t.Fatalf("new id does not parse: %v", err)
	}
	if newRef.Mailbox != "Archive" {
		t.Errorf("moved to %q, want Archive", newRef.Mailbox)
	}
	if srv.Count("INBOX") != 0 || srv.Count("Archive") != 1 {
		t.Errorf("server has inbox=%d archive=%d, want 0 and 1",
			srv.Count("INBOX"), srv.Count("Archive"))
	}
}

func TestTrashMovesToTheTrashFolder(t *testing.T) {
	m, srv := dial(t)
	srv.Mail("INBOX", "rubbish", "hello")

	id := enumerateAll(t, m, 10)[0].RemoteID
	renames, err := m.TrashRemap(testCtx(t), []string{id})
	if err != nil {
		t.Fatalf("TrashRemap: %v", err)
	}
	if len(renames) != 1 {
		t.Fatalf("renames = %+v, want one", renames)
	}
	if srv.Count("Trash") != 1 {
		t.Errorf("trash holds %d, want 1", srv.Count("Trash"))
	}
}

func TestRestoreMovesBackToTheInbox(t *testing.T) {
	m, srv := dial(t)
	srv.Mail("Trash", "leftover", "hello")

	id := enumerateAll(t, m, 10)[0].RemoteID
	renames, err := m.RestoreRemap(testCtx(t), []string{id})
	if err != nil {
		t.Fatalf("RestoreRemap: %v", err)
	}
	if len(renames) != 1 {
		t.Fatalf("renames = %+v, want one", renames)
	}
	if srv.Count("INBOX") != 1 || srv.Count("Trash") != 0 {
		t.Errorf("server has inbox=%d trash=%d, want 1 and 0",
			srv.Count("INBOX"), srv.Count("Trash"))
	}
}

// \Seen is inverted against the model: the model records unread, IMAP records
// read. Getting this backwards would mark the archive read on first contact.
func TestSetFlagsInvertsSeen(t *testing.T) {
	m, srv := dial(t)
	ctx := testCtx(t)
	srv.Mail("INBOX", "flag me", "hello")

	id := enumerateAll(t, m, 10)[0].RemoteID

	// Mark read.
	if err := m.SetFlags(ctx, []string{id}, model.Flags{}, model.Flags{Unread: true}); err != nil {
		t.Fatalf("SetFlags: %v", err)
	}
	envs, err := m.FetchEnvelopesSlice(ctx, []string{id})
	if err != nil {
		t.Fatalf("FetchEnvelopes: %v", err)
	}
	if envs[0].Flags.Unread {
		t.Error("clearing Unread did not set \\Seen")
	}

	// And back to unread.
	if err := m.SetFlags(ctx, []string{id}, model.Flags{Unread: true}, model.Flags{}); err != nil {
		t.Fatalf("SetFlags: %v", err)
	}
	envs, _ = m.FetchEnvelopesSlice(ctx, []string{id})
	if !envs[0].Flags.Unread {
		t.Error("setting Unread did not clear \\Seen")
	}
}

func TestSetFlagsRoundTripsFlaggedAndAnswered(t *testing.T) {
	m, srv := dial(t)
	ctx := testCtx(t)
	srv.Mail("INBOX", "star me", "hello")
	id := enumerateAll(t, m, 10)[0].RemoteID

	if err := m.SetFlags(ctx, []string{id}, model.Flags{Flagged: true, Answered: true}, model.Flags{}); err != nil {
		t.Fatalf("SetFlags: %v", err)
	}
	envs, _ := m.FetchEnvelopesSlice(ctx, []string{id})
	if !envs[0].Flags.Flagged || !envs[0].Flags.Answered {
		t.Errorf("flags = %+v, want flagged and answered", envs[0].Flags)
	}
}

func TestCreateDraftAppendsToDrafts(t *testing.T) {
	m, srv := dial(t)
	raw := []byte(imapfake.Message("a draft", "not sent yet"))

	id, err := m.CreateDraft(testCtx(t), raw)
	if err != nil {
		t.Fatalf("CreateDraft: %v", err)
	}
	if srv.Count("Drafts") != 1 {
		t.Fatalf("drafts holds %d, want 1", srv.Count("Drafts"))
	}
	if id == "" {
		t.Fatal("no id returned; the fake advertises UIDPLUS so APPENDUID should be there")
	}
	r, err := parseRef(id)
	if err != nil || r.Mailbox != "Drafts" {
		t.Errorf("draft id %q does not name the Drafts folder (%v)", id, err)
	}
}

// A server with no Archive folder must say so, not quietly do nothing.
func TestArchiveWithoutAnArchiveFolderIsAnError(t *testing.T) {
	srv := imapfake.New(t)
	srv.Mail("INBOX", "nowhere to go", "hello")
	m := newProvider(t, srv)

	id := enumerateAll(t, m, 10)[0].RemoteID
	_, err := m.SetMailboxesRemap(testCtx(t), []string{id}, nil, []string{"INBOX"})
	if err == nil {
		t.Fatal("archiving succeeded with no Archive folder")
	}
	if !strings.Contains(err.Error(), "archive_folder") {
		t.Errorf("error does not say how to fix it: %v", err)
	}
}

// Without MOVE the client must fall back, and must not reach for a bare
// EXPUNGE -- that would destroy every \Deleted message in the folder,
// including ones another client flagged.
func TestMoveFallsBackWithoutMoveCapability(t *testing.T) {
	m, srv := dialWith(t, imapfake.HideCaps(imapv2.CapMove))
	srv.Mail("INBOX", "the long way round", "hello")

	id := enumerateAll(t, m, 10)[0].RemoteID
	renames, err := m.SetMailboxesRemap(testCtx(t), []string{id}, []string{"Archive"}, []string{"INBOX"})
	if err != nil {
		t.Fatalf("SetMailboxesRemap: %v", err)
	}
	if srv.Count("Archive") != 1 {
		t.Errorf("archive holds %d, want the copy", srv.Count("Archive"))
	}
	if len(renames) != 1 {
		t.Errorf("renames = %+v, want one from COPYUID", renames)
	}
}
