package calendar

import (
	"fmt"
	"sort"
	"strconv"
	"strings"
	"time"
)

// Busy is one occupied interval, half-open: [Start, End).
type Busy struct {
	Start time.Time
	End   time.Time
	Title string
}

// Slot is a free interval, half-open: [Start, End).
type Slot struct {
	Start time.Time
	End   time.Time
}

// Duration of the slot.
func (s Slot) Duration() time.Duration { return s.End.Sub(s.Start) }

// WorkHours restricts free slots to a working day. Start and End are "HH:MM"
// in Location; Weekdays lists the days that count (empty means Mon–Fri).
type WorkHours struct {
	Start    string
	End      string
	Weekdays []time.Weekday
	Location *time.Location
}

// DefaultWorkHours is 09:00–18:00, Monday to Friday, in loc.
func DefaultWorkHours(loc *time.Location) *WorkHours {
	return &WorkHours{Start: "09:00", End: "18:00", Location: loc}
}

// ParseWorkHours parses the `--hours 09:00-18:00` flag. An empty string
// returns nil, meaning "no restriction".
func ParseWorkHours(s string, loc *time.Location) (*WorkHours, error) {
	s = strings.TrimSpace(s)
	if s == "" {
		return nil, nil
	}
	a, b, ok := strings.Cut(s, "-")
	if !ok {
		if a, b, ok = strings.Cut(s, "–"); !ok {
			return nil, fmt.Errorf("hours %q: want START-END, e.g. 09:00-18:00", s)
		}
	}
	h := &WorkHours{Start: strings.TrimSpace(a), End: strings.TrimSpace(b), Location: loc}
	if _, err := parseClock(h.Start); err != nil {
		return nil, fmt.Errorf("hours %q: %w", s, err)
	}
	if _, err := parseClock(h.End); err != nil {
		return nil, fmt.Errorf("hours %q: %w", s, err)
	}
	return h, nil
}

// clock is a time of day as minutes since midnight.
type clock int

func parseClock(s string) (clock, error) {
	s = strings.TrimSpace(s)
	hh, mm, ok := strings.Cut(s, ":")
	if !ok {
		mm = "0"
	}
	h, err := strconv.Atoi(strings.TrimSpace(hh))
	if err != nil {
		return 0, fmt.Errorf("bad time of day %q", s)
	}
	m, err := strconv.Atoi(strings.TrimSpace(mm))
	if err != nil {
		return 0, fmt.Errorf("bad time of day %q", s)
	}
	// 24:00 is allowed as "end of day".
	if h < 0 || h > 24 || m < 0 || m > 59 || (h == 24 && m != 0) {
		return 0, fmt.Errorf("bad time of day %q", s)
	}
	return clock(h*60 + m), nil
}

func (h *WorkHours) location() *time.Location {
	if h != nil && h.Location != nil {
		return h.Location
	}
	return time.Local
}

func (h *WorkHours) countsDay(d time.Weekday) bool {
	if len(h.Weekdays) == 0 {
		return d != time.Saturday && d != time.Sunday
	}
	for _, w := range h.Weekdays {
		if w == d {
			return true
		}
	}
	return false
}

// FreeSlots returns the gaps in [from, to) that are not covered by busy and
// last at least minDuration.
//
// Busy intervals are merged first, so overlapping and back-to-back meetings
// collapse into one block. When hours is non-nil the search is restricted to
// the working window of each qualifying weekday in hours.Location, and a slot
// never spans a night.
//
// Zero-length and inverted busy intervals are ignored, as are the parts of any
// interval outside the window.
func FreeSlots(busy []Busy, from, to time.Time, minDuration time.Duration, hours *WorkHours) []Slot {
	if !from.Before(to) {
		return nil
	}
	if minDuration <= 0 {
		minDuration = time.Nanosecond
	}
	merged := mergeBusy(busy)

	var out []Slot
	for _, w := range windows(from, to, hours) {
		out = append(out, subtract(w, merged, minDuration)...)
	}
	return out
}

// mergeBusy sorts and coalesces overlapping or touching intervals.
func mergeBusy(busy []Busy) []Slot {
	spans := make([]Slot, 0, len(busy))
	for _, b := range busy {
		if b.End.After(b.Start) {
			spans = append(spans, Slot{Start: b.Start, End: b.End})
		}
	}
	if len(spans) == 0 {
		return nil
	}
	sort.Slice(spans, func(i, j int) bool {
		if spans[i].Start.Equal(spans[j].Start) {
			return spans[i].End.Before(spans[j].End)
		}
		return spans[i].Start.Before(spans[j].Start)
	})
	merged := []Slot{spans[0]}
	for _, s := range spans[1:] {
		last := &merged[len(merged)-1]
		if !s.Start.After(last.End) { // overlapping or adjacent
			if s.End.After(last.End) {
				last.End = s.End
			}
			continue
		}
		merged = append(merged, s)
	}
	return merged
}

// windows splits [from, to) into the candidate ranges free time may fall in:
// the whole range when hours is nil, otherwise one window per working day.
func windows(from, to time.Time, hours *WorkHours) []Slot {
	if hours == nil {
		return []Slot{{Start: from, End: to}}
	}
	loc := hours.location()
	startMin, err1 := parseClock(hours.Start)
	endMin, err2 := parseClock(hours.End)
	if err1 != nil || err2 != nil || endMin <= startMin {
		// An unusable working day yields no free time rather than silently
		// ignoring the restriction.
		return nil
	}

	var out []Slot
	day := from.In(loc)
	day = time.Date(day.Year(), day.Month(), day.Day(), 0, 0, 0, 0, loc)
	for !day.After(to.In(loc)) {
		if hours.countsDay(day.Weekday()) {
			// Built with time.Date, not day+duration: on a clock-change
			// day the working window is still 09:00-17:00 wall clock.
			s := atClock(day, startMin, loc)
			e := atClock(day, endMin, loc)
			if s.Before(from) {
				s = from
			}
			if e.After(to) {
				e = to
			}
			if s.Before(e) {
				out = append(out, Slot{Start: s, End: e})
			}
		}
		// AddDate rather than +24h so DST days stay whole days.
		day = day.AddDate(0, 0, 1)
	}
	return out
}

// subtract removes the busy spans from one window, keeping gaps >= minDur.
func subtract(w Slot, busy []Slot, minDur time.Duration) []Slot {
	var out []Slot
	cur := w.Start
	for _, b := range busy {
		if !b.End.After(cur) {
			continue // entirely before the cursor
		}
		if !b.Start.Before(w.End) {
			break // entirely after the window; the list is sorted
		}
		if b.Start.After(cur) {
			if gap := b.Start.Sub(cur); gap >= minDur {
				out = append(out, Slot{Start: cur, End: b.Start})
			}
		}
		if b.End.After(cur) {
			cur = b.End
		}
		if !cur.Before(w.End) {
			return out
		}
	}
	if w.End.Sub(cur) >= minDur {
		out = append(out, Slot{Start: cur, End: w.End})
	}
	return out
}
