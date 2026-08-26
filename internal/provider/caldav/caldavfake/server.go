// Package caldavfake is an in-memory CalDAV server for tests: enough of
// RFC 4791 and RFC 6578 to exercise discovery, collection synchronisation,
// multiget and conditional writes.
//
// It lives outside _test.go so both the CalDAV provider's own tests and the
// CLI tests (which need a server the real Factory can be pointed at) can use
// it.
package caldavfake

import (
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"sort"
	"strconv"
	"strings"
	"sync"
)

// Calendar is one collection served by the fake.
type Calendar struct {
	Path       string // server path, always ending in "/"
	Name       string
	Color      string
	Timezone   string
	Components []string // supported-calendar-component-set; nil means VEVENT
	Privileges []string // current-user-privilege-set; nil means all
	// NoSyncToken makes sync-collection answer without a DAV:sync-token,
	// which the provider must refuse rather than persist.
	NoSyncToken bool
}

type object struct {
	href    string
	ics     string
	etag    string
	version int64
	deleted bool
}

// Request is one recorded call, for assertions on what went over the wire.
type Request struct {
	Method string
	Path   string
	Depth  string
	Header http.Header
	Body   string
}

// Server is the fake. The zero value is not usable; call New.
type Server struct {
	// Root is the DAV root path, "/dav/" by default.
	Root string
	// User and Password are the credentials the server accepts. Empty
	// disables the check.
	User, Password string
	// NoPrincipal makes the discovery PROPFIND fail, so the client has to
	// fall back to the conventional /dav/calendars/user/<email>/ path.
	NoPrincipal bool
	// MinToken is the oldest sync token still accepted; anything older gets
	// the DAV:valid-sync-token precondition. Set it above the current
	// version with ExpireTokens.
	MinToken int64

	ts *httptest.Server

	mu       sync.Mutex
	version  int64
	cals     []*Calendar
	objects  map[string]*object // by href
	requests []Request
}

// New starts a fake CalDAV server. Close it when the test is done.
func New() *Server {
	s := &Server{Root: "/dav/", objects: map[string]*object{}}
	s.ts = httptest.NewServer(http.HandlerFunc(s.serve))
	return s
}

func (s *Server) Close() { s.ts.Close() }

// BaseURL is the value for caldav.Options.BaseURL.
func (s *Server) BaseURL() string { return s.ts.URL + s.Root }

// PrincipalPath is where the fake advertises the current user principal.
func (s *Server) PrincipalPath() string { return s.Root + "principals/user/" }

// HomePath is the calendar-home-set the principal points at.
func (s *Server) HomePath(email string) string { return s.Root + "calendars/user/" + email + "/" }

// AddCalendar registers a collection and returns its path.
func (s *Server) AddCalendar(c Calendar) string {
	s.mu.Lock()
	defer s.mu.Unlock()
	if !strings.HasSuffix(c.Path, "/") {
		c.Path += "/"
	}
	cp := c
	s.cals = append(s.cals, &cp)
	return cp.Path
}

// Put stores an object and bumps the collection's sync version, returning the
// new ETag. name is the last path segment (".ics" included).
func (s *Server) Put(calPath, name, ics string) string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.putLocked(strings.TrimSuffix(calPath, "/")+"/"+name, ics)
}

func (s *Server) putLocked(href, ics string) string {
	s.version++
	etag := fmt.Sprintf("etag-%d", s.version)
	s.objects[href] = &object{href: href, ics: ics, etag: etag, version: s.version}
	return etag
}

// Delete tombstones an object so the next sync-collection reports it gone.
func (s *Server) Delete(href string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.deleteLocked(href)
}

func (s *Server) deleteLocked(href string) {
	obj, ok := s.objects[href]
	if !ok {
		return
	}
	s.version++
	obj.deleted = true
	obj.version = s.version
	obj.ics = ""
}

// Get returns a stored object's .ics text.
func (s *Server) Get(href string) (string, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	obj, ok := s.objects[href]
	if !ok || obj.deleted {
		return "", false
	}
	return obj.ics, true
}

