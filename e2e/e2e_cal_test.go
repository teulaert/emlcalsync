package e2e

import (
	"strings"
	"testing"
	"time"

	"github.com/lennert/emlcal/internal/testutil/jmapfake"
)

// calSeed is the calendar fixture: one weekly event repeated three times and
// one single event the account is invited to.
type calSeed struct {
	RecurringID string
	SingleID    string
	// Start of the first occurrence of the recurring event (UTC; the child
	// process runs with TZ=UTC).
	FirstStart time.Time
	// Start of the single event.
	SingleStart time.Time
}

func seedCalendar(t *testing.T, f *jmapfake.Server) calSeed {
	t.Helper()
	now := time.Now().UTC()
	midnight := time.Date(now.Year(), now.Month(), now.Day(), 0, 0, 0, 0, time.UTC)
	first := midnight.AddDate(0, 0, 1).Add(10 * time.Hour)  // tomorrow 10:00
	single := midnight.AddDate(0, 0, 3).Add(14 * time.Hour) // in three days, 14:00

	const local = "2006-01-02T15:04:05"
	utc := "UTC"

	recurring := f.AddEvent(jmapfake.CalendarDefault, map[string]any{
		"title":       "Weekly standup",
		"description": "Fifteen minutes, standing up.",
		"start":       first.Format(local),
		"duration":    "PT30M",
		"timeZone":    utc,
		"status":      "confirmed",
		"recurrenceRules": []any{map[string]any{
			"@type": "RecurrenceRule", "frequency": "weekly", "count": 3,
		}},
		"locations": map[string]any{
			"loc1": map[string]any{"@type": "Location", "name": "Big room"},
		},
	})

	singleID := f.AddEvent(jmapfake.CalendarDefault, map[string]any{
		"title":    "Design review",
		"start":    single.Format(local),
		"duration": "PT1H",
		"timeZone": utc,
		"status":   "confirmed",
		"replyTo":  map[string]any{"imip": "mailto:alice@example.com"},
		"participants": map[string]any{
			"alice": map[string]any{
				"@type": "Participant", "name": "Alice", "email": "alice@example.com",
				"roles":               map[string]any{"owner": true, "attendee": true},
				"participationStatus": "accepted",
			},
			"me": map[string]any{
				"@type": "Participant", "name": "Me", "email": "me@example.com",
				"roles":               map[string]any{"attendee": true},
				"participationStatus": "needs-action",
				"expectReply":         true,
			},
		},
	})

	return calSeed{RecurringID: recurring, SingleID: singleID, FirstStart: first, SingleStart: single}
}

func eventPublicID(remote string) string { return "work:c:" + jmapfake.CalendarDefault + ":" + remote }

