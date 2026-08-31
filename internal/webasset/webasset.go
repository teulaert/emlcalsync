// Package webasset fetches the pictures an HTML message points at somewhere
// else, so the page `mail open` hands to the browser can carry them inside it
// and still declare that it may load nothing.
//
// Doing the fetch here rather than letting the browser do it is the whole
// point. A browser asking a tracking host for a pixel sends the cookies it
// holds for that host, which hands the sender an account identity rather than
// an IP address; it also sends its own headers and warms its cache with the
// result. This client has no cookie jar, sends no referrer, gives up quickly,
// caps what it will read, follows few redirects, and refuses to dial a
// loopback or private address -- a message aiming an <img> at the router's
// admin page is an old trick and there is no reason to play along.
//
// What it cannot hide is the fetch itself. Asking for a URL minted per
// recipient tells the sender the message was opened, and only fetching every
// message's pictures ahead of any read would not. That is a bigger machine
// than this one; `remote_content = false` is the answer until it exists.
package webasset

import (
	"context"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"syscall"
	"time"
)

const (
	// DefaultMaxBytes is as much as one asset may weigh. A hero image in
	// marketing mail runs to a few hundred kilobytes; five megabytes is an
	// unreasonable picture and a reasonable place to stop reading.
	DefaultMaxBytes = 5 << 20
	// DefaultTimeout covers one asset end to end. Opening a message should
	// not wait on a host that has gone away, and a picture that misses the
	// deadline leaves a broken image rather than an empty page.
	DefaultTimeout = 15 * time.Second

	maxRedirects = 5

	// userAgent is plain and honest. Sending Go's default gets a fair number
	// of CDNs to answer 403, and claiming to be a specific browser would be a
	// lie told to no purpose.
	userAgent = "Mozilla/5.0 (compatible; emlcal)"
)

// Fetcher pulls one asset at a time over HTTP. The zero value is not usable;
// call New.
type Fetcher struct {
	// MaxBytes caps one asset. Zero means DefaultMaxBytes.
	MaxBytes int64
	// Timeout covers one asset. Zero means DefaultTimeout.
	Timeout time.Duration
	// AllowPrivate lets the fetcher dial loopback and private addresses.
	// Only a test wants this: it is how httptest servers are reachable.
	AllowPrivate bool

	once   sync.Once
	client *http.Client
}

// New returns a Fetcher with the defaults above.
func New() *Fetcher {
	return &Fetcher{MaxBytes: DefaultMaxBytes, Timeout: DefaultTimeout}
}

// Fetch returns the bytes of one remote asset and the content type to write
// into its data: URI. Every failure is ordinary -- the caller leaves the
// reference alone and the browser shows a broken image, which says "there was
// a picture here" and is truer than silence.
func (f *Fetcher) Fetch(ctx context.Context, rawURL string) ([]byte, string, error) {
	u, err := url.Parse(rawURL)
	if err != nil {
		return nil, "", fmt.Errorf("parse %q: %w", rawURL, err)
	}
	switch strings.ToLower(u.Scheme) {
	case "http", "https":
	default:
		return nil, "", fmt.Errorf("%s: not an http url", rawURL)
	}

	ctx, cancel := context.WithTimeout(ctx, f.timeout())
	defer cancel()

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, u.String(), nil)
	if err != nil {
		return nil, "", err
	}
	req.Header.Set("User-Agent", userAgent)
	req.Header.Set("Accept", "image/*,*/*;q=0.5")
	// No Referer: the sender does not get to learn where the request came
	// from, and there is no page it could honestly name anyway.

	resp, err := f.http().Do(req)
	if err != nil {
		return nil, "", err
	}
	defer func() {
		_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, 1<<16))
		_ = resp.Body.Close()
	}()
	if resp.StatusCode != http.StatusOK {
		return nil, "", fmt.Errorf("%s: %s", rawURL, resp.Status)
	}

	max := f.maxBytes()
	data, err := io.ReadAll(io.LimitReader(resp.Body, max+1))
	if err != nil {
		return nil, "", err
	}
	if int64(len(data)) > max {
		return nil, "", fmt.Errorf("%s: larger than %d bytes", rawURL, max)
	}
	if len(data) == 0 {
		return nil, "", fmt.Errorf("%s: empty", rawURL)
	}

	ctype, ok := assetType(resp.Header.Get("Content-Type"), data)
	if !ok {
		// A tracker that answers a pixel request with an HTML page, or a
		// CDN's error document. Inlining it would paint the error where the
		// picture was; a broken image is the better lie-free outcome.
		return nil, "", fmt.Errorf("%s: not a picture or a font", rawURL)
	}
	return data, ctype, nil
}

func (f *Fetcher) maxBytes() int64 {
	if f.MaxBytes > 0 {
		return f.MaxBytes
	}
	return DefaultMaxBytes
}

func (f *Fetcher) timeout() time.Duration {
	if f.Timeout > 0 {
		return f.Timeout
	}
	return DefaultTimeout
}

// http builds the client once. It is deliberately not http.DefaultClient:
// the point of this package is the things this client does not do.
func (f *Fetcher) http() *http.Client {
	f.once.Do(func() {
		dialer := &net.Dialer{
			Timeout:   10 * time.Second,
			KeepAlive: 30 * time.Second,
			Control:   f.control,
		}
		f.client = &http.Client{
			// Jar stays nil. A cookie would turn "somebody opened this" into
			// "this account opened this", which is the leak worth closing.
			Jar: nil,
			Transport: &http.Transport{
				DialContext:           dialer.DialContext,
				TLSHandshakeTimeout:   10 * time.Second,
				ResponseHeaderTimeout: 10 * time.Second,
				DisableKeepAlives:     false,
				MaxIdleConnsPerHost:   4,
			},
			CheckRedirect: func(req *http.Request, via []*http.Request) error {
				if len(via) >= maxRedirects {
					return fmt.Errorf("stopped after %d redirects", maxRedirects)
				}
				return nil
			},
		}
	})
	return f.client
}

// control runs after the address is resolved and before the socket is
// connected, which is the only place a redirect to a name that resolves to
// 127.0.0.1 can still be caught.
func (f *Fetcher) control(_, address string, _ syscall.RawConn) error {
	if f.AllowPrivate {
		return nil
	}
	host, _, err := net.SplitHostPort(address)
	if err != nil {
		host = address
	}
	ip := net.ParseIP(host)
	if ip == nil {
		return nil
	}
	switch {
	case ip.IsLoopback(), ip.IsPrivate(), ip.IsUnspecified(),
		ip.IsLinkLocalUnicast(), ip.IsLinkLocalMulticast():
		return fmt.Errorf("refusing to fetch from %s: not a public address", ip)
	}
	return nil
}

// assetType decides what a response may be inlined as. Mail asks for
// pictures and the occasional web font; anything else -- an HTML error page
// above all -- is refused rather than painted into the message.
func assetType(header string, data []byte) (string, bool) {
	m := strings.ToLower(strings.TrimSpace(strings.SplitN(header, ";", 2)[0]))
	if m == "" || m == "application/octet-stream" || m == "binary/octet-stream" {
		m = strings.ToLower(strings.SplitN(http.DetectContentType(data), ";", 2)[0])
	}
	switch {
	case strings.HasPrefix(m, "image/"), strings.HasPrefix(m, "font/"):
		return m, true
	case strings.HasPrefix(m, "application/font-"),
		m == "application/vnd.ms-fontobject",
		m == "application/x-font-ttf":
		return m, true
	}
	return "", false
}