// ETag returns a stored object's current ETag.
func (s *Server) ETag(href string) string {
	s.mu.Lock()
	defer s.mu.Unlock()
	if obj, ok := s.objects[href]; ok && !obj.deleted {
		return obj.etag
	}
	return ""
}

// Hrefs lists the live object hrefs, sorted.
func (s *Server) Hrefs() []string {
	s.mu.Lock()
	defer s.mu.Unlock()
	var out []string
	for href, obj := range s.objects {
		if !obj.deleted {
			out = append(out, href)
		}
	}
	sort.Strings(out)
	return out
}

// ExpireTokens invalidates every token handed out so far, which is what a
// server does when its change log has been truncated.
func (s *Server) ExpireTokens() {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.MinToken = s.version + 1
}

// Requests returns a copy of everything received so far.
func (s *Server) Requests() []Request {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]Request(nil), s.requests...)
}

// LastRequest returns the most recent request with the given method, or false.
func (s *Server) LastRequest(method string) (Request, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	for i := len(s.requests) - 1; i >= 0; i-- {
		if s.requests[i].Method == method {
			return s.requests[i], true
		}
	}
	return Request{}, false
}

// ---------------------------------------------------------------------------

func (s *Server) serve(w http.ResponseWriter, r *http.Request) {
	body, _ := io.ReadAll(r.Body)
	s.mu.Lock()
	s.requests = append(s.requests, Request{
		Method: r.Method, Path: r.URL.Path, Depth: r.Header.Get("Depth"),
		Header: r.Header.Clone(), Body: string(body),
	})
	s.mu.Unlock()

	if s.User != "" {
		user, pass, ok := r.BasicAuth()
		if !ok || user != s.User || pass != s.Password {
			w.Header().Set("WWW-Authenticate", `Basic realm="fake"`)
			http.Error(w, "bad credentials", http.StatusUnauthorized)
			return
		}
	}

	switch r.Method {
	case "PROPFIND":
		s.propfind(w, r, string(body))
	case "REPORT":
		s.report(w, r, string(body))
	case http.MethodGet:
		s.get(w, r)
	case http.MethodPut:
		s.put(w, r, string(body))
	case http.MethodDelete:
		s.delete(w, r)
	case "OPTIONS":
		w.Header().Set("DAV", "1, 2, 3, calendar-access")
		w.WriteHeader(http.StatusOK)
	default:
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
	}
}

func (s *Server) propfind(w http.ResponseWriter, r *http.Request, body string) {
	path := r.URL.Path
	switch {
	case strings.Contains(body, "current-user-principal"):
		if s.NoPrincipal {
			http.Error(w, "no principal here", http.StatusNotFound)
			return
		}
		writeMultistatus(w, respElem(path, `<d:current-user-principal><d:href>`+
			s.PrincipalPath()+`</d:href></d:current-user-principal>`), "")
	case strings.Contains(body, "calendar-home-set"):
		home := s.homeFor(path)
		writeMultistatus(w, respElem(path, `<c:calendar-home-set><d:href>`+home+
			`</d:href></c:calendar-home-set>`), "")
	default:
		s.propfindCalendars(w, path)
	}
}

// homeFor derives the calendar home from the principal path, which in this
// fake always contains the email as its last segment for a real account. Tests
// register calendars under whatever path they like, so the first registered
// calendar's parent wins when there is one.
func (s *Server) homeFor(string) string {
	s.mu.Lock()
	defer s.mu.Unlock()
	if len(s.cals) > 0 {
		p := strings.TrimSuffix(s.cals[0].Path, "/")
		if i := strings.LastIndex(p, "/"); i >= 0 {
			return p[:i+1]
		}
	}
	return s.Root + "calendars/user/"
}