func TestCalendarSyncAgendaAndShow(t *testing.T) {
	e := newEnv(t)
	cs := seedCalendar(t, e.fake)
	e.addAccount("work", "me@example.com")

	out := e.mustRun("sync")
	rows := decodeArray(t, out)
	calRow := findRow(t, rows, "resource", "calendar")
	if got := num(t, calRow, "added"); got != 2 {
		t.Errorf("calendar sync added = %v, want 2\n%s", got, out)
	}

	cals := decodeArray(t, e.mustRun("cal", "calendars"))
	if len(cals) != 1 {
		t.Fatalf("cal calendars = %d rows, want 1: %v", len(cals), cals)
	}
	if got := str(t, cals[0], "id"); got != "work:c:"+jmapfake.CalendarDefault {
		t.Errorf("calendar id = %q", got)
	}
	if got := str(t, cals[0], "name"); got != "Personal" {
		t.Errorf("calendar name = %q, want Personal", got)
	}
	if !boolean(t, cals[0], "primary") {
		t.Errorf("calendar is not marked primary: %v", cals[0])
	}

	// The window has to reach past the third weekly occurrence, which is 15
	// days out: today+0 .. today+21 covers all three plus the single event.
	agenda := decodeArray(t, e.mustRun("cal", "agenda", "--days", "21"))
	if len(agenda) != 4 {
		t.Fatalf("cal agenda --days 21 = %d rows, want 4 (3 occurrences + 1 event):\n%v",
			len(agenda), agenda)
	}
	var standups, reviews int
	for _, r := range agenda {
		switch str(t, r, "title") {
		case "Weekly standup":
			standups++
			if !boolean(t, r, "recurring") {
				t.Errorf("occurrence not marked recurring: %v", r)
			}
		case "Design review":
			reviews++
		default:
			t.Errorf("unexpected agenda row: %v", r)
		}
	}
	if standups != 3 || reviews != 1 {
		t.Errorf("agenda has %d standups and %d reviews, want 3 and 1", standups, reviews)
	}
	// Occurrences come out in time order.
	for i := 1; i < len(agenda); i++ {
		if num(t, agenda[i-1], "start_utc") > num(t, agenda[i], "start_utc") {
			t.Errorf("agenda is not sorted by start: %v", agenda)
			break
		}
	}
	// A shorter window drops the third occurrence.
	short := decodeArray(t, e.mustRun("cal", "agenda", "--days", "10"))
	if len(short) != 3 {
		t.Errorf("cal agenda --days 10 = %d rows, want 3: %v", len(short), short)
	}

	show := decodeObject(t, e.mustRun("cal", "show", eventPublicID(cs.RecurringID)))
	if got := str(t, show, "title"); got != "Weekly standup" {
		t.Errorf("cal show title = %q", got)
	}
	if got := str(t, show, "location"); got != "Big room" {
		t.Errorf("cal show location = %q, want Big room", got)
	}
	if got := show["rrule"]; got == nil || !strings.Contains(strings.ToUpper(got.(string)), "WEEKLY") {
		t.Errorf("cal show rrule = %v, want a WEEKLY rule", got)
	}
	if got := int64(num(t, show, "start_utc")); got != cs.FirstStart.Unix() {
		t.Errorf("cal show start_utc = %d, want %d", got, cs.FirstStart.Unix())
	}

	invite := decodeObject(t, e.mustRun("cal", "show", eventPublicID(cs.SingleID)))
	if got := str(t, invite, "my_response"); got != "needs-action" {
		t.Errorf("my_response = %q, want needs-action", got)
	}
	atts, _ := invite["attendees"].([]any)
	if len(atts) != 2 {
		t.Errorf("attendees = %v, want 2", invite["attendees"])
	}
}

func TestCalendarFree(t *testing.T) {
	e := newEnv(t)
	cs := seedCalendar(t, e.fake)
	e.addAccount("work", "me@example.com")
	e.mustRun("sync")

	day := cs.FirstStart.Format("2006-01-02")
	next := cs.FirstStart.AddDate(0, 0, 1).Format("2006-01-02")
	rows := decodeArray(t, e.mustRun("cal", "free", "--from", day, "--to", next, "--duration", "30m"))
	if len(rows) == 0 {
		t.Fatalf("cal free found no slots on a day with one 30-minute meeting")
	}
	// No reported slot may overlap the standup.
	busyStart := cs.FirstStart.Unix()
	busyEnd := busyStart + 30*60
	for _, r := range rows {
		s, en := int64(num(t, r, "start_utc")), int64(num(t, r, "end_utc"))
		if s < busyEnd && en > busyStart {
			t.Errorf("free slot %d–%d overlaps the meeting %d–%d", s, en, busyStart, busyEnd)
		}
		if en <= s {
			t.Errorf("free slot is not a forward range: %v", r)
		}
	}

	// --hours narrows the day.
	worky := decodeArray(t, e.mustRun("cal", "free", "--from", day, "--to", next,
		"--duration", "30m", "--hours", "09:00-18:00"))
	for _, r := range worky {
		s := time.Unix(int64(num(t, r, "start_utc")), 0).UTC()
		if s.Hour() < 9 || s.Hour() >= 18 {
			t.Errorf("--hours 09:00-18:00 returned a slot starting at %s", s)
		}
	}
	if _, _, code := e.run("cal", "free"); code != 2 {
		t.Errorf("cal free without --from/--to: want exit 2")
	}
}

