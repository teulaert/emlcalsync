package webasset

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

// gif is a real one-pixel GIF, so http.DetectContentType agrees with the
// header the test server sends.
var gif = []byte("GIF89a\x01\x00\x01\x00\x80\x00\x00\x00\x00\x00\xff\xff\xff!" +
	"\xf9\x04\x01\x00\x00\x00\x00,\x00\x00\x00\x00\x01\x00\x01\x00\x00\x02\x02D\x01\x00;")

// testFetcher is the real thing with the one concession a test needs: an
// httptest server lives on 127.0.0.1, which the fetcher otherwise refuses.
func testFetcher() *Fetcher {
	f := New()
	f.AllowPrivate = true
	return f
}

func TestFetchReturnsThePicture(t *testing.T) {
	var got *http.Request
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		got = r.Clone(r.Context())
		w.Header().Set("Content-Type", "image/gif")
		_, _ = w.Write(gif)
	}))
	defer srv.Close()

	data, ctype, err := testFetcher().Fetch(t.Context(), srv.URL+"/pixel.gif")
	if err != nil {
		t.Fatal(err)
	}
	if string(data) != string(gif) {
		t.Errorf("got %d bytes, want the %d-byte gif", len(data), len(gif))
	}
	if ctype != "image/gif" {
		t.Errorf("content type = %q", ctype)
	}
	// The two things a browser would have sent that this must not.
	if v := got.Header.Get("Cookie"); v != "" {
		t.Errorf("sent a cookie: %q", v)
	}
	if v := got.Header.Get("Referer"); v != "" {
		t.Errorf("sent a referrer: %q", v)
	}
}

// A tracker answering a pixel request with an HTML page is common enough.
// Inlining that would paint the error where the picture was.
func TestFetchRefusesWhatIsNotAPicture(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "text/html")
		_, _ = w.Write([]byte("<html><body>not found</body></html>"))
	}))
	defer srv.Close()

	if _, _, err := testFetcher().Fetch(t.Context(), srv.URL+"/pixel.gif"); err == nil {
		t.Fatal("an HTML page was accepted as a picture")
	}
}

// An unlabelled body is sniffed rather than trusted or refused outright.
func TestFetchSniffsAnUnlabelledBody(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/octet-stream")
		_, _ = w.Write(gif)
	}))
	defer srv.Close()

	_, ctype, err := testFetcher().Fetch(t.Context(), srv.URL+"/x")
	if err != nil {
		t.Fatal(err)
	}
	if ctype != "image/gif" {
		t.Errorf("content type = %q, want the sniffed image/gif", ctype)
	}
}

func TestFetchStopsAtTheSizeCap(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "image/gif")
		_, _ = w.Write(make([]byte, 4096))
	}))
	defer srv.Close()

	f := testFetcher()
	f.MaxBytes = 512
	if _, _, err := f.Fetch(t.Context(), srv.URL+"/big.gif"); err == nil {
		t.Fatal("a picture over the cap was returned in full")
	}
}

func TestFetchStopsGoingRoundInCircles(t *testing.T) {
	var srv *httptest.Server
	srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, srv.URL+"/next", http.StatusFound)
	}))
	defer srv.Close()

	if _, _, err := testFetcher().Fetch(t.Context(), srv.URL+"/a.gif"); err == nil {
		t.Fatal("followed redirects for ever")
	} else if !strings.Contains(err.Error(), "redirects") {
		t.Errorf("err = %v, want it to name the redirect limit", err)
	}
}

func TestFetchRefusesAStatusThatIsNotOK(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		http.Error(w, "gone", http.StatusNotFound)
	}))
	defer srv.Close()

	if _, _, err := testFetcher().Fetch(t.Context(), srv.URL+"/a.gif"); err == nil {
		t.Fatal("a 404 was treated as a picture")
	}
}

// A message aiming an <img> at the router's admin page is an old trick, and
// the archive is on the inside of the network it would be reaching into.
func TestFetchRefusesPrivateAddresses(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "image/gif")
		_, _ = w.Write(gif)
	}))
	defer srv.Close()

	// The real fetcher this time: AllowPrivate is off.
	if _, _, err := New().Fetch(t.Context(), srv.URL+"/pixel.gif"); err == nil {
		t.Fatal("dialled a loopback address")
	} else if !strings.Contains(err.Error(), "not a public address") {
		t.Errorf("err = %v, want it to name the reason", err)
	}
}

func TestFetchRefusesOtherSchemes(t *testing.T) {
	for _, u := range []string{"file:///etc/passwd", "ftp://example.com/a.gif", "data:image/gif;base64,AA"} {
		if _, _, err := testFetcher().Fetch(t.Context(), u); err == nil {
			t.Errorf("%s was fetched", u)
		}
	}
}

func TestFetchGivesUp(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		<-r.Context().Done()
	}))
	defer srv.Close()

	f := testFetcher()
	f.Timeout = 100 * time.Millisecond
	start := time.Now()
	if _, _, err := f.Fetch(t.Context(), srv.URL+"/slow.gif"); err == nil {
		t.Fatal("waited for a host that never answered")
	}
	if d := time.Since(start); d > 5*time.Second {
		t.Errorf("gave up after %v", d)
	}
}

func TestAssetType(t *testing.T) {
	for _, tc := range []struct {
		header string
		want   string
		ok     bool
	}{
		{"image/png", "image/png", true},
		{"image/svg+xml; charset=utf-8", "image/svg+xml", true},
		{"font/woff2", "font/woff2", true},
		{"application/font-woff", "application/font-woff", true},
		{"text/html", "", false},
		{"application/javascript", "", false},
		{"text/plain", "", false},
	} {
		got, ok := assetType(tc.header, []byte("xx"))
		if got != tc.want || ok != tc.ok {
			t.Errorf("assetType(%q) = %q, %v; want %q, %v", tc.header, got, ok, tc.want, tc.ok)
		}
	}
	if got, ok := assetType("", gif); !ok || got != "image/gif" {
		t.Errorf("assetType(\"\", gif) = %q, %v; want image/gif, true", got, ok)
	}
}
