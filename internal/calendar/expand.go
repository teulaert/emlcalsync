// Package calendar turns stored events into concrete occurrences: recurrence
// expansion, exception handling, free/busy computation and the time parsing
// the `cal` commands need for their flags.
//
// It is provider-neutral — it works on model.Event and knows nothing about
// Google Calendar or JMAP.
package calendar

import (
	"fmt"
	"sort"
	"strings"
	"time"

	"github.com/lennert/emlcal/internal/model"
	"github.com/teambition/rrule-go"
)

// MaxOccurrences caps a single expansion. A malformed or unbounded rule over a
// three-year window cannot make a command hang or eat memory; the excess is
// dropped silently, which is what a display window wants.
const MaxOccurrences = 5000

// DefaultWindow is the span occurrences are materialised for: one year back,
// two years forward (DESIGN.md §6.3).
func DefaultWindow(now time.Time) (from, to time.Time) {
	return now.AddDate(-1, 0, 0), now.AddDate(2, 0, 0)
}

// Location resolves an event's timezone, falling back to the zone its Start
// already carries (which is UTC for anything read straight from SQLite).
func Location(ev *model.Event) *time.Location {
	if ev.Timezone != "" {
		if loc, err := time.LoadLocation(ev.Timezone); err == nil {
			return loc
		}
	}
	if ev.Start.Location() != nil {
		return ev.Start.Location()
	}
	return time.UTC
}

// Expand returns the occurrences of ev that overlap [from, to).
//
// A single event yields at most one occurrence. A recurring event is expanded
// with its RRULE, anchored at Start in the event's timezone, preserving the
// event's duration; wall-clock times survive DST transitions because the
// expansion happens in that zone. All-day events are anchored at local
// midnight. COUNT and UNTIL are honoured by the rule itself.
//
// Exceptions (EXDATE-style overrides stored as separate events) are not
// applied here — pass the result through ApplyExceptions.
func Expand(ev *model.Event, from, to time.Time) ([]model.Occurrence, error) {
	if ev == nil {
		return nil, nil
	}
	loc := Location(ev)
	dur := ev.End.Sub(ev.Start)
	if dur < 0 {
		dur = 0
	}
	start := anchor(ev, loc)

	if strings.TrimSpace(ev.RRule) == "" {
		occ := model.Occurrence{EventID: ev.ID, Start: start, End: start.Add(dur)}
		if !overlaps(occ, from, to) {
			return nil, nil
		}
		return []model.Occurrence{occ}, nil
	}

	r, err := buildRule(ev.RRule, start, loc)
	if err != nil {
		return nil, fmt.Errorf("event %s: %w", ev.UID, err)
	}

	var out []model.Occurrence
	next := r.Iterator()
	for {
		t, ok := next()
		if !ok {
			break
		}
		// The iterator is chronological, so once a start is at or past the
		// window end nothing later can overlap.
		if !t.Before(to) {
			break
		}
		occ := model.Occurrence{EventID: ev.ID, Start: t.In(loc), End: t.In(loc).Add(dur)}
		if !overlaps(occ, from, to) {
			continue // still before the window
		}
		out = append(out, occ)
		if len(out) >= MaxOccurrences {
			break
		}
	}
	return out, nil
}

// anchor is DTSTART: the event start in its own zone, or local midnight of the
// start date for an all-day event.
func anchor(ev *model.Event, loc *time.Location) time.Time {
	if ev.AllDay {
		y, m, d := ev.Start.Date()
		return time.Date(y, m, d, 0, 0, 0, 0, loc)
	}
	return ev.Start.In(loc)
}

// buildRule parses an RRULE (with or without the "RRULE:" prefix) and anchors
// it at dtstart. UNTIL values without a zone are read in loc.
func buildRule(rule string, dtstart time.Time, loc *time.Location) (*rrule.RRule, error) {
	rule = strings.TrimSpace(rule)
	// Providers occasionally hand back a full RECUR property, sometimes with
	// EXDATE/RDATE lines appended; only the RRULE line is ours to expand.
	for _, line := range strings.Split(rule, "\n") {
		line = strings.TrimSpace(line)
		if line == "" {
			continue
		}
		upper := strings.ToUpper(line)
		if strings.HasPrefix(upper, "RRULE:") || !strings.Contains(line, ":") {
			rule = line
			break
		}
	}
	opt, err := rrule.StrToROptionInLocation(rule, loc)
	if err != nil {
		return nil, fmt.Errorf("bad RRULE %q: %w", rule, err)
	}
	opt.Dtstart = dtstart
	r, err := rrule.NewRRule(*opt)
	if err != nil {
		return nil, fmt.Errorf("bad RRULE %q: %w", rule, err)
	}
	return r, nil
}