func (s *Server) propfindCalendars(w http.ResponseWriter, home string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	var sb strings.Builder
	sb.WriteString(respElem(home, `<d:resourcetype><d:collection/></d:resourcetype>`))
	for _, c := range s.cals {
		if !strings.HasPrefix(c.Path, home) || c.Path == home {
			continue
		}
		var props strings.Builder
		props.WriteString(`<d:resourcetype><d:collection/><c:calendar/></d:resourcetype>`)
		props.WriteString(`<d:displayname>` + esc(c.Name) + `</d:displayname>`)
		if c.Color != "" {
			props.WriteString(`<ic:calendar-color>` + esc(c.Color) + `</ic:calendar-color>`)
		}
		if c.Timezone != "" {
			props.WriteString(`<c:calendar-timezone-id>` + esc(c.Timezone) + `</c:calendar-timezone-id>`)
		}
		comps := c.Components
		if comps == nil {
			comps = []string{"VEVENT"}
		}
		props.WriteString(`<c:supported-calendar-component-set>`)
		for _, comp := range comps {
			props.WriteString(`<c:comp name="` + esc(comp) + `"/>`)
		}
		props.WriteString(`</c:supported-calendar-component-set>`)
		privs := c.Privileges
		if privs == nil {
			privs = []string{"all", "read", "write"}
		}
		props.WriteString(`<d:current-user-privilege-set>`)
		for _, p := range privs {
			props.WriteString(`<d:privilege><d:` + esc(p) + `/></d:privilege>`)
		}
		props.WriteString(`</d:current-user-privilege-set>`)
		sb.WriteString(respElem(c.Path, props.String()))
	}
	writeMultistatus(w, sb.String(), "")
}

func (s *Server) report(w http.ResponseWriter, r *http.Request, body string) {
	switch {
	case strings.Contains(body, "sync-collection"):
		s.syncCollection(w, r, body)
	case strings.Contains(body, "calendar-multiget"):
		s.multiget(w, body)
	default:
		http.Error(w, "unsupported report", http.StatusBadRequest)
	}
}

func (s *Server) syncCollection(w http.ResponseWriter, r *http.Request, body string) {
	token := between(body, "<d:sync-token>", "</d:sync-token>")
	since := int64(0)
	if token != "" {
		n, err := parseToken(token)
		if err != nil {
			writePrecondition(w, http.StatusForbidden, "valid-sync-token")
			return
		}
		since = n
	}

	s.mu.Lock()
	defer s.mu.Unlock()
	if token != "" && since < s.MinToken {
		writePrecondition(w, http.StatusForbidden, "valid-sync-token")
		return
	}
	cal := s.calendarAt(r.URL.Path)

	var sb strings.Builder
	var hrefs []string
	for href := range s.objects {
		hrefs = append(hrefs, href)
	}
	sort.Strings(hrefs)
	for _, href := range hrefs {
		obj := s.objects[href]
		if !strings.HasPrefix(href, r.URL.Path) || obj.version <= since {
			continue
		}
		if obj.deleted {
			if token == "" {
				continue // an initial sync never reports tombstones
			}
			sb.WriteString(`<d:response><d:href>` + esc(href) +
				`</d:href><d:status>HTTP/1.1 404 Not Found</d:status></d:response>`)
			continue
		}
		sb.WriteString(respElem(href, `<d:getetag>"`+esc(obj.etag)+`"</d:getetag>`))
	}
	next := ""
	if cal == nil || !cal.NoSyncToken {
		next = fmt.Sprintf("sync-%d", s.version)
	}
	writeMultistatus(w, sb.String(), next)
}

func (s *Server) calendarAt(path string) *Calendar {
	for _, c := range s.cals {
		if strings.TrimSuffix(c.Path, "/") == strings.TrimSuffix(path, "/") {
			return c
		}
	}
	return nil
}

func (s *Server) multiget(w http.ResponseWriter, body string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	var sb strings.Builder
	rest := body
	for {
		href := between(rest, "<d:href>", "</d:href>")
		if href == "" {
			break
		}
		rest = rest[strings.Index(rest, "</d:href>")+len("</d:href>"):]
		obj, ok := s.objects[href]
		if !ok || obj.deleted {
			sb.WriteString(`<d:response><d:href>` + esc(href) +
				`</d:href><d:status>HTTP/1.1 404 Not Found</d:status></d:response>`)
			continue
		}
		sb.WriteString(respElem(href, `<d:getetag>"`+esc(obj.etag)+`"</d:getetag>`+
			`<c:calendar-data>`+esc(obj.ics)+`</c:calendar-data>`))
	}
	writeMultistatus(w, sb.String(), "")
}

