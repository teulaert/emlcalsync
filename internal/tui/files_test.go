package tui

import (
	"bytes"
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/teulaert/emlcalsync/internal/mime"
	"github.com/teulaert/emlcalsync/internal/model"
	"github.com/teulaert/emlcalsync/internal/provider/fake"
	"github.com/teulaert/emlcalsync/internal/sync"
)

// openFiles seeds the forwarded receipt -- two PDFs and an inline logo --
// and returns a root on the mail list, the URLs enter would have handed the
// desktop, the folder w saves into, and the raw message the parts came from.
func openFiles(t *testing.T) (*root, *[]string, string, []byte) {
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
	opened := &[]string{}
	d.ViewDir = filepath.Join(t.TempDir(), "view")
	d.DownloadDir = filepath.Join(t.TempDir(), "Downloads")
	d.Browser = func(url string) error {
		*opened = append(*opened, url)
		return nil
	}
	return newTestRoot(t, d), opened, d.DownloadDir, raw
}

// toFiles walks from the list into the reader and presses v.
func toFiles(t *testing.T, r *root) *filesView {
	t.Helper()
	send(t, r, "enter") // the thread
	send(t, r, "enter") // the reader
	send(t, r, "v")
	fv, ok := r.top().(*filesView)
	if !ok {
		t.Fatalf("top screen is %T, want the files screen", r.top())
	}
	return fv
}

func TestFilesScreenListsWhatTheMessageCarries(t *testing.T) {
	r, _, _, _ := openFiles(t)
	fv := toFiles(t, r)
	if len(fv.rows) != 3 {
		t.Fatalf("%d rows, want the two PDFs and the logo", len(fv.rows))
	}
	// The documents first, the letterhead after them.
	if fv.rows[0].att.Inline || fv.rows[1].att.Inline || !fv.rows[2].att.Inline {
		t.Errorf("rows are not files-then-inline: %+v", fv.rows)
	}
	view := fv.View(r.w, r.bodyHeight())
	for _, want := range []string{
		"Invoice-GRK6NZDJ-0025.pdf", "Receipt-2164-3490.pdf", "application/pdf",
		"logo.png", "inline", "part ",
	} {
		if !strings.Contains(view, want) {
			t.Errorf("files screen misses %q:\n%s", want, view)
		}
	}
	if !strings.Contains(r.top().footer(r.w), "enter open · w save to") {
		t.Errorf("footer says %q, not what the keys do", r.top().footer(r.w))
	}
	if !strings.HasPrefix(r.top().Title(), "files · ") {
		t.Errorf("title is %q", r.top().Title())
	}
}

func TestFilesOpenOnTheDesktop(t *testing.T) {
	r, opened, _, raw := openFiles(t)
	fv := toFiles(t, r)

	send(t, r, "enter")
	if len(*opened) != 1 {
		t.Fatalf("the desktop was handed %d files, want 1: %v", len(*opened), *opened)
	}
	url := (*opened)[0]
	if !strings.HasPrefix(url, "file://") || !strings.HasSuffix(url, "/Invoice-GRK6NZDJ-0025.pdf") {
		t.Errorf("opened %q, want a file:// URL ending in the invoice's own name", url)
	}
	got, err := os.ReadFile(strings.TrimPrefix(url, "file://"))
	if err != nil {
		t.Fatal(err)
	}
	want, _, _, err := mime.PartContent(raw, fv.rows[0].att.PartPath)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(got, want) {
		t.Error("the file handed to the desktop is not the part's bytes")
	}
	if !strings.Contains(r.status, "opened Invoice-GRK6NZDJ-0025.pdf") {
		t.Errorf("status line says %q", r.status)
	}
	if fv.busy != "" {
		t.Errorf("the screen still says it is fetching %q", fv.busy)
	}
	// It sits in the cache beside the pages, under the message, 0600.
	info, err := os.Stat(strings.TrimPrefix(url, "file://"))
	if err != nil {
		t.Fatal(err)
	}
	if info.Mode().Perm() != 0o600 {
		t.Errorf("file is %v, want 0600", info.Mode().Perm())
	}
	if !strings.HasPrefix(strings.TrimPrefix(url, "file://"), r.d.ViewDir) {
		t.Errorf("the file is at %s, not under the view directory", url)
	}
}

