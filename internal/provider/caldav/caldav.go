// Package caldav implements provider.CalendarProvider on top of CalDAV
// (RFC 4791). It is vendor-neutral: Fastmail and iCloud both serve calendars
// this way, and a self-hosted server works by base URL alone. What differs
// between them — the DAV root, where the user creates a password, whether the
// conventional home path can be guessed — lives in presets.go.
//
// Authentication is HTTP basic with a per-application password, never an
// account login password. Fastmail's JMAP API tokens carry no calendars scope
// (DESIGN.md §6.4), and iCloud requires an app-specific password.
//
// Deltas use RFC 6578 collection synchronisation: a sync-collection REPORT
// with the stored sync-token returns the hrefs that changed and the hrefs that
// were deleted, and the changed objects are then fetched with a
// calendar-multiget REPORT. A token the server no longer understands (the
// DAV:valid-sync-token precondition) surfaces as provider.ErrStateExpired, and
// the sync engine re-lists from scratch.
//
// The .ics text is kept verbatim in model.Event.RawJSON (as
// {"ics":…,"href":…,"etag":…}) so an update can patch the object the server
// actually holds instead of a lossy re-rendering of it, and so If-Match can
// make that write conditional.
//
// # Hosts and hrefs
//
// Remote ids are bare paths, never absolute URLs, because they are persisted
// in the events table. Some servers — iCloud in particular — answer discovery
// with a calendar home on a different, per-user host (p<NN>-caldav.icloud.com).
// The client therefore records that host separately and resolves every later
// path against it, leaving stored ids untouched. Redirects are refused rather
// than followed: Go rewrites a redirected PROPFIND or PUT into a GET, which
// would silently do nothing.
package caldav

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"time"

	"github.com/teulaert/emlcalsync/internal/model"
	"github.com/teulaert/emlcalsync/internal/provider"
)

// defaultTimeout bounds a single request. The multiget batches are the slow
// calls and they are bounded by batchSize, not by the size of the calendar.
const defaultTimeout = 2 * time.Minute

// Options configures a CalDAV Calendar provider.
type Options struct {
	// Email is the account's address: how the account's own ATTENDEE line is
	// spotted, and the default basic-auth username.
	Email string
	// Username is the basic-auth user when it is not Email. An Apple ID is
	// frequently not the iCloud address, while the address is still what
	// identifies the user on an invitation.
	Username string
	// Password is a per-application password, not a login password.
	Password string
	// Vendor selects the preset. It may be empty when BaseURL is given.
	Vendor model.Vendor
	// BaseURL is the DAV root; empty takes the vendor preset's. Tests point it
	// at an httptest server.
	BaseURL string
	// HTTPClient defaults to a client with defaultTimeout.
	HTTPClient *http.Client
	// Logger receives one Debug record per request. Defaults to slog.Default().
	Logger *slog.Logger
	// BatchSize is how many objects one calendar-multiget asks for
	// (default 50).
	BatchSize int
}

// Calendar is a CalDAV-backed provider.CalendarProvider.
type Calendar struct {
	dav    *davClient
	log    *slog.Logger
	opts   Options
	preset Preset

	// home is the discovered calendar-home-set path, cached after the first
	// successful discovery. Discovery may also move dav.host.
	home string
	// done guards discovery so the concurrent callers the sync engine and the
	// outbox produce run it once.
	once sync.Once
	// discErr is the outcome of that single run.
	discErr error
}

var _ provider.CalendarProvider = (*Calendar)(nil)

const defaultBatchSize = 50

// New builds a CalDAV provider. It performs no I/O.
func New(opts Options) (*Calendar, error) {
	if strings.TrimSpace(opts.Email) == "" {
		return nil, errors.New("caldav: Options.Email is required")
	}
	preset, _ := PresetFor(opts.Vendor)
	if strings.TrimSpace(opts.Password) == "" {
		return nil, fmt.Errorf("caldav: Options.Password is required (a %s)", preset.credentialPhrase())
	}
	if opts.BaseURL == "" {
		opts.BaseURL = preset.BaseURL
	}
	if opts.BaseURL == "" {
		return nil, fmt.Errorf("caldav: no base URL: vendor %q has no preset, so Options.BaseURL is required", opts.Vendor)
	}
	base, err := url.Parse(opts.BaseURL)
	if err != nil {
		return nil, fmt.Errorf("caldav: bad BaseURL %q: %w", opts.BaseURL, err)
	}
	if base.Path == "" {
		base.Path = "/"
	}
	if !strings.HasSuffix(base.Path, "/") {
		base.Path += "/"
	}
	hc := opts.HTTPClient
	if hc == nil {
		hc = &http.Client{
			Timeout: defaultTimeout,
			// Refuse redirects. Go turns a redirected PROPFIND, REPORT or PUT
			// into a GET, so following one would silently drop the request;
			// discovery adopts a redirect's host explicitly instead.
			CheckRedirect: func(*http.Request, []*http.Request) error {
				return http.ErrUseLastResponse
			},
		}
	}
	log := opts.Logger
	if log == nil {
		log = slog.Default()
	}
	log = log.With("provider", "caldav", "email", opts.Email)
	if opts.BatchSize <= 0 {
		opts.BatchSize = defaultBatchSize
	}

	return &Calendar{
		dav: &davClient{
			hc:   hc,
			base: base,
			host: hostOf(base),
			user: opts.User(),
			pass: opts.Password,
			cred: credential{name: preset.CredentialName, url: preset.CredentialURL},
		},
		log:    log,
		opts:   opts,
		preset: preset,
	}, nil
}