// overlaps reports whether an occurrence intersects [from, to). Zero-length
// occurrences count when their instant falls inside the window.
func overlaps(o model.Occurrence, from, to time.Time) bool {
	if !o.Start.Before(to) {
		return false
	}
	if o.End.Equal(o.Start) {
		return !o.Start.Before(from)
	}
	return o.End.After(from)
}

// ---------------------------------------------------------------------------
// Exceptions

// ApplyExceptions rewrites an expanded series with its exception instances.
//
// Each exception is an event carrying a RecurrenceID identifying the instance
// it replaces. The matching occurrence is removed; unless the exception is
// cancelled, its own start/end are appended in its place (which is how a moved
// meeting shows up at its new time). Occurrences whose instance was deleted
// simply disappear.
//
// The result is sorted by start time.
func ApplyExceptions(occ []model.Occurrence, exceptions []model.Event) []model.Occurrence {
	if len(exceptions) == 0 {
		return occ
	}

	// Index the recurrence ids two ways: as an instant, and as a wall-clock
	// stamp, because JSCalendar writes local date-times without a zone.
	byInstant := make(map[int64]bool, len(exceptions))
	byWall := make(map[string]bool, len(exceptions))
	for i := range exceptions {
		ex := &exceptions[i]
		rid, wall, ok := parseRecurrenceID(ex.RecurrenceID, Location(ex))
		if !ok {
			continue
		}
		if !rid.IsZero() {
			byInstant[rid.Unix()] = true
		}
		byWall[wall] = true
	}

	out := make([]model.Occurrence, 0, len(occ)+len(exceptions))
	for _, o := range occ {
		if byInstant[o.Start.Unix()] || byWall[o.Start.Format(localStamp)] {
			continue
		}
		out = append(out, o)
	}

	for i := range exceptions {
		ex := &exceptions[i]
		if ex.Status == model.StatusCancelled || ex.DeletedAt != nil {
			continue
		}
		if _, _, ok := parseRecurrenceID(ex.RecurrenceID, Location(ex)); !ok {
			continue
		}
		loc := Location(ex)
		start := anchor(ex, loc)
		dur := ex.End.Sub(ex.Start)
		if dur < 0 {
			dur = 0
		}
		id := ex.ID
		if id == 0 {
			// Exceptions read straight from a provider may not have a local
			// row id yet; keep whatever the series carried.
			if len(occ) > 0 {
				id = occ[0].EventID
			}
		}
		out = append(out, model.Occurrence{EventID: id, Start: start, End: start.Add(dur)})
	}

	sort.SliceStable(out, func(i, j int) bool { return out[i].Start.Before(out[j].Start) })
	return out
}

// localStamp is the JSCalendar LocalDateTime layout (RFC 8984 §1.4.4).
const localStamp = "2006-01-02T15:04:05"

// parseRecurrenceID accepts RFC 3339 ("2026-08-25T09:00:00+02:00"), the
// JSCalendar local form ("2026-08-25T09:00:00") and a bare date
// ("2026-08-25", used by all-day series). It returns the instant when the
// value carried a zone, plus the wall-clock stamp used for zone-less matching.
func parseRecurrenceID(s string, loc *time.Location) (instant time.Time, wall string, ok bool) {
	s = strings.TrimSpace(s)
	if s == "" {
		return time.Time{}, "", false
	}
	if loc == nil {
		loc = time.UTC
	}
	if t, err := time.Parse(time.RFC3339, s); err == nil {
		return t, t.In(loc).Format(localStamp), true
	}
	// iCalendar's compact UTC form carries its zone in the trailing Z, which
	// is a literal rather than a zone token in a Go layout, so it is parsed
	// against UTC explicitly.
	if t, err := time.Parse("20060102T150405Z", s); err == nil {
		return t, t.In(loc).Format(localStamp), true
	}
	for _, layout := range []string{localStamp, "2006-01-02T15:04", "20060102T150405", "2006-01-02", "20060102"} {
		if t, err := time.ParseInLocation(layout, s, loc); err == nil {
			return t, t.Format(localStamp), true
		}
	}
	return time.Time{}, "", false
}
