package caldav

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/emersion/go-ical"

	"github.com/teulaert/emlcalsync/internal/model"
)

// prodID identifies emlcal in the objects it writes.
const prodID = "-//emlcal//CalDAV//EN"

// CreateEvent writes a new object into the calendar collection and returns it
// as the server stored it.
//
// Fastmail performs iTIP scheduling itself when an object with ATTENDEEs is
// PUT into the organiser's calendar, so no separate invitation step is needed.
func (c *Calendar) CreateEvent(ctx context.Context, calendarRemote string, ev *model.Event) (*model.Event, error) {
	if calendarRemote == "" || ev == nil {
		return nil, errors.New("caldav: CreateEvent needs a calendar path and an event")
	}
	uid := strings.TrimSpace(ev.UID)
	if uid == "" {
		var err error
		if uid, err = newUID(); err != nil {
			return nil, err
		}
	}
	href := strings.TrimSuffix(calendarRemote, "/") + "/" + objectFilename(uid)

	cal := ical.NewCalendar()
	cal.Props.SetText(ical.PropProductID, prodID)
	cal.Props.SetText(ical.PropVersion, "2.0")
	vevent := ical.NewEvent()
	vevent.Props.SetText(ical.PropUID, uid)
	setStamp(vevent, c.now())
	vevent.Props.Set(intProp(ical.PropSequence, 0))
	applyModel(vevent, ev, true)
	cal.Children = append(cal.Children, vevent.Component)

	ics, err := encodeICS(cal)
	if err != nil {
		return nil, wrapErr("create event", err)
	}
	// If-None-Match: * refuses to clobber an object that already lives at
	// this href — a retried outbox entry must not overwrite a later edit.
	if err := c.put(ctx, href, ics, "", true); err != nil {
		return nil, wrapErr("create event "+href, err)
	}
	return c.readBack(ctx, calendarRemote, href, ics)
}

// UpdateEvent rewrites the object's master VEVENT from ev, preserving every
// property this package does not model (alarms, colours, attachments,
// scheduling state) and every exception VEVENT in the same object.
//
// The write is conditional on the ETag recorded in ev.RawJSON, so an edit made
// elsewhere between the last sync and this write is reported rather than lost.
func (c *Calendar) UpdateEvent(ctx context.Context, ev *model.Event) (*model.Event, error) {
	if ev == nil || ev.RemoteID == "" {
		return nil, errors.New("caldav: UpdateEvent needs an event with a remote id")
	}
	if isExceptionRemoteID(ev.RemoteID) {
		return nil, fmt.Errorf("caldav: %s is one occurrence of %s, and writing a single "+
			"occurrence back as a RECURRENCE-ID override is not implemented",
			ev.RemoteID, objectHref(ev.RemoteID))
	}
	href := ev.RemoteID
	calendarRemote := ev.CalendarRemote
	if calendarRemote == "" {
		calendarRemote = collectionOf(href)
	}

	ics, etag := "", ""
	if raw, ok := decodeRaw(ev.RawJSON); ok && raw.Href == href {
		ics, etag = raw.ICS, raw.ETag
	} else {
		var err error
		if ics, etag, err = c.getObject(ctx, href); err != nil {
			return nil, err
		}
	}
	cal, err := decodeICS(ics)
	if err != nil {
		return nil, wrapErr("update event "+href, err)
	}
	master := masterOf(cal)
	if master == nil {
		return nil, fmt.Errorf("caldav object %s holds no master VEVENT to update", href)
	}
	applyModel(master, ev, false)
	setStamp(master, c.now())
	bumpSequence(master)

	next, err := encodeICS(cal)
	if err != nil {
		return nil, wrapErr("update event "+href, err)
	}
	if err := c.put(ctx, href, next, etag, false); err != nil {
		return nil, wrapErr("update event "+href, err)
	}
	return c.readBack(ctx, calendarRemote, href, next)
}

// DeleteEvent removes an object. Deleting one that is already gone is not an
// error: the outbox may well be retrying a write that did land.
func (c *Calendar) DeleteEvent(ctx context.Context, calendarRemote, remoteID string) error {
	if remoteID == "" {
		return errors.New("caldav: DeleteEvent needs a remote id")
	}
	if isExceptionRemoteID(remoteID) {
		return fmt.Errorf("caldav: %s is one occurrence of %s, and excluding a single "+
			"occurrence with an EXDATE is not implemented",
			remoteID, objectHref(remoteID))
	}
	_, err := c.dav.do(ctx, http.MethodDelete, remoteID, nil, nil)
	if err != nil {
		if isNotFound(err) {
			c.log.Debug("caldav object already deleted", "href", remoteID)
			return nil
		}
		return wrapErr("delete event "+remoteID, err)
	}
	return nil
}

