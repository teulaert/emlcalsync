package jmap

import (
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"maps"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/lennert/emlcal/internal/model"
)

// localDateTimeLayout is JSCalendar's LocalDateTime (RFC 8984 §1.4.4): a
// date-time with no zone; the zone comes from the event's timeZone property.
const localDateTimeLayout = "2006-01-02T15:04:05"

// ---------------------------------------------------------------------------
// JSCalendar wire types (RFC 8984) plus the JMAP CalendarEvent additions.

type jsLocation struct {
	Type        string `json:"@type,omitempty"`
	Name        string `json:"name,omitempty"`
	Description string `json:"description,omitempty"`
	TimeZone    string `json:"timeZone,omitempty"`
}

type jsParticipant struct {
	Type                string            `json:"@type,omitempty"`
	Name                string            `json:"name,omitempty"`
	Email               string            `json:"email,omitempty"`
	SendTo              map[string]string `json:"sendTo,omitempty"`
	Kind                string            `json:"kind,omitempty"`
	Roles               map[string]bool   `json:"roles,omitempty"`
	ParticipationStatus string            `json:"participationStatus,omitempty"`
	ExpectReply         bool              `json:"expectReply,omitempty"`
	ScheduleAgent       string            `json:"scheduleAgent,omitempty"`
}

// address returns the participant's mail address, preferring the explicit
// email property and falling back to the imip sendTo URI.
func (p jsParticipant) address() string {
	if p.Email != "" {
		return p.Email
	}
	if v := p.SendTo["imip"]; v != "" {
		return strings.TrimPrefix(v, "mailto:")
	}
	return ""
}

type jsNDay struct {
	Type        string `json:"@type,omitempty"`
	Day         string `json:"day"`
	NthOfPeriod *int   `json:"nthOfPeriod,omitempty"`
}

type jsRecurrenceRule struct {
	Type           string   `json:"@type,omitempty"`
	Frequency      string   `json:"frequency,omitempty"`
	Interval       int      `json:"interval,omitempty"`
	RScale         string   `json:"rscale,omitempty"`
	Skip           string   `json:"skip,omitempty"`
	FirstDayOfWeek string   `json:"firstDayOfWeek,omitempty"`
	ByDay          []jsNDay `json:"byDay,omitempty"`
	ByMonthDay     []int    `json:"byMonthDay,omitempty"`
	ByMonth        []string `json:"byMonth,omitempty"`
	ByYearDay      []int    `json:"byYearDay,omitempty"`
	ByWeekNo       []int    `json:"byWeekNo,omitempty"`
	ByHour         []int    `json:"byHour,omitempty"`
	ByMinute       []int    `json:"byMinute,omitempty"`
	BySecond       []int    `json:"bySecond,omitempty"`
	BySetPosition  []int    `json:"bySetPosition,omitempty"`
	Count          int      `json:"count,omitempty"`
	Until          string   `json:"until,omitempty"`
}

// jsEvent is a JSCalendar Event as served by CalendarEvent/get.
type jsEvent struct {
	Type string `json:"@type,omitempty"`

	// JMAP CalendarEvent additions (draft-ietf-jmap-calendars).
	ID          string          `json:"id,omitempty"`
	CalendarIDs map[string]bool `json:"calendarIds,omitempty"`
	BaseEventID string          `json:"baseEventId,omitempty"`
	IsDraft     bool            `json:"isDraft,omitempty"`

	// JSCalendar Event.
	UID                  string                     `json:"uid,omitempty"`
	Title                string                     `json:"title,omitempty"`
	Description          string                     `json:"description,omitempty"`
	Start                string                     `json:"start,omitempty"`
	Duration             string                     `json:"duration,omitempty"`
	TimeZone             *string                    `json:"timeZone,omitempty"`
	ShowWithoutTime      bool                       `json:"showWithoutTime,omitempty"`
	Status               string                     `json:"status,omitempty"`
	Created              jTime                      `json:"created,omitempty"`
	Updated              jTime                      `json:"updated,omitempty"`
	Sequence             int                        `json:"sequence,omitempty"`
	Locations            map[string]jsLocation      `json:"locations,omitempty"`
	RecurrenceRules      []jsRecurrenceRule         `json:"recurrenceRules,omitempty"`
	RecurrenceOverrides  map[string]json.RawMessage `json:"recurrenceOverrides,omitempty"`
	RecurrenceID         string                     `json:"recurrenceId,omitempty"`
	RecurrenceIDTimeZone *string                    `json:"recurrenceIdTimeZone,omitempty"`
	Participants         map[string]jsParticipant   `json:"participants,omitempty"`
	ReplyTo              map[string]string          `json:"replyTo,omitempty"`
	FreeBusyStatus       string                     `json:"freeBusyStatus,omitempty"`
	Privacy              string                     `json:"privacy,omitempty"`
	Keywords             map[string]bool            `json:"keywords,omitempty"`
}

