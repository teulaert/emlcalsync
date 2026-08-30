package caldav

import (
	"log/slog"
	"strings"

	"github.com/emersion/go-ical"

	"github.com/teulaert/emlcalsync/internal/model"
)

// ParseICS reads a free-standing iCalendar object -- the payload of an
// invitation mailed as text/calendar, in practice -- into the event model,
// the same mapping a CalDAV object gets. It returns the object's METHOD
// (upper-cased, "" when there is none) and its events: the master first,
// then any RECURRENCE-ID instances, then a cancelled instance per EXDATE.
//
// The events belong to no calendar: CalendarRemote and RemoteID are empty,
// and RawJSON holds the text as a CalDAV object would. selfEmail marks the
// recipient's own ATTENDEE line, which is where MyResponse comes from.
func ParseICS(ics, selfEmail string, log *slog.Logger) (method string, events []model.Event, err error) {
	if log == nil {
		log = slog.New(slog.DiscardHandler)
	}
	cal, err := decodeICS(ics)
	if err != nil {
		return "", nil, err
	}
	if p := cal.Props.Get(ical.PropMethod); p != nil {
		method = strings.ToUpper(strings.TrimSpace(p.Value))
	}
	events, err = parseObject("", "", "", ics, selfEmail, log)
	return method, events, err
}
