package itip

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/teulaert/emlcalsync/internal/model"
)

func loadFixture(t *testing.T, name string) []byte {
	t.Helper()
	raw, err := os.ReadFile(filepath.Join("..", "mime", "testdata", name))
	if err != nil {
		t.Fatalf("read %s: %v", name, err)
	}
	return raw
}

func TestFromMessageExchangeInvite(t *testing.T) {
	inv, err := FromMessage(loadFixture(t, "invite.eml"), "lennert@example.com")
	if err != nil {
		t.Fatalf("FromMessage: %v", err)
	}
	if inv.Method != MethodRequest || inv.Kind() != "invitation" {
		t.Errorf("method = %q kind = %q", inv.Method, inv.Kind())
	}
	ev := inv.Event
	if ev.Title != "Momentum FO" {
		t.Errorf("title = %q", ev.Title)
	}
	if ev.UID != "040000008200E00074C5B7101A82E00800000000BB3DDF993738DD01000000000000000010000000D9B5581854DF3640B533A07A2B4B5089" {
		t.Errorf("uid = %q", ev.UID)
	}
	// "W. Europe Standard Time" is Exchange's name for Europe/Berlin: 10:00
	// there is 08:00Z on a September day, not 10:00Z.
	if want := time.Date(2026, 9, 2, 8, 0, 0, 0, time.UTC); !ev.Start.Equal(want) {
		t.Errorf("start = %s, want %s", ev.Start, want)
	}
	if want := time.Date(2026, 9, 2, 8, 45, 0, 0, time.UTC); !ev.End.Equal(want) {
		t.Errorf("end = %s, want %s", ev.End, want)
	}
	if ev.Timezone != "Europe/Berlin" {
		t.Errorf("timezone = %q", ev.Timezone)
	}
	if ev.Organizer.Email != "martijn@example.org" || ev.Organizer.Name != "Martijn Organiser" {
		t.Errorf("organizer = %+v", ev.Organizer)
	}
	if ev.MyResponse != model.PartNeedsAction || !inv.NeedsAnswer() {
		t.Errorf("my response = %q, needs answer = %v", ev.MyResponse, inv.NeedsAnswer())
	}
	if self := inv.Self(); self == nil || self.Email != "lennert@example.com" {
		t.Errorf("self = %+v", self)
	}
	if ev.Location != "Microsoft Teams-vergadering" {
		t.Errorf("location = %q", ev.Location)
	}

	loc, _ := time.LoadLocation("Europe/Amsterdam")
	var got []string
	for _, f := range inv.Fields(loc) {
		got = append(got, f.Key+"="+f.Value)
	}
	joined := strings.Join(got, "\n")
	for _, want := range []string{
		"Invitation=Momentum FO",
		"When=Wed 2 Sep 10:00–10:45",
		"Where=Microsoft Teams-vergadering",
		"Organizer=Martijn Organiser <martijn@example.org>",
		"You=not answered",
	} {
		if !strings.Contains(joined, want) {
			t.Errorf("fields miss %q:\n%s", want, joined)
		}
	}
	if strings.Contains(joined, "Attendees=") {
		t.Errorf("the recipient alone is not an attendee list:\n%s", joined)
	}
}

func TestFromMessageWithoutCalendarPart(t *testing.T) {
	_, err := FromMessage(loadFixture(t, "html_only.eml"), "")
	if err != ErrNoInvite {
		t.Fatalf("err = %v, want ErrNoInvite", err)
	}
}

const cancelICS = `BEGIN:VCALENDAR
METHOD:CANCEL
VERSION:2.0
PRODID:test
BEGIN:VEVENT
UID:abc-1
SUMMARY:Standup
DTSTART:20260903T090000Z
DTEND:20260903T091500Z
ORGANIZER;CN=Alice:mailto:alice@example.org
ATTENDEE;PARTSTAT=ACCEPTED:mailto:me@example.com
STATUS:CANCELLED
SEQUENCE:2
END:VEVENT
END:VCALENDAR
`

func TestParseCancel(t *testing.T) {
	inv, err := Parse([]byte(cancelICS), "me@example.com")
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}
	if inv.Kind() != "cancellation" || inv.NeedsAnswer() {
		t.Errorf("kind = %q needs answer = %v", inv.Kind(), inv.NeedsAnswer())
	}
	f := inv.Fields(time.UTC)
	if len(f) == 0 || f[0].Key != "Cancelled" || f[0].Value != "Standup" {
		t.Errorf("first field = %+v", f)
	}
	for _, x := range f {
		if x.Key == "You" {
			t.Errorf("a cancellation asks nothing: %+v", f)
		}
	}
}

const replyICS = `BEGIN:VCALENDAR
METHOD:REPLY
VERSION:2.0
PRODID:test
BEGIN:VEVENT
UID:abc-1
SUMMARY:Standup
DTSTART:20260903T090000Z
DTEND:20260903T091500Z
ORGANIZER:mailto:me@example.com
ATTENDEE;CN=Bob;PARTSTAT=DECLINED:mailto:bob@example.org
END:VEVENT
END:VCALENDAR
`

func TestParseReply(t *testing.T) {
	inv, err := Parse([]byte(replyICS), "me@example.com")
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}
	if inv.Kind() != "reply" {
		t.Errorf("kind = %q", inv.Kind())
	}
	var from string
	for _, x := range inv.Fields(time.UTC) {
		if x.Key == "From" {
			from = x.Value
		}
	}
	if from != "Bob: no" {
		t.Errorf("From = %q", from)
	}
}

func TestMethodFallsBackToPartParameter(t *testing.T) {
	raw := "From: a@example.org\r\nTo: me@example.com\r\nSubject: x\r\n" +
		"Content-Type: text/calendar; method=REQUEST\r\n\r\n" +
		"BEGIN:VCALENDAR\r\nVERSION:2.0\r\nPRODID:t\r\nBEGIN:VEVENT\r\nUID:u1\r\nSUMMARY:No method line\r\n" +
		"DTSTART:20260903T090000Z\r\nEND:VEVENT\r\nEND:VCALENDAR\r\n"
	inv, err := FromMessage([]byte(raw), "me@example.com")
	if err != nil {
		t.Fatalf("FromMessage: %v", err)
	}
	if inv.Method != MethodRequest {
		t.Errorf("method = %q", inv.Method)
	}
}