func (s *Server) get(w http.ResponseWriter, r *http.Request) {
	s.mu.Lock()
	obj, ok := s.objects[r.URL.Path]
	var ics, etag string
	if ok && !obj.deleted {
		ics, etag = obj.ics, obj.etag
	} else {
		ok = false
	}
	s.mu.Unlock()
	if !ok {
		http.Error(w, "no such object", http.StatusNotFound)
		return
	}
	w.Header().Set("Content-Type", "text/calendar; charset=utf-8")
	w.Header().Set("ETag", `"`+etag+`"`)
	_, _ = io.WriteString(w, ics)
}

func (s *Server) put(w http.ResponseWriter, r *http.Request, body string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	obj, exists := s.objects[r.URL.Path]
	exists = exists && !obj.deleted

	if r.Header.Get("If-None-Match") == "*" && exists {
		writePrecondition(w, http.StatusPreconditionFailed, "no-uid-conflict")
		return
	}
	if m := r.Header.Get("If-Match"); m != "" {
		want := strings.Trim(m, `"`)
		if !exists || want != obj.etag {
			writePrecondition(w, http.StatusPreconditionFailed, "")
			return
		}
	}
	etag := s.putLocked(r.URL.Path, body)
	w.Header().Set("ETag", `"`+etag+`"`)
	if exists {
		w.WriteHeader(http.StatusNoContent)
	} else {
		w.WriteHeader(http.StatusCreated)
	}
}

func (s *Server) delete(w http.ResponseWriter, r *http.Request) {
	s.mu.Lock()
	defer s.mu.Unlock()
	obj, ok := s.objects[r.URL.Path]
	if !ok || obj.deleted {
		http.Error(w, "no such object", http.StatusNotFound)
		return
	}
	s.deleteLocked(r.URL.Path)
	w.WriteHeader(http.StatusNoContent)
}

// ---------------------------------------------------------------------------

const nsDecl = ` xmlns:d="DAV:" xmlns:c="urn:ietf:params:xml:ns:caldav" xmlns:ic="http://apple.com/ns/ical/"`

func writeMultistatus(w http.ResponseWriter, responses, syncToken string) {
	w.Header().Set("Content-Type", "application/xml; charset=utf-8")
	w.WriteHeader(http.StatusMultiStatus)
	_, _ = io.WriteString(w, `<?xml version="1.0" encoding="utf-8"?>`+"\n"+
		`<d:multistatus`+nsDecl+`>`+responses)
	if syncToken != "" {
		_, _ = io.WriteString(w, `<d:sync-token>`+esc(syncToken)+`</d:sync-token>`)
	}
	_, _ = io.WriteString(w, `</d:multistatus>`)
}

func writePrecondition(w http.ResponseWriter, code int, element string) {
	w.Header().Set("Content-Type", "application/xml; charset=utf-8")
	w.WriteHeader(code)
	body := `<?xml version="1.0" encoding="utf-8"?><d:error` + nsDecl + `>`
	if element != "" {
		body += `<d:` + element + `/>`
	}
	_, _ = io.WriteString(w, body+`</d:error>`)
}

func respElem(href, props string) string {
	return `<d:response><d:href>` + esc(href) + `</d:href><d:propstat><d:prop>` +
		props + `</d:prop><d:status>HTTP/1.1 200 OK</d:status></d:propstat></d:response>`
}

func esc(s string) string {
	return strings.NewReplacer("&", "&amp;", "<", "&lt;", ">", "&gt;").Replace(s)
}

func between(s, open, shut string) string {
	i := strings.Index(s, open)
	if i < 0 {
		return ""
	}
	rest := s[i+len(open):]
	j := strings.Index(rest, shut)
	if j < 0 {
		return ""
	}
	return strings.TrimSpace(rest[:j])
}

func parseToken(tok string) (int64, error) {
	n, err := strconv.ParseInt(strings.TrimPrefix(tok, "sync-"), 10, 64)
	if err != nil || !strings.HasPrefix(tok, "sync-") {
		return 0, fmt.Errorf("bad token %q", tok)
	}
	return n, nil
}