// User is the basic-auth username: Username when set, else the address.
func (o Options) User() string {
	if s := strings.TrimSpace(o.Username); s != "" {
		return s
	}
	return o.Email
}

// hostOf reduces a URL to scheme://host, dropping a default port so two
// spellings of the same origin compare equal.
func hostOf(u *url.URL) *url.URL {
	out := &url.URL{Scheme: u.Scheme, Host: u.Host}
	if (out.Scheme == "https" && out.Port() == "443") || (out.Scheme == "http" && out.Port() == "80") {
		out.Host = out.Hostname()
	}
	return out
}

// fallbackHome is the conventional calendar home for this vendor, or "" when
// there is no guessing it (iCloud's is a numeric account id).
func (c *Calendar) fallbackHome() string {
	if c.preset.HomeFallback == nil {
		return ""
	}
	return c.preset.HomeFallback(c.dav.base, c.opts.User())
}

// discover resolves the calendar home, and the host serving it, exactly once.
//
// It runs its own PROPFINDs rather than using go-webdav, whose
// FindCurrentUserPrincipal and FindCalendarHomeSet both return only the path
// of the href they found (caldav/client.go). That is fatal here: iCloud
// answers with a home on a per-user partition host, and if the host is thrown
// away every later request goes back to the front door and is redirected —
// which, for a PROPFIND or a PUT, means silently turned into a GET.
//
// Every public method calls this. It cannot be a side effect of Calendars():
// the outbox reaches CreateEvent and friends on a client that never listed
// anything.
func (c *Calendar) discover(ctx context.Context) error {
	c.once.Do(func() { c.discErr = c.doDiscover(ctx) })
	return c.discErr
}

func (c *Calendar) doDiscover(ctx context.Context) error {
	principal, err := c.findHref(ctx, c.dav.base.Path, propfindPrincipal, func(p msProp) string {
		return p.CurrentUserPrincipal.Href
	})
	if err != nil {
		return c.discoveryFallback("current-user-principal", err)
	}

	home, err := c.findHref(ctx, principal.path, propfindHomeSet, func(p msProp) string {
		return p.CalendarHomeSet.Href
	})
	if err != nil {
		return c.discoveryFallback("calendar-home-set", err)
	}

	// The home's host wins; failing that the principal's, which is where the
	// server first pointed us.
	switch {
	case home.host != nil:
		c.dav.host = home.host
	case principal.host != nil:
		c.dav.host = principal.host
	}
	path := home.path
	if !strings.HasSuffix(path, "/") {
		path += "/"
	}
	c.home = path
	c.log.Debug("caldav discovered calendar home", "home", c.home, "host", c.dav.host)
	return nil
}

// discoveryFallback decides what a failed discovery step means. Bad
// credentials are always fatal — every later request would fail the same way —
// and so is any failure at all when the vendor has no conventional home path
// to fall back on.
func (c *Calendar) discoveryFallback(step string, cause error) error {
	if ae := c.probeAuth(cause); ae != nil {
		return ae
	}
	fb := c.fallbackHome()
	if fb == "" {
		return wrapErr("discover "+step, fmt.Errorf(
			"%w; %s calendars have no conventional path to fall back on",
			cause, vendorLabel(c.opts.Vendor)))
	}
	c.log.Debug("caldav: "+step+" lookup failed, using the default path", "err", cause, "home", fb)
	c.home = fb
	return nil
}

func vendorLabel(v model.Vendor) string {
	if v == "" {
		return "this server's"
	}
	return string(v)
}

// href is one discovered location: its path, and the host it named if it was
// an absolute URL.
type href struct {
	path string
	host *url.URL
}

// findHref issues one PROPFIND and pulls a single href out of the response.
func (c *Calendar) findHref(ctx context.Context, at, body string, pick func(msProp) string) (href, error) {
	c.log.Debug("caldav propfind", "path", at, "depth", 0)
	ms, err := c.dav.propfind(ctx, at, "0", body)
	if err != nil {
		return href{}, err
	}
	for i := range ms.Responses {
		prop, ok := ms.Responses[i].ok()
		if !ok {
			continue
		}
		raw := strings.TrimSpace(pick(prop))
		if raw == "" {
			continue
		}
		u, err := url.Parse(raw)
		if err != nil {
			return href{}, fmt.Errorf("bad href %q: %w", raw, err)
		}
		out := href{path: hrefPath(raw)}
		if u.IsAbs() {
			out.host = hostOf(u)
		}
		if out.path == "" {
			continue
		}
		return out, nil
	}
	return href{}, fmt.Errorf("no href in the response")
}

