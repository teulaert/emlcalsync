// Package gcal implements provider.CalendarProvider on top of the Google
// Calendar API (DESIGN.md §6.3).
//
// Deltas use Google's sync tokens: events.list is called with
// singleEvents=false (so masters and exception instances are returned
// separately, and recurrence is expanded locally by internal/calendar) and
// showDeleted=true. A 410 GONE means the token is too old and the caller must
// re-list from scratch, which is reported as provider.ErrStateExpired.
package gcal

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net/http"

	calendarapi "google.golang.org/api/calendar/v3"
	"google.golang.org/api/option"

	"github.com/teulaert/emlcalsync/internal/model"
	"github.com/teulaert/emlcalsync/internal/provider"
)

// Options configures a Calendar provider.
type Options struct {
	// HTTPClient is an authenticated client, typically from
	// oauth.HTTPClient. Required.
	HTTPClient *http.Client
	// Email is the account's own address. It is the fallback for spotting
	// "me" among the attendees when Google does not set the self flag.
	Email string
	// Logger receives one Debug record per API call. Defaults to
	// slog.Default().
	Logger *slog.Logger
	// Endpoint overrides the API base URL. It replaces the whole base,
	// including the version segment, so it looks like
	// "https://host/calendar/v3/" and must end in "/". Tests point this at
	// an httptest server.
	Endpoint string
	// PageSize overrides the events.list page size (default 2500, the API
	// maximum).
	PageSize int64
}

// Calendar is a Google-Calendar-backed provider.CalendarProvider.
type Calendar struct {
	svc  *calendarapi.Service
	log  *slog.Logger
	opts Options
}

var _ provider.CalendarProvider = (*Calendar)(nil)

const defaultPageSize = 2500

// New builds a Calendar provider. The HTTP client must already carry
// credentials.
func New(ctx context.Context, opts Options) (*Calendar, error) {
	if opts.HTTPClient == nil {
		return nil, errors.New("gcal: Options.HTTPClient is required")
	}
	log := opts.Logger
	if log == nil {
		log = slog.Default()
	}
	log = log.With("provider", "gcal", "email", opts.Email)

	apiOpts := []option.ClientOption{option.WithHTTPClient(opts.HTTPClient)}
	if opts.Endpoint != "" {
		apiOpts = append(apiOpts, option.WithEndpoint(opts.Endpoint))
	}
	svc, err := calendarapi.NewService(ctx, apiOpts...)
	if err != nil {
		return nil, fmt.Errorf("gcal: new service: %w", err)
	}
	if opts.PageSize <= 0 {
		opts.PageSize = defaultPageSize
	}
	return &Calendar{svc: svc, log: log, opts: opts}, nil
}

// Calendars returns every calendar in the user's calendar list.
func (c *Calendar) Calendars(ctx context.Context) ([]model.Calendar, error) {
	var out []model.Calendar
	pageToken := ""
	for {
		var resp *calendarapi.CalendarList
		err := c.do(ctx, "calendarList.list", func() error {
			call := c.svc.CalendarList.List().Context(ctx)
			if pageToken != "" {
				call = call.PageToken(pageToken)
			}
			var err error
			resp, err = call.Do()
			return err
		})
		if err != nil {
			return nil, err
		}
		for _, item := range resp.Items {
			if item.Deleted {
				continue
			}
			name := item.Summary
			if item.SummaryOverride != "" {
				name = item.SummaryOverride
			}
			out = append(out, model.Calendar{
				RemoteID:   item.Id,
				Name:       name,
				Color:      item.BackgroundColor,
				Timezone:   item.TimeZone,
				Primary:    item.Primary,
				AccessRole: item.AccessRole,
			})
		}
		if resp.NextPageToken == "" {
			return out, nil
		}
		pageToken = resp.NextPageToken
	}
}

// EventChanges lists the changes to one calendar since the given sync token.
// since=="" performs a full listing and still returns a fresh token.
//
// Cancelled masters are reported as removals; cancelled instances of a
// recurring event are upserted with model.StatusCancelled, because the local
// index needs them to punch a hole in the expanded occurrences.
func (c *Calendar) EventChanges(ctx context.Context, calendarRemote, since string) (*provider.EventChanges, error) {
	if calendarRemote == "" {
		return nil, errors.New("gcal: EventChanges needs a calendar id")
	}
	out := &provider.EventChanges{}
	pageToken := ""
	for {
		var resp *calendarapi.Events
		err := c.do(ctx, "events.list", func() error {
			call := c.svc.Events.List(calendarRemote).
				SingleEvents(false).
				ShowDeleted(true).
				MaxResults(c.opts.PageSize).
				Context(ctx)
			if since != "" {
				call = call.SyncToken(since)
			}
			if pageToken != "" {
				call = call.PageToken(pageToken)
			}
			var err error
			resp, err = call.Do()
			return err
		})
		if err != nil {
			if isGone(err) {
				return nil, fmt.Errorf("gcal sync token for %s expired: %w",
					calendarRemote, provider.ErrStateExpired)
			}
			return nil, err
		}

		for _, item := range resp.Items {
			if item.Status == statusCancelled && item.RecurringEventId == "" {
				// A cancelled master is a deletion.
				out.Removed = append(out.Removed, item.Id)
				continue
			}
			ev, err := mapEvent(calendarRemote, item, resp.TimeZone, c.opts.Email)
			if err != nil {
				return nil, err
			}
			out.Upserted = append(out.Upserted, ev)
		}

		if resp.NextPageToken != "" {
			pageToken = resp.NextPageToken
			continue
		}
		if resp.NextSyncToken == "" {
			// The last page must carry a sync token. Persisting "" would
			// silently turn every later delta into a full listing, and the
			// changes just collected would be applied against a state that
			// claims nothing was ever synced.
			return nil, fmt.Errorf(
				"gcal: events.list for %s ended with neither nextPageToken nor nextSyncToken",
				calendarRemote)
		}
		out.NewState = resp.NextSyncToken
		return out, nil
	}
}
