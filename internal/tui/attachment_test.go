package tui

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/teulaert/emlcalsync/internal/model"
	"github.com/teulaert/emlcalsync/internal/provider/fake"
	"github.com/teulaert/emlcalsync/internal/sync"
)

// openForward seeds the message this whole column exists for: a receipt
// forwarded to bookkeeping, carrying the two PDFs that are the entire point of
// sending it, plus an inline logo that is not.
func openForward(t *testing.T) *root {
	t.Helper()
	d, mail := newTriageDeps(t)
	raw, err := os.ReadFile(filepath.Join("..", "mime", "testdata", "attachments.eml"))
	if err != nil {
		t.Fatal(err)
	}
	mail.Add(fake.NewMsg("fwd-1", raw).WithMailboxes("INBOX"))
	if _, err := d.Engine.SyncAccount(context.Background(), "work", sync.SyncOptions{}); err != nil {
		t.Fatalf("sync: %v", err)
	}
	return newTestRoot(t, d)
}

func TestMailListMarksThreadsCarryingFiles(t *testing.T) {
	r := openForward(t)
	list, ok := r.top().(*mailList)
	if !ok {
		t.Fatalf("top screen is %T, want the mail list", r.top())
	}
	if len(list.threads) != 1 {
		t.Fatalf("list has %d threads, want the forward", len(list.threads))
	}
	if !list.threads[0].HasAttachments {
		t.Error("thread summary says no attachments; the forward carries two PDFs")
	}
	// The mark is what the eye is actually asking for -- a row that says the
	// invoice is on it without opening anything.
	// Anchored to the account column so a stray "A" in a subject cannot make
	// this pass: the mark sits in its own cell, right after the unread dot.
	view := list.View(r.w, r.bodyHeight())
	row := strings.SplitN(view, "\n", 2)[0]
	if !strings.Contains(row, "work  A ") {
		t.Errorf("row carries no attachment mark:\n%q", row)
	}
	// And it does not appear on a thread that has none.
	addMessage(t, list.d, "work", "w2", "t2", "Just a note", "anna", 0, false)
	list2 := pump(t, list, list.reload(), r.keys, r.w, r.bodyHeight()).(*mailList)
	for _, th := range list2.threads {
		if th.ThreadID == "t2" && th.HasAttachments {
			t.Error("a plain message is marked as carrying files")
		}
	}
}

func TestReaderNamesTheAttachedFiles(t *testing.T) {
	r := openForward(t)
	send(t, r, "enter") // the thread
	send(t, r, "enter") // the reader
	rd, ok := r.top().(*reader)
	if !ok {
		t.Fatalf("top screen is %T, want the reader", r.top())
	}
	view := rd.View(r.w, r.bodyHeight())
	for _, want := range []string{
		"Attach:  Invoice-GRK6NZDJ-0025.pdf",
		"Receipt-2164-3490.pdf",
		"+1 inline", // the logo is counted, not listed
	} {
		if !strings.Contains(view, want) {
			t.Errorf("reader header misses %q:\n%s", want, view)
		}
	}
	if strings.Contains(view, "logo.png") {
		t.Errorf("the inline logo is listed as an attachment:\n%s", view)
	}
}

func TestThreadViewNamesTheAttachedFiles(t *testing.T) {
	r := openForward(t)
	send(t, r, "enter")
	tv, ok := r.top().(*threadView)
	if !ok {
		t.Fatalf("top screen is %T, want the thread view", r.top())
	}
	if !tv.expanded {
		send(t, r, "t")
	}
	view := r.top().View(r.w, r.bodyHeight())
	for _, want := range []string{
		"attached: Invoice-GRK6NZDJ-0025.pdf",
		// The label hangs: the second file lines up under the first.
		"            Receipt-2164-3490.pdf",
	} {
		if !strings.Contains(view, want) {
			t.Errorf("thread view misses %q:\n%s", want, view)
		}
	}
}

func TestAttachmentDescs(t *testing.T) {
	pdf := func(name string, size int64) model.Attachment {
		return model.Attachment{Filename: name, ContentType: "application/pdf", Size: size}
	}
	tests := []struct {
		name string
		atts []model.Attachment
		has  bool
		want []string
	}{
		{
			name: "size next to the name, because that is what proves it went",
			atts: []model.Attachment{pdf("invoice.pdf", 58648)},
			want: []string{"invoice.pdf · 57.3KB"},
		},
		{
			name: "an unnamed part falls back to its type",
			atts: []model.Attachment{{ContentType: "application/octet-stream", PartPath: "3", Size: 12}},
			want: []string{"application/octet-stream · 12B"},
		},
		{
			name: "a part with neither is named by where it sits",
			atts: []model.Attachment{{PartPath: "3", Size: 12}},
			want: []string{"part 3 · 12B"},
		},
		{
			name: "inline parts are counted, never listed",
			atts: []model.Attachment{
				pdf("invoice.pdf", 1024),
				{Filename: "logo.png", ContentType: "image/png", Inline: true, Size: 90},
				{Filename: "sig.png", ContentType: "image/png", Inline: true, Size: 90},
			},
			want: []string{"invoice.pdf · 1KB", "+2 inline"},
		},
		{
			name: "one over the cap is listed rather than truncated, since it costs the same line",
			atts: []model.Attachment{pdf("a", 1), pdf("b", 1), pdf("c", 1), pdf("d", 1), pdf("e", 1)},
			want: []string{"a · 1B", "b · 1B", "c · 1B", "d · 1B", "e · 1B"},
		},
		{
			name: "past that the tail is a count",
			atts: []model.Attachment{pdf("a", 1), pdf("b", 1), pdf("c", 1), pdf("d", 1), pdf("e", 1), pdf("f", 1)},
			want: []string{"a · 1B", "b · 1B", "c · 1B", "d · 1B", "+2 more"},
		},
		{
			name: "the index's flag is the fallback when no rows name the files",
			has:  true,
			want: []string{"yes"},
		},
		{
			name: "and nothing is claimed when there is nothing",
			want: nil,
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got := attachmentDescs(tc.atts, tc.has)
			if strings.Join(got, "|") != strings.Join(tc.want, "|") {
				t.Errorf("attachmentDescs = %q, want %q", got, tc.want)
			}
		})
	}
}
