package itip

import (
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/teulaert/emlcalsync/internal/model"
)

var replyNow = time.Date(2026, 9, 16, 9, 30, 0, 0, time.UTC)

func selfAddr() model.Address {
	return model.Address{Name: "Lennert den Teuling", Email: "lennert@example.com"}
}

// unfoldICS undoes the 75-octet folding so a test can look for a whole
// property on one line, the way a reader of the bytes thinks of it.
func unfoldICS(b []byte) string {
	return strings.ReplaceAll(strings.ReplaceAll(string(b), "\r\n ", ""), "\r\n\t", "")
}

func buildReply(t *testing.T, resp model.Participation) *Reply {
	t.Helper()
	inv, err := FromMessage(loadFixture(t, "invite.eml"), "lennert@example.com")
	if err != nil {
		t.Fatalf("FromMessage: %v", err)
	}
	r, err := inv.Reply(selfAddr(), resp, replyNow)
	if err != nil {
		t.Fatalf("Reply: %v", err)
	}
	return r
}

func TestReplyCarriesWhatTheOrganizerMatchesOn(t *testing.T) {
	r := buildReply(t, model.PartAccepted)

	if r.To.Email != "martijn@example.org" {
		t.Errorf("answering to %+v, want the organizer", r.To)
	}
	if r.Response != model.PartAccepted {
		t.Errorf("response = %q", r.Response)
	}

	ics := unfoldICS(r.Calendar)
	for _, want := range []string{
		"METHOD:REPLY",
		"VERSION:2.0",
		"BEGIN:VEVENT",
		// The UID is what the organizer's scheduler files the answer against.
		"UID:040000008200E00074C5B7101A82E00800000000BB3DDF993738DD01000000000000000010000000D9B5581854DF3640B533A07A2B4B5089",
		"ORGANIZER;CN=Martijn Organiser:mailto:martijn@example.org",
		"DTSTAMP:20260916T093000Z",
		"SEQUENCE:",
	} {
		if !strings.Contains(ics, want) {
			t.Errorf("the reply does not carry %q:\n%s", want, ics)
		}
	}

	// One attendee, the one answering, with the answer on it.
	att := 0
	for _, line := range strings.Split(ics, "\r\n") {
		if strings.HasPrefix(line, "ATTENDEE") {
			att++
			if !strings.Contains(line, "PARTSTAT=ACCEPTED") || !strings.Contains(line, "mailto:lennert@example.com") {
				t.Errorf("attendee line = %q", line)
			}
		}
	}
	if att != 1 {
		t.Errorf("the reply carries %d attendee lines, want only the one answering", att)
	}
}

// The invitation states its times against a VTIMEZONE it carries. The reply
// carries no VTIMEZONE, so a time written against a TZID would name a zone
// the reader cannot resolve -- it goes out as UTC instead, which is the same
// instant with nothing to get wrong.
func TestReplyStatesItsTimesInUTC(t *testing.T) {
	r := buildReply(t, model.PartAccepted)
	ics := unfoldICS(r.Calendar)

	if !strings.Contains(ics, "DTSTART:20260902T080000Z") {
		t.Errorf("DTSTART is not the invitation's instant in UTC:\n%s", ics)
	}
	if !strings.Contains(ics, "DTEND:20260902T084500Z") {
		t.Errorf("DTEND is not the invitation's instant in UTC:\n%s", ics)
	}
	if strings.Contains(ics, "TZID") || strings.Contains(ics, "BEGIN:VTIMEZONE") {
		t.Errorf("the reply names a zone it does not define:\n%s", ics)
	}
}