// ---------------------------------------------------------------------------
// Time zones, LocalDateTime, Duration

// loadLocation resolves a JSCalendar TimeZoneId. A null/absent zone means a
// floating time, which we anchor to UTC.
//
// This needs the system tzdata. A statically linked build should import
// _ "time/tzdata" from package main, or every zone will silently degrade to UTC.
func loadLocation(tz *string, log *slog.Logger) *time.Location {
	if tz == nil || *tz == "" {
		return time.UTC
	}
	loc, err := time.LoadLocation(*tz)
	if err != nil {
		if log != nil {
			log.Warn("jmap: unknown time zone, falling back to UTC", "timeZone", *tz, "err", err)
		}
		return time.UTC
	}
	return loc
}

func parseLocalDateTime(s string, loc *time.Location) (time.Time, error) {
	s = strings.TrimSpace(s)
	if s == "" {
		return time.Time{}, errors.New("empty LocalDateTime")
	}
	// Be lenient about servers that append a Z or an offset.
	for _, layout := range []string{localDateTimeLayout, "2006-01-02T15:04:05.999999999", time.RFC3339} {
		if t, err := time.ParseInLocation(layout, s, loc); err == nil {
			return t, nil
		}
	}
	if t, err := time.ParseInLocation("2006-01-02", s, loc); err == nil {
		return t, nil
	}
	return time.Time{}, fmt.Errorf("bad LocalDateTime %q", s)
}

func formatLocalDateTime(t time.Time, loc *time.Location) string {
	return t.In(loc).Format(localDateTimeLayout)
}

// isoDuration is a parsed JSCalendar Duration / SignedDuration.
type isoDuration struct {
	neg                        bool
	years, months, weeks, days int
	hours, minutes             int
	seconds                    float64
}

func parseISODuration(s string) (isoDuration, error) {
	var d isoDuration
	s = strings.TrimSpace(s)
	if s == "" {
		return d, errors.New("empty duration")
	}
	switch s[0] {
	case '-':
		d.neg, s = true, s[1:]
	case '+':
		s = s[1:]
	}
	if s == "" || (s[0] != 'P' && s[0] != 'p') {
		return d, fmt.Errorf("bad duration %q", s)
	}
	s = s[1:]
	inTime := false
	sawComponent := false
	num := ""
	for i := 0; i < len(s); i++ {
		ch := s[i]
		if ch == 'T' || ch == 't' {
			inTime = true
			continue
		}
		if ch >= '0' && ch <= '9' || ch == '.' {
			num += string(ch)
			continue
		}
		v, err := strconv.ParseFloat(num, 64)
		if err != nil {
			return d, fmt.Errorf("bad duration %q: no number before %q", s, string(ch))
		}
		num = ""
		sawComponent = true
		switch ch {
		case 'Y', 'y':
			d.years = int(v)
		case 'W', 'w':
			d.weeks = int(v)
		case 'D', 'd':
			d.days = int(v)
		case 'H', 'h':
			d.hours = int(v)
		case 'S', 's':
			d.seconds = v
		case 'M', 'm':
			if inTime {
				d.minutes = int(v)
			} else {
				d.months = int(v)
			}
		default:
			return d, fmt.Errorf("bad duration %q: unexpected %q", s, string(ch))
		}
	}
	if num != "" {
		return d, fmt.Errorf("bad duration %q: trailing number", s)
	}
	if !sawComponent {
		return d, fmt.Errorf("bad duration %q: no components", s)
	}
	return d, nil
}

