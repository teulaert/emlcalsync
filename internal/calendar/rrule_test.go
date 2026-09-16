package calendar

import (
	"errors"
	"strings"
	"testing"
	"time"
)

// anchor is any instant: a rule that parses at one parses at any other, which
// is why ParseRRule need not know the event's start.
var ruleAnchor = time.Date(2026, 9, 16, 9, 0, 0, 0, time.UTC)

func TestParseRRuleAcceptsBothSpellings(t *testing.T) {
	// A person types what their last client showed them, and the two in
	// circulation disagree about the prefix.
	for _, in := range []string{
		"FREQ=WEEKLY;BYDAY=MO",
		"RRULE:FREQ=WEEKLY;BYDAY=MO",
		"rrule:freq=weekly;byday=mo",
		"  RRULE:FREQ=WEEKLY;BYDAY=MO  ",
	} {
		got, err := ParseRRule(in)
		if err != nil {
			t.Errorf("ParseRRule(%q): %v", in, err)
			continue
		}
		if got != "FREQ=WEEKLY;BYDAY=MO" {
			t.Errorf("ParseRRule(%q) = %q, want the bare value upper-cased", in, got)
		}
	}
}

// Empty is not a rejection: it is how the CLI says "no recurrence", and what
// `cal update --rrule ""` has to be able to mean.
func TestParseRRuleEmptyIsNotAnError(t *testing.T) {
	for _, in := range []string{"", "   ", "\t"} {
		got, err := ParseRRule(in)
		if err != nil || got != "" {
			t.Errorf("ParseRRule(%q) = %q, %v; want \"\", nil", in, got, err)
		}
	}
}

// A rule reaches the wire inside an iCalendar property, one per line. A value
// carrying a line break ends that property and starts whatever follows on the
// calendars of everybody the event is shared with.
func TestParseRRuleRefusesPropertyInjection(t *testing.T) {
	cases := map[string]string{
		"LF":                  "FREQ=DAILY\nSUMMARY:Injected",
		"CRLF":                "FREQ=DAILY\r\nATTENDEE;PARTSTAT=ACCEPTED:mailto:x@example.org",
		"CR":                  "FREQ=DAILY\rORGANIZER:mailto:x@example.org",
		"whole VEVENT":        "FREQ=DAILY\nEND:VEVENT\nBEGIN:VEVENT\nUID:evil",
		"break after a space": "FREQ=DAILY \n SUMMARY:Injected",
		"NUL":                 "FREQ=DAILY\x00",
		"escape":              "FREQ=DAILY\x1b[0m",
		"second property":     "FREQ=DAILY;COUNT=3:SUMMARY=x",
		"prefix with nothing": "RRULE:",
	}
	for name, in := range cases {
		t.Run(name, func(t *testing.T) {
			got, err := ParseRRule(in)
			if err == nil {
				t.Fatalf("ParseRRule(%q) = %q, want a refusal", in, got)
			}
			if !errors.Is(err, ErrRRule) {
				t.Errorf("err = %v, want it to wrap ErrRRule", err)
			}
			if got != "" {
				t.Errorf("a refused rule came back as %q, want \"\"", got)
			}
		})
	}
}

// Whitespace around the rule is not an attack, it is a shell: --rrule
// "$(cat rule.txt)" arrives with a trailing newline and means what it says.
// Only a break *inside* the value can end the property early, so the outside
// is trimmed and the inside refused.
func TestParseRRuleTrimsSurroundingWhitespace(t *testing.T) {
	for _, in := range []string{"FREQ=DAILY\n", "\nFREQ=DAILY", "\r\n FREQ=DAILY \r\n", "\tFREQ=DAILY\t"} {
		got, err := ParseRRule(in)
		if err != nil {
			t.Errorf("ParseRRule(%q): %v", in, err)
			continue
		}
		if got != "FREQ=DAILY" {
			t.Errorf("ParseRRule(%q) = %q, want FREQ=DAILY", in, got)
		}
	}
}

// A rule stored but not expandable is a series that silently has no
// occurrences, so nonsense is refused at the point it is typed.
func TestParseRRuleRefusesWhatCannotBeExpanded(t *testing.T) {
	for _, in := range []string{
		"BYDAY=MO",             // no FREQ
		"COUNT=3",              // no FREQ
		"X-FREQ=WEEKLY",        // FREQ only as part of another name
		"FREQ=FORTNIGHTLY",     // not a frequency
		"FREQ=WEEKLY;BYDAY=XX", // not a weekday
		"nonsense",
		"FREQ=WEEKLY;COUNT=notanumber",
	} {
		if got, err := ParseRRule(in); err == nil {
			t.Errorf("ParseRRule(%q) = %q, want a refusal", in, got)
		} else if !errors.Is(err, ErrRRule) {
			t.Errorf("ParseRRule(%q) err = %v, want it to wrap ErrRRule", in, err)
		}
	}
}

// What comes out has to be what the expander takes in, or validation would be
// checking a different string from the one that gets stored.
func TestParseRRuleOutputExpands(t *testing.T) {
	for _, in := range []string{
		"FREQ=DAILY;COUNT=3",
		"RRULE:FREQ=WEEKLY;BYDAY=MO,WE;UNTIL=20261231T000000Z",
		"FREQ=MONTHLY;BYMONTHDAY=1",
		"FREQ=YEARLY;INTERVAL=2",
	} {
		rule, err := ParseRRule(in)
		if err != nil {
			t.Fatalf("ParseRRule(%q): %v", in, err)
		}
		if strings.HasPrefix(rule, "RRULE:") {
			t.Errorf("%q kept the prefix", rule)
		}
		if _, err := buildRule(rule, ruleAnchor, time.UTC); err != nil {
			t.Errorf("the accepted rule %q does not expand: %v", rule, err)
		}
	}
}
