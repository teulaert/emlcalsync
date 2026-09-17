package tui

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	tea "charm.land/bubbletea/v2"

	"github.com/teulaert/emlcalsync/internal/mime"
)

func writeFile(t *testing.T, dir, name, content string) string {
	t.Helper()
	p := filepath.Join(dir, name)
	if err := os.WriteFile(p, []byte(content), 0o600); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}
	return p
}

// ctrl+o puts a file off the disk on the message, and the message that goes
// out carries it.
func TestAttachingAFileSendsIt(t *testing.T) {
	d, mail := newTriageDeps(t)
	addAnswerable(t, d, mail, "m1", "Offerte")
	path := writeFile(t, t.TempDir(), "offerte.pdf", "%PDF-the-quote")

	r := newTestRoot(t, d)
	send(t, r, "r")
	send(t, r, "ctrl+o")
	typeText(t, r, path)
	send(t, r, "enter")

	c := composerOn(t, r)
	if c.attaching {
		t.Fatalf("the prompt is still open: %v", c.err)
	}
	if len(c.files) != 1 || c.files[0].Filename != "offerte.pdf" || c.files[0].ContentType != "application/pdf" {
		t.Fatalf("files = %+v, want offerte.pdf as a pdf", c.files)
	}
	if got := r.render(); !strings.Contains(got, "Files") || !strings.Contains(got, "offerte.pdf") {
		t.Errorf("the composer does not show the file it is sending:\n%s", got)
	}

	send(t, r, "ctrl+d")
	sent := mail.Sent()
	if len(sent) != 1 {
		t.Fatalf("the provider was handed %d messages, want 1", len(sent))
	}
	parsed, err := mime.Parse(sent[0])
	if err != nil {
		t.Fatalf("what went out does not parse: %v", err)
	}
	if len(parsed.Attachments) != 1 || parsed.Attachments[0].Filename != "offerte.pdf" {
		t.Fatalf("what went out carries %+v", parsed.Attachments)
	}
	data, _, _, err := mime.PartContent(sent[0], parsed.Attachments[0].Path)
	if err != nil || string(data) != "%PDF-the-quote" {
		t.Errorf("attachment content = %q, %v", data, err)
	}
}

// The path is typed into the prompt, not into the body under it.
func TestTheAttachPromptKeepsTheBodyClean(t *testing.T) {
	d := newTestDeps(t, "work")
	addConversation(t, d, "work", "w1", "t1")
	r := newTestRoot(t, d)
	send(t, r, "r")
	c := composerOn(t, r)
	before := c.body.Value()

	send(t, r, "ctrl+o")
	typeText(t, r, "/tmp/x")
	send(t, r, "esc")

	if c.attaching || c.body.Value() != before {
		t.Errorf("attaching = %v, body changed = %v", c.attaching, c.body.Value() != before)
	}
	if _, ok := r.top().(*composeView); !ok {
		t.Errorf("esc in the prompt closed the composer")
	}
}

// A path that will not read keeps the prompt open with the path in it.
func TestAttachingAMissingFileSaysSoAndKeepsThePath(t *testing.T) {
	d := newTestDeps(t, "work")
	addConversation(t, d, "work", "w1", "t1")
	r := newTestRoot(t, d)
	send(t, r, "r")
	c := composerOn(t, r)

	missing := filepath.Join(t.TempDir(), "nope.pdf")
	send(t, r, "ctrl+o")
	typeText(t, r, missing)
	send(t, r, "enter")

	if !c.attaching || c.path != missing || c.err == nil || len(c.files) != 0 {
		t.Errorf("attaching=%v path=%q err=%v files=%d", c.attaching, c.path, c.err, len(c.files))
	}
	if got := r.render(); !strings.Contains(got, "no such file") {
		t.Errorf("the failure is not on screen:\n%s", got)
	}
}

func TestADirectoryIsNotAnAttachment(t *testing.T) {
	d := newTestDeps(t, "work")
	addConversation(t, d, "work", "w1", "t1")
	r := newTestRoot(t, d)
	send(t, r, "r")
	c := composerOn(t, r)

	send(t, r, "ctrl+o")
	typeText(t, r, t.TempDir())
	send(t, r, "enter")
	if c.err == nil || len(c.files) != 0 {
		t.Errorf("err=%v files=%d, want a refusal", c.err, len(c.files))
	}
}

