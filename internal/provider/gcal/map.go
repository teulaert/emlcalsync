package gcal

import (
	"crypto/rand"
	"encoding/binary"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"strings"
	"time"

	calendarapi "google.golang.org/api/calendar/v3"

	"github.com/teulaert/emlcalsync/internal/model"
)

const (
	statusConfirmed = "confirmed"
	statusTentative = "tentative"
	statusCancelled = "cancelled"

	dateLayout = "2006-01-02"
)

// mapEvent converts one API event into the provider-neutral model. calTZ is
// the calendar's own IANA timezone, used for all-day events and whenever the
// event carries no zone of its own. selfEmail identifies "me" among the
// attendees when Google does not set the self flag (it omits it for some
// shared calendars).
func mapEvent(calendarRemote string, in *calendarapi.Event, calTZ, selfEmail string) (model.Event, error) {
	ev := model.Event{
		CalendarRemote: calendarRemote,
		RemoteID:       in.Id,
		UID:            in.ICalUID,
		Title:          in.Summary,
		Description:    in.Description,
		Location:       in.Location,
		ConferenceURL:  conferenceURL(in),
		Status:         mapStatus(in.Status),
	}

	start, allDay, tz, err := parseEventTime(in.Start, calTZ)
	if err != nil {
		return model.Event{}, fmt.Errorf("gcal event %s: start: %w", in.Id, err)
	}
	end, _, _, err := parseEventTime(in.End, calTZ)
	if err != nil {
		return model.Event{}, fmt.Errorf("gcal event %s: end: %w", in.Id, err)
	}

	// A cancelled instance of a recurring event is delivered stripped down to
	// id, status, recurringEventId and originalStartTime — no start, no end.
	// Fall back to the original start so the hole it punches can still be
	// matched against an expanded occurrence, and so the event never reaches
	// the index with a zero (i.e. 1970) start.
	var orig time.Time
	var origAllDay bool
	var origTZ string
	if in.OriginalStartTime != nil {
		orig, origAllDay, origTZ, err = parseEventTime(in.OriginalStartTime, calTZ)
		if err != nil {
			return model.Event{}, fmt.Errorf("gcal event %s: originalStartTime: %w", in.Id, err)
		}
	}
	if start.IsZero() && !orig.IsZero() {
		start, allDay, tz = orig, origAllDay, origTZ
	}
	if end.IsZero() {
		// A point in time is better than 1970: the occurrence matcher only
		// needs the start, and a zero end would be stored as end_utc=0.
		end = start
	}
	ev.Start, ev.End, ev.AllDay, ev.Timezone = start, end, allDay, tz

	// Only the RRULE is modelled; EXDATE/RDATE stay in RawJSON, which the
	// recurrence expander reads for exceptions.
	for _, line := range in.Recurrence {
		if rule, ok := strings.CutPrefix(line, "RRULE:"); ok {
			ev.RRule = rule
			break
		}
	}

	if in.RecurringEventId != "" && !orig.IsZero() {
		ev.RecurrenceID = orig.Format(time.RFC3339)
	}

	if in.Organizer != nil {
		ev.Organizer = model.Address{Name: in.Organizer.DisplayName, Email: in.Organizer.Email}
	}
	for _, a := range in.Attendees {
		self := a.Self || (selfEmail != "" && strings.EqualFold(a.Email, selfEmail))
		att := model.Attendee{
			Name:     a.DisplayName,
			Email:    a.Email,
			Response: mapResponse(a.ResponseStatus),
			Optional: a.Optional,
			Self:     self,
		}
		ev.Attendees = append(ev.Attendees, att)
		if self {
			ev.MyResponse = att.Response
		}
	}

	if in.Updated != "" {
		if t, err := time.Parse(time.RFC3339, in.Updated); err == nil {
			ev.Updated = t.UTC()
		}
	}

	raw, err := json.Marshal(in)
	if err != nil {
		return model.Event{}, fmt.Errorf("gcal event %s: keep raw json: %w", in.Id, err)
	}
	ev.RawJSON = raw
	return ev, nil
}

func mapStatus(s string) model.EventStatus {
	switch s {
	case statusTentative:
		return model.StatusTentative
	case statusCancelled:
		return model.StatusCancelled
	default:
		return model.StatusConfirmed
	}
}

func mapResponse(s string) model.Participation {
	switch s {
	case "accepted":
		return model.PartAccepted
	case "declined":
		return model.PartDeclined
	case "tentative":
		return model.PartTentative
	case "needsAction":
		return model.PartNeedsAction
	default:
		return ""
	}
}

// responseString is mapResponse's inverse, for writes.
func responseString(p model.Participation) string {
	switch p {
	case model.PartAccepted:
		return "accepted"
	case model.PartDeclined:
		return "declined"
	case model.PartTentative:
		return "tentative"
	case model.PartNeedsAction:
		return "needsAction"
	default:
		return ""
	}
}