func TestReplySubjectAndTextSayWhatWasAnswered(t *testing.T) {
	for _, tc := range []struct {
		resp      model.Participation
		subject   string
		inText    string
		partstat  string
		wantError bool
	}{
		{resp: model.PartAccepted, subject: "Accepted: Momentum FO", inText: "has accepted", partstat: "ACCEPTED"},
		{resp: model.PartDeclined, subject: "Declined: Momentum FO", inText: "has declined", partstat: "DECLINED"},
		{resp: model.PartTentative, subject: "Tentative: Momentum FO", inText: "has tentatively accepted", partstat: "TENTATIVE"},
	} {
		t.Run(string(tc.resp), func(t *testing.T) {
			r := buildReply(t, tc.resp)
			if r.Subject != tc.subject {
				t.Errorf("subject = %q, want %q", r.Subject, tc.subject)
			}
			if !strings.Contains(r.Text, tc.inText) {
				t.Errorf("text = %q, want it to say %q", r.Text, tc.inText)
			}
			// The fixture's organizer wrote the address as the CN, so that is
			// the name the answer goes under and the one the text uses.
			if !strings.Contains(r.Text, "lennert@example.com") {
				t.Errorf("text = %q, want it to name who answered", r.Text)
			}
			if !strings.Contains(unfoldICS(r.Calendar), "PARTSTAT="+tc.partstat) {
				t.Errorf("PARTSTAT is not %s:\n%s", tc.partstat, unfoldICS(r.Calendar))
			}
		})
	}
}

// The organizer already wrote a name for this person. Answering under theirs
// rather than ours keeps their attendee list from growing a second entry for
// the same address.
func TestReplyAnswersUnderTheNameTheOrganizerWrote(t *testing.T) {
	inv, err := FromMessage(loadFixture(t, "invite.eml"), "lennert@example.com")
	if err != nil {
		t.Fatalf("FromMessage: %v", err)
	}
	invited := inv.Self()
	if invited == nil || invited.Name == "" {
		t.Skip("the fixture's attendee line carries no CN")
	}
	r, err := inv.Reply(model.Address{Name: "Someone Else", Email: "lennert@example.com"},
		model.PartAccepted, replyNow)
	if err != nil {
		t.Fatalf("Reply: %v", err)
	}
	if got := unfoldICS(r.Calendar); !strings.Contains(got, "CN="+invited.Name) {
		t.Errorf("the reply does not answer under %q:\n%s", invited.Name, got)
	}
}

// SEQUENCE says which revision of the meeting is being answered, and
// RECURRENCE-ID which occurrence. Neither is in the event model, so both are
// read back off the invitation's own bytes; an answer that dropped them is
// one the organizer files against the wrong thing.
func TestReplyCarriesTheSequenceAndOccurrenceItAnswers(t *testing.T) {
	ics := "BEGIN:VCALENDAR\r\nVERSION:2.0\r\nMETHOD:REQUEST\r\nBEGIN:VEVENT\r\n" +
		"UID:series-1\r\nSEQUENCE:4\r\nRECURRENCE-ID:20260918T090000Z\r\n" +
		"DTSTART:20260918T090000Z\r\nDTEND:20260918T100000Z\r\nSUMMARY:Weekly\r\n" +
		"ORGANIZER;CN=Gert:mailto:gert@example.org\r\n" +
		"ATTENDEE;PARTSTAT=NEEDS-ACTION:mailto:lennert@example.com\r\n" +
		"END:VEVENT\r\nEND:VCALENDAR\r\n"
	inv, err := Parse([]byte(ics), "lennert@example.com")
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}
	r, err := inv.Reply(selfAddr(), model.PartDeclined, replyNow)
	if err != nil {
		t.Fatalf("Reply: %v", err)
	}
	got := unfoldICS(r.Calendar)
	if !strings.Contains(got, "SEQUENCE:4") {
		t.Errorf("the reply answers the wrong revision:\n%s", got)
	}
	if !strings.Contains(got, "RECURRENCE-ID:20260918T090000Z") {
		t.Errorf("the reply answers the series instead of the occurrence:\n%s", got)
	}
}

// An invitation that never said writes SEQUENCE:0, which is what RFC 5545
// says a missing one means -- the organizer should not have to guess.
func TestReplyWritesSequenceZeroWhenTheInvitationHadNone(t *testing.T) {
	r := buildReply(t, model.PartAccepted)
	if got := unfoldICS(r.Calendar); !strings.Contains(got, "SEQUENCE:") {
		t.Errorf("no SEQUENCE in the reply:\n%s", got)
	}
}