// probeAuth returns the AuthError carried by err, if any.
func (c *Calendar) probeAuth(cause error) *AuthError {
	var ae *AuthError
	if errors.As(cause, &ae) {
		return ae
	}
	return nil
}

// calendarHome returns the discovered calendar-home-set path.
func (c *Calendar) calendarHome(ctx context.Context) (string, error) {
	if err := c.discover(ctx); err != nil {
		return "", err
	}
	return c.home, nil
}

const propfindPrincipal = `<d:propfind xmlns:d="DAV:"><d:prop><d:current-user-principal/></d:prop></d:propfind>`

const propfindHomeSet = `<d:propfind xmlns:d="DAV:" xmlns:c="urn:ietf:params:xml:ns:caldav">` +
	`<d:prop><c:calendar-home-set/></d:prop></d:propfind>`

const propfindCalendars = `<d:propfind xmlns:d="DAV:" xmlns:c="urn:ietf:params:xml:ns:caldav" xmlns:ic="http://apple.com/ns/ical/">
  <d:prop>
    <d:resourcetype/>
    <d:displayname/>
    <d:current-user-privilege-set/>
    <c:supported-calendar-component-set/>
    <c:calendar-timezone-id/>
    <ic:calendar-color/>
  </d:prop>
</d:propfind>`

// Calendars lists every calendar collection in the account's calendar home.
//
// The calendar-timezone property is deliberately not requested: it is a whole
// VTIMEZONE document per calendar, and Fastmail exposes the far cheaper
// RFC 7809 calendar-timezone-id alongside it.
func (c *Calendar) Calendars(ctx context.Context) ([]model.Calendar, error) {
	home, err := c.calendarHome(ctx)
	if err != nil {
		return nil, wrapErr("discover calendar home", err)
	}
	c.log.Debug("caldav propfind", "path", home, "depth", 1)
	ms, err := c.dav.propfind(ctx, home, "1", propfindCalendars)
	if err != nil {
		return nil, wrapErr("list calendars", err)
	}

	var out []model.Calendar
	for i := range ms.Responses {
		resp := &ms.Responses[i]
		path := hrefPath(resp.Href)
		if path == "" || samePath(path, home) {
			continue // the home collection itself
		}
		prop, ok := resp.ok()
		if !ok || prop.ResourceType.Calendar == nil {
			continue
		}
		if !supportsEvents(prop) {
			continue // a task list or a subscription of another kind
		}
		if !strings.HasSuffix(path, "/") {
			path += "/"
		}
		name := prop.DisplayName
		if name == "" {
			name = strings.Trim(lastSegment(path), "/")
		}
		out = append(out, model.Calendar{
			RemoteID:   path,
			Name:       name,
			Color:      normaliseColor(prop.Color),
			Timezone:   strings.TrimSpace(prop.CalendarTimezoneID),
			AccessRole: accessRole(prop.Privileges),
		})
	}
	markPrimary(out, c.preset.PrimaryNames)
	return out, nil
}

// supportsEvents reports whether a collection holds VEVENTs. A server that
// does not advertise supported-calendar-component-set is assumed to.
func supportsEvents(p msProp) bool {
	if len(p.SupportedComps) == 0 {
		return true
	}
	for _, comp := range p.SupportedComps {
		if strings.EqualFold(comp.Name, "VEVENT") {
			return true
		}
	}
	return false
}

// accessRole maps the DAV privileges onto the owner|writer|reader vocabulary
// model.Calendar shares with Google Calendar. A server that does not report
// privileges is assumed to grant ownership, which is what a personal account
// always has.
func accessRole(privs []msPrivilege) string {
	if len(privs) == 0 {
		return "owner"
	}
	write, read := false, false
	for _, p := range privs {
		switch {
		case p.All != nil:
			return "owner"
		case p.Write != nil, p.WriteContent != nil, p.Bind != nil:
			write = true
		case p.Read != nil:
			read = true
		}
	}
	switch {
	case write:
		return "writer"
	case read:
		return "reader"
	}
	return "reader"
}

// normaliseColor trims the alpha channel Apple's calendar-color carries
// (#RRGGBBAA) down to the #RRGGBB the rest of emlcal uses.
func normaliseColor(s string) string {
	s = strings.TrimSpace(s)
	if len(s) == 9 && strings.HasPrefix(s, "#") {
		return s[:7]
	}
	return s
}

// markPrimary flags the account's main calendar: one the vendor names as its
// default, else the collection at .../Default/, else the first.
func markPrimary(cals []model.Calendar, names []string) {
	if len(cals) == 0 {
		return
	}
	for _, want := range names {
		for i := range cals {
			if strings.EqualFold(cals[i].Name, want) {
				cals[i].Primary = true
				return
			}
		}
	}
	for i := range cals {
		if strings.EqualFold(strings.Trim(lastSegment(cals[i].RemoteID), "/"), "Default") {
			cals[i].Primary = true
			return
		}
	}
	cals[0].Primary = true
}

// lastSegment returns the final path segment, ignoring a trailing slash.
func lastSegment(p string) string {
	p = strings.TrimSuffix(p, "/")
	if i := strings.LastIndex(p, "/"); i >= 0 {
		return p[i+1:]
	}
	return p
}