// Respond sets the account's own PARTSTAT on every VEVENT in the object and
// PUTs it back, which is how a CalDAV client sends an RSVP: the server turns
// the changed attendee line into the iTIP REPLY.
func (c *Calendar) Respond(ctx context.Context, calendarRemote, remoteID string, resp model.Participation) error {
	partstat := partStatString(resp)
	if partstat == "" {
		return fmt.Errorf("caldav: unknown participation status %q", resp)
	}
	if isExceptionRemoteID(remoteID) {
		return fmt.Errorf("caldav: %s is one occurrence of %s, and responding to a single "+
			"occurrence is not implemented", remoteID, objectHref(remoteID))
	}
	ics, etag, err := c.getObject(ctx, remoteID)
	if err != nil {
		return err
	}
	cal, err := decodeICS(ics)
	if err != nil {
		return wrapErr("respond "+remoteID, err)
	}
	found := false
	for _, child := range cal.Children {
		if !strings.EqualFold(child.Name, ical.CompEvent) {
			continue
		}
		for i := range child.Props[ical.PropAttendee] {
			p := &child.Props[ical.PropAttendee][i]
			if !strings.EqualFold(mailtoAddr(p.Value), c.opts.Email) {
				continue
			}
			p.Params.Set(ical.ParamParticipationStatus, partstat)
			found = true
		}
		if found {
			setStamp(&ical.Event{Component: child}, c.now())
		}
	}
	if !found {
		return fmt.Errorf("caldav object %s: %q is not an attendee, cannot respond",
			remoteID, c.opts.Email)
	}
	next, err := encodeICS(cal)
	if err != nil {
		return wrapErr("respond "+remoteID, err)
	}
	if err := c.put(ctx, remoteID, next, etag, false); err != nil {
		return wrapErr("respond "+remoteID, err)
	}
	return nil
}

// ---------------------------------------------------------------------------
// helpers

func (c *Calendar) now() time.Time { return time.Now().UTC() }

// put writes an object. ifMatch makes the write conditional on the ETag we
// last saw; ifNoneMatch refuses to overwrite an existing object at all.
func (c *Calendar) put(ctx context.Context, href, ics, ifMatch string, ifNoneMatch bool) error {
	hdr := map[string]string{"Content-Type": "text/calendar; charset=utf-8"}
	switch {
	case ifNoneMatch:
		hdr["If-None-Match"] = "*"
	case ifMatch != "":
		hdr["If-Match"] = quoteETag(ifMatch)
	}
	c.log.Debug("caldav put", "href", href, "bytes", len(ics), "if-match", ifMatch)
	_, err := c.dav.do(ctx, http.MethodPut, href, []byte(ics), hdr)
	if statusOf(err) == http.StatusPreconditionFailed {
		return fmt.Errorf("%w: the event changed on the server since it was last synced; sync and retry", err)
	}
	return err
}

// getObject fetches one object's .ics text and ETag.
func (c *Calendar) getObject(ctx context.Context, href string) (ics, etag string, err error) {
	resp, err := c.dav.do(ctx, http.MethodGet, href, nil, map[string]string{"Accept": "text/calendar"})
	if err != nil {
		if isNotFound(err) {
			return "", "", fmt.Errorf("caldav object %s: %w", href, model.ErrNotFound)
		}
		return "", "", wrapErr("get "+href, err)
	}
	return string(resp.Body), unquoteETag(resp.Header.Get("ETag")), nil
}

// readBack re-reads the object after a write. The server rewrites what it is
// handed — Fastmail adds scheduling properties and normalises timezones — so
// mapping the request body would store an .ics (and an ETag) that no longer
// matches the resource, and the next conditional update would then quietly
// drop whatever the server added.
func (c *Calendar) readBack(ctx context.Context, calendarRemote, href, sent string) (*model.Event, error) {
	ics, etag, err := c.getObject(ctx, href)
	if err != nil {
		c.log.Warn("caldav: cannot re-read the object just written", "href", href, "err", err)
		ics = sent
	}
	events, err := parseObject(calendarRemote, href, etag, ics, c.opts.Email, c.log)
	if err != nil {
		return nil, wrapErr("read back "+href, err)
	}
	for i := range events {
		if events[i].RemoteID == href {
			return &events[i], nil
		}
	}
	if len(events) > 0 {
		return &events[0], nil
	}
	return nil, fmt.Errorf("caldav object %s holds no VEVENT after the write", href)
}

// masterOf returns the VEVENT without a RECURRENCE-ID.
func masterOf(cal *ical.Calendar) *ical.Event {
	for _, child := range cal.Children {
		if !strings.EqualFold(child.Name, ical.CompEvent) {
			continue
		}
		if child.Props.Get(ical.PropRecurrenceID) == nil {
			return &ical.Event{Component: child}
		}
	}
	return nil
}

