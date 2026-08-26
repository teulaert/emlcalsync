// Package caldav implements provider.CalendarProvider on top of CalDAV
// (RFC 4791), which is how emlcal reaches Fastmail calendars.
//
// Fastmail's JMAP API tokens carry no calendars scope, so the JMAP client can
// only report provider.ErrNotSupported for calendars (DESIGN.md §6.4 named
// CalDAV as the documented fallback). This package is that fallback: it talks
// to https://caldav.fastmail.com/dav/ with HTTP basic auth using the account's
// email address and an **app password** created at
// https://app.fastmail.com/settings/security/devices with access
// "Calendars (CalDAV)".
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
package caldav

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/emersion/go-webdav"
	wcaldav "github.com/emersion/go-webdav/caldav"

	"github.com/teulaert/emlcalsync/internal/model"
	"github.com/teulaert/emlcalsync/internal/provider"
)

// DefaultBaseURL is Fastmail's DAV root. Calendars live under
// <base>/calendars/user/<email>/.
const DefaultBaseURL = "https://caldav.fastmail.com/dav/"

// defaultTimeout bounds a single request. The multiget batches are the slow
// calls and they are bounded by batchSize, not by the size of the calendar.
const defaultTimeout = 2 * time.Minute

// Options configures a CalDAV Calendar provider.
type Options struct {
	// Email is the account's address. It is the basic-auth username, the
	// fallback path segment for calendar discovery, and how the account's own
	// ATTENDEE line is spotted.
	Email string
	// Password is a Fastmail **app password** (not the API token and not the
	// login password).
	Password string
	// BaseURL is the DAV root; it defaults to DefaultBaseURL. Tests point it
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
	dav  *davClient
	disc *wcaldav.Client
	log  *slog.Logger
	opts Options

	// home is the discovered calendar-home-set path, cached after the first
	// successful Calendars call.
	home string
}

var _ provider.CalendarProvider = (*Calendar)(nil)

const defaultBatchSize = 50

// New builds a CalDAV provider. It performs no I/O.
func New(opts Options) (*Calendar, error) {
	if strings.TrimSpace(opts.Email) == "" {
		return nil, errors.New("caldav: Options.Email is required")
	}
	if strings.TrimSpace(opts.Password) == "" {
		return nil, errors.New("caldav: Options.Password is required (a Fastmail app password with Calendars (CalDAV) access)")
	}
	if opts.BaseURL == "" {
		opts.BaseURL = DefaultBaseURL
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
		hc = &http.Client{Timeout: defaultTimeout}
	}
	log := opts.Logger
	if log == nil {
		log = slog.Default()
	}
	log = log.With("provider", "caldav", "email", opts.Email)
	if opts.BatchSize <= 0 {
		opts.BatchSize = defaultBatchSize
	}

	// go-webdav drives the two discovery PROPFINDs (current-user-principal
	// and calendar-home-set); everything after that needs raw .ics text,
	// sync-collection or If-Match, none of which its client exposes.
	disc, err := wcaldav.NewClient(
		webdav.HTTPClientWithBasicAuth(hc, opts.Email, opts.Password),
		base.String(),
	)
	if err != nil {
		return nil, fmt.Errorf("caldav: client for %s: %w", base, err)
	}

	return &Calendar{
		dav:  &davClient{hc: hc, base: base, user: opts.Email, pass: opts.Password},
		disc: disc,
		log:  log,
		opts: opts,
	}, nil
}

// fallbackHome is the path Fastmail actually serves, used when discovery is
// unavailable (an old server, a proxy that drops REPORT/PROPFIND on the root).
func (c *Calendar) fallbackHome() string {
	return strings.TrimSuffix(c.dav.base.Path, "/") + "/calendars/user/" + c.opts.Email + "/"
}

// calendarHome discovers the calendar-home-set, falling back to the
// conventional Fastmail path. An authentication failure is fatal — every later
// request would fail the same way — but any other discovery hiccup just means
// the fallback is used.
func (c *Calendar) calendarHome(ctx context.Context) (string, error) {
	if c.home != "" {
		return c.home, nil
	}
	principal, err := c.disc.FindCurrentUserPrincipal(ctx)
	if err != nil {
		if ae := c.probeAuth(ctx, err); ae != nil {
			return "", ae
		}
		c.log.Debug("caldav: current-user-principal lookup failed, using the default path", "err", err)
		c.home = c.fallbackHome()
		return c.home, nil
	}
	home, err := c.disc.FindCalendarHomeSet(ctx, principal)
	if err != nil || strings.TrimSpace(home) == "" {
		c.log.Debug("caldav: calendar-home-set lookup failed, using the default path",
			"principal", principal, "err", err)
		c.home = c.fallbackHome()
		return c.home, nil
	}
	if !strings.HasSuffix(home, "/") {
		home += "/"
	}
	c.home = home
	return c.home, nil
}

// probeAuth turns a discovery failure into an *AuthError when the credentials
// are the problem. go-webdav hides its HTTP status behind an unexported type,
// so the check is a cheap PROPFIND of our own on the same URL.
func (c *Calendar) probeAuth(ctx context.Context, cause error) error {
	var ae *AuthError
	if errors.As(cause, &ae) {
		return ae
	}
	if !strings.Contains(cause.Error(), "401") && !strings.Contains(cause.Error(), "Unauthorized") {
		return nil
	}
	_, err := c.dav.propfind(ctx, c.dav.base.Path, "0", propfindPrincipal)
	if errors.As(err, &ae) {
		return ae
	}
	return nil
}

const propfindPrincipal = `<d:propfind xmlns:d="DAV:"><d:prop><d:current-user-principal/></d:prop></d:propfind>`

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
	markPrimary(out)
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

// markPrimary flags the account's main calendar: the one Fastmail calls
// "Calendar", else the collection at .../Default/, else the first.
func markPrimary(cals []model.Calendar) {
	if len(cals) == 0 {
		return
	}
	for i := range cals {
		if strings.EqualFold(cals[i].Name, "Calendar") {
			cals[i].Primary = true
			return
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
