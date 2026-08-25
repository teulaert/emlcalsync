package calendar

import (
	"strings"
	"testing"
	"time"
)

func TestParseWhen(t *testing.T) {
	loc := amsterdam(t)
	// Tuesday 25 August 2026, 15:30 local.
	now := time.Date(2026, 8, 25, 15, 30, 0, 0, loc)
	if now.Weekday() != time.Tuesday {
		t.Fatalf("test fixture: 2026-08-25 is a %v, expected Tuesday", now.Weekday())
	}

	tests := []struct {
		in   string
		want string // RFC 3339 in loc
	}{
		{"2026-09-01T08:15:00+02:00", "2026-09-01T08:15:00+02:00"},
		{"2026-09-01T06:15:00Z", "2026-09-01T08:15:00+02:00"}, // converted into loc
		{"2026-08-25", "2026-08-25T00:00:00+02:00"},
		{"2026-08-25 14:00", "2026-08-25T14:00:00+02:00"},
		{"2026-08-25T14:00", "2026-08-25T14:00:00+02:00"},
		{"2026-08-25 14:00:30", "2026-08-25T14:00:30+02:00"},
		{"14:00", "2026-08-25T14:00:00+02:00"},
		{"9:05", "2026-08-25T09:05:00+02:00"},
		{"now", "2026-08-25T15:30:00+02:00"},
		{"today", "2026-08-25T00:00:00+02:00"},
		{"today 08:00", "2026-08-25T08:00:00+02:00"},
		{"tomorrow", "2026-08-26T00:00:00+02:00"},
		{"tomorrow 09:30", "2026-08-26T09:30:00+02:00"},
		{"TOMORROW 09:30", "2026-08-26T09:30:00+02:00"},
		{"yesterday", "2026-08-24T00:00:00+02:00"},
		{"+2h", "2026-08-25T17:30:00+02:00"},
		{"-30m", "2026-08-25T15:00:00+02:00"},
		{"+3d", "2026-08-28T15:30:00+02:00"},
		{"+1w", "2026-09-01T15:30:00+02:00"},
		{"next monday", "2026-08-31T00:00:00+02:00"},
		{"next monday 09:00", "2026-08-31T09:00:00+02:00"},
		{"monday", "2026-08-31T00:00:00+02:00"},
		{"friday", "2026-08-28T00:00:00+02:00"},
		{"fri", "2026-08-28T00:00:00+02:00"},
		{"last friday", "2026-08-21T00:00:00+02:00"},
		{"this friday", "2026-08-28T00:00:00+02:00"},
		// The coming Tuesday is a week out, never today.
		{"next tuesday", "2026-09-01T00:00:00+02:00"},
		{"this tuesday", "2026-08-25T00:00:00+02:00"},
	}
	for _, tc := range tests {
		t.Run(tc.in, func(t *testing.T) {
			got, err := ParseWhen(tc.in, now, loc)
			if err != nil {
				t.Fatalf("ParseWhen(%q): %v", tc.in, err)
			}
			if s := got.In(loc).Format(time.RFC3339); s != tc.want {
				t.Errorf("ParseWhen(%q) = %s, want %s", tc.in, s, tc.want)
			}
		})
	}

	for _, bad := range []string{"", "  ", "someday", "2026-13-45", "next flurbday", "25:00"} {
		if got, err := ParseWhen(bad, now, loc); err == nil {
			t.Errorf("ParseWhen(%q) = %v, want an error", bad, got)
		}
	}
}

func TestParseWhenAcrossDST(t *testing.T) {
	loc := amsterdam(t)
	// The evening before the autumn change: "tomorrow 09:30" is a wall-clock
	// time on the 25-hour day, so the offset differs from now's.
	now := time.Date(2026, 10, 24, 20, 0, 0, 0, loc)
	got, err := ParseWhen("tomorrow 09:30", now, loc)
	if err != nil {
		t.Fatal(err)
	}
	if s := got.Format(time.RFC3339); s != "2026-10-25T09:30:00+01:00" {
		t.Errorf("got %s, want 09:30 local on the 25th", s)
	}
}

func TestParseWhenDefaultsToLocal(t *testing.T) {
	now := time.Date(2026, 8, 25, 15, 30, 0, 0, time.UTC)
	got, err := ParseWhen("2026-08-25", now, nil)
	if err != nil {
		t.Fatal(err)
	}
	if got.Location() != time.Local {
		t.Errorf("a nil location should mean the system zone, got %v", got.Location())
	}
}

