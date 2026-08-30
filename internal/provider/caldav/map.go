package caldav

import (
	"encoding/json"
	"fmt"
	"log/slog"
	"strings"
	"time"

	"github.com/emersion/go-ical"

	"github.com/teulaert/emlcalsync/internal/model"
)

// overrideIDSeparator joins an object href and an occurrence stamp to form the
// remote id of an exception instance, exactly as the JMAP provider does. A
// LocalDateTime is full of ':' and model.ParseID splits public ids on that
// character, so the stamp is written compactly.
const overrideIDSeparator = ";"

const stampLayout = "20060102T150405"

// rawObject is what model.Event.RawJSON holds for a CalDAV event: the .ics
// text exactly as the server sent it, plus the coordinates needed to write it
// back conditionally.
type rawObject struct {
	ICS  string `json:"ics"`
	Href string `json:"href"`
	ETag string `json:"etag"`
	// RecurrenceID is set on an exception so a reader can tell which VEVENT
	// inside ICS the row came from.
	RecurrenceID string `json:"recurrence_id,omitempty"`
}

// decodeRaw reads back what parseObject stored. A missing or unparseable value
// is not an error: the caller falls back to re-reading the object.
func decodeRaw(b []byte) (rawObject, bool) {
	if len(b) == 0 {
		return rawObject{}, false
	}
	var ro rawObject
	if err := json.Unmarshal(b, &ro); err != nil || ro.ICS == "" {
		return rawObject{}, false
	}
	return ro, true
}

func (r rawObject) encode() []byte {
	b, err := json.Marshal(r)
	if err != nil { // impossible for a struct of strings
		return nil
	}
	return b
}

// decodeICS parses .ics text into a calendar object.
func decodeICS(s string) (*ical.Calendar, error) {
	cal, err := ical.NewDecoder(strings.NewReader(s)).Decode()
	if err != nil {
		return nil, fmt.Errorf("parse ics: %w", err)
	}
	return cal, nil
}

func encodeICS(cal *ical.Calendar) (string, error) {
	var sb strings.Builder
	if err := ical.NewEncoder(&sb).Encode(cal); err != nil {
		return "", fmt.Errorf("encode ics: %w", err)
	}
	return sb.String(), nil
}

// parseObject maps one calendar object (.ics text) onto the flat event model:
// the master VEVENT, one event per VEVENT carrying RECURRENCE-ID, and one
// cancelled event per EXDATE instant.
//
// Everything in one object shares a UID; the master owns the object's href as
// its remote id and each exception gets "<href>;<stamp>".
func parseObject(calendarRemote, href, etag, ics, selfEmail string, log *slog.Logger) ([]model.Event, error) {
	cal, err := decodeICS(ics)
	if err != nil {
		return nil, fmt.Errorf("caldav object %s: %w", href, err)
	}
	zones := timezoneNames(cal)

	var master *ical.Event
	var overrides []*ical.Event
	for _, ev := range cal.Events() {
		e := ev
		if e.Props.Get(ical.PropRecurrenceID) != nil {
			overrides = append(overrides, &e)
			continue
		}
		if master == nil {
			master = &e
		}
	}
	if master == nil && len(overrides) == 0 {
		return nil, nil // a VTODO/VJOURNAL object, or an empty one
	}

	raw := rawObject{ICS: ics, Href: href, ETag: etag}
	out := make([]model.Event, 0, 1+len(overrides))

	var masterEv *model.Event
	if master != nil {
		ev, err := mapVEvent(master, calendarRemote, href, raw, zones, selfEmail, log)
		if err != nil {
			return nil, err
		}
		out = append(out, ev)
		masterEv = &out[0]
	}

	for i, ov := range overrides {
		ev, err := mapVEvent(ov, calendarRemote, href, raw, zones, selfEmail, log)
		if err != nil {
			return nil, err
		}
		// RFC 5545 §3.8.4.4: an instance carries no recurrence of its own.
		ev.RRule = ""
		if ev.RecurrenceID == "" {
			log.Warn("caldav: VEVENT with an unparseable RECURRENCE-ID", "href", href, "uid", ev.UID)
			continue
		}
		if masterEv == nil && i == 0 {
			// An object with no master (a single moved instance shared on its
			// own) still has to own its href, or deleting the object would
			// match no local row.
			ev.RemoteID = href
		} else {
			ev.RemoteID = exceptionRemoteID(href, ev.RecurrenceID)
		}
		exRaw := raw
		exRaw.RecurrenceID = ev.RecurrenceID
		ev.RawJSON = exRaw.encode()
		out = append(out, ev)
	}

	if masterEv != nil {
		out = append(out, excludedEvents(master, masterEv, href, calendarRemote, raw, zones, log)...)
	}
	return out, nil
}

