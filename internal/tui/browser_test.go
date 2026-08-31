package tui

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/teulaert/emlcalsync/internal/provider/fake"
	"github.com/teulaert/emlcalsync/internal/sync"
)

// openOTP seeds the message this escape hatch exists for -- HTML-only, with a
// <style> tag html2text loses the body of -- and returns a root sitting on
// the mail list, plus the URLs o would have handed the desktop.
func openOTP(t *testing.T) (*root, Deps, *[]string) {
	t.Helper()
	d, mail := newTriageDeps(t)
	raw, err := os.ReadFile(filepath.Join("..", "mime", "testdata", "html_style_selectors.eml"))
	if err != nil {
		t.Fatal(err)
	}
	mail.Add(fake.NewMsg("otp-1", raw).WithMailboxes("INBOX"))
	if _, err := d.Engine.SyncAccount(context.Background(), "work", sync.SyncOptions{}); err != nil {
		t.Fatalf("sync: %v", err)
	}

	opened := &[]string{}
	d.ViewDir = filepath.Join(t.TempDir(), "view")
	d.Browser = func(url string) error {
		*opened = append(*opened, url)
		return nil
	}
	return newTestRoot(t, d), d, opened
}

func TestBrowserOpensTheMessageInFocus(t *testing.T) {
	r, _, opened := openOTP(t)

	send(t, r, "enter") // the thread
	send(t, r, "enter") // the reader
	rd, ok := r.top().(*reader)
	if !ok {
		t.Fatalf("top screen is %T, want the reader", r.top())
	}
	// The reason o exists: the extracted body has no code in it.
	if strings.Contains(rd.body, "678863") {
		t.Log("note: text extraction now finds the code; o is still the escape hatch")
	}

	send(t, r, "o")
	if len(*opened) != 1 {
		t.Fatalf("browser was handed %d URLs, want 1: %v", len(*opened), *opened)
	}
	url := (*opened)[0]
	if !strings.HasPrefix(url, "file://") {
		t.Errorf("opened %q, want a file:// URL", url)
	}
	page, err := os.ReadFile(strings.TrimPrefix(url, "file://"))
	if err != nil {
		t.Fatalf("read the page: %v", err)
	}
	if !strings.Contains(string(page), "678863") {
		t.Error("the page lost the code, which is the whole point of it")
	}
	if !strings.Contains(string(page), "Content-Security-Policy") {
		t.Error("no policy: opening the message would fire its tracking pixels")
	}
	if !strings.Contains(r.status, "browser") {
		t.Errorf("status line says %q, nothing about the browser", r.status)
	}
}

// A list row is a conversation rather than a message, so o opens the newest
// message in it -- the one the row is showing.
func TestBrowserOpensFromTheList(t *testing.T) {
	r, _, opened := openOTP(t)

	if _, ok := r.top().(*mailList); !ok {
		t.Fatalf("top screen is %T, want the mail list", r.top())
	}
	send(t, r, "o")
	if len(*opened) != 1 {
		t.Fatalf("browser was handed %d URLs, want 1: %v", len(*opened), *opened)
	}
	page, err := os.ReadFile(strings.TrimPrefix((*opened)[0], "file://"))
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(page), "678863") {
		t.Error("the page is not the message the row was showing")
	}
}

// The composer takes every key it is given; o there is a letter being typed.
func TestBrowserKeyIsNotTakenFromTheComposer(t *testing.T) {
	r, _, opened := openOTP(t)

	send(t, r, "enter")
	send(t, r, "enter")
	send(t, r, "r") // the composer
	if _, ok := r.top().(*composeView); !ok {
		t.Fatalf("top screen is %T, want the composer", r.top())
	}
	send(t, r, "o")
	if len(*opened) != 0 {
		t.Errorf("o in the composer launched a browser: %v", *opened)
	}
}

func TestBrowserReportsAFailureToOpen(t *testing.T) {
	r, _, _ := openOTP(t)
	r.d.Browser = func(string) error { return errNoBrowser }

	send(t, r, "enter")
	send(t, r, "enter")
	send(t, r, "o")
	if !strings.Contains(r.status, errNoBrowser.Error()) {
		t.Errorf("status line says %q, nothing about the failure", r.status)
	}
}

var errNoBrowser = errors.New("no browser on this desktop")
