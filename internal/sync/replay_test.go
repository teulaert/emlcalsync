package sync

import (
	"context"
	"fmt"
	"testing"

	"github.com/teulaert/emlcalsync/internal/model"
)

// A delta whose download dies partway (the server restarting during a big
// import) must not download again, on the replay, what it already archived.
// Before, every interruption restarted the whole change set from its first
// message, and a change set that took longer than the gap between server
// restarts never finished.
func TestInterruptedDeltaDoesNotRedownloadWhatItArchived(t *testing.T) {
	h := newHarness(t)
	h.mail.Add(&fakeMsg{id: "seed", raw: mailRaw(t, "seed", "seed")})
	h.sync(SyncOptions{Mail: true})

	const n = 350
	for i := 0; i < n; i++ {
		id := fmt.Sprintf("imp%03d", i)
		h.mail.Add(&fakeMsg{id: id, raw: mailRaw(t, id, id)})
	}
	// Cut the download after 250: two full batches of 100 are committed, the
	// third is not.
	h.mail.mu.Lock()
	h.mail.cutFetchAfter = 250
	h.mail.mu.Unlock()

	before := h.state()
	if _, err := h.eng.SyncAccount(context.Background(), "work", SyncOptions{Mail: true}); err == nil {
		t.Fatal("the interrupted pass reported success")
	}
	if h.state() != before {
		t.Fatalf("state advanced by an interrupted pass: %q -> %q", before, h.state())
	}

	// A flag that changed on the server between the passes must still land
	// on a message the replay does not download again.
	h.mail.Update("imp000", model.Flags{Flagged: true}, nil)

	rep := h.sync(SyncOptions{Mail: true})
	if rep.Mail.Kind != KindDelta {
		t.Fatalf("kind = %q, want delta", rep.Mail.Kind)
	}
	for i := 0; i < n; i++ {
		if id := fmt.Sprintf("imp%03d", i); !h.indexed(id) {
			t.Fatalf("%s not indexed after the replay", id)
		}
	}
	h.mail.mu.Lock()
	total, twice := 0, 0
	for id, c := range h.mail.rawFetches {
		if id == "seed" {
			continue
		}
		total += c
		if c > 1 {
			twice++
		}
	}
	h.mail.mu.Unlock()
	// Only the uncommitted third batch (50 messages) may be downloaded twice.
	if twice > 50 || total > n+50 {
		t.Errorf("replay downloaded %d messages again (%d downloads for %d messages), want at most the 50 uncommitted",
			twice, total, n)
	}
	if !h.message("imp000").Flags.Flagged {
		t.Errorf("a flag changed between the passes was lost on a message the replay skipped")
	}
	if h.state() == before {
		t.Errorf("state did not advance after the replay finished")
	}
}
