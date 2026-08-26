package caldav

import (
	"context"
	"errors"
	"fmt"
	"strings"

	"github.com/teulaert/emlcalsync/internal/model"
	"github.com/teulaert/emlcalsync/internal/provider"
)

// EventChanges returns everything that changed in one calendar since the given
// sync token (RFC 6578). since=="" is the initial synchronisation: the server
// reports every object and still hands back a token for the next call.
//
// The two REPORTs are deliberately separate. sync-collection returns hrefs and
// ETags — cheap, and the only way to learn about deletions — and
// calendar-multiget then fetches the .ics text of just the objects that moved.
func (c *Calendar) EventChanges(ctx context.Context, calendarRemote, since string) (*provider.EventChanges, error) {
	if calendarRemote == "" {
		return nil, errors.New("caldav: EventChanges needs a calendar path")
	}
	if err := c.discover(ctx); err != nil {
		return nil, err
	}
	c.log.Debug("caldav sync-collection", "calendar", calendarRemote, "initial", since == "")

	ms, err := c.dav.report(ctx, calendarRemote, "1", syncCollectionBody(since))
	if err != nil {
		if isInvalidSyncToken(err) {
			return nil, fmt.Errorf("caldav sync token for %s is no longer valid: %w",
				calendarRemote, provider.ErrStateExpired)
		}
		if isNotFound(err) {
			return nil, fmt.Errorf("caldav calendar %s: %w", calendarRemote, model.ErrNotFound)
		}
		return nil, wrapErr("sync-collection "+calendarRemote, err)
	}
	if strings.TrimSpace(ms.SyncToken) == "" {
		// Persisting "" would silently turn every later delta into a full
		// listing and, worse, apply these changes against a state claiming
		// nothing was ever synced.
		return nil, fmt.Errorf("caldav: sync-collection for %s returned no sync-token", calendarRemote)
	}

	out := &provider.EventChanges{NewState: strings.TrimSpace(ms.SyncToken)}
	var changed []string
	for i := range ms.Responses {
		resp := &ms.Responses[i]
		href := hrefPath(resp.Href)
		if href == "" || samePath(href, calendarRemote) {
			continue // the collection itself
		}
		if resp.gone() {
			out.Removed = append(out.Removed, href)
			continue
		}
		if _, ok := resp.ok(); !ok {
			continue
		}
		changed = append(changed, href)
	}

	if len(changed) > 0 {
		upserted, err := c.fetchObjects(ctx, calendarRemote, changed)
		if err != nil {
			return nil, err
		}
		out.Upserted = upserted
	}
	c.log.Debug("caldav sync-collection done", "calendar", calendarRemote,
		"changed", len(changed), "removed", len(out.Removed), "events", len(out.Upserted))
	return out, nil
}

// syncCollectionBody builds the REPORT body. An empty <sync-token/> is what
// RFC 6578 §3.2 defines as "send me everything".
func syncCollectionBody(token string) string {
	tok := "<d:sync-token/>"
	if token != "" {
		tok = "<d:sync-token>" + xmlEscape(token) + "</d:sync-token>"
	}
	return `<d:sync-collection xmlns:d="DAV:">` + tok +
		`<d:sync-level>1</d:sync-level><d:prop><d:getetag/></d:prop></d:sync-collection>`
}

// fetchObjects multigets the given hrefs in batches and maps every VEVENT they
// contain. An href that has vanished between the two REPORTs is skipped: the
// next delta reports it as a deletion.
func (c *Calendar) fetchObjects(ctx context.Context, calendarRemote string, hrefs []string) ([]model.Event, error) {
	var out []model.Event
	for chunk := range batches(hrefs, c.opts.BatchSize) {
		ms, err := c.dav.report(ctx, calendarRemote, "0", multigetBody(chunk))
		if err != nil {
			return nil, wrapErr("calendar-multiget "+calendarRemote, err)
		}
		for i := range ms.Responses {
			resp := &ms.Responses[i]
			href := hrefPath(resp.Href)
			prop, ok := resp.ok()
			if !ok || href == "" {
				continue
			}
			ics := strings.TrimSpace(prop.CalendarData)
			if ics == "" {
				c.log.Warn("caldav: multiget returned no calendar-data", "href", href)
				continue
			}
			events, err := parseObject(calendarRemote, href, unquoteETag(prop.ETag), ics, c.opts.Email, c.log)
			if err != nil {
				// One unparseable object must not stop the whole calendar.
				c.log.Warn("caldav: skipping unparseable object", "href", href, "err", err)
				continue
			}
			out = append(out, events...)
		}
	}
	return out, nil
}

// multigetBody builds a calendar-multiget REPORT for a batch of hrefs.
func multigetBody(hrefs []string) string {
	var sb strings.Builder
	sb.WriteString(`<c:calendar-multiget xmlns:d="DAV:" xmlns:c="urn:ietf:params:xml:ns:caldav">`)
	sb.WriteString(`<d:prop><d:getetag/><c:calendar-data/></d:prop>`)
	for _, h := range hrefs {
		sb.WriteString("<d:href>")
		sb.WriteString(xmlEscape(h))
		sb.WriteString("</d:href>")
	}
	sb.WriteString(`</c:calendar-multiget>`)
	return sb.String()
}

// batches yields consecutive slices of at most n elements.
func batches[T any](in []T, n int) func(func([]T) bool) {
	if n <= 0 {
		n = defaultBatchSize
	}
	return func(yield func([]T) bool) {
		for i := 0; i < len(in); i += n {
			end := min(i+n, len(in))
			if !yield(in[i:end]) {
				return
			}
		}
	}
}