func TestParseRange(t *testing.T) {
	loc := amsterdam(t)
	now := time.Date(2026, 8, 25, 15, 30, 0, 0, loc)

	// Bare spans go backwards for --since and forwards for --until.
	from, to, err := ParseRange("2d", "12h", now, loc)
	if err != nil {
		t.Fatal(err)
	}
	if got := from.Format(time.RFC3339); got != "2026-08-23T15:30:00+02:00" {
		t.Errorf("from = %s", got)
	}
	if got := to.Format(time.RFC3339); got != "2026-08-26T03:30:00+02:00" {
		t.Errorf("to = %s", got)
	}

	// Weeks.
	from, _, err = ParseRange("3w", "", now, loc)
	if err != nil {
		t.Fatal(err)
	}
	if got := from.Format(time.RFC3339); got != "2026-08-04T15:30:00+02:00" {
		t.Errorf("from = %s", got)
	}

	// Absolute dates still work.
	from, to, err = ParseRange("2026-08-01", "2026-09-01", now, loc)
	if err != nil {
		t.Fatal(err)
	}
	if from.Format("2006-01-02") != "2026-08-01" || to.Format("2006-01-02") != "2026-09-01" {
		t.Errorf("from = %v, to = %v", from, to)
	}

	// Empty flags mean unbounded, which callers replace with their default.
	from, to, err = ParseRange("", "", now, loc)
	if err != nil || !from.IsZero() || !to.IsZero() {
		t.Errorf("ParseRange with no flags = %v, %v, %v", from, to, err)
	}

	// An inverted range is a usage error, named clearly.
	if _, _, err := ParseRange("2026-09-01", "2026-08-01", now, loc); err == nil {
		t.Error("until before since should fail")
	}
	if _, _, err := ParseRange("flurb", "", now, loc); err == nil || !strings.Contains(err.Error(), "--since") {
		t.Errorf("error should name the flag: %v", err)
	}
	if _, _, err := ParseRange("", "flurb", now, loc); err == nil || !strings.Contains(err.Error(), "--until") {
		t.Errorf("error should name the flag: %v", err)
	}
}

func TestParseSpan(t *testing.T) {
	for _, tc := range []struct {
		in   string
		want time.Duration
	}{
		{"30m", 30 * time.Minute},
		{"12h", 12 * time.Hour},
		{"2d", 48 * time.Hour},
		{"3w", 21 * 24 * time.Hour},
		{"1h30m", 90 * time.Minute},
		{"0.5d", 12 * time.Hour},
	} {
		got, err := ParseSpan(tc.in)
		if err != nil || got != tc.want {
			t.Errorf("ParseSpan(%q) = %v, %v; want %v", tc.in, got, err, tc.want)
		}
	}
	for _, bad := range []string{"", "soon", "5", "tomorrow"} {
		if got, err := ParseSpan(bad); err == nil {
			t.Errorf("ParseSpan(%q) = %v, want an error", bad, got)
		}
	}
}

func TestFormatRange(t *testing.T) {
	loc := amsterdam(t)
	for _, tc := range []struct {
		name   string
		start  time.Time
		end    time.Time
		allDay bool
		want   string
	}{
		{
			"same day",
			time.Date(2026, 8, 25, 9, 0, 0, 0, loc),
			time.Date(2026, 8, 25, 9, 30, 0, 0, loc),
			false, "Tue 25 Aug 09:00–09:30",
		},
		{
			"crossing midnight",
			time.Date(2026, 8, 25, 22, 0, 0, 0, loc),
			time.Date(2026, 8, 26, 1, 0, 0, 0, loc),
			false, "Tue 25 Aug 22:00 – Wed 26 Aug 01:00",
		},
		{
			"single all day",
			time.Date(2026, 8, 26, 0, 0, 0, 0, loc),
			time.Date(2026, 8, 27, 0, 0, 0, 0, loc),
			true, "Wed 26 Aug (all day)",
		},
		{
			"multi day all day",
			time.Date(2026, 8, 26, 0, 0, 0, 0, loc),
			time.Date(2026, 8, 29, 0, 0, 0, 0, loc),
			true, "Wed 26 Aug – Fri 28 Aug (all day)",
		},
		{
			"zero length",
			time.Date(2026, 8, 25, 9, 0, 0, 0, loc),
			time.Date(2026, 8, 25, 9, 0, 0, 0, loc),
			false, "Tue 25 Aug 09:00",
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := FormatRange(tc.start, tc.end, tc.allDay, loc); got != tc.want {
				t.Errorf("FormatRange = %q, want %q", got, tc.want)
			}
		})
	}

	// Times are converted into the display zone.
	utc := time.Date(2026, 8, 25, 7, 0, 0, 0, time.UTC)
	if got := FormatRange(utc, utc.Add(time.Hour), false, loc); got != "Tue 25 Aug 09:00–10:00" {
		t.Errorf("FormatRange in another zone = %q", got)
	}
}
