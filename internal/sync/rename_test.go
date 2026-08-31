package sync

import (
	"context"
	"testing"

	"github.com/teulaert/emlcalsync/internal/provider"
)

// remappingMail is a provider whose writes move remote ids, the way IMAP does:
// a copy or a move mints a fresh uid, so the id the caller passed in stops
// naming anything. It exists to prove the engine's rename path without any IMAP
// code in the picture.
type remappingMail struct {
	*fakeMail

	// writeRenames is handed back by the next SetMailboxesRemap/TrashRemap.
	writeRenames []provider.Rename
	// deltaRenames is reported by the next Changes call.
	deltaRenames []provider.Rename

	remapCalls   int
	trashCalls   int
	restoreCalls int
	// plainCalls counts falls through to the non-remapping methods, which must
	// stay at zero: the engine has to prefer Remapper when it is available.
	plainCalls int
}

var _ provider.Remapper = (*remappingMail)(nil)

func (r *remappingMail) SetMailboxesRemap(ctx context.Context, ids, add, remove []string) ([]provider.Rename, error) {
	r.remapCalls++
	if err := r.fakeMail.SetMailboxes(ctx, ids, add, remove); err != nil {
		return nil, err
	}
	return r.takeWriteRenames(), nil
}

func (r *remappingMail) TrashRemap(ctx context.Context, ids []string) ([]provider.Rename, error) {
	r.trashCalls++
	if err := r.fakeMail.Trash(ctx, ids); err != nil {
		return nil, err
	}
	return r.takeWriteRenames(), nil
}

func (r *remappingMail) RestoreRemap(ctx context.Context, ids []string) ([]provider.Rename, error) {
	r.restoreCalls++
	if err := r.fakeMail.Restore(ctx, ids); err != nil {
		return nil, err
	}
	return r.takeWriteRenames(), nil
}

func (r *remappingMail) SetMailboxes(ctx context.Context, ids, add, remove []string) error {
	r.plainCalls++
	return r.fakeMail.SetMailboxes(ctx, ids, add, remove)
}

func (r *remappingMail) Trash(ctx context.Context, ids []string) error {
	r.plainCalls++
	return r.fakeMail.Trash(ctx, ids)
}

func (r *remappingMail) Restore(ctx context.Context, ids []string) error {
	r.plainCalls++
	return r.fakeMail.Restore(ctx, ids)
}

func (r *remappingMail) takeWriteRenames() []provider.Rename {
	out := r.writeRenames
	r.writeRenames = nil
	return out
}

func (r *remappingMail) Changes(ctx context.Context, since string) (*provider.Changes, error) {
	ch, err := r.fakeMail.Changes(ctx, since)
	if err != nil {
		return nil, err
	}
	ch.Renamed, r.deltaRenames = r.deltaRenames, nil
	return ch, nil
}

// remapHarness swaps the harness's provider for one that moves ids.
func remapHarness(t *testing.T) (*harness, *remappingMail) {
	t.Helper()
	h := newHarness(t)
	rm := &remappingMail{fakeMail: h.mail}
	h.fact.mail = rm
	return h, rm
}

func (h *harness) remoteIDs(t *testing.T) map[string]bool {
	t.Helper()
	out := map[string]bool{}
	for _, m := range h.messages() {
		if m.DeletedAt == nil {
			out[m.RemoteID] = true
		}
	}
	return out
}

