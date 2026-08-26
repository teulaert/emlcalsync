package caldav

import (
	"bytes"
	"context"
	"encoding/xml"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strconv"
	"strings"
)

// maxErrorBody is how much of a failing response is kept in the error. CalDAV
// preconditions live in the first few hundred bytes; the rest is noise.
const maxErrorBody = 2048

// davClient is the thin WebDAV/CalDAV transport this package needs. It exists
// instead of go-webdav's client because emlcal needs three things that client
// does not offer: the raw .ics text (go-webdav decodes and discards it), a
// sync-collection REPORT (implemented for CardDAV only), and If-Match on PUT.
type davClient struct {
	hc   *http.Client
	base *url.URL
	user string
	pass string
}

// resolve turns a server path (the form hrefs come back in, already
// unescaped by hrefPath) or an absolute URL into a request URL on the
// configured host. Path is set unescaped so url.URL re-escapes it correctly.
func (d *davClient) resolve(ref string) (*url.URL, error) {
	if strings.HasPrefix(ref, "http://") || strings.HasPrefix(ref, "https://") {
		u, err := url.Parse(ref)
		if err != nil {
			return nil, fmt.Errorf("bad url %q: %w", ref, err)
		}
		return u, nil
	}
	if ref == "" {
		return nil, fmt.Errorf("caldav: empty path")
	}
	out := *d.base
	out.RawPath = ""
	out.RawQuery = ""
	out.Fragment = ""
	out.RawFragment = ""
	if strings.HasPrefix(ref, "/") {
		out.Path = ref
	} else {
		out.Path = strings.TrimSuffix(d.base.Path, "/") + "/" + ref
	}
	return &out, nil
}

type davResponse struct {
	Status int
	Header http.Header
	Body   []byte
}

// do performs one request. Non-2xx answers become *httpError; transport
// failures are returned raw so offlineErr can classify them.
func (d *davClient) do(ctx context.Context, method, ref string, body []byte, hdr map[string]string) (*davResponse, error) {
	u, err := d.resolve(ref)
	if err != nil {
		return nil, err
	}
	var rdr io.Reader
	if body != nil {
		rdr = bytes.NewReader(body)
	}
	req, err := http.NewRequestWithContext(ctx, method, u.String(), rdr)
	if err != nil {
		return nil, err
	}
	if body != nil {
		req.ContentLength = int64(len(body))
	}
	req.SetBasicAuth(d.user, d.pass)
	for k, v := range hdr {
		if v != "" {
			req.Header.Set(k, v)
		}
	}
	resp, err := d.hc.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	raw, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, err
	}
	if resp.StatusCode == http.StatusUnauthorized {
		return nil, &AuthError{Email: d.user, Status: resp.StatusCode, Detail: truncate(string(raw))}
	}
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return nil, &httpError{Method: method, Path: u.Path, Code: resp.StatusCode, Body: truncate(string(raw))}
	}
	return &davResponse{Status: resp.StatusCode, Header: resp.Header, Body: raw}, nil
}

func truncate(s string) string {
	s = strings.TrimSpace(s)
	if len(s) > maxErrorBody {
		return s[:maxErrorBody] + "…"
	}
	return s
}

// propfind issues a PROPFIND and decodes the multistatus body.
func (d *davClient) propfind(ctx context.Context, ref, depth, body string) (*multistatus, error) {
	resp, err := d.do(ctx, "PROPFIND", ref, []byte(xmlHeader+body), map[string]string{
		"Content-Type": "application/xml; charset=utf-8",
		"Depth":        depth,
	})
	if err != nil {
		return nil, err
	}
	return decodeMultistatus(resp.Body)
}

// report issues a REPORT and decodes the multistatus body.
func (d *davClient) report(ctx context.Context, ref, depth, body string) (*multistatus, error) {
	resp, err := d.do(ctx, "REPORT", ref, []byte(xmlHeader+body), map[string]string{
		"Content-Type": "application/xml; charset=utf-8",
		"Depth":        depth,
	})
	if err != nil {
		return nil, err
	}
	return decodeMultistatus(resp.Body)
}

const xmlHeader = `<?xml version="1.0" encoding="utf-8"?>` + "\n"

// ---------------------------------------------------------------------------
// multistatus decoding

type multistatus struct {
	XMLName   xml.Name `xml:"DAV: multistatus"`
	Responses []msResp `xml:"DAV: response"`
	SyncToken string   `xml:"DAV: sync-token"`
}

type msResp struct {
	Href      string       `xml:"DAV: href"`
	Status    string       `xml:"DAV: status"`
	PropStats []msPropStat `xml:"DAV: propstat"`
}

