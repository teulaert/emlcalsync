package itip

import (
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/emersion/go-ical"

	"github.com/teulaert/emlcalsync/internal/model"
	"github.com/teulaert/emlcalsync/internal/provider/caldav"
)

// ErrNotAnswerable is returned when the message is not something an RSVP
// answers: a cancellation, a reply, a plain published event.
var ErrNotAnswerable = errors.New("itip: not an invitation, so there is nothing to answer")

// prodID identifies the writer of the calendar object, as RFC 5545 requires.
const prodID = "-//emlcal//iTIP//EN"

// Reply is the RSVP to an invitation: everything the message back to the
// organizer needs, ready for mime.Build.
//
// The archive answers an invitation through the calendar wherever it can --
// a CalDAV or JMAP server that holds the event turns a changed PARTSTAT into
// the REPLY itself, and doing it twice would tell the organizer twice. This
// is the other road, for an invitation no calendar has: the answer is mailed
// to the organizer directly, which is what RFC 6047 describes and what every
// Exchange and Google organizer has accepted since long before CalDAV
// scheduling existed.
type Reply struct {
	// To is the organizer. An RSVP goes to them and to nobody else: the other
	// attendees hear about it from the organizer, if at all.
	To      model.Address
	Subject string
	Text    string
	// Calendar is the iCalendar object, METHOD:REPLY.
	Calendar []byte
	// Response is what was answered, so a caller that records the answer does
	// not have to remember what it asked for.
	Response model.Participation
}

// Reply builds the RSVP to this invitation. self is the address the answer
// goes out as, which has to be the one on the ATTENDEE line the organizer
// wrote -- an answer from an address they did not invite is one their
// scheduler cannot match to an attendee and will drop.
//
// now is DTSTAMP: the moment the answer was given, which is how an organizer
// orders two answers from the same person.
func (i *Invite) Reply(self model.Address, resp model.Participation, now time.Time) (*Reply, error) {
	if i == nil {
		return nil, ErrNotAnswerable
	}
	if i.Method != MethodRequest {
		return nil, fmt.Errorf("%w: this is a %s", ErrNotAnswerable, strings.ToLower(i.Kind()))
	}
	partstat := caldav.PartStatString(resp)
	if partstat == "" || resp == model.PartNeedsAction {
		return nil, fmt.Errorf("itip: %q is not an answer", resp)
	}
	if i.Event.Organizer.Email == "" {
		return nil, errors.New("itip: the invitation names no organizer, so there is nowhere to send the answer")
	}
	if strings.TrimSpace(self.Email) == "" {
		return nil, errors.New("itip: no address to answer from")
	}
	if i.Event.UID == "" {
		return nil, errors.New("itip: the invitation has no UID, so an answer could not be matched to it")
	}

	// The organizer already wrote a name for this person; answering under
	// theirs rather than ours keeps their attendee list from growing a second
	// entry for the same address. It is also the only name there is, since an
	// emlcal account carries an address and no display name.
	name := replyName(self, i.Self())
	ics, err := i.replyICS(self, name, partstat, now)
	if err != nil {
		return nil, err
	}
	return &Reply{
		To:       i.Event.Organizer,
		Subject:  replySubject(resp, i.Event.Title),
		Text:     replyText(name, self.Email, resp, i.Event.Title),
		Calendar: ics,
		Response: resp,
	}, nil
}