// addTo applies the duration to t. Years, months and days go through AddDate so
// that DST transitions and month lengths are honoured.
func (d isoDuration) addTo(t time.Time) time.Time {
	sign := 1
	if d.neg {
		sign = -1
	}
	t = t.AddDate(sign*d.years, sign*d.months, sign*(d.days+7*d.weeks))
	clock := time.Duration(d.hours)*time.Hour +
		time.Duration(d.minutes)*time.Minute +
		time.Duration(d.seconds*float64(time.Second))
	return t.Add(time.Duration(sign) * clock)
}

// formatISODuration renders a wall-clock span. allDay spans are rendered in
// whole days, which is what JSCalendar expects alongside showWithoutTime.
func formatISODuration(d time.Duration, allDay bool) string {
	if d <= 0 {
		return "PT0S"
	}
	if allDay {
		days := int((d + 23*time.Hour) / (24 * time.Hour))
		if days < 1 {
			days = 1
		}
		return fmt.Sprintf("P%dD", days)
	}
	var b strings.Builder
	b.WriteString("P")
	if days := int(d / (24 * time.Hour)); days > 0 {
		fmt.Fprintf(&b, "%dD", days)
		d -= time.Duration(days) * 24 * time.Hour
	}
	if d > 0 {
		b.WriteString("T")
		if h := int(d / time.Hour); h > 0 {
			fmt.Fprintf(&b, "%dH", h)
			d -= time.Duration(h) * time.Hour
		}
		if m := int(d / time.Minute); m > 0 {
			fmt.Fprintf(&b, "%dM", m)
			d -= time.Duration(m) * time.Minute
		}
		if s := int(d / time.Second); s > 0 {
			fmt.Fprintf(&b, "%dS", s)
		}
	}
	if b.String() == "P" {
		return "PT0S"
	}
	return b.String()
}

// ---------------------------------------------------------------------------
// Recurrence: JSCalendar RecurrenceRule <-> RFC 5545 RRULE

var weekdays = []string{"mo", "tu", "we", "th", "fr", "sa", "su"}

// recurrenceToRRule renders a JSCalendar RecurrenceRule as an RFC 5545 RRULE
// value (without the "RRULE:" prefix).
//
// loc is the event's time zone and allDay its showWithoutTime, both needed to
// render UNTIL: RFC 5545 requires UNTIL to be a UTC date-time when DTSTART is
// zoned, and a plain DATE when DTSTART is a date.
func recurrenceToRRule(r jsRecurrenceRule, loc *time.Location, allDay bool) string {
	if r.Frequency == "" {
		return ""
	}
	parts := []string{"FREQ=" + strings.ToUpper(r.Frequency)}
	if r.Interval > 1 {
		parts = append(parts, "INTERVAL="+strconv.Itoa(r.Interval))
	}
	if r.Count > 0 {
		parts = append(parts, "COUNT="+strconv.Itoa(r.Count))
	}
	if r.Until != "" {
		if t, err := parseLocalDateTime(r.Until, loc); err == nil {
			if allDay {
				parts = append(parts, "UNTIL="+t.Format("20060102"))
			} else {
				parts = append(parts, "UNTIL="+t.UTC().Format("20060102T150405Z"))
			}
		}
	}
	if len(r.ByMonth) > 0 {
		months := make([]string, len(r.ByMonth))
		for i, m := range r.ByMonth {
			months[i] = strings.ToUpper(m) // may be "5L" under a leap-month rscale
		}
		parts = append(parts, "BYMONTH="+strings.Join(months, ","))
	}
	if len(r.ByWeekNo) > 0 {
		parts = append(parts, "BYWEEKNO="+joinInts(r.ByWeekNo))
	}
	if len(r.ByYearDay) > 0 {
		parts = append(parts, "BYYEARDAY="+joinInts(r.ByYearDay))
	}
	if len(r.ByMonthDay) > 0 {
		parts = append(parts, "BYMONTHDAY="+joinInts(r.ByMonthDay))
	}
	if len(r.ByDay) > 0 {
		days := make([]string, 0, len(r.ByDay))
		for _, nd := range r.ByDay {
			s := strings.ToUpper(nd.Day)
			if nd.NthOfPeriod != nil && *nd.NthOfPeriod != 0 {
				s = strconv.Itoa(*nd.NthOfPeriod) + s
			}
			days = append(days, s)
		}
		parts = append(parts, "BYDAY="+strings.Join(days, ","))
	}
	if len(r.ByHour) > 0 {
		parts = append(parts, "BYHOUR="+joinInts(r.ByHour))
	}
	if len(r.ByMinute) > 0 {
		parts = append(parts, "BYMINUTE="+joinInts(r.ByMinute))
	}
	if len(r.BySecond) > 0 {
		parts = append(parts, "BYSECOND="+joinInts(r.BySecond))
	}
	if len(r.BySetPosition) > 0 {
		parts = append(parts, "BYSETPOS="+joinInts(r.BySetPosition))
	}
	if fd := strings.ToLower(r.FirstDayOfWeek); fd != "" && fd != "mo" && slices.Contains(weekdays, fd) {
		parts = append(parts, "WKST="+strings.ToUpper(fd))
	}
	return strings.Join(parts, ";")
}