// mapVEvent converts one VEVENT into the provider-neutral model.
func mapVEvent(in *ical.Event, calendarRemote, href string, raw rawObject,
	zones map[string]string, selfEmail string, log *slog.Logger) (model.Event, error) {

	ev := model.Event{
		CalendarRemote: calendarRemote,
		RemoteID:       href,
		UID:            text(in.Props, ical.PropUID),
		Title:          text(in.Props, ical.PropSummary),
		Description:    text(in.Props, ical.PropDescription),
		Location:       text(in.Props, ical.PropLocation),
		Status:         mapStatus(text(in.Props, ical.PropStatus)),
		RawJSON:        raw.encode(),
	}

	start, allDay, tz, err := propTime(in.Props.Get(ical.PropDateTimeStart), zones, log)
	if err != nil {
		return model.Event{}, fmt.Errorf("caldav object %s: DTSTART: %w", href, err)
	}
	ev.Start, ev.AllDay, ev.Timezone = start, allDay, tz
	ev.End = eventEnd(in, start, allDay, zones, log)

	if p := in.Props.Get(ical.PropRecurrenceRule); p != nil {
		ev.RRule = strings.TrimSpace(p.Value)
	}
	if p := in.Props.Get(ical.PropRecurrenceID); p != nil {
		if rid, _, _, err := propTime(p, zones, log); err == nil && !rid.IsZero() {
			ev.RecurrenceID = rid.Format(time.RFC3339)
		} else if err != nil {
			log.Warn("caldav: unparseable RECURRENCE-ID", "href", href, "value", p.Value, "err", err)
		}
	}
	if p := in.Props.Get(ical.PropOrganizer); p != nil {
		ev.Organizer = model.Address{Name: p.Params.Get(ical.ParamCommonName), Email: mailtoAddr(p.Value)}
	}
	for _, p := range in.Props.Values(ical.PropAttendee) {
		email := mailtoAddr(p.Value)
		if email == "" {
			continue
		}
		self := selfEmail != "" && strings.EqualFold(email, selfEmail)
		att := model.Attendee{
			Name:     p.Params.Get(ical.ParamCommonName),
			Email:    email,
			Response: mapPartStat(p.Params.Get(ical.ParamParticipationStatus)),
			Optional: strings.EqualFold(p.Params.Get(ical.ParamRole), "OPT-PARTICIPANT"),
			Self:     self,
		}
		ev.Attendees = append(ev.Attendees, att)
		if self {
			ev.MyResponse = att.Response
		}
	}
	ev.Updated = lastModified(in, zones, log)
	return ev, nil
}

// excludedEvents turns the master's EXDATE instants into cancelled exception
// events. model.Event carries a single RRULE line and calendar.Expand ignores
// EXDATE, so an excluded occurrence has nowhere else to live; a cancelled
// exception is exactly how calendar.ApplyExceptions drops an instance (the
// JMAP provider materialises "excluded": true overrides the same way).
func excludedEvents(in *ical.Event, master *model.Event, href, calendarRemote string,
	raw rawObject, zones map[string]string, log *slog.Logger) []model.Event {

	dur := master.End.Sub(master.Start)
	if dur < 0 {
		dur = 0
	}
	seen := map[string]bool{}
	var out []model.Event
	for _, p := range in.Props.Values(ical.PropExceptionDates) {
		for _, one := range strings.Split(p.Value, ",") {
			one = strings.TrimSpace(one)
			if one == "" {
				continue
			}
			single := p
			single.Value = one
			t, allDay, tz, err := propTime(&single, zones, log)
			if err != nil || t.IsZero() {
				log.Warn("caldav: unparseable EXDATE", "href", href, "value", one, "err", err)
				continue
			}
			rid := t.Format(time.RFC3339)
			if seen[rid] {
				continue
			}
			seen[rid] = true
			exRaw := raw
			exRaw.RecurrenceID = rid
			out = append(out, model.Event{
				CalendarRemote: calendarRemote,
				RemoteID:       exceptionRemoteID(href, rid),
				UID:            master.UID,
				Title:          master.Title,
				Description:    master.Description,
				Location:       master.Location,
				Start:          t,
				End:            t.Add(dur),
				AllDay:         allDay,
				Timezone:       tz,
				RecurrenceID:   rid,
				Status:         model.StatusCancelled,
				Organizer:      master.Organizer,
				Updated:        master.Updated,
				RawJSON:        exRaw.encode(),
			})
		}
	}
	return out
}

