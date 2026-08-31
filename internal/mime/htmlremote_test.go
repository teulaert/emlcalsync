package mime

import (
	"context"
	"errors"
	"slices"
	"strings"
	"sync"
	"testing"
)

// remoteMail wraps an HTML body in the smallest message that carries it.
func remoteMail(body string) []byte {
	return []byte("From: Shop <news@example.com>\r\n" +
		"To: me@example.com\r\n" +
		"Subject: Midsummer sale\r\n" +
		"Date: Mon, 24 Aug 2026 09:00:00 +0000\r\n" +
		"Message-ID: <r-1@example.com>\r\n" +
		"MIME-Version: 1.0\r\n" +
		"Content-Type: text/html; charset=utf-8\r\n\r\n" + body + "\r\n")
}

// recorder is a FetchFunc that answers everything the same way and remembers
// what it was asked for.
type recorder struct {
	mu    sync.Mutex
	got   []string
	data  []byte
	ctype string
	err   error
}

func (r *recorder) fetch(_ context.Context, url string) ([]byte, string, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.got = append(r.got, url)
	if r.err != nil {
		return nil, "", r.err
	}
	return r.data, r.ctype, nil
}

func (r *recorder) urls() []string {
	r.mu.Lock()
	defer r.mu.Unlock()
	out := slices.Clone(r.got)
	slices.Sort(out)
	return out
}

func newRecorder() *recorder {
	return &recorder{data: []byte("GIF89a"), ctype: "image/gif"}
}

// Mail points at pictures in every way HTML has ever offered, and a page that
// only understood <img src> would still come out full of holes.
func TestHTMLDocumentFoldsInRemotePictures(t *testing.T) {
	raw := remoteMail(`<html><head>` +
		`<style>.hero{background-image:url('https://cdn.example.com/bg.png')}</style>` +
		`</head><body background="https://cdn.example.com/paper.gif">` +
		`<img src="https://cdn.example.com/hero.png" srcset="https://cdn.example.com/hero@2x.png 2x" width="600">` +
		`<div style="background:url(https://cdn.example.com/dot.gif)">Half off</div>` +
		`<p>Write to url(https://cdn.example.com/prose.png) for details</p>` +
		`</body></html>`)

	rec := newRecorder()
	doc, err := HTMLDocument(t.Context(), raw, HTMLDocOptions{Fetch: rec.fetch})
	if err != nil {
		t.Fatal(err)
	}
	want := []string{
		"https://cdn.example.com/bg.png",
		"https://cdn.example.com/dot.gif",
		"https://cdn.example.com/hero.png",
		"https://cdn.example.com/paper.gif",
	}
	if got := rec.urls(); !slices.Equal(got, want) {
		t.Errorf("fetched\n %q\nwant\n %q", got, want)
	}

	page := string(doc)
	// srcset would have won over the src beside it, and it is a list rather
	// than a URL, so it is dropped instead of followed.
	if strings.Contains(page, "hero@2x") {
		t.Error("srcset survived; the browser would prefer it and load nothing")
	}
	// url() written in prose is prose.
	if !strings.Contains(page, "url(https://cdn.example.com/prose.png)") {
		t.Error("a url() in the message's own text was rewritten")
	}
	for _, gone := range []string{"bg.png", "dot.gif", "hero.png", "paper.gif"} {
		if strings.Contains(page, "cdn.example.com/"+gone) {
			t.Errorf("%s is still pointed at the CDN", gone)
		}
	}
	if n := strings.Count(page, "data:image/gif;base64,"); n != 4 {
		t.Errorf("%d data: URIs in the page, want 4", n)
	}
	if !strings.Contains(page, "4 pictures came from the sender's servers") {
		t.Error("the header block does not say the pictures were fetched")
	}
}

// A URL in an attribute is HTML-escaped; what goes on the wire is not.
func TestHTMLDocumentUnescapesRemoteURLs(t *testing.T) {
	raw := remoteMail(`<html><body><img src="https://cdn.example.com/p.gif?a=1&amp;b=2"></body></html>`)

	rec := newRecorder()
	if _, err := HTMLDocument(t.Context(), raw, HTMLDocOptions{Fetch: rec.fetch}); err != nil {
		t.Fatal(err)
	}
	want := []string{"https://cdn.example.com/p.gif?a=1&b=2"}
	if got := rec.urls(); !slices.Equal(got, want) {
		t.Errorf("fetched %q, want %q", got, want)
	}
}

// A picture that cannot be had keeps its reference: a broken image says
// "there was a picture here", which is truer than a gap.
func TestHTMLDocumentKeepsUnfetchablePictures(t *testing.T) {
	raw := remoteMail(`<html><body><img src="https://cdn.example.com/gone.png"></body></html>`)

	rec := newRecorder()
	rec.err = errors.New("404")
	doc, err := HTMLDocument(t.Context(), raw, HTMLDocOptions{Fetch: rec.fetch})
	if err != nil {
		t.Fatal(err)
	}
	page := string(doc)
	if !strings.Contains(page, "https://cdn.example.com/gone.png") {
		t.Error("the reference was dropped rather than left to break visibly")
	}
	if !strings.Contains(page, "1 picture hosted elsewhere could not be fetched") {
		t.Errorf("the header block does not report the failure:\n%s", page[:min(len(page), 900)])
	}
}

// The budget is one pot: what the message brought with it is spent first,
// and a download that no longer fits is left alone rather than truncated.
func TestHTMLDocumentRemoteRespectsTheBudget(t *testing.T) {
	raw := remoteMail(`<html><body><img src="https://cdn.example.com/big.png"></body></html>`)

	rec := newRecorder()
	rec.data = []byte("0123456789")
	doc, err := HTMLDocument(t.Context(), raw, HTMLDocOptions{Fetch: rec.fetch, MaxInlineBytes: 4})
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(doc), "https://cdn.example.com/big.png") {
		t.Error("a picture over the budget was inlined anyway")
	}
}

// Nothing is fetched when the option is not set, and the page says so.
func TestHTMLDocumentWithoutFetchAsksNobody(t *testing.T) {
	raw := remoteMail(`<html><body><img src="https://tracker.example.com/pixel.gif"></body></html>`)

	doc, err := HTMLDocument(t.Context(), raw, HTMLDocOptions{})
	if err != nil {
		t.Fatal(err)
	}
	page := string(doc)
	if !strings.Contains(page, "https://tracker.example.com/pixel.gif") {
		t.Error("the reference was rewritten with no fetcher to rewrite it from")
	}
	if !strings.Contains(page, "left out") {
		t.Error("the page does not say the pictures were left out")
	}
}

func TestIsRemoteURL(t *testing.T) {
	for _, tc := range []struct {
		in   string
		want bool
	}{
		{"https://example.com/a.png", true},
		{"http://example.com/a.png", true},
		{"HTTPS://EXAMPLE.COM/a.png", true},
		{"//example.com/a.png", false}, // protocol-relative: no base to resolve against
		{"/images/a.png", false},
		{"cid:photo1@example.net", false},
		{"data:image/png;base64,AAAA", false},
		{"mailto:me@example.com", false},
		{"https://", false},
		{"", false},
	} {
		if got := isRemoteURL(tc.in); got != tc.want {
			t.Errorf("isRemoteURL(%q) = %v, want %v", tc.in, got, tc.want)
		}
	}
}
