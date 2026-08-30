// Package itip reads the calendar payload a message carries: the invitation,
// update, cancellation or RSVP that RFC 6047 mails as a text/calendar part
// and that a mail client shows as a card above the text. The archive keeps
// the raw message, so the card is built on demand from the bytes rather than
// indexed; what the index records is only that the part is there (see
// mime.Parsed.Calendar).
package itip

import (
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/teulaert/emlcalsync/internal/calendar"
	"github.com/teulaert/emlcalsync/internal/mime"
	"github.com/teulaert/emlcalsync/internal/model"
	"github.com/teulaert/emlcalsync/internal/output"
	"github.com/teulaert/emlcalsync/internal/provider/caldav"
)

// ErrNoInvite is returned when a message carries no calendar part.
var ErrNoInvite = errors.New("itip: no calendar part")

// The iTIP methods that mean something to a recipient.
const (
	MethodRequest = "REQUEST" // an invitation, or an update to one
	MethodCancel  = "CANCEL"
	MethodReply   = "REPLY"
	MethodPublish = "PUBLISH" // an event shared with no RSVP expected
)

// Invite is one iTIP message read out of a mail.
type Invite struct {
	// Method is REQUEST, CANCEL, REPLY, PUBLISH or whatever else the sender
	// wrote, upper-cased. "" when neither the payload nor the part said.
	Method string
	// Event is the master VEVENT in the event model. It belongs to no
	// calendar -- CalendarRemote and RemoteID are empty -- and MyResponse is
	// the recipient's own PARTSTAT as the sender wrote it, which for a fresh
	// invitation is needs-action.
	Event model.Event
}

// Parse reads an iCalendar object. selfEmail marks the recipient's own
// ATTENDEE line; "" leaves MyResponse empty.
func Parse(ics []byte, selfEmail string) (*Invite, error) {
	method, events, err := caldav.ParseICS(string(ics), selfEmail, nil)
	if err != nil {
		return nil, err
	}
	if len(events) == 0 {
		return nil, fmt.Errorf("itip: no VEVENT in the calendar part")
	}
	return &Invite{Method: method, Event: events[0]}, nil
}

// FromMessage reads the invitation out of a raw RFC 822 message, or returns
// ErrNoInvite when there is none. The part's Content-Type method parameter
// stands in for a METHOD line the payload forgot.
func FromMessage(raw []byte, selfEmail string) (*Invite, error) {
	parsed, err := mime.Parse(raw)
	if err != nil {
		return nil, err
	}
	if parsed.Calendar == nil {
		return nil, ErrNoInvite
	}
	data, _, _, err := mime.PartContent(raw, parsed.Calendar.Path)
	if err != nil {
		return nil, err
	}
	inv, err := Parse(data, selfEmail)
	if err != nil {
		return nil, err
	}
	if inv.Method == "" {
		inv.Method = parsed.Calendar.Method
	}
	return inv, nil
}

// Kind names what the message is, for a heading: "invitation",
// "cancellation", "reply" or "event" (a PUBLISH, or a payload with no method).
func (i *Invite) Kind() string {
	switch i.Method {
	case MethodRequest:
		return "invitation"
	case MethodCancel:
		return "cancellation"
	case MethodReply:
		return "reply"
	default:
		return "event"
	}
}

// NeedsAnswer reports whether the message is asking the recipient for an
// RSVP: an invitation they have not answered, or one whose answer the
// organizer has since re-requested.
func (i *Invite) NeedsAnswer() bool {
	if i.Method != MethodRequest {
		return false
	}
	return i.Event.MyResponse == "" || i.Event.MyResponse == model.PartNeedsAction
}

// Self is the recipient's own attendee line, or nil.
func (i *Invite) Self() *model.Attendee {
	for k := range i.Event.Attendees {
		if i.Event.Attendees[k].Self {
			return &i.Event.Attendees[k]
		}
	}
	return nil
}

// Field is one line of the card.
type Field struct{ Key, Value string }

// Fields is the card: what a person needs to answer the invitation, in the
// order they need it. It leaves out what the mail's own headers already say.
// loc is the zone the times are shown in.
func (i *Invite) Fields(loc *time.Location) []Field {
	ev := &i.Event
	if loc == nil {
		loc = time.Local
	}
	var out []Field
	add := func(k, v string) {
		if strings.TrimSpace(v) != "" {
			out = append(out, Field{k, v})
		}
	}
	switch i.Method {
	case MethodCancel:
		add("Cancelled", ev.Title)
	case MethodReply:
		// A REPLY carries the one attendee who answered.
		who := ""
		for _, a := range ev.Attendees {
			s := a.Email
			if a.Name != "" {
				s = a.Name
			}
			if r := output.RSVP(a.Response); r != "" {
				s += ": " + r
			}
			who = s
			break
		}
		add("Reply", ev.Title)
		add("From", who)
	default:
		add(strings.ToUpper(i.Kind()[:1])+i.Kind()[1:], ev.Title)
	}
	if !ev.Start.IsZero() {
		add("When", calendar.FormatRange(ev.Start, ev.End, ev.AllDay, loc))
	}
	if ev.RRule != "" {
		add("Repeats", ev.RRule)
	}
	add("Where", ev.Location)
	if ev.Organizer.Email != "" || ev.Organizer.Name != "" {
		add("Organizer", ev.Organizer.String())
	}
	if i.Method != MethodReply {
		var others []string
		for _, a := range ev.Attendees {
			if a.Self {
				continue
			}
			s := a.Email
			if r := output.RSVP(a.Response); r != "" && r != "?" {
				s += " (" + r + ")"
			}
			others = append(others, s)
		}
		add("Attendees", strings.Join(others, ", "))
		if i.Method == MethodRequest {
			switch r := output.RSVP(ev.MyResponse); r {
			case "", "?":
				add("You", "not answered")
			default:
				add("You", r)
			}
		}
	}
	if ev.Status == model.StatusCancelled && i.Method != MethodCancel {
		add("Status", "cancelled")
	}
	return out
}

// IsCalendarAttachment reports whether an indexed attachment is the calendar
// part of a message -- the sign, without opening the raw bytes, that the
// message carries an invitation.
func IsCalendarAttachment(a model.Attachment) bool {
	switch strings.ToLower(a.ContentType) {
	case "text/calendar", "application/ics":
		return true
	}
	return strings.HasSuffix(strings.ToLower(a.Filename), ".ics")
}

// Match picks, out of the calendar's copies of an invited event (see
// store.FindEventsByUID), the one an RSVP should go to: the copy on the
// account the mail arrived at when there is one, else the first. A person
// with Gmail and Fastmail on the same invitation has it filed twice; the
// organizer hears from whichever calendar answers, and the one the mail
// came to is the one they invited.
func Match(events []model.Event, account string) *model.Event {
	for k := range events {
		if events[k].AccountID == account {
			return &events[k]
		}
	}
	if len(events) > 0 {
		return &events[0]
	}
	return nil
}
