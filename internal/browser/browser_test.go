package browser

import (
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"
)

func TestFileURL(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "page.html")

	got, err := FileURL(path)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.HasPrefix(got, "file://") {
		t.Errorf("FileURL(%q) = %q, want a file:// URL", path, got)
	}
	if !strings.HasSuffix(got, "/page.html") {
		t.Errorf("FileURL(%q) = %q, lost the file name", path, got)
	}
}

// A relative path would resolve against the browser's working directory
// rather than ours, which is somebody else's directory entirely.
func TestFileURLIsAbsolute(t *testing.T) {
	got, err := FileURL("page.html")
	if err != nil {
		t.Fatal(err)
	}
	if strings.HasPrefix(got, "file://page.html") {
		t.Errorf("FileURL kept a relative path: %q", got)
	}
}

func TestWritePage(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "view")
	now := time.Now()

	path, err := WritePage(dir, "work:18f3a2b9", []byte("<p>hi</p>"), now)
	if err != nil {
		t.Fatal(err)
	}
	if !filepath.IsAbs(path) {
		t.Errorf("WritePage returned a relative path: %q", path)
	}
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if string(b) != "<p>hi</p>" {
		t.Errorf("page content = %q", b)
	}
	// The id is not a file name: nothing in it may name a directory.
	if base := filepath.Base(path); strings.ContainsAny(base, `:/\`) {
		t.Errorf("page name %q still carries path characters", base)
	}

	if runtime.GOOS != "windows" {
		fi, err := os.Stat(path)
		if err != nil {
			t.Fatal(err)
		}
		if fi.Mode().Perm() != 0o600 {
			t.Errorf("page mode = %v, want 0600: a rendered message is the message", fi.Mode().Perm())
		}
		di, err := os.Stat(dir)
		if err != nil {
			t.Fatal(err)
		}
		if di.Mode().Perm() != 0o700 {
			t.Errorf("view dir mode = %v, want 0700", di.Mode().Perm())
		}
	}
}

// Opening the same message twice reuses the file rather than growing the
// directory a page at a time.
func TestWritePageReusesTheName(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "view")
	now := time.Now()

	first, err := WritePage(dir, "work:abc", []byte("one"), now)
	if err != nil {
		t.Fatal(err)
	}
	second, err := WritePage(dir, "work:abc", []byte("two"), now)
	if err != nil {
		t.Fatal(err)
	}
	if first != second {
		t.Errorf("second write went to %q, want %q", second, first)
	}
	entries, _ := os.ReadDir(dir)
	if len(entries) != 1 {
		t.Errorf("view dir holds %d files, want 1", len(entries))
	}
	if b, _ := os.ReadFile(second); string(b) != "two" {
		t.Errorf("page was not rewritten: %q", b)
	}
}

func TestWritePageSweepsStalePages(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "view")
	now := time.Now()

	stale, err := WritePage(dir, "old", []byte("old"), now)
	if err != nil {
		t.Fatal(err)
	}
	keep, err := WritePage(dir, "recent", []byte("recent"), now)
	if err != nil {
		t.Fatal(err)
	}
	// Age the first one past the TTL, and something else that is not ours.
	old := now.Add(-pageTTL - time.Hour)
	if err := os.Chtimes(stale, old, old); err != nil {
		t.Fatal(err)
	}
	notes := filepath.Join(dir, "notes.txt")
	if err := os.WriteFile(notes, []byte("x"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Chtimes(notes, old, old); err != nil {
		t.Fatal(err)
	}

	if _, err := WritePage(dir, "another", []byte("another"), now); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(stale); !os.IsNotExist(err) {
		t.Errorf("stale page survived the sweep: %v", err)
	}
	if _, err := os.Stat(keep); err != nil {
		t.Errorf("recent page was swept: %v", err)
	}
	if _, err := os.Stat(notes); err != nil {
		t.Errorf("the sweep took a file that was not ours: %v", err)
	}
}