// rruleToRecurrence parses an RFC 5545 RRULE value back into a JSCalendar
// RecurrenceRule, for events we create or update.
func rruleToRecurrence(rrule string, loc *time.Location) (jsRecurrenceRule, error) {
	r := jsRecurrenceRule{Type: "RecurrenceRule"}
	s := strings.TrimSpace(rrule)
	s = strings.TrimPrefix(s, "RRULE:")
	if s == "" {
		return r, errors.New("empty RRULE")
	}
	for _, kv := range strings.Split(s, ";") {
		if kv == "" {
			continue
		}
		k, v, ok := strings.Cut(kv, "=")
		if !ok {
			return r, fmt.Errorf("bad RRULE part %q", kv)
		}
		k, v = strings.ToUpper(strings.TrimSpace(k)), strings.TrimSpace(v)
		switch k {
		case "FREQ":
			r.Frequency = strings.ToLower(v)
		case "INTERVAL":
			n, err := strconv.Atoi(v)
			if err != nil {
				return r, fmt.Errorf("bad INTERVAL %q", v)
			}
			r.Interval = n
		case "COUNT":
			n, err := strconv.Atoi(v)
			if err != nil {
				return r, fmt.Errorf("bad COUNT %q", v)
			}
			r.Count = n
		case "UNTIL":
			t, err := parseICalDateTime(v, loc)
			if err != nil {
				return r, err
			}
			// JSCalendar's until is a LocalDateTime in the event's time zone.
			r.Until = formatLocalDateTime(t, loc)
		case "BYDAY":
			for _, d := range strings.Split(v, ",") {
				nd, err := parseByDay(d)
				if err != nil {
					return r, err
				}
				r.ByDay = append(r.ByDay, nd)
			}
		case "BYMONTHDAY":
			ns, err := splitInts(v)
			if err != nil {
				return r, err
			}
			r.ByMonthDay = ns
		case "BYMONTH":
			for _, m := range strings.Split(v, ",") {
				r.ByMonth = append(r.ByMonth, strings.ToUpper(strings.TrimSpace(m)))
			}
		case "BYYEARDAY":
			ns, err := splitInts(v)
			if err != nil {
				return r, err
			}
			r.ByYearDay = ns
		case "BYWEEKNO":
			ns, err := splitInts(v)
			if err != nil {
				return r, err
			}
			r.ByWeekNo = ns
		case "BYHOUR":
			ns, err := splitInts(v)
			if err != nil {
				return r, err
			}
			r.ByHour = ns
		case "BYMINUTE":
			ns, err := splitInts(v)
			if err != nil {
				return r, err
			}
			r.ByMinute = ns
		case "BYSECOND":
			ns, err := splitInts(v)
			if err != nil {
				return r, err
			}
			r.BySecond = ns
		case "BYSETPOS":
			ns, err := splitInts(v)
			if err != nil {
				return r, err
			}
			r.BySetPosition = ns
		case "WKST":
			r.FirstDayOfWeek = strings.ToLower(v)
		default:
			// X- extensions and anything newer than we know: ignore rather
			// than reject a rule we could otherwise round-trip.
		}
	}
	if r.Frequency == "" {
		return r, errors.New("RRULE has no FREQ")
	}
	return r, nil
}