func TestReplyRefusesWhatIsNotAnInvitation(t *testing.T) {
	cases := map[string]string{
		"a cancellation":    "CANCEL",
		"a reply":           "REPLY",
		"a published event": "PUBLISH",
	}
	for name, method := range cases {
		t.Run(name, func(t *testing.T) {
			ics := "BEGIN:VCALENDAR\r\nVERSION:2.0\r\nMETHOD:" + method + "\r\nBEGIN:VEVENT\r\n" +
				"UID:u1\r\nDTSTART:20260918T090000Z\r\nSUMMARY:Thing\r\n" +
				"ORGANIZER:mailto:gert@example.org\r\n" +
				"ATTENDEE;PARTSTAT=NEEDS-ACTION:mailto:lennert@example.com\r\n" +
				"END:VEVENT\r\nEND:VCALENDAR\r\n"
			inv, err := Parse([]byte(ics), "lennert@example.com")
			if err != nil {
				t.Fatalf("Parse: %v", err)
			}
			if _, err := inv.Reply(selfAddr(), model.PartAccepted, replyNow); !errors.Is(err, ErrNotAnswerable) {
				t.Errorf("err = %v, want ErrNotAnswerable", err)
			}
		})
	}
}

func TestReplyRefusesWhatItCannotAddress(t *testing.T) {
	// An invitation with everything but an organizer: there is nowhere to
	// send an answer, and mailing it to the attendees would tell the wrong
	// people.
	t.Run("no organizer", func(t *testing.T) {
		ics := "BEGIN:VCALENDAR\r\nVERSION:2.0\r\nMETHOD:REQUEST\r\nBEGIN:VEVENT\r\n" +
			"UID:u1\r\nDTSTART:20260918T090000Z\r\nSUMMARY:Thing\r\n" +
			"ATTENDEE;PARTSTAT=NEEDS-ACTION:mailto:lennert@example.com\r\n" +
			"END:VEVENT\r\nEND:VCALENDAR\r\n"
		inv, err := Parse([]byte(ics), "lennert@example.com")
		if err != nil {
			t.Fatalf("Parse: %v", err)
		}
		if _, err := inv.Reply(selfAddr(), model.PartAccepted, replyNow); err == nil {
			t.Error("answered an invitation with nowhere to send the answer")
		}
	})

	t.Run("needs-action is not an answer", func(t *testing.T) {
		inv, err := FromMessage(loadFixture(t, "invite.eml"), "lennert@example.com")
		if err != nil {
			t.Fatalf("FromMessage: %v", err)
		}
		if _, err := inv.Reply(selfAddr(), model.PartNeedsAction, replyNow); err == nil {
			t.Error("needs-action was accepted as an answer")
		}
	})

	t.Run("no address to answer from", func(t *testing.T) {
		inv, err := FromMessage(loadFixture(t, "invite.eml"), "lennert@example.com")
		if err != nil {
			t.Fatalf("FromMessage: %v", err)
		}
		if _, err := inv.Reply(model.Address{}, model.PartAccepted, replyNow); err == nil {
			t.Error("answered from nowhere")
		}
	})
}

// The reply has to parse back as an iTIP object, or the organizer cannot read
// it either. Round-tripping it through the archive's own reader is the
// cheapest way to know the bytes are well formed.
func TestReplyParsesBackAsAReply(t *testing.T) {
	r := buildReply(t, model.PartTentative)
	back, err := Parse(r.Calendar, "lennert@example.com")
	if err != nil {
		t.Fatalf("the reply does not parse back: %v\n%s", err, r.Calendar)
	}
	if back.Method != MethodReply || back.Kind() != "reply" {
		t.Errorf("method = %q kind = %q", back.Method, back.Kind())
	}
	if back.Event.UID == "" {
		t.Error("the round trip lost the UID")
	}
	if len(back.Event.Attendees) != 1 || back.Event.Attendees[0].Response != model.PartTentative {
		t.Errorf("attendees = %+v", back.Event.Attendees)
	}
	if back.NeedsAnswer() {
		t.Error("a reply should not look like something to answer")
	}
}
