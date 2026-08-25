package calendar

import (
	"fmt"
	"strconv"
	"strings"
	"time"
)

// ParseWhen parses the time expressions the `cal` flags accept, relative to
// now and interpreted in loc:
//
//	RFC 3339               2026-08-25T14:00:00+02:00
//	date                   2026-08-25            (midnight)
//	date + time            2026-08-25 14:00  ·  2026-08-25T14:00
//	time only              14:00                 (today)
//	keyword                now · today · tomorrow · yesterday
//	keyword + time         tomorrow 09:30
//	relative               +2h · -30m · +3d
//	weekday                next monday · last friday · monday [09:00]
//
// Parsing is case-insensitive.
func ParseWhen(s string, now time.Time, loc *time.Location) (time.Time, error) {
	if loc == nil {
		loc = time.Local
	}
	orig := strings.TrimSpace(s)
	if orig == "" {
		return time.Time{}, fmt.Errorf("empty time value")
	}
	now = now.In(loc)

	// Absolute forms first: they are unambiguous and the most common in
	// scripts.
	if t, ok := parseAbsolute(orig, loc); ok {
		return t, nil
	}

	str := strings.ToLower(strings.Join(strings.Fields(orig), " "))

	// Signed offsets: "+2h", "-30m", "+3d", "+1w".
	if str[0] == '+' || str[0] == '-' {
		d, err := parseSpan(str[1:])
		if err != nil {
			return time.Time{}, fmt.Errorf("time %q: %w", orig, err)
		}
		if str[0] == '-' {
			d = -d
		}
		return now.Add(d), nil
	}

	// Everything below may carry a trailing clock time: "tomorrow 09:30".
	head, clockPart := splitTrailingClock(str)
	at := func(day time.Time) (time.Time, error) {
		if clockPart == "" {
			return atClock(day, 0, loc), nil
		}
		c, err := parseClock(clockPart)
		if err != nil {
			return time.Time{}, fmt.Errorf("time %q: %w", orig, err)
		}
		return atClock(day, c, loc), nil
	}

	switch head {
	case "now":
		if clockPart == "" {
			return now, nil
		}
	case "today":
		return at(now)
	case "tomorrow":
		return at(now.AddDate(0, 0, 1))
	case "yesterday":
		return at(now.AddDate(0, 0, -1))
	}

	// Bare clock: "14:00" means today.
	if head == "" && clockPart != "" {
		return at(now)
	}

	// Weekdays, optionally prefixed with next/last/this.
	if t, err, matched := parseWeekday(head, clockPart, now, loc); matched {
		return t, err
	}

	return time.Time{}, fmt.Errorf("time %q: unrecognised (try 2026-08-25, 14:00, tomorrow 09:30, +2h, next monday)", orig)
}

// parseAbsolute handles the machine-readable layouts.
func parseAbsolute(s string, loc *time.Location) (time.Time, bool) {
	if t, err := time.Parse(time.RFC3339, s); err == nil {
		return t.In(loc), true
	}
	for _, layout := range []string{
		"2006-01-02T15:04:05",
		"2006-01-02 15:04:05",
		"2006-01-02T15:04",
		"2006-01-02 15:04",
		"2006-01-02",
		"2006/01/02 15:04",
		"2006/01/02",
	} {
		if t, err := time.ParseInLocation(layout, s, loc); err == nil {
			return t, true
		}
	}
	return time.Time{}, false
}

// splitTrailingClock peels a trailing "HH:MM" off a phrase.
func splitTrailingClock(s string) (head, clock string) {
	fields := strings.Fields(s)
	if len(fields) == 0 {
		return "", ""
	}
	last := fields[len(fields)-1]
	if strings.Contains(last, ":") || isAllDigits(last) && len(fields) > 1 {
		if _, err := parseClock(last); err == nil {
			return strings.Join(fields[:len(fields)-1], " "), last
		}
	}
	// A lone "14:00" is a clock with no head.
	if len(fields) == 1 {
		if _, err := parseClock(fields[0]); err == nil && strings.Contains(fields[0], ":") {
			return "", fields[0]
		}
	}
	return strings.Join(fields, " "), ""
}

func isAllDigits(s string) bool {
	if s == "" {
		return false
	}
	for _, r := range s {
		if r < '0' || r > '9' {
			return false
		}
	}
	return true
}

var weekdayNames = map[string]time.Weekday{
	"sunday": time.Sunday, "sun": time.Sunday,
	"monday": time.Monday, "mon": time.Monday,
	"tuesday": time.Tuesday, "tue": time.Tuesday, "tues": time.Tuesday,
	"wednesday": time.Wednesday, "wed": time.Wednesday,
	"thursday": time.Thursday, "thu": time.Thursday, "thur": time.Thursday, "thurs": time.Thursday,
	"friday": time.Friday, "fri": time.Friday,
	"saturday": time.Saturday, "sat": time.Saturday,
}