// A delta that reports a rename must move the row, not delete it and fetch the
// same bytes again under a new id.
func TestDeltaAppliesRenames(t *testing.T) {
	h, rm := remapHarness(t)
	h.mail.Add(&fakeMsg{id: "INBOX.1.7", raw: mailRaw(t, "moved", "body")})
	h.sync(SyncOptions{Mail: true})

	before := h.messages()
	if len(before) != 1 {
		t.Fatalf("stored %d messages, want 1", len(before))
	}
	rowID, blob := before[0].ID, before[0].BlobSHA256

	rm.deltaRenames = []provider.Rename{{Old: "INBOX.1.7", New: "Archive.1.3"}}
	rep := h.sync(SyncOptions{Mail: true})

	if rep.Mail.Renamed != 1 {
		t.Errorf("renamed = %d, want 1", rep.Mail.Renamed)
	}
	after := h.messages()
	if len(after) != 1 {
		t.Fatalf("stored %d messages after the rename, want 1", len(after))
	}
	if after[0].RemoteID != "Archive.1.3" {
		t.Errorf("remote id = %q, want the renamed one", after[0].RemoteID)
	}
	if after[0].ID != rowID {
		t.Errorf("row id changed: %d -> %d", rowID, after[0].ID)
	}
	if after[0].BlobSHA256 != blob {
		t.Errorf("blob changed: %q -> %q", blob, after[0].BlobSHA256)
	}
}

// A rename for an id we never indexed is routine, not a failure.
func TestDeltaIgnoresRenamesForUnknownIDs(t *testing.T) {
	h, rm := remapHarness(t)
	h.mail.Add(&fakeMsg{id: "m1", raw: mailRaw(t, "here", "body")})
	h.sync(SyncOptions{Mail: true})

	rm.deltaRenames = []provider.Rename{{Old: "never-indexed", New: "somewhere-else"}}
	rep := h.sync(SyncOptions{Mail: true})

	if rep.Mail.Renamed != 0 {
		t.Errorf("renamed = %d, want 0", rep.Mail.Renamed)
	}
	if !h.remoteIDs(t)["m1"] {
		t.Error("the real message went missing")
	}
}

// A write that moves ids must rename the row as it retires the outbox item, so
// the id the caller is handed back is the one the message now has.
func TestTrashThroughRemapperRenamesTheRow(t *testing.T) {
	h, rm := remapHarness(t)
	h.mail.Add(&fakeMsg{id: "INBOX.1.7", raw: mailRaw(t, "doomed", "body")})
	h.sync(SyncOptions{Mail: true})

	rm.writeRenames = []provider.Rename{{Old: "INBOX.1.7", New: "Trash.1.2"}}
	res, err := h.eng.Apply(context.Background(), "work", Op{Kind: OpTrash, IDs: []string{"INBOX.1.7"}})
	if err != nil {
		t.Fatalf("Apply: %v", err)
	}

	if rm.trashCalls != 1 {
		t.Errorf("TrashRemap called %d times, want 1", rm.trashCalls)
	}
	if rm.plainCalls != 0 {
		t.Errorf("the engine fell through to plain Trash %d times; Remapper must win", rm.plainCalls)
	}
	if len(res.Renames) != 1 || res.Renames[0].New != "Trash.1.2" {
		t.Errorf("ApplyResult.Renames = %+v, want the new id reported back", res.Renames)
	}
	if ids := h.remoteIDs(t); ids["INBOX.1.7"] {
		t.Error("the old id is still live in the index")
	}
}

// Same for a restore out of the trash: on IMAP the move back to the inbox
// mints a new uid too, so RestoreRemap has to win over the plain Restore.
func TestRestoreThroughRemapperRenamesTheRow(t *testing.T) {
	h, rm := remapHarness(t)
	h.mail.Add(&fakeMsg{id: "Trash.1.2", raw: mailRaw(t, "rescued", "body")})
	h.sync(SyncOptions{Mail: true})

	rm.writeRenames = []provider.Rename{{Old: "Trash.1.2", New: "INBOX.1.9"}}
	res, err := h.eng.Apply(context.Background(), "work", Op{Kind: OpRestore, IDs: []string{"Trash.1.2"}})
	if err != nil {
		t.Fatalf("Apply: %v", err)
	}

	if rm.restoreCalls != 1 {
		t.Errorf("RestoreRemap called %d times, want 1", rm.restoreCalls)
	}
	if rm.plainCalls != 0 {
		t.Errorf("the engine fell through to plain Restore %d times; Remapper must win", rm.plainCalls)
	}
	if len(res.Renames) != 1 || res.Renames[0].New != "INBOX.1.9" {
		t.Errorf("ApplyResult.Renames = %+v, want the new id reported back", res.Renames)
	}
	if ids := h.remoteIDs(t); ids["Trash.1.2"] {
		t.Error("the old id is still live in the index")
	}
}