func TestCalendarCreateAndRespond(t *testing.T) {
	e := newEnv(t)
	cs := seedCalendar(t, e.fake)
	e.addAccount("work", "me@example.com")
	e.mustRun("sync")

	start := cs.SingleStart.AddDate(0, 0, 1)
	end := start.Add(90 * time.Minute)
	const layout = "2006-01-02 15:04"

	dry := decodeObject(t, e.mustRun("cal", "create", "--title", "Retro",
		"--start", start.Format(layout), "--end", end.Format(layout), "--dry-run"))
	if got := str(t, dry, "title"); got != "Retro" {
		t.Errorf("dry run title = %q", got)
	}
	if n := len(e.fake.Events()); n != 2 {
		t.Fatalf("--dry-run created an event on the server (%d events, want 2)", n)
	}

	out := decodeObject(t, e.mustRun("cal", "create", "--title", "Retro",
		"--start", start.Format(layout), "--end", end.Format(layout),
		"--location", "Cafeteria", "--description", "What went well"))
	if boolean(t, out, "queued") {
		t.Errorf("cal create was queued while the server was up: %v", out)
	}
	newID := str(t, out, "id")
	if !strings.HasPrefix(newID, "work:c:"+jmapfake.CalendarDefault+":") {
		t.Errorf("created event id = %q", newID)
	}

	events := e.fake.Events()
	if len(events) != 3 {
		t.Fatalf("fake has %d events after create, want 3", len(events))
	}
	var created map[string]any
	for _, ev := range events {
		if ev["title"] == "Retro" {
			created = ev
		}
	}
	if created == nil {
		t.Fatalf("no event titled Retro reached the server: %v", events)
	}
	if got := created["start"]; got != start.Format("2006-01-02T15:04:05") {
		t.Errorf("server-side start = %v, want %s", got, start.Format("2006-01-02T15:04:05"))
	}
	if got := created["duration"]; got != "PT1H30M" {
		t.Errorf("server-side duration = %v, want PT1H30M", got)
	}

	// It shows up in the agenda without another sync: the write is applied to
	// the index optimistically.
	agenda := decodeArray(t, e.mustRun("cal", "agenda", "--days", "21"))
	if len(agenda) != 5 {
		t.Errorf("agenda after create = %d rows, want 5: %v", len(agenda), agenda)
	}

	// RSVP to the invitation.
	resp := decodeObject(t, e.mustRun("cal", "respond", eventPublicID(cs.SingleID), "--accept"))
	if got := str(t, resp, "response"); got != "accepted" {
		t.Errorf("respond response = %q, want accepted", got)
	}
	if boolean(t, resp, "queued") {
		t.Errorf("respond was queued while the server was up")
	}
	ev := e.fake.Events()[cs.SingleID]
	parts, _ := ev["participants"].(map[string]any)
	me, _ := parts["me"].(map[string]any)
	if me == nil || me["participationStatus"] != "accepted" {
		t.Errorf("server-side participationStatus = %v, want accepted", me)
	}

	// And the index agrees after a sync.
	e.mustRun("sync")
	shown := decodeObject(t, e.mustRun("cal", "show", eventPublicID(cs.SingleID)))
	if got := str(t, shown, "my_response"); got != "accepted" {
		t.Errorf("my_response after RSVP = %q, want accepted", got)
	}

	// Deleting removes it from the server.
	del := decodeObject(t, e.mustRun("cal", "delete", newID))
	if !boolean(t, del, "deleted") {
		t.Errorf("cal delete: %v", del)
	}
	if n := len(e.fake.Events()); n != 2 {
		t.Errorf("fake has %d events after delete, want 2", n)
	}
}