// parseWeekday handles "monday", "next monday", "this friday", "last tue".
// "next"/bare both mean the coming occurrence, and never today; "last" means
// the previous one.
func parseWeekday(head, clockPart string, now time.Time, loc *time.Location) (time.Time, error, bool) {
	fields := strings.Fields(head)
	if len(fields) == 0 || len(fields) > 2 {
		return time.Time{}, nil, false
	}
	qualifier := ""
	name := fields[len(fields)-1]
	if len(fields) == 2 {
		qualifier = fields[0]
		switch qualifier {
		case "next", "last", "this", "coming", "previous":
		default:
			return time.Time{}, nil, false
		}
	}
	wd, ok := weekdayNames[name]
	if !ok {
		return time.Time{}, nil, false
	}

	today := time.Date(now.Year(), now.Month(), now.Day(), 0, 0, 0, 0, loc)
	var day time.Time
	switch qualifier {
	case "last", "previous":
		delta := int(today.Weekday()-wd+7) % 7
		if delta == 0 {
			delta = 7
		}
		day = today.AddDate(0, 0, -delta)
	case "this":
		delta := int(wd-today.Weekday()+7) % 7
		day = today.AddDate(0, 0, delta)
	default: // "", "next", "coming"
		delta := int(wd-today.Weekday()+7) % 7
		if delta == 0 {
			delta = 7
		}
		day = today.AddDate(0, 0, delta)
	}

	if clockPart == "" {
		return day, nil, true
	}
	c, err := parseClock(clockPart)
	if err != nil {
		return time.Time{}, err, true
	}
	return atClock(day, c, loc), nil, true
}

// atClock places a time of day on day's calendar date. It builds the result
// with time.Date rather than adding a duration to midnight, so a clock change
// during the day cannot shift the wall-clock time the user asked for.
func atClock(day time.Time, c clock, loc *time.Location) time.Time {
	y, m, d := day.Date()
	return time.Date(y, m, d, int(c)/60, int(c)%60, 0, 0, loc)
}

// parseSpan parses a bare relative span: "2d", "12h", "3w", "90m", "1h30m".
// Days and weeks are calendar-agnostic here (24h / 168h); callers that care
// about DST use ParseWhen with a date instead.
func parseSpan(s string) (time.Duration, error) {
	s = strings.TrimSpace(strings.ToLower(s))
	if s == "" {
		return 0, fmt.Errorf("empty duration")
	}
	// Single unit with a day/week suffix, which time.ParseDuration rejects.
	if n := len(s); n > 1 {
		unit := s[n-1]
		if unit == 'd' || unit == 'w' || unit == 'y' {
			v, err := strconv.ParseFloat(s[:n-1], 64)
			if err == nil {
				switch unit {
				case 'd':
					return time.Duration(v * 24 * float64(time.Hour)), nil
				case 'w':
					return time.Duration(v * 7 * 24 * float64(time.Hour)), nil
				case 'y':
					return time.Duration(v * 365 * 24 * float64(time.Hour)), nil
				}
			}
		}
	}
	d, err := time.ParseDuration(s)
	if err != nil {
		return 0, fmt.Errorf("bad duration %q", s)
	}
	return d, nil
}

// ParseSpan exposes the relative-span grammar used by --since/--until and
// --duration ("30m", "2d", "3w").
func ParseSpan(s string) (time.Duration, error) { return parseSpan(s) }

// ParseRange resolves the --since/--until pair.
//
// A bare span means "relative to now" in the direction the flag implies:
// --since 2d is two days ago, --until 2d is two days ahead. Anything else goes
// through ParseWhen. An empty flag yields the zero time, which callers read as
// "unbounded" and usually replace with their own default window.
func ParseRange(since, until string, now time.Time, loc *time.Location) (from, to time.Time, err error) {
	if loc == nil {
		loc = time.Local
	}
	now = now.In(loc)

	if s := strings.TrimSpace(since); s != "" {
		if d, e := parseSpan(s); e == nil {
			from = now.Add(-d)
		} else if from, err = ParseWhen(s, now, loc); err != nil {
			return time.Time{}, time.Time{}, fmt.Errorf("--since: %w", err)
		}
	}
	if s := strings.TrimSpace(until); s != "" {
		if d, e := parseSpan(s); e == nil {
			to = now.Add(d)
		} else if to, err = ParseWhen(s, now, loc); err != nil {
			return time.Time{}, time.Time{}, fmt.Errorf("--until: %w", err)
		}
	}
	if !from.IsZero() && !to.IsZero() && to.Before(from) {
		return time.Time{}, time.Time{}, fmt.Errorf("--until %s is before --since %s", until, since)
	}
	return from, to, nil
}

// ---------------------------------------------------------------------------
// Rendering

// FormatRange renders an occurrence for table output:
//
//	Tue 26 Aug 09:00–09:30            same day
//	Tue 26 Aug 22:00 – Wed 27 Aug 01:00
//	Wed 27 Aug (all day)
//	Wed 27 Aug – Fri 29 Aug (all day)
//
// All-day ends are treated as exclusive, the way both providers store them.
func FormatRange(start, end time.Time, allDay bool, loc *time.Location) string {
	if loc == nil {
		loc = time.Local
	}
	s := start.In(loc)

	if allDay {
		last := end.In(loc).Add(-time.Nanosecond)
		if !last.After(s) || sameDay(s, last) {
			return s.Format("Mon 2 Jan") + " (all day)"
		}
		return s.Format("Mon 2 Jan") + " – " + last.Format("Mon 2 Jan") + " (all day)"
	}

	e := end.In(loc)
	if !e.After(s) {
		return s.Format("Mon 2 Jan 15:04")
	}
	if sameDay(s, e) {
		return s.Format("Mon 2 Jan 15:04") + "–" + e.Format("15:04")
	}
	return s.Format("Mon 2 Jan 15:04") + " – " + e.Format("Mon 2 Jan 15:04")
}

func sameDay(a, b time.Time) bool {
	ay, am, ad := a.Date()
	by, bm, bd := b.Date()
	return ay == by && am == bm && ad == bd
}
