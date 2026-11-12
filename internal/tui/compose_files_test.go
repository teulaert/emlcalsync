package tui

import (
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// composerWithFiles opens a reply and puts the named files on it with ctrl+o,
// with the desktop opener recorded rather than run.
func composerWithFiles(t *testing.T, files ...string) (*root, *composeView, *[]string) {
	t.Helper()
	d := newTestDeps(t, "work")
	addConversation(t, d, "work", "w1", "t1")
	d.ViewDir = filepath.Join(t.TempDir(), "view")
	d.DownloadDir = filepath.Join(t.TempDir(), "Downloads")
	opened := &[]string{}
	d.Browser = func(u string) error {
		*opened = append(*opened, u)
		return nil
	}
	r := newTestRoot(t, d)
	send(t, r, "r")
	dir := t.TempDir()
	for _, name := range files {
		send(t, r, "ctrl+o")
		typeText(t, r, writeFile(t, dir, name, "content of "+name))
		send(t, r, "enter")
	}
	c := composerOn(t, r)
	if len(c.files) != len(files) {
		t.Fatalf("files = %d, want %d: %v", len(c.files), len(files), c.err)
	}
	return r, c, opened
}

func openedContent(t *testing.T, u string) string {
	t.Helper()
	parsed, err := url.Parse(u)
	if err != nil {
		t.Fatalf("parse %q: %v", u, err)
	}
	data, err := os.ReadFile(parsed.Path)
	if err != nil {
		t.Fatalf("read what was opened: %v", err)
	}
	return string(data)
}

// Tab reaches the Files row, the arrows pick a file, and enter opens it.
func TestComposerFilesRowOpensTheFileUnderTheCursor(t *testing.T) {
	r, c, opened := composerWithFiles(t, "foto1.jpg", "foto2.jpg")

	send(t, r, "shift+tab") // from the body back onto Files
	if c.focus != filesFocus {
		t.Fatalf("focus = %d, want the Files row", c.focus)
	}
	send(t, r, "right")
	send(t, r, "enter")

	if len(*opened) != 1 {
		t.Fatalf("opened %v, want one file", *opened)
	}
	if got := openedContent(t, (*opened)[0]); got != "content of foto2.jpg" {
		t.Errorf("opened %q, want the second photo", got)
	}
	if !strings.HasSuffix((*opened)[0], "foto2.jpg") {
		t.Errorf("opened %s, want it under its own name", (*opened)[0])
	}
}

// Keys on the Files row are not text: nothing typed there lands anywhere.
func TestComposerFilesRowTakesNoText(t *testing.T) {
	r, c, _ := composerWithFiles(t, "a.pdf")
	before := [5]string{c.to.Value(), c.cc.Value(), c.bcc.Value(), c.subj.Value(), c.body.Value()}

	send(t, r, "shift+tab")
	typeText(t, r, "zq")
	send(t, r, "enter") // opens, does not add a line

	after := [5]string{c.to.Value(), c.cc.Value(), c.bcc.Value(), c.subj.Value(), c.body.Value()}
	if before != after {
		t.Errorf("typing on the Files row changed a field: %q -> %q", before, after)
	}
}

// w saves the file under the cursor to the downloads folder.
func TestComposerFilesRowSaves(t *testing.T) {
	r, _, _ := composerWithFiles(t, "contract.pdf")
	send(t, r, "shift+tab")
	send(t, r, "w")

	data, err := os.ReadFile(filepath.Join(r.d.DownloadDir, "contract.pdf"))
	if err != nil || string(data) != "content of contract.pdf" {
		t.Errorf("saved %q, %v", data, err)
	}
}

// delete takes the chosen file off, not the last; taking the last one off
// puts the cursor in the body, since the row is gone.
func TestComposerFilesRowTakesTheChosenFileOff(t *testing.T) {
	r, c, _ := composerWithFiles(t, "a.pdf", "b.pdf")
	send(t, r, "shift+tab")
	send(t, r, "delete") // the cursor is on a.pdf

	if len(c.files) != 1 || c.files[0].Filename != "b.pdf" {
		t.Fatalf("files = %+v, want b.pdf left", c.files)
	}
	if c.focus != filesFocus {
		t.Fatalf("focus = %d, want still on Files", c.focus)
	}
	send(t, r, "delete")
	if len(c.files) != 0 || c.focus != bodyFocus {
		t.Errorf("files = %d, focus = %d; want none, in the body", len(c.files), c.focus)
	}
	if got := r.render(); strings.Contains(got, " Files") {
		t.Errorf("the Files row is still drawn:\n%s", got)
	}
}

// Without a file on the message the row is not a stop: shift+tab from the
// body goes to Subject as it always has.
func TestComposerSkipsTheFilesRowWithNothingOnIt(t *testing.T) {
	d := newTestDeps(t, "work")
	addConversation(t, d, "work", "w1", "t1")
	r := newTestRoot(t, d)
	send(t, r, "r")
	c := composerOn(t, r)
	send(t, r, "shift+tab")
	if c.focus != 3 {
		t.Errorf("focus = %d, want Subject", c.focus)
	}
}

// The file under the cursor is the one marked on screen.
func TestComposerFilesRowMarksTheChosenFile(t *testing.T) {
	r, c, _ := composerWithFiles(t, "a.pdf", "b.pdf")
	send(t, r, "shift+tab")
	send(t, r, "right")
	row := c.filesView(80)
	if !strings.Contains(row, styleSelected.Render("b.pdf 16B")) {
		t.Errorf("b.pdf is not marked in %q", row)
	}
}