// applyModel writes the modelled fields onto a VEVENT. On a create everything
// is written; on an update only the fields the caller actually set are, so a
// partial model.Event never blanks server-side data (mirroring the Google
// Calendar provider's patch semantics).
func applyModel(ve *ical.Event, ev *model.Event, create bool) {
	setText := func(name, v string) {
		switch {
		case v != "":
			ve.Props.SetText(name, v)
		case create:
			ve.Props.Del(name)
		}
	}
	setText(ical.PropSummary, ev.Title)
	setText(ical.PropDescription, ev.Description)
	setText(ical.PropLocation, ev.Location)

	if !ev.Start.IsZero() {
		setTimeProp(ve.Props, ical.PropDateTimeStart, ev.Start, ev.AllDay, ev.Timezone)
		if !ev.End.IsZero() {
			setTimeProp(ve.Props, ical.PropDateTimeEnd, ev.End, ev.AllDay, ev.Timezone)
		} else if create {
			ve.Props.Del(ical.PropDateTimeEnd)
		}
		// DTEND and DURATION must not coexist (RFC 5545 §3.6.1).
		if ve.Props.Get(ical.PropDateTimeEnd) != nil {
			ve.Props.Del(ical.PropDuration)
		}
	}

	switch {
	case ev.RRule != "":
		p := ical.NewProp(ical.PropRecurrenceRule)
		p.Value = strings.TrimPrefix(strings.TrimSpace(ev.RRule), "RRULE:")
		ve.Props.Set(p)
	case create:
		ve.Props.Del(ical.PropRecurrenceRule)
	}

	if s := statusString(ev.Status); s != "" {
		ve.Props.SetText(ical.PropStatus, s)
	} else if create {
		ve.Props.Del(ical.PropStatus)
	}

	if ev.Organizer.Email != "" {
		p := ical.NewProp(ical.PropOrganizer)
		p.Value = "mailto:" + ev.Organizer.Email
		if ev.Organizer.Name != "" {
			p.Params.Set(ical.ParamCommonName, ev.Organizer.Name)
		}
		ve.Props.Set(p)
	}

	if len(ev.Attendees) > 0 || create {
		ve.Props.Del(ical.PropAttendee)
		for _, a := range ev.Attendees {
			if a.Email == "" {
				continue
			}
			p := ical.NewProp(ical.PropAttendee)
			p.Value = "mailto:" + a.Email
			if a.Name != "" {
				p.Params.Set(ical.ParamCommonName, a.Name)
			}
			role := "REQ-PARTICIPANT"
			if a.Optional {
				role = "OPT-PARTICIPANT"
			}
			p.Params.Set(ical.ParamRole, role)
			ps := partStatString(a.Response)
			if ps == "" {
				ps = "NEEDS-ACTION"
			}
			p.Params.Set(ical.ParamParticipationStatus, ps)
			ve.Props.Add(p)
		}
	}
}

// setTimeProp writes a DATE or DATE-TIME value, carrying the event's TZID when
// it names a zone this machine knows and falling back to UTC when it does not.
func setTimeProp(props ical.Props, name string, t time.Time, allDay bool, tz string) {
	p := ical.NewProp(name)
	if allDay {
		p.SetValueType(ical.ValueDate)
		p.Value = t.Format("20060102")
		props.Set(p)
		return
	}
	if tz != "" {
		if loc, err := time.LoadLocation(tz); err == nil {
			p.Params.Set(ical.PropTimezoneID, tz)
			p.Value = t.In(loc).Format("20060102T150405")
			props.Set(p)
			return
		}
	}
	p.Value = t.UTC().Format("20060102T150405Z")
	props.Set(p)
}

// setStamp refreshes DTSTAMP and LAST-MODIFIED, which every write must do.
func setStamp(ve *ical.Event, now time.Time) {
	for _, name := range []string{ical.PropDateTimeStamp, ical.PropLastModified} {
		p := ical.NewProp(name)
		p.Value = now.UTC().Format("20060102T150405Z")
		ve.Props.Set(p)
	}
}

// bumpSequence increments SEQUENCE so attendees' clients accept the update.
func bumpSequence(ve *ical.Event) {
	seq := 0
	if p := ve.Props.Get(ical.PropSequence); p != nil {
		if n, err := strconv.Atoi(strings.TrimSpace(p.Value)); err == nil {
			seq = n
		}
	}
	ve.Props.Set(intProp(ical.PropSequence, seq+1))
}

func intProp(name string, v int) *ical.Prop {
	p := ical.NewProp(name)
	p.Value = strconv.Itoa(v)
	return p
}

// newUID mints a globally unique event id.
func newUID() (string, error) {
	var b [16]byte
	if _, err := rand.Read(b[:]); err != nil {
		return "", fmt.Errorf("caldav: generate uid: %w", err)
	}
	return hex.EncodeToString(b[:]) + "@emlcal", nil
}

// objectFilename turns a UID into a safe last path segment.
func objectFilename(uid string) string {
	var sb strings.Builder
	for _, r := range uid {
		switch {
		case r >= 'a' && r <= 'z', r >= 'A' && r <= 'Z', r >= '0' && r <= '9',
			r == '-', r == '_', r == '.', r == '@':
			sb.WriteRune(r)
		default:
			sb.WriteByte('-')
		}
	}
	name := strings.Trim(sb.String(), ".-")
	if name == "" {
		name = "event"
	}
	if len(name) > 200 {
		name = name[:200]
	}
	return name + ".ics"
}

// collectionOf is the collection an object href lives in.
func collectionOf(href string) string {
	if i := strings.LastIndex(strings.TrimSuffix(href, "/"), "/"); i >= 0 {
		return href[:i+1]
	}
	return href
}