// parseEventTime handles both shapes of an EventDateTime: a timed event
// (dateTime + optional timeZone) and an all-day one (date), which becomes
// midnight in the calendar's timezone.
//
// Note that Google's all-day end date is exclusive — a one-day event ends on
// the following day — and that convention is preserved as-is.
func parseEventTime(dt *calendarapi.EventDateTime, calTZ string) (t time.Time, allDay bool, tz string, err error) {
	if dt == nil {
		return time.Time{}, false, calTZ, nil
	}
	tz = dt.TimeZone
	if tz == "" {
		tz = calTZ
	}
	loc := loadLocation(tz)

	switch {
	case dt.DateTime != "":
		t, err = time.Parse(time.RFC3339, dt.DateTime)
		if err != nil {
			return time.Time{}, false, tz, fmt.Errorf("parse dateTime %q: %w", dt.DateTime, err)
		}
		if loc != nil {
			t = t.In(loc)
		}
		return t, false, tz, nil
	case dt.Date != "":
		if loc == nil {
			loc = time.UTC
		}
		t, err = time.ParseInLocation(dateLayout, dt.Date, loc)
		if err != nil {
			return time.Time{}, true, tz, fmt.Errorf("parse date %q: %w", dt.Date, err)
		}
		return t, true, tz, nil
	}
	return time.Time{}, false, tz, nil
}

// loadLocation resolves an IANA name, returning nil when it is unknown (a
// machine without tzdata, or a Windows-style zone name from an old client).
func loadLocation(name string) *time.Location {
	if name == "" {
		return nil
	}
	loc, err := time.LoadLocation(name)
	if err != nil {
		return nil
	}
	return loc
}

// toAPITime is parseEventTime's inverse.
func toAPITime(t time.Time, allDay bool, tz string) *calendarapi.EventDateTime {
	if t.IsZero() {
		return nil
	}
	if allDay {
		return &calendarapi.EventDateTime{Date: t.Format(dateLayout)}
	}
	out := &calendarapi.EventDateTime{DateTime: t.Format(time.RFC3339)}
	// "Local" is Go's name for an unresolved system zone, not an IANA
	// identifier: Google rejects it outright. The RFC 3339 offset in
	// DateTime already pins the instant, so it is safer to say nothing.
	if tz != "" && tz != "Local" {
		out.TimeZone = tz
	}
	return out
}

// toAPIEvent builds the API object for a create. Only fields emlcal models are
// sent; everything else the server owns.
func toAPIEvent(ev *model.Event) *calendarapi.Event {
	out := &calendarapi.Event{
		Summary:     ev.Title,
		Description: ev.Description,
		Location:    ev.Location,
		Start:       toAPITime(ev.Start, ev.AllDay, ev.Timezone),
		End:         toAPITime(ev.End, ev.AllDay, ev.Timezone),
	}
	if ev.RRule != "" {
		out.Recurrence = []string{"RRULE:" + ev.RRule}
	}
	if ev.Status != "" {
		out.Status = string(ev.Status)
	}
	for _, a := range ev.Attendees {
		out.Attendees = append(out.Attendees, &calendarapi.EventAttendee{
			DisplayName:    a.Name,
			Email:          a.Email,
			Optional:       a.Optional,
			ResponseStatus: responseString(a.Response),
		})
	}
	if ev.CreateConference {
		out.ConferenceData = newConferenceRequest()
	}
	return out
}

// conferenceURL extracts the video-call link Google attached to an event: the
// hangoutLink when it is set, else the video entry point of the conference
// data (add-on conferencing sets only the latter).
func conferenceURL(in *calendarapi.Event) string {
	if in.HangoutLink != "" {
		return in.HangoutLink
	}
	if in.ConferenceData == nil {
		return ""
	}
	for _, ep := range in.ConferenceData.EntryPoints {
		if ep != nil && ep.EntryPointType == "video" && ep.Uri != "" {
			return ep.Uri
		}
	}
	return ""
}

// newConferenceRequest asks the server to mint a Google Meet room for the
// event. The request id only has to be unique per request — the server
// ignores a repeat of an id it has already honoured.
func newConferenceRequest() *calendarapi.ConferenceData {
	var buf [16]byte
	if _, err := rand.Read(buf[:]); err != nil {
		// crypto/rand failing means a broken platform; a clock-based id is
		// still unique enough per request to mint the room.
		binary.BigEndian.PutUint64(buf[:8], uint64(time.Now().UnixNano()))
	}
	return &calendarapi.ConferenceData{
		CreateRequest: &calendarapi.CreateConferenceRequest{
			RequestId:             "emlcal-" + hex.EncodeToString(buf[:]),
			ConferenceSolutionKey: &calendarapi.ConferenceSolutionKey{Type: "hangoutsMeet"},
		},
	}
}

// toAPIPatch builds a minimal patch: only the fields that are actually set on
// ev are sent, so a partial model.Event never blanks server-side data.
func toAPIPatch(ev *model.Event) *calendarapi.Event {
	patch := &calendarapi.Event{}
	if ev.Title != "" {
		patch.Summary = ev.Title
	}
	if ev.Description != "" {
		patch.Description = ev.Description
	}
	if ev.Location != "" {
		patch.Location = ev.Location
	}
	if !ev.Start.IsZero() {
		patch.Start = toAPITime(ev.Start, ev.AllDay, ev.Timezone)
	}
	if !ev.End.IsZero() {
		patch.End = toAPITime(ev.End, ev.AllDay, ev.Timezone)
	}
	if ev.RRule != "" {
		patch.Recurrence = []string{"RRULE:" + ev.RRule}
	}
	if ev.Status != "" {
		patch.Status = string(ev.Status)
	}
	if len(ev.Attendees) > 0 {
		for _, a := range ev.Attendees {
			patch.Attendees = append(patch.Attendees, &calendarapi.EventAttendee{
				DisplayName:    a.Name,
				Email:          a.Email,
				Optional:       a.Optional,
				ResponseStatus: responseString(a.Response),
			})
		}
	}
	if ev.CreateConference {
		patch.ConferenceData = newConferenceRequest()
	}
	return patch
}