type msPropStat struct {
	Status string `xml:"DAV: status"`
	Prop   msProp `xml:"DAV: prop"`
}

type msProp struct {
	DisplayName  string `xml:"DAV: displayname"`
	ETag         string `xml:"DAV: getetag"`
	SyncToken    string `xml:"DAV: sync-token"`
	ResourceType struct {
		Calendar   *struct{} `xml:"urn:ietf:params:xml:ns:caldav calendar"`
		Collection *struct{} `xml:"DAV: collection"`
	} `xml:"DAV: resourcetype"`
	CurrentUserPrincipal struct {
		Href string `xml:"DAV: href"`
	} `xml:"DAV: current-user-principal"`
	CalendarHomeSet struct {
		Href string `xml:"DAV: href"`
	} `xml:"urn:ietf:params:xml:ns:caldav calendar-home-set"`
	CalendarData       string `xml:"urn:ietf:params:xml:ns:caldav calendar-data"`
	CalendarTimezone   string `xml:"urn:ietf:params:xml:ns:caldav calendar-timezone"`
	CalendarTimezoneID string `xml:"urn:ietf:params:xml:ns:caldav calendar-timezone-id"`
	Color              string `xml:"http://apple.com/ns/ical/ calendar-color"`
	SupportedComps     []struct {
		Name string `xml:"name,attr"`
	} `xml:"urn:ietf:params:xml:ns:caldav supported-calendar-component-set>comp"`
	Privileges []msPrivilege `xml:"DAV: current-user-privilege-set>privilege"`
}

type msPrivilege struct {
	All          *struct{} `xml:"DAV: all"`
	Read         *struct{} `xml:"DAV: read"`
	Write        *struct{} `xml:"DAV: write"`
	WriteContent *struct{} `xml:"DAV: write-content"`
	Bind         *struct{} `xml:"DAV: bind"`
}

func decodeMultistatus(b []byte) (*multistatus, error) {
	var ms multistatus
	if err := xml.Unmarshal(b, &ms); err != nil {
		return nil, fmt.Errorf("decode multistatus: %w (%s)", err, truncate(string(b)))
	}
	return &ms, nil
}

// ok returns the first propstat whose status is 2xx, and whether there was
// one. A server answers with several propstats: found properties under 200,
// missing ones under 404.
func (r *msResp) ok() (msProp, bool) {
	for _, ps := range r.PropStats {
		if code := statusFromLine(ps.Status); code >= 200 && code < 300 {
			return ps.Prop, true
		}
	}
	return msProp{}, false
}

// gone reports a response-level 404/410, which is how sync-collection
// announces a deleted resource.
func (r *msResp) gone() bool {
	code := statusFromLine(r.Status)
	return code == http.StatusNotFound || code == http.StatusGone
}

// statusFromLine parses "HTTP/1.1 200 OK" into 200. It returns 0 for an empty
// or unparseable line.
func statusFromLine(s string) int {
	fields := strings.Fields(s)
	if len(fields) < 2 {
		return 0
	}
	code, err := strconv.Atoi(fields[1])
	if err != nil {
		return 0
	}
	return code
}

// hrefPath is the path component of an href, which is what this package uses
// as a remote id. Servers return either a bare path or a full URL.
func hrefPath(href string) string {
	href = strings.TrimSpace(href)
	if href == "" {
		return ""
	}
	if u, err := url.Parse(href); err == nil && u.Path != "" {
		if p, err := url.PathUnescape(u.EscapedPath()); err == nil {
			return p
		}
		return u.Path
	}
	return href
}

// samePath compares two collection/object paths ignoring a trailing slash.
func samePath(a, b string) bool {
	return strings.TrimSuffix(a, "/") == strings.TrimSuffix(b, "/")
}

// unquoteETag strips the quotes and any weak marker from an ETag header so the
// value can be stored and compared as-is.
func unquoteETag(s string) string {
	s = strings.TrimSpace(s)
	s = strings.TrimPrefix(s, "W/")
	if len(s) >= 2 && s[0] == '"' && s[len(s)-1] == '"' {
		return s[1 : len(s)-1]
	}
	return s
}

// quoteETag is unquoteETag's inverse, for If-Match.
func quoteETag(s string) string {
	if s == "" {
		return ""
	}
	if strings.HasPrefix(s, "\"") || strings.HasPrefix(s, "W/") {
		return s
	}
	return `"` + s + `"`
}

// xmlEscape escapes text destined for an XML element body.
func xmlEscape(s string) string {
	var buf bytes.Buffer
	_ = xml.EscapeText(&buf, []byte(s))
	return buf.String()
}