func parseByDay(s string) (jsNDay, error) {
	s = strings.ToUpper(strings.TrimSpace(s))
	if len(s) < 2 {
		return jsNDay{}, fmt.Errorf("bad BYDAY %q", s)
	}
	day := strings.ToLower(s[len(s)-2:])
	if !slices.Contains(weekdays, day) {
		return jsNDay{}, fmt.Errorf("bad BYDAY weekday in %q", s)
	}
	nd := jsNDay{Type: "NDay", Day: day}
	if prefix := s[:len(s)-2]; prefix != "" {
		n, err := strconv.Atoi(prefix)
		if err != nil {
			return jsNDay{}, fmt.Errorf("bad BYDAY ordinal in %q", s)
		}
		nd.NthOfPeriod = &n
	}
	return nd, nil
}

// parseICalDateTime parses an RFC 5545 DATE or DATE-TIME value.
func parseICalDateTime(v string, loc *time.Location) (time.Time, error) {
	v = strings.TrimSpace(v)
	switch {
	case strings.HasSuffix(v, "Z"):
		t, err := time.Parse("20060102T150405Z", v)
		if err != nil {
			return time.Time{}, fmt.Errorf("bad UTC date-time %q", v)
		}
		return t, nil
	case len(v) == 8:
		return time.ParseInLocation("20060102", v, loc)
	default:
		t, err := time.ParseInLocation("20060102T150405", v, loc)
		if err != nil {
			return time.Time{}, fmt.Errorf("bad date-time %q", v)
		}
		return t, nil
	}
}

func joinInts(ns []int) string {
	parts := make([]string, len(ns))
	for i, n := range ns {
		parts[i] = strconv.Itoa(n)
	}
	return strings.Join(parts, ",")
}

func splitInts(v string) ([]int, error) {
	var out []int
	for _, p := range strings.Split(v, ",") {
		p = strings.TrimSpace(p)
		if p == "" {
			continue
		}
		n, err := strconv.Atoi(p)
		if err != nil {
			return nil, fmt.Errorf("bad integer list %q", v)
		}
		out = append(out, n)
	}
	return out, nil
}

// ---------------------------------------------------------------------------
// JSCalendar <-> model.Event

func statusFromJS(s string) model.EventStatus {
	switch strings.ToLower(s) {
	case "cancelled":
		return model.StatusCancelled
	case "tentative":
		return model.StatusTentative
	case "confirmed", "":
		return model.StatusConfirmed
	default:
		return model.EventStatus(strings.ToLower(s))
	}
}

func participationFromJS(s string) model.Participation {
	switch strings.ToLower(s) {
	case "accepted":
		return model.PartAccepted
	case "declined":
		return model.PartDeclined
	case "tentative":
		return model.PartTentative
	case "needs-action", "":
		return model.PartNeedsAction
	case "delegated":
		// The model has no delegated state; the invitation is still open as
		// far as this user is concerned.
		return model.PartNeedsAction
	default:
		return model.Participation(strings.ToLower(s))
	}
}

func participationToJS(p model.Participation) string {
	switch p {
	case model.PartAccepted:
		return "accepted"
	case model.PartDeclined:
		return "declined"
	case model.PartTentative:
		return "tentative"
	default:
		return "needs-action"
	}
}

