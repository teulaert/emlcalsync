package gcal

import (
	"encoding/json"
	"log/slog"
	"strings"
	"time"

	calendarapi "google.golang.org/api/calendar/v3"

	"github.com/teulaert/emlcalsync/internal/model"
)

// Google keeps a deleted instance of a recurring event in one of two ways:
// as a cancelled instance object in the listing, or -- for a series whose
// organiser lives elsewhere, among other cases -- as an EXDATE line on the
// master's recurrence, with no instance object at all. The flat event model
// carries one RRULE and nothing else of the recurrence, and calendar.Expand
// ignores EXDATE, so the second form used to be invisible: the meeting kept
// showing up on the days it had been deleted.
//
// excludedInstances turns each EXDATE instant into the cancelled instance
// Google would otherwise have sent, under the id Google gives that instance.
// That is how the CalDAV and JMAP providers already hand their exclusions
// over, and it is what calendar.ApplyExceptions drops an occurrence for.
// Sharing Google's id means a cancelled instance that does also arrive
// lands on the same row rather than beside it.
func excludedInstances(calendarRemote string, in *calendarapi.Event, master *model.Event, log *slog.Logger) []model.Event {
	if in.RecurringEventId != "" {
		return nil // an instance has no recurrence of its own
	}
	masterLoc := loadLocation(master.Timezone)
	if masterLoc == nil {
		masterLoc = master.Start.Location()
	}
	dur := master.End.Sub(master.Start)
	if dur < 0 {
		dur = 0
	}

	seen := map[string]bool{}
	var out []model.Event
	for _, line := range in.Recurrence {
		for _, ex := range parseExdateLine(line, masterLoc, master.Timezone) {
			if ex.err != nil {
				if log != nil {
					log.Warn("gcal: unreadable EXDATE skipped", "event", in.Id, "value", ex.value, "err", ex.err)
				}
				continue
			}
			rid := ex.at.Format(time.RFC3339)
			if seen[rid] {
				continue
			}
			seen[rid] = true

			id := instanceID(in.Id, ex.at, ex.allDay)
			end := ex.at.Add(dur)
			if ex.allDay {
				// Google's all-day end is the following day; a day series
				// keeps whole days whatever its master's duration says.
				end = ex.at.AddDate(0, 0, 1)
			}
			raw, _ := json.Marshal(&calendarapi.Event{
				Id:                id,
				ICalUID:           in.ICalUID,
				Status:            statusCancelled,
				RecurringEventId:  in.Id,
				OriginalStartTime: toAPITime(ex.at, ex.allDay, ex.tz),
				Updated:           in.Updated,
			})
			out = append(out, model.Event{
				CalendarRemote: calendarRemote,
				RemoteID:       id,
				UID:            master.UID,
				Title:          master.Title,
				Description:    master.Description,
				Location:       master.Location,
				Start:          ex.at,
				End:            end,
				AllDay:         ex.allDay,
				Timezone:       ex.tz,
				RecurrenceID:   rid,
				Status:         model.StatusCancelled,
				Organizer:      master.Organizer,
				Updated:        master.Updated,
				RawJSON:        raw,
			})
		}
	}
	return out
}

// instanceID is the id Google gives one instance of a recurring event: the
// master's id and the instance's original start, as a UTC stamp for a timed
// series and a bare date for an all-day one.
func instanceID(masterID string, at time.Time, allDay bool) string {
	if allDay {
		return masterID + "_" + at.Format("20060102")
	}
	return masterID + "_" + at.UTC().Format("20060102T150405Z")
}

// exdate is one value of an EXDATE line, resolved or not.
type exdate struct {
	value  string
	at     time.Time
	allDay bool
	tz     string
	err    error
}

// parseExdateLine reads one recurrence line, returning nothing for a line
// that is not an EXDATE. RFC 5545 §3.8.5.1: a parameter list after the name,
// then a colon, then one or more comma-separated values. A value is a date
// (VALUE=DATE, or eight digits), a UTC stamp (trailing Z), or a local stamp
// in the TZID's zone -- or, floating, in the series' own zone.
func parseExdateLine(line string, seriesLoc *time.Location, seriesTZ string) []exdate {
	head, values, ok := strings.Cut(line, ":")
	if !ok {
		return nil
	}
	params := strings.Split(head, ";")
	if !strings.EqualFold(strings.TrimSpace(params[0]), "EXDATE") {
		return nil
	}
	loc, tz := seriesLoc, seriesTZ
	dateOnly := false
	var locErr error
	for _, p := range params[1:] {
		k, v, _ := strings.Cut(p, "=")
		switch strings.ToUpper(strings.TrimSpace(k)) {
		case "TZID":
			v = strings.Trim(strings.TrimSpace(v), `"`)
			if l := loadLocation(v); l != nil {
				loc, tz = l, v
			} else {
				locErr = &time.ParseError{Layout: "TZID", Value: v, Message: ": unknown time zone"}
			}
		case "VALUE":
			dateOnly = strings.EqualFold(strings.TrimSpace(v), "DATE")
		}
	}
	if loc == nil {
		loc = time.UTC
	}

	var out []exdate
	for _, v := range strings.Split(values, ",") {
		v = strings.TrimSpace(v)
		if v == "" {
			continue
		}
		ex := exdate{value: v, tz: tz}
		switch {
		case locErr != nil:
			ex.err = locErr
		case dateOnly || len(v) == 8:
			ex.at, ex.err = time.ParseInLocation("20060102", v, loc)
			ex.allDay = true
		case strings.HasSuffix(v, "Z"):
			ex.at, ex.err = time.Parse("20060102T150405Z", v)
			ex.at = ex.at.In(loc)
		default:
			ex.at, ex.err = time.ParseInLocation("20060102T150405", v, loc)
		}
		out = append(out, ex)
	}
	return out
}

// recurrenceLines is what a write sends as the event's recurrence: the rule
// in hand, plus every line the server already holds that is not a rule --
// its EXDATEs above all. Google replaces the array whole, so an RRULE sent
// on its own would restore every instance deleted from the series. An
// exclusion the new rule never generates is harmless; one dropped is a
// meeting back on the calendar. No rule means no recurrence at all, and the
// caller sends the empty array that says so.
func recurrenceLines(ev *model.Event) []string {
	if ev.RRule == "" {
		return nil
	}
	lines := []string{"RRULE:" + ev.RRule}
	if len(ev.RawJSON) == 0 {
		return lines
	}
	var held calendarapi.Event
	if err := json.Unmarshal(ev.RawJSON, &held); err != nil {
		return lines
	}
	for _, line := range held.Recurrence {
		if strings.HasPrefix(strings.ToUpper(strings.TrimSpace(line)), "RRULE") {
			continue
		}
		lines = append(lines, line)
	}
	return lines
}
