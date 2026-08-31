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

// remotePicMail is the shape the picture question is about: a message whose
// pictures live on the sender's CDN, so that leaving them out leaves holes.
const remotePicMail = "From: Shop <news@example.com>\r\n" +
	"To: work@example.com\r\n" +
	"Subject: Midsummer sale\r\n" +
	"Date: Mon, 24 Aug 2026 09:00:00 +0000\r\n" +
	"Message-ID: <pix-1@example.com>\r\n" +
	"MIME-Version: 1.0\r\n" +
	"Content-Type: text/html; charset=utf-8\r\n\r\n" +
	`<html><body><img src="https://cdn.example.com/hero.png" width="600">` +
	`<div>Everything half off</div></body></html>` + "\r\n"

// o follows general.remote_content on the pictures a message hosts elsewhere,
// and O reverses it -- for the newsletter that is all pictures, and for the
// message one would rather read without telling anyone.
func TestBrowserPicturesFollowTheConfiguration(t *testing.T) {
	d, mail := newTriageDeps(t)
	mail.Add(fake.NewMsg("pix-1", []byte(remotePicMail)).WithMailboxes("INBOX"))
	if _, err := d.Engine.SyncAccount(context.Background(), "work", sync.SyncOptions{}); err != nil {
		t.Fatalf("sync: %v", err)
	}
	opened := &[]string{}
	d.ViewDir = filepath.Join(t.TempDir(), "view")
	d.Browser = func(url string) error {
		*opened = append(*opened, url)
		return nil
	}
	// A channel rather than a slice: the fetches run in parallel.
	fetched := make(chan string, 8)
	d.Fetch = func(_ context.Context, url string) ([]byte, string, error) {
		fetched <- url
		return []byte("GIF89a"), "image/gif", nil
	}

	// remote_content = false: o asks nobody, and the reference stays put so
	// the reader can see a picture was meant to be there.
	r := newTestRoot(t, d)
	send(t, r, "o")
	if n := len(fetched); n != 0 {
		t.Errorf("o fetched %d pictures with remote_content = false", n)
	}
	if page := lastPage(t, opened); !strings.Contains(page, "https://cdn.example.com/hero.png") {
		t.Error("the reference was rewritten even though nothing was fetched")
	}

	// O reverses it, one message at a time.
	send(t, r, "O")
	if n := len(fetched); n != 1 {
		t.Fatalf("O fetched %d pictures, want 1", n)
	}
	if got := <-fetched; got != "https://cdn.example.com/hero.png" {
		t.Errorf("fetched %q", got)
	}
	page := lastPage(t, opened)
	if strings.Contains(page, "https://cdn.example.com/hero.png") {
		t.Error("the picture was fetched but the page still points at the CDN")
	}
	if !strings.Contains(page, "data:image/gif;base64,") {
		t.Error("the picture did not arrive as a data: URI")
	}
	if !strings.Contains(page, "Content-Security-Policy") {
		t.Error("fetching the pictures dropped the policy off the page")
	}

	// And with the configuration the other way, o is the one that fetches.
	d.Config.General.RemoteContent = true
	r2 := newTestRoot(t, d)
	send(t, r2, "o")
	if n := len(fetched); n != 1 {
		t.Errorf("o fetched %d pictures with remote_content = true, want 1", n)
	}
}

// lastPage reads the page behind the most recent URL handed to the browser.
func lastPage(t *testing.T, opened *[]string) string {
	t.Helper()
	if len(*opened) == 0 {
		t.Fatal("the browser was handed nothing")
	}
	b, err := os.ReadFile(strings.TrimPrefix((*opened)[len(*opened)-1], "file://"))
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}