// toModelEvent maps a JSCalendar event onto the provider-neutral model.
// raw is stored verbatim on the result so writes can patch minimally.
func toModelEvent(js *jsEvent, raw json.RawMessage, calendarRemote, selfEmail string, log *slog.Logger) model.Event {
	loc := loadLocation(js.TimeZone, log)
	ev := model.Event{
		CalendarRemote: calendarRemote,
		RemoteID:       js.ID,
		UID:            js.UID,
		Title:          js.Title,
		Description:    js.Description,
		AllDay:         js.ShowWithoutTime,
		Status:         statusFromJS(js.Status),
		RecurrenceID:   js.RecurrenceID,
		Updated:        js.Updated.Time,
		RawJSON:        append(json.RawMessage(nil), raw...),
	}
	if js.TimeZone != nil {
		ev.Timezone = *js.TimeZone
	}

	if start, err := parseLocalDateTime(js.Start, loc); err == nil {
		ev.Start = start
		ev.End = start
		if js.Duration != "" {
			if d, err := parseISODuration(js.Duration); err == nil {
				ev.End = d.addTo(start)
			} else if log != nil {
				log.Warn("jmap: unparseable event duration", "event", js.ID, "duration", js.Duration)
			}
		}
	} else if log != nil && js.Start != "" {
		log.Warn("jmap: unparseable event start", "event", js.ID, "start", js.Start)
	}

	// Location: JSCalendar allows several; the model has one. Take the first
	// named one in key order so the choice is stable across syncs.
	for _, k := range sortedKeys(js.Locations) {
		if n := js.Locations[k].Name; n != "" {
			ev.Location = n
			break
		}
	}

	if len(js.RecurrenceRules) > 0 {
		ev.RRule = recurrenceToRRule(js.RecurrenceRules[0], loc, js.ShowWithoutTime)
		if len(js.RecurrenceRules) > 1 && log != nil {
			log.Debug("jmap: event has multiple recurrence rules, keeping the first",
				"event", js.ID, "rules", len(js.RecurrenceRules))
		}
	}

	self := strings.ToLower(selfEmail)
	ev.MyResponse = model.PartNeedsAction
	for _, k := range sortedKeys(js.Participants) {
		p := js.Participants[k]
		addr := p.address()
		isSelf := self != "" && strings.EqualFold(addr, self)
		if p.Roles["owner"] {
			ev.Organizer = model.Address{Name: p.Name, Email: addr}
		}
		// Anything that is not purely the organiser is listed as an attendee;
		// Fastmail marks the organiser with both "owner" and "attendee" when
		// they are also going.
		if p.Roles["attendee"] || p.Roles["optional"] || p.Roles["chair"] || len(p.Roles) == 0 {
			ev.Attendees = append(ev.Attendees, model.Attendee{
				Name:     p.Name,
				Email:    addr,
				Response: participationFromJS(p.ParticipationStatus),
				Optional: p.Roles["optional"],
				Self:     isSelf,
			})
		}
		if isSelf {
			ev.MyResponse = participationFromJS(p.ParticipationStatus)
		}
	}
	if ev.Organizer.Email == "" {
		if v := js.ReplyTo["imip"]; v != "" {
			ev.Organizer = model.Address{Email: strings.TrimPrefix(v, "mailto:")}
		}
	}
	return ev
}