// Same for a move between mailboxes.
func TestSetMailboxesThroughRemapperRenamesTheRow(t *testing.T) {
	h, rm := remapHarness(t)
	h.mail.Add(&fakeMsg{id: "INBOX.1.7", raw: mailRaw(t, "moving", "body")})
	h.sync(SyncOptions{Mail: true})

	rm.writeRenames = []provider.Rename{{Old: "INBOX.1.7", New: "Archive.1.9"}}
	res, err := h.eng.Apply(context.Background(), "work", Op{
		Kind: OpMailboxes, IDs: []string{"INBOX.1.7"},
		AddMailboxes: []string{"Archive"}, RemoveMailboxes: []string{"INBOX"},
	})
	if err != nil {
		t.Fatalf("Apply: %v", err)
	}
	if rm.remapCalls != 1 || rm.plainCalls != 0 {
		t.Errorf("remap=%d plain=%d, want the Remapper path only", rm.remapCalls, rm.plainCalls)
	}
	if len(res.Renames) != 1 {
		t.Fatalf("Renames = %+v, want one", res.Renames)
	}
	ids := h.remoteIDs(t)
	if ids["INBOX.1.7"] || !ids["Archive.1.9"] {
		t.Errorf("index still holds the old id: %v", ids)
	}
}

// A provider that implements no Remapper must be untouched by any of this --
// Gmail and JMAP go down exactly the path they always did.
func TestPlainProviderIsUnaffectedByRenames(t *testing.T) {
	h := newHarness(t)
	h.mail.Add(&fakeMsg{id: "m1", raw: mailRaw(t, "stays", "body")})
	h.sync(SyncOptions{Mail: true})

	if _, err := h.eng.Apply(context.Background(), "work", Op{Kind: OpTrash, IDs: []string{"m1"}}); err != nil {
		t.Fatalf("Apply: %v", err)
	}
	rep := h.sync(SyncOptions{Mail: true})
	if rep.Mail.Renamed != 0 {
		t.Errorf("renamed = %d for a provider with no Remapper, want 0", rep.Mail.Renamed)
	}
}

// Reindex rebuilds the Message-ID graph for free, because recordRefs lives in
// UpsertMessage. A plain pass must not disturb the thread ids themselves.
func TestReindexRebuildsRefsWithoutMovingThreads(t *testing.T) {
	h := newHarness(t)
	h.mail.Add(&fakeMsg{id: "m1", raw: mailRaw(t, "one", "body")})
	h.sync(SyncOptions{Mail: true})

	before := h.messages()[0].ThreadID
	if _, err := h.eng.Reindex(context.Background(), "work"); err != nil {
		t.Fatalf("Reindex: %v", err)
	}
	if got := h.messages()[0].ThreadID; got != before {
		t.Errorf("a plain reindex moved the thread: %q -> %q", before, got)
	}
}

// --rethread drops the derived threading and works it out again. The provider
// here supplies thread ids, so this also documents the sharp edge: rethreading
// an account whose backend threads server-side replaces that with our guess.
func TestReindexRethreadRecomputesFromHeaders(t *testing.T) {
	h := newHarness(t)
	h.mail.Add(&fakeMsg{id: "m1", raw: mailRaw(t, "one", "body")})
	h.sync(SyncOptions{Mail: true})

	before := h.messages()[0].ThreadID
	if _, err := h.eng.ReindexWith(context.Background(), "work", ReindexOptions{Rethread: true}); err != nil {
		t.Fatalf("ReindexWith: %v", err)
	}
	after := h.messages()[0].ThreadID
	if after == "" {
		t.Fatal("rethread left the message with no thread at all")
	}
	if after == before {
		t.Errorf("thread id unchanged (%q); the provider id should have been replaced", after)
	}
}