// exceptionRemoteID is the remote id of one materialised occurrence. rid is
// the RFC 3339 recurrence id; the stamp is written compactly so the value
// stays free of ':' (see model.ParseID).
func exceptionRemoteID(href, rid string) string {
	t, err := time.Parse(time.RFC3339, rid)
	if err != nil {
		return href + overrideIDSeparator + strings.NewReplacer(":", "", "-", "").Replace(rid)
	}
	return href + overrideIDSeparator + t.Format(stampLayout)
}

// isExceptionRemoteID reports whether an id names one occurrence rather than a
// whole object.
func isExceptionRemoteID(id string) bool { return strings.Contains(id, overrideIDSeparator) }

// objectHref strips an occurrence suffix, returning the object's own href.
func objectHref(remoteID string) string {
	if i := strings.Index(remoteID, overrideIDSeparator); i >= 0 {
		return remoteID[:i]
	}
	return remoteID
}

// eventEnd resolves DTEND, or DTSTART+DURATION, or the RFC 5545 defaults: a
// whole day for a DATE start, a zero-length instant otherwise.
func eventEnd(in *ical.Event, start time.Time, allDay bool, zones map[string]string, log *slog.Logger) time.Time {
	if p := in.Props.Get(ical.PropDateTimeEnd); p != nil {
		if end, _, _, err := propTime(p, zones, log); err == nil && !end.IsZero() {
			return end
		}
	}
	if p := in.Props.Get(ical.PropDuration); p != nil {
		if d, err := p.Duration(); err == nil {
			return start.Add(d)
		}
		log.Warn("caldav: unparseable DURATION", "value", p.Value)
	}
	if allDay {
		return start.AddDate(0, 0, 1)
	}
	return start
}

// propTime parses a DATE or DATE-TIME property, tolerating a TZID the machine
// has no tzdata for (it falls back to UTC rather than failing the whole sync).
func propTime(p *ical.Prop, zones map[string]string, log *slog.Logger) (t time.Time, allDay bool, tz string, err error) {
	if p == nil {
		return time.Time{}, false, "", nil
	}
	value := strings.TrimSpace(p.Value)
	if value == "" {
		return time.Time{}, false, "", nil
	}
	tzid := p.Params.Get(ical.PropTimezoneID)
	loc := time.UTC
	if tzid != "" {
		if l, ok := loadLocation(tzid, zones); ok {
			loc, tz = l, l.String()
		} else {
			log.Warn("caldav: unknown TZID, reading the value as UTC", "tzid", tzid)
		}
	}

	if p.ValueType() == ical.ValueDate || len(value) == len("20060102") {
		t, err = time.ParseInLocation("20060102", value, loc)
		if err != nil {
			return time.Time{}, true, tz, fmt.Errorf("parse date %q: %w", value, err)
		}
		return t, true, tz, nil
	}
	if strings.HasSuffix(value, "Z") {
		t, err = time.ParseInLocation("20060102T150405Z", value, time.UTC)
		if err != nil {
			return time.Time{}, false, tz, fmt.Errorf("parse utc date-time %q: %w", value, err)
		}
		return t, false, tz, nil
	}
	t, err = time.ParseInLocation("20060102T150405", value, loc)
	if err != nil {
		return time.Time{}, false, tz, fmt.Errorf("parse date-time %q: %w", value, err)
	}
	return t, false, tz, nil
}