// fromModelEvent builds the JSCalendar object for a create or a full update.
// base, when non-nil, is the current server object: everything we do not model
// (alerts, custom properties, per-occurrence overrides) is carried over from it.
func fromModelEvent(ev *model.Event, calendarRemote string, base *jsEvent, log *slog.Logger) (*jsEvent, error) {
	js := &jsEvent{Type: "Event"}
	if base != nil {
		*js = *base
		js.Type = "Event"
	}
	if calendarRemote != "" {
		js.CalendarIDs = map[string]bool{calendarRemote: true}
	}
	if ev.UID != "" {
		js.UID = ev.UID
	}
	js.Title = ev.Title
	js.Description = ev.Description

	loc := time.UTC
	if ev.Timezone != "" {
		tz := ev.Timezone
		loc = loadLocation(&tz, log)
		js.TimeZone = &tz
	} else if base != nil {
		loc = loadLocation(base.TimeZone, log)
	}
	if ev.AllDay {
		// RFC 8984: an all-day event has showWithoutTime true and no zone.
		js.ShowWithoutTime = true
		js.TimeZone = nil
		loc = time.UTC
	} else {
		js.ShowWithoutTime = false
	}

	if ev.Start.IsZero() {
		return nil, errors.New("jmap: event has no start time")
	}
	js.Start = formatLocalDateTime(ev.Start, loc)
	if !ev.End.IsZero() && ev.End.After(ev.Start) {
		js.Duration = formatISODuration(ev.End.Sub(ev.Start), ev.AllDay)
	} else if ev.AllDay {
		js.Duration = "P1D"
	} else {
		js.Duration = "PT0S"
	}

	// Keep the server's own location object when it already says what the
	// model says, so an unrelated edit does not rewrite (and flatten) it.
	switch {
	case ev.Location == "":
		js.Locations = nil
	case base != nil && firstLocationName(base.Locations) == ev.Location:
		js.Locations = base.Locations
	default:
		js.Locations = map[string]jsLocation{"1": {Type: "Location", Name: ev.Location}}
	}

	switch ev.Status {
	case "":
		js.Status = "confirmed"
	default:
		js.Status = string(ev.Status)
	}

	switch {
	case ev.RRule == "":
		js.RecurrenceRules = nil
	case base != nil && len(base.RecurrenceRules) > 0 &&
		recurrenceToRRule(base.RecurrenceRules[0], loc, ev.AllDay) == ev.RRule:
		// Unchanged: keep the server's rule verbatim rather than a lossy
		// re-encoding of our RRULE string.
		js.RecurrenceRules = base.RecurrenceRules
	default:
		r, err := rruleToRecurrence(ev.RRule, loc)
		if err != nil {
			return nil, fmt.Errorf("jmap: converting RRULE: %w", err)
		}
		js.RecurrenceRules = []jsRecurrenceRule{r}
	}

	if participantsUnchanged(base, ev) {
		// Leave the server's participant objects (and their ids, which iTIP
		// replies are keyed by) exactly as they are.
		js.Participants = base.Participants
		js.ReplyTo = base.ReplyTo
	} else if len(ev.Attendees) > 0 || ev.Organizer.Email != "" {
		js.Participants = map[string]jsParticipant{}
		if ev.Organizer.Email != "" {
			js.Participants[participantKey(ev.Organizer.Email)] = jsParticipant{
				Type:   "Participant",
				Name:   ev.Organizer.Name,
				Email:  ev.Organizer.Email,
				SendTo: map[string]string{"imip": "mailto:" + ev.Organizer.Email},
				Kind:   "individual",
				Roles:  map[string]bool{"owner": true},
			}
			js.ReplyTo = map[string]string{"imip": "mailto:" + ev.Organizer.Email}
		}
		for _, a := range ev.Attendees {
			if a.Email == "" {
				continue
			}
			key := participantKey(a.Email)
			roles := map[string]bool{"attendee": true}
			if a.Optional {
				roles["optional"] = true
			}
			if p, ok := js.Participants[key]; ok {
				// The organiser is also attending: merge the roles.
				for r := range roles {
					p.Roles[r] = true
				}
				p.ParticipationStatus = participationToJS(a.Response)
				js.Participants[key] = p
				continue
			}
			js.Participants[key] = jsParticipant{
				Type:                "Participant",
				Name:                a.Name,
				Email:               a.Email,
				SendTo:              map[string]string{"imip": "mailto:" + a.Email},
				Kind:                "individual",
				Roles:               roles,
				ParticipationStatus: participationToJS(a.Response),
				ExpectReply:         true,
			}
		}
	}
	return js, nil
}

// firstLocationName returns the name of the first named location, in key order.
func firstLocationName(locs map[string]jsLocation) string {
	for _, k := range sortedKeys(locs) {
		if n := locs[k].Name; n != "" {
			return n
		}
	}
	return ""
}

// participantsUnchanged reports whether the model's organiser and attendee set
// already matches what the server holds, so an update can leave participants
// alone.
func participantsUnchanged(base *jsEvent, ev *model.Event) bool {
	if base == nil {
		return false
	}
	type attendeeKey struct {
		email    string
		response model.Participation
		optional bool
	}
	want := map[attendeeKey]bool{}
	for _, a := range ev.Attendees {
		if a.Email == "" {
			continue
		}
		want[attendeeKey{strings.ToLower(a.Email), a.Response, a.Optional}] = true
	}
	got := map[attendeeKey]bool{}
	organizer := ""
	for _, k := range sortedKeys(base.Participants) {
		p := base.Participants[k]
		addr := strings.ToLower(p.address())
		if p.Roles["owner"] {
			organizer = addr
		}
		if p.Roles["attendee"] || p.Roles["optional"] || p.Roles["chair"] || len(p.Roles) == 0 {
			got[attendeeKey{addr, participationFromJS(p.ParticipationStatus), p.Roles["optional"]}] = true
		}
	}
	if organizer != strings.ToLower(ev.Organizer.Email) {
		return false
	}
	return maps.Equal(want, got)
}

// participantKey derives a stable participant id from an address. JSCalendar
// wants opaque ids; using the lowercased address keeps updates idempotent.
func participantKey(email string) string {
	return strings.ToLower(strings.TrimSpace(email))
}

func sortedKeys[V any](m map[string]V) []string {
	if len(m) == 0 {
		return nil
	}
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	slices.Sort(out)
	return out
}