// replyICS renders the VCALENDAR the organizer's scheduler reads.
//
// It is built rather than edited from the invitation, because a REPLY is a
// different object from the REQUEST it answers and carrying the difference by
// deletion is how one forgets. RFC 5546 §3.2.3 wants ATTENDEE, DTSTAMP,
// ORGANIZER and UID; the rest is there because organizers in the field use it.
// SEQUENCE and RECURRENCE-ID say *which* invitation is being answered, and
// both come off the original bytes -- see Invite.Raw. DTSTART, DTEND and
// SUMMARY are what a human reads in a client that shows the reply as a card.
//
// The times go out in UTC. The invitation states them against a VTIMEZONE it
// carries, and copying a time without the zone that defines it would move the
// meeting; carrying the VTIMEZONE across means reproducing a component nobody
// here needs, since the event model has already resolved both ends to
// instants. UTC says the same instant with nothing to get wrong.
func (i *Invite) replyICS(self model.Address, name, partstat string, now time.Time) ([]byte, error) {
	cal := ical.NewCalendar()
	cal.Props.SetText(ical.PropVersion, "2.0")
	cal.Props.SetText(ical.PropProductID, prodID)
	cal.Props.SetText(ical.PropMethod, MethodReply)

	ev := ical.NewEvent()
	ev.Props.SetText(ical.PropUID, i.Event.UID)
	setUTC(ev.Props, ical.PropDateTimeStamp, now)
	if !i.Event.Start.IsZero() {
		setUTC(ev.Props, ical.PropDateTimeStart, i.Event.Start)
	}
	if !i.Event.End.IsZero() {
		setUTC(ev.Props, ical.PropDateTimeEnd, i.Event.End)
	}
	if i.Event.Title != "" {
		ev.Props.SetText(ical.PropSummary, i.Event.Title)
	}
	// SEQUENCE defaults to 0 when the invitation left it out, which is what
	// RFC 5545 §3.8.7.4 says it means -- so it is always written, rather than
	// left for the organizer to guess at.
	seq, recurrenceID := i.sequenceAndRecurrence()
	p := ical.NewProp(ical.PropSequence)
	p.Value = seq
	ev.Props.Set(p)
	if recurrenceID != nil {
		ev.Props.Set(recurrenceID)
	}

	org := ical.NewProp(ical.PropOrganizer)
	org.Value = "mailto:" + i.Event.Organizer.Email
	if i.Event.Organizer.Name != "" {
		org.Params.Set(ical.ParamCommonName, i.Event.Organizer.Name)
	}
	ev.Props.Set(org)

	// One attendee: the one answering. A REPLY that repeated the whole list
	// would be claiming to answer for all of them.
	att := ical.NewProp(ical.PropAttendee)
	att.Value = "mailto:" + self.Email
	if name != "" {
		att.Params.Set(ical.ParamCommonName, name)
	}
	att.Params.Set(ical.ParamParticipationStatus, partstat)
	ev.Props.Set(att)

	cal.Children = append(cal.Children, ev.Component)

	var sb strings.Builder
	if err := ical.NewEncoder(&sb).Encode(cal); err != nil {
		return nil, fmt.Errorf("itip: encode the reply: %w", err)
	}
	return []byte(sb.String()), nil
}

// sequenceAndRecurrence reads back the two properties that say which
// invitation is being answered. The event model carries neither: SEQUENCE is
// the organizer's revision count, and RECURRENCE-ID is set when the
// invitation is for one moved occurrence rather than the series.
//
// A payload that cannot be decoded is not an error here. The invitation was
// parsed once already to get this far, so the bytes are readable; if they
// have become unreadable since, an answer at sequence 0 against the series is
// still a better answer than none.
func (i *Invite) sequenceAndRecurrence() (seq string, recurrenceID *ical.Prop) {
	seq = "0"
	if len(i.Raw) == 0 {
		return seq, nil
	}
	cal, err := ical.NewDecoder(strings.NewReader(string(i.Raw))).Decode()
	if err != nil {
		return seq, nil
	}
	for _, child := range cal.Children {
		if !strings.EqualFold(child.Name, ical.CompEvent) {
			continue
		}
		// The master is the VEVENT without a RECURRENCE-ID; an invitation to
		// one occurrence has exactly one VEVENT, and it carries one.
		if p := child.Props.Get(ical.PropSequence); p != nil {
			if v := strings.TrimSpace(p.Value); v != "" {
				seq = v
			}
		}
		if p := child.Props.Get(ical.PropRecurrenceID); p != nil {
			c := *p
			recurrenceID = &c
		}
		return seq, recurrenceID
	}
	return seq, nil
}

// replyName is the name to answer under: the one the organizer wrote on the
// attendee line, falling back to whatever the sending address calls itself.
func replyName(self model.Address, invited *model.Attendee) string {
	if invited != nil && invited.Name != "" {
		return invited.Name
	}
	return self.Name
}

func replySubject(resp model.Participation, title string) string {
	if title == "" {
		title = "(no subject)"
	}
	return replyWord(resp) + ": " + title
}

// replyWord is the one that goes in front of the subject. Outlook, Google and
// Fastmail all write these, and an organizer reading their mail sorts answers
// by them, so they are in English and capitalised the way those three do it
// rather than translated or restyled.
func replyWord(resp model.Participation) string {
	switch resp {
	case model.PartAccepted:
		return "Accepted"
	case model.PartDeclined:
		return "Declined"
	case model.PartTentative:
		return "Tentative"
	}
	return "Answered"
}

// replyText is the human half. A scheduler reads the calendar part and throws
// the text away, but a person whose client does not understand invitations
// gets this and nothing else, so it says who answered and what they said.
func replyText(name, email string, resp model.Participation, title string) string {
	who := name
	if who == "" {
		who = email
	}
	var said string
	switch resp {
	case model.PartAccepted:
		said = "has accepted"
	case model.PartDeclined:
		said = "has declined"
	case model.PartTentative:
		said = "has tentatively accepted"
	default:
		said = "has answered"
	}
	if title == "" {
		return fmt.Sprintf("%s %s this invitation.", who, said)
	}
	return fmt.Sprintf("%s %s the invitation %q.", who, said, title)
}

func setUTC(props ical.Props, name string, t time.Time) {
	p := ical.NewProp(name)
	p.Value = t.UTC().Format("20060102T150405Z")
	props.Set(p)
}