// loadLocation resolves a TZID: as an IANA name, then through the
// X-LIC-LOCATION of the VTIMEZONE that defines it (which is how a client that
// invented its own TZID still lands in the right zone), then as the Windows
// name Exchange writes.
func loadLocation(tzid string, zones map[string]string) (*time.Location, bool) {
	if tzid == "" {
		return nil, false
	}
	if loc, err := time.LoadLocation(tzid); err == nil {
		return loc, true
	}
	if alias, ok := zones[tzid]; ok && alias != tzid {
		if loc, err := time.LoadLocation(alias); err == nil {
			return loc, true
		}
	}
	if iana, ok := windowsZones[tzid]; ok {
		if loc, err := time.LoadLocation(iana); err == nil {
			return loc, true
		}
	}
	return nil, false
}

// timezoneNames indexes the object's VTIMEZONE components by TZID, mapping
// each to the IANA name it claims in X-LIC-LOCATION (libical's convention,
// emitted by most servers including Fastmail).
func timezoneNames(cal *ical.Calendar) map[string]string {
	out := map[string]string{}
	for _, child := range cal.Children {
		if !strings.EqualFold(child.Name, ical.CompTimezone) {
			continue
		}
		tzid := strings.TrimSpace(textOf(child.Props, ical.PropTimezoneID))
		if tzid == "" {
			continue
		}
		if loc := strings.TrimSpace(textOf(child.Props, "X-LIC-LOCATION")); loc != "" {
			out[tzid] = loc
		}
	}
	return out
}

func lastModified(in *ical.Event, zones map[string]string, log *slog.Logger) time.Time {
	for _, name := range []string{ical.PropLastModified, ical.PropDateTimeStamp} {
		if p := in.Props.Get(name); p != nil {
			if t, _, _, err := propTime(p, zones, log); err == nil && !t.IsZero() {
				return t.UTC()
			}
		}
	}
	return time.Time{}
}

// text reads a property's value, returning "" when it is absent.
func text(props ical.Props, name string) string { return textOf(props, name) }

func textOf(props ical.Props, name string) string {
	p := props.Get(name)
	if p == nil {
		return ""
	}
	// Text values arrive escaped (\, \; \n); Text() unescapes them.
	if v, err := p.Text(); err == nil {
		return v
	}
	return p.Value
}

// mailtoAddr strips the "mailto:" scheme from a CAL-ADDRESS.
func mailtoAddr(v string) string {
	v = strings.TrimSpace(v)
	if i := strings.Index(strings.ToLower(v), "mailto:"); i == 0 {
		return strings.TrimSpace(v[len("mailto:"):])
	}
	return v
}

func mapStatus(s string) model.EventStatus {
	switch strings.ToUpper(strings.TrimSpace(s)) {
	case "TENTATIVE":
		return model.StatusTentative
	case "CANCELLED":
		return model.StatusCancelled
	default:
		return model.StatusConfirmed
	}
}

func mapPartStat(s string) model.Participation {
	switch strings.ToUpper(strings.TrimSpace(s)) {
	case "ACCEPTED":
		return model.PartAccepted
	case "DECLINED":
		return model.PartDeclined
	case "TENTATIVE":
		return model.PartTentative
	case "NEEDS-ACTION":
		return model.PartNeedsAction
	default:
		return ""
	}
}

// partStatString is mapPartStat's inverse, for writes.
func partStatString(p model.Participation) string {
	switch p {
	case model.PartAccepted:
		return "ACCEPTED"
	case model.PartDeclined:
		return "DECLINED"
	case model.PartTentative:
		return "TENTATIVE"
	case model.PartNeedsAction:
		return "NEEDS-ACTION"
	default:
		return ""
	}
}

func statusString(s model.EventStatus) string {
	switch s {
	case model.StatusTentative:
		return "TENTATIVE"
	case model.StatusCancelled:
		return "CANCELLED"
	case model.StatusConfirmed:
		return "CONFIRMED"
	default:
		return ""
	}
}