func TestFilesSaveToTheDownloadsFolder(t *testing.T) {
	r, opened, dl, raw := openFiles(t)
	fv := toFiles(t, r)

	send(t, r, "j")
	send(t, r, "w")
	if len(*opened) != 0 {
		t.Errorf("w launched something on the desktop: %v", *opened)
	}
	path := filepath.Join(dl, "Receipt-2164-3490.pdf")
	got, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("nothing saved: %v", err)
	}
	want, _, _, err := mime.PartContent(raw, fv.rows[1].att.PartPath)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(got, want) {
		t.Error("the saved file is not the part's bytes")
	}
	if !strings.Contains(r.status, "saved ") || !strings.Contains(r.status, "Receipt-2164-3490.pdf") {
		t.Errorf("status line says %q", r.status)
	}

	// Saving it again does not overwrite what is there.
	send(t, r, "w")
	if _, err := os.Stat(filepath.Join(dl, "Receipt-2164-3490 (1).pdf")); err != nil {
		t.Errorf("second save did not take the next name: %v", err)
	}
}

// A list row's mark is the whole conversation's, so v there lists every
// message's files, with a heading naming each message.
func TestFilesFromAListRowSpanTheConversation(t *testing.T) {
	r, _, _, _ := openFiles(t)
	list := r.top().(*mailList)
	th := list.threads[0]
	addMessage(t, r.d, "work", "reply-1", th.ThreadID, "Re: receipt", "anna", 0, false)

	send(t, r, "v")
	fv, ok := r.top().(*filesView)
	if !ok {
		t.Fatalf("top screen is %T, want the files screen", r.top())
	}
	if len(fv.rows) != 3 {
		t.Fatalf("%d rows, want the forward's three", len(fv.rows))
	}
	if fv.remote != "" {
		t.Errorf("screen is pinned to message %q; from a row it should span the thread", fv.remote)
	}
	// Only one message carries anything, so no headings are drawn.
	if fv.spansThread() {
		t.Error("rows from one message are said to span the thread")
	}
}

func TestFilesKeySaysSoWhenNothingIsAttached(t *testing.T) {
	d := newTestDeps(t, "work")
	addMessage(t, d, "work", "w1", "t1", "Just a note", "anna", 0, false)
	r := newTestRoot(t, d)

	send(t, r, "v")
	if _, ok := r.top().(*mailList); !ok {
		t.Errorf("v pushed a %T for a message without files", r.top())
	}
	if r.status != "nothing attached" {
		t.Errorf("status line says %q", r.status)
	}
	send(t, r, "enter")
	send(t, r, "enter")
	send(t, r, "v")
	if _, ok := r.top().(*reader); !ok {
		t.Errorf("v in the reader pushed a %T for a message without files", r.top())
	}
}

func TestFilesReportAFailureToOpen(t *testing.T) {
	r, _, _, _ := openFiles(t)
	fv := toFiles(t, r)
	r.d.Browser = func(string) error { return errors.New("no viewer on this desktop") }
	fv.d.Browser = r.d.Browser

	send(t, r, "enter")
	if !strings.Contains(r.status, "no viewer on this desktop") {
		t.Errorf("status line says %q, nothing about the failure", r.status)
	}
	if fv.busy != "" {
		t.Error("a failed open left the screen busy")
	}
}

func TestFileRowName(t *testing.T) {
	tests := []struct {
		att  model.Attachment
		want string
	}{
		{model.Attachment{Filename: "invoice.pdf", PartPath: "2"}, "invoice.pdf"},
		// A sender can type a path; only the name survives.
		{model.Attachment{Filename: "../../.bashrc", PartPath: "2"}, ".bashrc"},
		{model.Attachment{Filename: `C:\Users\x\report.docx`, PartPath: "2"}, "report.docx"},
		// No name: where it sits and what it is.
		{model.Attachment{ContentType: "application/pdf", PartPath: "1.2"}, "part-1-2.pdf"},
		{model.Attachment{ContentType: "image/png", PartPath: "3"}, "part-3.png"},
		{model.Attachment{PartPath: "3"}, "part-3"},
	}
	for _, tc := range tests {
		if got := (fileRow{att: tc.att}).name(); got != tc.want {
			t.Errorf("name(%+v) = %q, want %q", tc.att, got, tc.want)
		}
	}
}
