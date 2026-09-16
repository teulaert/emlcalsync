package calendar

import (
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/teambition/rrule-go"
)

// ErrRRule is the class of every rejection from ParseRRule, so a caller can
// map the whole family onto one exit code without matching on prose.
var ErrRRule = errors.New("bad recurrence rule")

// ParseRRule validates a recurrence rule typed by a person and returns it in
// the form the event model stores: the RECUR value alone, upper-cased, with no
// "RRULE:" prefix.
//
// What a person types is not what a provider hands back. Both spellings are in
// circulation -- iCalendar writes the property as "RRULE:FREQ=WEEKLY", a
// JSCalendar or Google client thinks in the value alone -- so both are taken,
// and the model keeps the value, which is what internal/calendar expands and
// what every backend's writer prefixes for itself.
//
// An empty string is not an error: it is how the CLI says "no recurrence", and
// the caller decides what that means. Everything else must be a rule the
// expander can actually run, because a rule stored but never expandable is a
// series that silently has no occurrences.
func ParseRRule(s string) (string, error) {
	v := strings.TrimSpace(s)
	if v == "" {
		return "", nil
	}
	// A rule reaches the wire inside an iCalendar property, one per line. A
	// value carrying a line break would end that property and start whatever
	// the rest of the string says -- an ORGANIZER, an ATTENDEE, a whole
	// VEVENT -- on the calendars of everybody the event is shared with. There
	// is no legitimate newline inside a RECUR value, so this is a refusal
	// rather than a sanitisation: quietly dropping half of what somebody
	// typed is its own kind of surprise.
	if i := strings.IndexAny(v, "\r\n"); i >= 0 {
		return "", fmt.Errorf("%w: a rule is one line, and this one breaks at byte %d", ErrRRule, i)
	}
	// The same argument for the rest of the control range: a NUL or an
	// escape cannot mean anything in a RECUR value and can mean plenty to
	// whatever parses it next.
	for i, r := range v {
		if r < 0x20 || r == 0x7f {
			return "", fmt.Errorf("%w: control character %q at byte %d", ErrRRule, r, i)
		}
	}

	if strings.HasPrefix(strings.ToUpper(v), "RRULE:") {
		v = strings.TrimSpace(v[len("RRULE:"):])
	}
	if v == "" {
		return "", fmt.Errorf("%w: RRULE: with no rule after it", ErrRRule)
	}
	// One property, so one colon's worth of prefix and no more. A second
	// colon means a second property was appended.
	if strings.Contains(v, ":") {
		return "", fmt.Errorf("%w: %q is more than one property", ErrRRule, v)
	}
	v = strings.ToUpper(v)

	// FREQ is the one part RFC 5545 §3.3.10 requires, and rrule-go is lenient
	// about its absence in a way that would leave a "recurring" event with
	// exactly one occurrence.
	if !hasRRulePart(v, "FREQ") {
		return "", fmt.Errorf("%w: %q has no FREQ", ErrRRule, v)
	}
	// Parsed against a fixed instant purely to prove it parses: the real
	// anchor is the event's own start, which this function has no business
	// knowing. A rule that is valid at one instant is valid at any other --
	// only the instances it yields differ.
	if _, err := rrule.StrToROptionInLocation(v, time.UTC); err != nil {
		return "", fmt.Errorf("%w: %v", ErrRRule, err)
	}
	return v, nil
}

// hasRRulePart reports whether the rule sets a named part, so that "FREQ" is
// found in "FREQ=WEEKLY" but not in "BYSETPOS=1;X-FREQ=2".
func hasRRulePart(rule, name string) bool {
	for _, part := range strings.Split(rule, ";") {
		key, _, ok := strings.Cut(part, "=")
		if ok && strings.TrimSpace(key) == name {
			return true
		}
	}
	return false
}
