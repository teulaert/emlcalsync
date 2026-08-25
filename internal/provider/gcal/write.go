package gcal

import (
	"context"
	"errors"
	"fmt"
	"strings"

	calendarapi "google.golang.org/api/calendar/v3"

	"github.com/lennert/emlcal/internal/model"
)

// sendUpdatesAll is the value of the sendUpdates query parameter that makes
// Google mail an invitation, update or cancellation to every guest. The
// parameter is shared by events.insert, events.patch/update and events.delete;
// its other values are "externalOnly" and "none", and omitting it falls back
// to the deprecated sendNotifications default, which sends nothing.
//
// Without it a guest never hears that they were invited, that the meeting
// moved or that it was called off, which is the single most visible way a
// calendar sync can misbehave.
const sendUpdatesAll = "all"

// hasGuests reports whether an event has attendees other than the account
// itself. Notifying is pointless — and noisy — for a private appointment, so
// the parameter is only sent when somebody else is on the invitation.
func (c *Calendar) hasGuests(ev *model.Event) bool {
	if ev == nil {
		return false
	}
	for _, a := range ev.Attendees {
		if a.Self {
			continue
		}
		if c.opts.Email != "" && strings.EqualFold(a.Email, c.opts.Email) {
			continue
		}
		return true
	}
	return false
}

// CreateEvent inserts a new event and returns it as the server stored it.
func (c *Calendar) CreateEvent(ctx context.Context, calendarRemote string, ev *model.Event) (*model.Event, error) {
	if calendarRemote == "" || ev == nil {
		return nil, errors.New("gcal: CreateEvent needs a calendar id and an event")
	}
	var created *calendarapi.Event
	err := c.do(ctx, "events.insert", func() error {
		call := c.svc.Events.Insert(calendarRemote, toAPIEvent(ev)).Context(ctx)
		if c.hasGuests(ev) {
			call = call.SendUpdates(sendUpdatesAll)
		}
		var err error
		created, err = call.Do()
		return err
	})
	if err != nil {
		return nil, err
	}
	out, err := mapEvent(calendarRemote, created, ev.Timezone, c.opts.Email)
	if err != nil {
		return nil, err
	}
	return &out, nil
}

// UpdateEvent patches an existing event with only the fields that are set on
// ev, so a caller can change a title without resending the whole object.
func (c *Calendar) UpdateEvent(ctx context.Context, ev *model.Event) (*model.Event, error) {
	if ev == nil || ev.CalendarRemote == "" || ev.RemoteID == "" {
		return nil, errors.New("gcal: UpdateEvent needs an event with a calendar and remote id")
	}
	var updated *calendarapi.Event
	err := c.do(ctx, "events.patch", func() error {
		call := c.svc.Events.Patch(ev.CalendarRemote, ev.RemoteID, toAPIPatch(ev)).Context(ctx)
		if c.hasGuests(ev) {
			call = call.SendUpdates(sendUpdatesAll)
		}
		var err error
		updated, err = call.Do()
		return err
	})
	if err != nil {
		if isNotFound(err) {
			return nil, fmt.Errorf("gcal event %s: %w", ev.RemoteID, model.ErrNotFound)
		}
		return nil, err
	}
	out, err := mapEvent(ev.CalendarRemote, updated, ev.Timezone, c.opts.Email)
	if err != nil {
		return nil, err
	}
	return &out, nil
}

// DeleteEvent removes an event. Deleting one that is already gone is not an
// error: the outbox may well be retrying.
//
// sendUpdates=all is always sent here. Unlike Create/UpdateEvent this call is
// handed nothing but an id, so there is no attendee list to inspect, and
// cancelling a meeting without telling the guests is much worse than passing
// the parameter for a solo appointment, where it is a no-op.
func (c *Calendar) DeleteEvent(ctx context.Context, calendarRemote, remoteID string) error {
	if calendarRemote == "" || remoteID == "" {
		return errors.New("gcal: DeleteEvent needs a calendar id and an event id")
	}
	err := c.do(ctx, "events.delete", func() error {
		return c.svc.Events.Delete(calendarRemote, remoteID).
			SendUpdates(sendUpdatesAll).Context(ctx).Do()
	})
	if err != nil && isNotFound(err) {
		c.log.Debug("gcal event already deleted", "event", remoteID)
		return nil
	}
	return err
}

// Respond sets the user's own participation status by patching the self
// attendee. Google requires the full attendee list on a patch, so the event is
// read first.
func (c *Calendar) Respond(ctx context.Context, calendarRemote, remoteID string, resp model.Participation) error {
	status := responseString(resp)
	if status == "" {
		return fmt.Errorf("gcal: unknown participation status %q", resp)
	}
	var ev *calendarapi.Event
	err := c.do(ctx, "events.get", func() error {
		var err error
		ev, err = c.svc.Events.Get(calendarRemote, remoteID).Context(ctx).Do()
		return err
	})
	if err != nil {
		if isNotFound(err) {
			return fmt.Errorf("gcal event %s: %w", remoteID, model.ErrNotFound)
		}
		return err
	}

	patch := &calendarapi.Event{}
	found := false
	for _, a := range ev.Attendees {
		cp := *a
		if a.Self || (c.opts.Email != "" && strings.EqualFold(a.Email, c.opts.Email)) {
			cp.ResponseStatus = status
			found = true
		}
		patch.Attendees = append(patch.Attendees, &cp)
	}
	if !found {
		return fmt.Errorf("gcal event %s: %q is not an attendee, cannot respond",
			remoteID, c.opts.Email)
	}
	// A response is only interesting to the other guests, so it is always
	// announced.
	return c.do(ctx, "events.patch", func() error {
		_, err := c.svc.Events.Patch(calendarRemote, remoteID, patch).
			SendUpdates(sendUpdatesAll).Context(ctx).Do()
		return err
	})
}
