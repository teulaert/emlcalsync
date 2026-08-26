package sync

import (
	"context"
	"os"
	"testing"
	"time"

	"github.com/teulaert/emlcalsync/internal/model"
	"github.com/teulaert/emlcalsync/internal/store"
)

func TestReindexRebuildsRowsFromBlobs(t *testing.T) {
	h := newHarness(t)
	h.mail.Add(&fakeMsg{id: "m1", raw: mailRaw(t, "Quarterly report", "numbers about tungsten"),
		flags: model.Flags{Flagged: true}, mailboxes: []string{"INBOX", "WORK"}})
	h.mail.Add(&fakeMsg{id: "m2", raw: mailRaw(t, "Lunch", "sandwiches")})
	h.sync(SyncOptions{Mail: true})

	before := h.message("m1")

	// Corrupt the derived columns the way a bad migration would.
	if _, err := h.st.DB().ExecContext(context.Background(),
		`UPDATE messages SET subject = '', text_body = '', snippet = ''`); err != nil {
		t.Fatalf("corrupt: %v", err)
	}

	rep, err := h.eng.Reindex(context.Background(), "work")
	if err != nil {
		t.Fatalf("Reindex: %v", err)
	}
	if rep.Scanned != 2 || rep.Reindexed != 2 || rep.MissingBlobs != 0 {
		t.Fatalf("report = %+v", rep)
	}

	after := h.message("m1")
	if after.Subject != before.Subject || after.TextBody != before.TextBody {
		t.Fatalf("reindex did not reproduce the row:\n before %+v\n after  %+v", before, after)
	}
	if after.Flags != before.Flags {
		t.Fatalf("flags changed: %+v -> %+v", before.Flags, after.Flags)
	}
	if len(after.MailboxRemotes) != len(before.MailboxRemotes) {
		t.Fatalf("mailboxes changed: %v -> %v", before.MailboxRemotes, after.MailboxRemotes)
	}
	if after.Received != before.Received {
		t.Fatalf("received changed: %v -> %v", before.Received, after.Received)
	}

	hits, err := h.st.Search(context.Background(), "tungsten", store.MessageFilter{})
	if err != nil {
		t.Fatalf("Search: %v", err)
	}
	if len(hits) != 1 {
		t.Fatalf("FTS not rebuilt: %d hits", len(hits))
	}
}

func TestReindexReportsMissingBlobs(t *testing.T) {
	h := newHarness(t)
	h.mail.Add(&fakeMsg{id: "m1", raw: mailRaw(t, "one", "one")})
	h.sync(SyncOptions{Mail: true})

	sha := h.message("m1").BlobSHA256
	if err := h.blobs.Delete(sha); err != nil {
		t.Fatalf("Delete: %v", err)
	}
	rep, err := h.eng.Reindex(context.Background(), "")
	if err != nil {
		t.Fatalf("Reindex: %v", err)
	}
	if rep.MissingBlobs != 1 || rep.Reindexed != 0 {
		t.Fatalf("report = %+v", rep)
	}
}

func TestGCRemovesOrphanBlobs(t *testing.T) {
	h := newHarness(t)
	h.mail.Add(&fakeMsg{id: "m1", raw: mailRaw(t, "one", "one")})
	h.sync(SyncOptions{Mail: true})

	orphan, _, err := h.blobs.Put([]byte("From: nobody\r\n\r\nleft behind by a crash\r\n"))
	if err != nil {
		t.Fatalf("Put: %v", err)
	}
	kept := h.message("m1").BlobSHA256

	// A fresh orphan is left alone: it may belong to a batch that has not
	// committed yet.
	rep, err := h.eng.GC(context.Background(), false)
	if err != nil {
		t.Fatalf("GC: %v", err)
	}
	if rep.Deleted != 0 || rep.Skipped != 1 {
		t.Fatalf("young orphan was not spared: %+v", rep)
	}
	old := time.Now().Add(-2 * orphanGrace)
	if err := os.Chtimes(h.blobs.Path(orphan), old, old); err != nil {
		t.Fatalf("Chtimes: %v", err)
	}

	rep, err = h.eng.GC(context.Background(), false)
	if err != nil {
		t.Fatalf("GC: %v", err)
	}
	if rep.Deleted != 1 || rep.Blobs != 2 {
		t.Fatalf("report = %+v", rep)
	}
	if h.blobs.Exists(orphan) {
		t.Fatal("orphan blob survived")
	}
	if !h.blobs.Exists(kept) {
		t.Fatal("referenced blob was collected")
	}
	if rep.PurgedMessages != 0 {
		t.Fatalf("GC purged rows without being asked: %+v", rep)
	}
}

func TestGCPurgeDeletedDropsRowsAndBlobs(t *testing.T) {
	h := newHarness(t)
	h.mail.Add(&fakeMsg{id: "m1", raw: mailRaw(t, "one", "one")})
	h.mail.Add(&fakeMsg{id: "m2", raw: mailRaw(t, "two", "two")})
	h.sync(SyncOptions{Mail: true})

	gone := h.message("m2").BlobSHA256
	h.mail.Remove("m2")
	h.sync(SyncOptions{Mail: true})
	if h.message("m2").DeletedAt == nil {
		t.Fatal("m2 not marked deleted")
	}

	rep, err := h.eng.GC(context.Background(), true)
	if err != nil {
		t.Fatalf("GC: %v", err)
	}
	if rep.PurgedMessages != 1 || rep.Deleted != 1 {
		t.Fatalf("report = %+v", rep)
	}
	if h.blobs.Exists(gone) {
		t.Fatal("purged message's blob survived")
	}
	if _, err := h.st.GetMessage(context.Background(), "work", "m2"); err == nil {
		t.Fatal("purged row still readable")
	}
	if got := len(h.messages()); got != 1 {
		t.Fatalf("%d rows left, want 1", got)
	}
	threads, err := h.st.ListThreads(context.Background(), store.MessageFilter{})
	if err != nil {
		t.Fatalf("ListThreads: %v", err)
	}
	if len(threads) != 1 {
		t.Fatalf("%d thread summaries left, want 1", len(threads))
	}
}

func TestFetchAttachmentPrefersTheArchive(t *testing.T) {
	h := newHarness(t)
	h.mail.Add(&fakeMsg{id: "m1", raw: mailRaw(t, "one", "one")})
	h.sync(SyncOptions{Mail: true})

	// No attachment rows: the provider is asked.
	data, err := h.eng.FetchAttachment(context.Background(), "work", "m1", "att-1")
	if err != nil {
		t.Fatalf("FetchAttachment: %v", err)
	}
	if string(data) != "attachment:m1:att-1" {
		t.Fatalf("data = %q", data)
	}
}