// tab completes as far as the disk is unambiguous, and lists the rest.
func TestTabCompletesThePath(t *testing.T) {
	d := newTestDeps(t, "work")
	addConversation(t, d, "work", "w1", "t1")
	dir := t.TempDir()
	writeFile(t, dir, "offerte-2026.pdf", "a")
	writeFile(t, dir, "offerte-2025.pdf", "b")
	writeFile(t, dir, "notes [final].txt", "c")
	if err := os.Mkdir(filepath.Join(dir, "scans"), 0o700); err != nil {
		t.Fatal(err)
	}

	r := newTestRoot(t, d)
	send(t, r, "r")
	c := composerOn(t, r)
	send(t, r, "ctrl+o")

	c.path = filepath.Join(dir, "off")
	send(t, r, "tab")
	if want := filepath.Join(dir, "offerte-202"); c.path != want {
		t.Errorf("path = %q, want %q", c.path, want)
	}
	if !strings.Contains(c.info, "offerte-2025.pdf") || !strings.Contains(c.info, "offerte-2026.pdf") {
		t.Errorf("the candidates are not offered: %q", c.info)
	}

	c.path = filepath.Join(dir, "sc")
	send(t, r, "tab")
	if want := filepath.Join(dir, "scans") + "/"; c.path != want {
		t.Errorf("path = %q, want the directory with its slash", c.path)
	}

	c.path = filepath.Join(dir, "notes [")
	send(t, r, "tab")
	if want := filepath.Join(dir, "notes [final].txt"); c.path != want {
		t.Errorf("path = %q, want %q", c.path, want)
	}
}

// A file dropped on the terminal arrives as a paste, dressed by the sender.
func TestAPastedPathIsUndressed(t *testing.T) {
	for in, want := range map[string]string{
		"'/tmp/a b.pdf'\n":      "/tmp/a b.pdf",
		`/tmp/a\ b.pdf`:         "/tmp/a b.pdf",
		"file:///tmp/a%20b.pdf": "/tmp/a b.pdf",
		"/tmp/plain.pdf":        "/tmp/plain.pdf",
	} {
		if got := cleanPastedPath(in); got != want {
			t.Errorf("cleanPastedPath(%q) = %q, want %q", in, got, want)
		}
	}

	d := newTestDeps(t, "work")
	addConversation(t, d, "work", "w1", "t1")
	path := writeFile(t, t.TempDir(), "a b.txt", "x")
	r := newTestRoot(t, d)
	send(t, r, "r")
	c := composerOn(t, r)
	send(t, r, "ctrl+o")
	_, cmd := r.Update(tea.PasteMsg{Content: "'" + path + "'"})
	drain(t, r, cmd)
	send(t, r, "enter")
	if len(c.files) != 1 || c.files[0].Filename != "a b.txt" {
		t.Errorf("files = %+v", c.files)
	}
}

// ctrl+r takes the last file off, and a file put on is work esc asks about.
func TestDetachTakesTheLastFileOff(t *testing.T) {
	d := newTestDeps(t, "work")
	addConversation(t, d, "work", "w1", "t1")
	path := writeFile(t, t.TempDir(), "a.txt", "x")
	r := newTestRoot(t, d)
	send(t, r, "r")
	c := composerOn(t, r)

	send(t, r, "ctrl+r")
	if c.err != errNothingAttached {
		t.Errorf("err = %v, want errNothingAttached", c.err)
	}

	send(t, r, "ctrl+o")
	typeText(t, r, path)
	send(t, r, "enter")
	if !c.edited() {
		t.Errorf("an attached file does not count as work")
	}
	send(t, r, "esc")
	if c.pending != pendingDiscard {
		t.Errorf("esc closed a composer holding a file without asking")
	}

	send(t, r, "ctrl+r")
	if len(c.files) != 0 {
		t.Errorf("files = %+v, want none", c.files)
	}
	if got := r.render(); strings.Contains(got, " Files") {
		t.Errorf("the Files row outlived its last file:\n%s", got)
	}
}
