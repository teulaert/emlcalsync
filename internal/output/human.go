package output

import (
	"strconv"
	"strings"
	"time"
	"unicode/utf8"
)

// Truncate shortens s to at most n display runes, ending with "…" when it had
// to cut. It counts runes, not bytes, so accented and CJK text is not mangled.
func Truncate(s string, n int) string {
	if n <= 0 {
		return ""
	}
	if utf8.RuneCountInString(s) <= n {
		return s
	}
	if n == 1 {
		return "…"
	}
	var b strings.Builder
	count := 0
	for _, r := range s {
		if count == n-1 {
			break
		}
		b.WriteRune(r)
		count++
	}
	return strings.TrimRight(b.String(), " ") + "…"
}

// HumanSize formats a byte count compactly: "912B", "4.3KB", "1.2MB". Units
// are powers of 1024 — mail sizes are compared against disk, not marketing.
func HumanSize(n int64) string {
	if n < 0 {
		return "-" + HumanSize(-n)
	}
	if n < 1024 {
		return strconv.FormatInt(n, 10) + "B"
	}
	units := []string{"KB", "MB", "GB", "TB", "PB"}
	v := float64(n)
	i := -1
	for v >= 1024 && i < len(units)-1 {
		v /= 1024
		i++
	}
	prec := 1
	if v >= 100 {
		prec = 0
	}
	s := strconv.FormatFloat(v, 'f', prec, 64)
	s = strings.TrimSuffix(s, ".0")
	return s + units[i]
}

// RelTime renders t relative to now, the way a mail list column wants it:
//
//	<1 min      "now"
//	<1 hour     "12m"
//	<24 hours   "3h"
//	yesterday   "yesterday"
//	this year   "Aug 20"
//	older       "2025-01-03"
//
// Future times get the same shapes with "in " ("in 12m", "tomorrow").
// Both times are compared in now's location.
func RelTime(t time.Time, now time.Time) string {
	if t.IsZero() {
		return ""
	}
	loc := now.Location()
	t = t.In(loc)

	d := now.Sub(t)
	future := d < 0
	if future {
		d = -d
	}

	switch {
	case d < time.Minute:
		return "now"
	case d < time.Hour:
		return withDir(strconv.Itoa(int(d/time.Minute))+"m", future)
	case d < 24*time.Hour:
		// Under a day, hours beat "yesterday": at 02:00 a message sent at
		// 23:00 is "3h", not a different day.
		return withDir(strconv.Itoa(int(d/time.Hour))+"h", future)
	}

	switch dayDiff(t, now) {
	case 1:
		return "yesterday"
	case -1:
		return "tomorrow"
	}
	if t.Year() == now.Year() {
		return t.Format("Jan 2")
	}
	return t.Format("2006-01-02")
}

func withDir(s string, future bool) string {
	if future {
		return "in " + s
	}
	return s
}

// dayDiff is the number of calendar days now is ahead of t (both already in
// the same location): 1 means t was yesterday, -1 means tomorrow.
func dayDiff(t, now time.Time) int {
	// Midnights are built in UTC so a DST transition cannot make a calendar
	// day 23 or 25 hours long and skew the division.
	td := time.Date(t.Year(), t.Month(), t.Day(), 0, 0, 0, 0, time.UTC)
	nd := time.Date(now.Year(), now.Month(), now.Day(), 0, 0, 0, 0, time.UTC)
	return int(nd.Sub(td) / (24 * time.Hour))
}
