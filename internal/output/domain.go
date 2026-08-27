package output

import (
	"fmt"
	"strings"
	"time"

	"github.com/teulaert/emlcalsync/internal/model"
)

// Formatters for domain values that more than one surface has to render the
// same way. They live here rather than in a command file because the CLI and
// the TUI must not disagree about what a flag letter or an RSVP word means.

// ShortAddr renders an address the way a narrow FROM column wants it.
func ShortAddr(a model.Address) string {
	if a.Name != "" {
		return a.Name
	}
	return a.Email
}

// MailFlags is the compact flag column: U unread, * flagged, A attachments,
// R answered.
func MailFlags(f model.Flags, hasAttachments bool) string {
	var b strings.Builder
	if f.Unread {
		b.WriteByte('U')
	}
	if f.Flagged {
		b.WriteByte('*')
	}
	if hasAttachments {
		b.WriteByte('A')
	}
	if f.Answered {
		b.WriteByte('R')
	}
	return b.String()
}

// TimeCell is a occurrence's time without the leading day, which an agenda's
// group header already carries.
func TimeCell(start, end time.Time, allDay bool, loc *time.Location) string {
	if allDay {
		return "all day"
	}
	s, e := start.In(loc), end.In(loc)
	if !e.After(s) {
		return s.Format("15:04")
	}
	if s.Year() == e.Year() && s.YearDay() == e.YearDay() {
		return s.Format("15:04") + "–" + e.Format("15:04")
	}
	return s.Format("15:04") + " – " + e.Format("Mon 2 Jan 15:04")
}

// RSVP renders a participation status as the one word an agenda column shows.
func RSVP(p model.Participation) string {
	switch p {
	case model.PartAccepted:
		return "yes"
	case model.PartDeclined:
		return "no"
	case model.PartTentative:
		return "maybe"
	case model.PartNeedsAction:
		return "?"
	}
	return ""
}

// Duration renders a span compactly: 30m, 1h, 1h30m, 2d3h.
func Duration(d time.Duration) string {
	if d <= 0 {
		return "0m"
	}
	d = d.Round(time.Minute)
	var b strings.Builder
	if days := int(d / (24 * time.Hour)); days > 0 {
		fmt.Fprintf(&b, "%dd", days)
		d -= time.Duration(days) * 24 * time.Hour
	}
	if h := int(d / time.Hour); h > 0 {
		fmt.Fprintf(&b, "%dh", h)
		d -= time.Duration(h) * time.Hour
	}
	if m := int(d / time.Minute); m > 0 {
		fmt.Fprintf(&b, "%dm", m)
	}
	if b.Len() == 0 {
		return "0m"
	}
	return b.String()
}
