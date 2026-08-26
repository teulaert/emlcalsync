package cli

import (
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/teulaert/emlcalsync/internal/config"
	"github.com/teulaert/emlcalsync/internal/model"
)

// calAgendaJSON is the shape `cal agenda` emits, for decoding in tests.
type calAgendaJSON struct {
	ID         string `json:"id"`
	Start      string `json:"start"`
	StartUTC   int64  `json:"start_utc"`
	End        string `json:"end"`
	EndUTC     int64  `json:"end_utc"`
	AllDay     bool   `json:"all_day"`
	Title      string `json:"title"`
	Calendar   string `json:"calendar"`
	Location   string `json:"location"`
	Status     string `json:"status"`
	MyResponse string `json:"my_response"`
	Recurring  bool   `json:"recurring"`
	Account    string `json:"account"`
}

const (
	calStandupID = "work:c:primary:e1"
	calOffsiteID = "work:c:primary:e2"
)

// calSeed puts a weekly standup (Wed 26 Aug 09:00–09:30 UTC, four times) and a
// one-day all-day event (Thu 27 Aug) on the work account's primary calendar.
func calSeed(t *testing.T) *testEnv {
	t.Helper()
	// Calendars must be selected explicitly: an account with no `calendars`
	// key never syncs its calendars (sync/engine.go).
	env := newTestEnv(t,
		config.NewAccount("work", "me@example.com", model.VendorFastmail),
		config.NewAccount("home", "me@gmail.example", model.VendorGoogle))
	env.Cal["work"].Put("primary", model.Event{
		RemoteID: "e1",
		UID:      "u1",
		Title:    "Standup",
		Location: "Room 2",
		Start:    time.Date(2026, 8, 26, 9, 0, 0, 0, time.UTC),
		End:      time.Date(2026, 8, 26, 9, 30, 0, 0, time.UTC),
		Timezone: "UTC",
		Status:   model.StatusConfirmed,
		RRule:    "FREQ=WEEKLY;COUNT=4",
		Attendees: []model.Attendee{
			{Email: "me@example.com", Response: model.PartNeedsAction, Self: true},
			{Email: "alice@example.com", Response: model.PartAccepted},
		},
	})
	env.Cal["work"].Put("primary", model.Event{
		RemoteID: "e2",
		UID:      "u2",
		Title:    "Company offsite",
		Start:    time.Date(2026, 8, 27, 0, 0, 0, 0, time.UTC),
		End:      time.Date(2026, 8, 28, 0, 0, 0, 0, time.UTC),
		AllDay:   true,
		Timezone: "UTC",
		Status:   model.StatusConfirmed,
	})
	env.Sync("work")
	return env
}

func calDecodeAgenda(t *testing.T, s string) []calAgendaJSON {
	t.Helper()
	var rows []calAgendaJSON
	if err := json.Unmarshal([]byte(s), &rows); err != nil {
		t.Fatalf("decode agenda: %v\n%s", err, s)
	}
	return rows
}

func TestCalCalendars(t *testing.T) {
	env := calSeed(t)
	out := env.MustRun("cal", "calendars", "-a", "work")
	var rows []struct {
		ID       string `json:"id"`
		Account  string `json:"account"`
		Name     string `json:"name"`
		Primary  bool   `json:"primary"`
		Timezone string `json:"timezone"`
	}
	if err := json.Unmarshal([]byte(out), &rows); err != nil {
		t.Fatalf("decode: %v\n%s", err, out)
	}
	if len(rows) != 1 {
		t.Fatalf("want one calendar, got %d: %s", len(rows), out)
	}
	if rows[0].ID != "work:c:primary" || rows[0].Account != "work" || rows[0].Name != "Primary" {
		t.Fatalf("calendar row = %+v", rows[0])
	}
	if !rows[0].Primary {
		t.Fatalf("primary flag missing: %s", out)
	}
}

func TestCalAgendaJSON(t *testing.T) {
	env := calSeed(t)
	rows := calDecodeAgenda(t, env.MustRun("cal", "agenda", "--days", "7"))
	if len(rows) != 2 {
		t.Fatalf("want 2 occurrences in the default week, got %d: %+v", len(rows), rows)
	}
	if rows[0].StartUTC > rows[1].StartUTC {
		t.Fatalf("rows are not ordered by start: %+v", rows)
	}
	standup := rows[0]
	if standup.ID != calStandupID || standup.Title != "Standup" {
		t.Fatalf("first row = %+v", standup)
	}
	if got := time.Unix(standup.StartUTC, 0).UTC(); !got.Equal(time.Date(2026, 8, 26, 9, 0, 0, 0, time.UTC)) {
		t.Fatalf("standup start = %s", got)
	}
	if !standup.Recurring || standup.AllDay || standup.Calendar != "Primary" || standup.Account != "work" {
		t.Fatalf("standup row = %+v", standup)
	}
	if standup.Location != "Room 2" {
		t.Fatalf("standup location = %q", standup.Location)
	}
	offsite := rows[1]
	if offsite.ID != calOffsiteID || !offsite.AllDay {
		t.Fatalf("all-day row = %+v", offsite)
	}

	// The second instance of the weekly series is a week later.
	rows = calDecodeAgenda(t, env.MustRun("cal", "agenda", "--days", "14"))
	var second bool
	for _, r := range rows {
		if r.ID == calStandupID && time.Unix(r.StartUTC, 0).UTC().Equal(time.Date(2026, 9, 2, 9, 0, 0, 0, time.UTC)) {
			second = true
		}
	}
	if !second {
		t.Fatalf("no standup on 2 Sep in a fortnight: %+v", rows)
	}
}

func TestCalAgendaWindowFlags(t *testing.T) {
	env := calSeed(t)
	rows := calDecodeAgenda(t, env.MustRun("cal", "agenda", "--from", "2026-08-27", "--to", "2026-08-28"))
	if len(rows) != 1 || rows[0].ID != calOffsiteID {
		t.Fatalf("--from/--to window = %+v", rows)
	}
	// An unknown calendar is exit 3, not an empty list.
	_, _, code := env.Run("cal", "agenda", "--calendar", "nope")
	if code != 3 {
		t.Fatalf("unknown calendar exit = %d", code)
	}
	// A known one filters without dropping anything.
	rows = calDecodeAgenda(t, env.MustRun("cal", "agenda", "--calendar", "Primary", "--days", "7"))
	if len(rows) != 2 {
		t.Fatalf("--calendar Primary = %+v", rows)
	}
}

func TestCalAgendaTableGroupsByDay(t *testing.T) {
	env := calSeed(t)
	out := env.MustRun("cal", "agenda", "--days", "7", "-o", "table")
	wed := "Wed 26 Aug 2026"
	thu := "Thu 27 Aug 2026"
	if !strings.Contains(out, wed) || !strings.Contains(out, thu) {
		t.Fatalf("day headers missing:\n%s", out)
	}
	if strings.Index(out, wed) > strings.Index(out, thu) {
		t.Fatalf("days out of order:\n%s", out)
	}
	if !strings.Contains(out, "09:00–09:30") {
		t.Fatalf("time cell missing:\n%s", out)
	}
	if !strings.Contains(out, "all day") {
		t.Fatalf("all-day cell missing:\n%s", out)
	}
	// The header line carries the day; the rows themselves do not repeat it.
	for _, line := range strings.Split(strings.TrimSpace(out), "\n") {
		if strings.HasPrefix(line, "  ") && strings.Contains(line, "Aug 2026") {
			t.Fatalf("row repeats the day: %q", line)
		}
	}
}

func TestCalShow(t *testing.T) {
	env := calSeed(t)
	out := env.MustRun("cal", "show", calStandupID)
	var ev struct {
		ID         string `json:"id"`
		Account    string `json:"account"`
		Calendar   string `json:"calendar"`
		UID        string `json:"uid"`
		Title      string `json:"title"`
		RRule      string `json:"rrule"`
		Status     string `json:"status"`
		AllDay     bool   `json:"all_day"`
		MyResponse string `json:"my_response"`
		Attendees  []struct {
			Email    string `json:"email"`
			Response string `json:"response"`
			Self     bool   `json:"self"`
		} `json:"attendees"`
	}
	if err := json.Unmarshal([]byte(out), &ev); err != nil {
		t.Fatalf("decode: %v\n%s", err, out)
	}
	if ev.ID != calStandupID || ev.Title != "Standup" {
		t.Fatalf("event = %+v", ev)
	}
	if ev.RRule != "FREQ=WEEKLY;COUNT=4" {
		t.Fatalf("rrule = %q", ev.RRule)
	}
	if ev.UID != "u1" || ev.Account != "work" || ev.Calendar != "primary" {
		t.Fatalf("event = %+v", ev)
	}
	if len(ev.Attendees) != 2 || ev.Attendees[0].Email != "me@example.com" || !ev.Attendees[0].Self {
		t.Fatalf("attendees = %+v", ev.Attendees)
	}

	if _, _, code := env.Run("cal", "show", "work:c:primary:nope"); code != 3 {
		t.Fatalf("unknown event exit = %d", code)
	}
	if _, _, code := env.Run("cal", "show", "not-an-id"); code != 2 {
		t.Fatalf("malformed id exit = %d", code)
	}
}

func TestCalFree(t *testing.T) {
	env := calSeed(t)
	out := env.MustRun("cal", "free",
		"--from", "2026-08-26 08:00", "--to", "2026-08-26 12:00", "--duration", "30m")
	var slots []struct {
		StartUTC int64  `json:"start_utc"`
		EndUTC   int64  `json:"end_utc"`
		Duration string `json:"duration"`
	}
	if err := json.Unmarshal([]byte(out), &slots); err != nil {
		t.Fatalf("decode: %v\n%s", err, out)
	}
	if len(slots) != 2 {
		t.Fatalf("want two slots around the standup, got %d: %s", len(slots), out)
	}
	busyStart := time.Date(2026, 8, 26, 9, 0, 0, 0, time.UTC).Unix()
	busyEnd := time.Date(2026, 8, 26, 9, 30, 0, 0, time.UTC).Unix()
	for _, s := range slots {
		if s.StartUTC < busyEnd && s.EndUTC > busyStart {
			t.Fatalf("slot overlaps the standup: %+v", s)
		}
		if s.EndUTC-s.StartUTC < int64(30*time.Minute/time.Second) {
			t.Fatalf("slot shorter than --duration: %+v", s)
		}
	}
	if slots[0].Duration != "1h" {
		t.Fatalf("first slot duration = %q", slots[0].Duration)
	}

	// --hours keeps the search inside the working day.
	out = env.MustRun("cal", "free",
		"--from", "2026-08-26 00:00", "--to", "2026-08-27 00:00",
		"--duration", "1h", "--hours", "09:00-18:00")
	slots = slots[:0]
	if err := json.Unmarshal([]byte(out), &slots); err != nil {
		t.Fatalf("decode: %v\n%s", err, out)
	}
	if len(slots) == 0 {
		t.Fatalf("no slots inside working hours: %s", out)
	}
	for _, s := range slots {
		h := time.Unix(s.StartUTC, 0).UTC().Hour()
		if h < 9 || h >= 18 {
			t.Fatalf("slot outside 09:00-18:00: %+v", s)
		}
	}

	if _, _, code := env.Run("cal", "free", "--from", "2026-08-26"); code != 2 {
		t.Fatalf("missing --to exit = %d", code)
	}
}

func TestCalCreate(t *testing.T) {
	env := calSeed(t)
	out := env.MustRun("cal", "create", "-a", "work",
		"--title", "Design review",
		"--start", "2026-08-28 14:00", "--end", "2026-08-28 15:00",
		"--location", "Room 1", "--attendees", "alice@example.com,bob@example.com")
	var res struct {
		ID       string `json:"id"`
		Queued   bool   `json:"queued"`
		Title    string `json:"title"`
		StartUTC int64  `json:"start_utc"`
		EndUTC   int64  `json:"end_utc"`
	}
	if err := json.Unmarshal([]byte(out), &res); err != nil {
		t.Fatalf("decode: %v\n%s", err, out)
	}
	if res.Queued || res.Title != "Design review" {
		t.Fatalf("create result = %+v", res)
	}
	if !strings.HasPrefix(res.ID, "work:c:primary:") {
		t.Fatalf("create id = %q", res.ID)
	}

	// It reached the provider…
	var found bool
	for _, ev := range env.Cal["work"].Events("primary") {
		if ev.Title == "Design review" {
			found = true
			if len(ev.Attendees) != 2 || ev.Attendees[0].Response != model.PartNeedsAction {
				t.Fatalf("attendees = %+v", ev.Attendees)
			}
			if ev.Location != "Room 1" || ev.Status != model.StatusConfirmed {
				t.Fatalf("created event = %+v", ev)
			}
		}
	}
	if !found {
		t.Fatalf("event not on the fake provider: %+v", env.Cal["work"].Events("primary"))
	}

	// …and the index, so the agenda shows it without another sync.
	rows := calDecodeAgenda(t, env.MustRun("cal", "agenda", "--from", "2026-08-28", "--to", "2026-08-29"))
	if len(rows) != 1 || rows[0].Title != "Design review" {
		t.Fatalf("agenda after create = %+v", rows)
	}
	if rows[0].ID != res.ID {
		t.Fatalf("agenda id %q != create id %q", rows[0].ID, res.ID)
	}
}

func TestCalCreateDefaultsAndAllDay(t *testing.T) {
	env := calSeed(t)
	// No --end: one hour.
	out := env.MustRun("cal", "create", "-a", "work", "--title", "Coffee", "--start", "2026-08-28 10:00")
	var res struct {
		StartUTC int64 `json:"start_utc"`
		EndUTC   int64 `json:"end_utc"`
	}
	if err := json.Unmarshal([]byte(out), &res); err != nil {
		t.Fatalf("decode: %v\n%s", err, out)
	}
	if res.EndUTC-res.StartUTC != int64(time.Hour/time.Second) {
		t.Fatalf("default duration = %ds", res.EndUTC-res.StartUTC)
	}

	// --all-day: midnight to midnight, one day.
	out = env.MustRun("cal", "create", "-a", "work", "--title", "Holiday",
		"--start", "2026-08-29 11:00", "--all-day")
	var day struct {
		StartUTC int64 `json:"start_utc"`
		EndUTC   int64 `json:"end_utc"`
	}
	if err := json.Unmarshal([]byte(out), &day); err != nil {
		t.Fatalf("decode: %v\n%s", err, out)
	}
	start := time.Unix(day.StartUTC, 0).UTC()
	end := time.Unix(day.EndUTC, 0).UTC()
	if !start.Equal(time.Date(2026, 8, 29, 0, 0, 0, 0, time.UTC)) || !end.Equal(time.Date(2026, 8, 30, 0, 0, 0, 0, time.UTC)) {
		t.Fatalf("all-day range = %s … %s", start, end)
	}

	// An end before the start is a usage error.
	if _, _, code := env.Run("cal", "create", "-a", "work", "--title", "X",
		"--start", "2026-08-28 10:00", "--end", "2026-08-28 09:00"); code != 2 {
		t.Fatalf("inverted range exit = %d", code)
	}
	if _, _, code := env.Run("cal", "create", "-a", "work", "--start", "2026-08-28 10:00"); code != 2 {
		t.Fatalf("missing --title exit = %d", code)
	}
}

func TestCalCreateDryRun(t *testing.T) {
	env := calSeed(t)
	before := len(env.Cal["work"].Events("primary"))
	out := env.MustRun("cal", "create", "-a", "work", "--title", "Nope",
		"--start", "2026-08-28 14:00", "--dry-run")
	if !strings.Contains(out, "\"title\":\"Nope\"") {
		t.Fatalf("dry-run output = %s", out)
	}
	if got := len(env.Cal["work"].Events("primary")); got != before {
		t.Fatalf("dry-run created %d events", got-before)
	}
	rows := calDecodeAgenda(t, env.MustRun("cal", "agenda", "--from", "2026-08-28", "--to", "2026-08-29"))
	if len(rows) != 0 {
		t.Fatalf("dry-run touched the index: %+v", rows)
	}
}

func TestCalCreateOfflineQueues(t *testing.T) {
	env := calSeed(t)
	env.Cal["work"].FailNext(1)
	out, _, code := env.Run("cal", "create", "-a", "work", "--title", "Queued one",
		"--start", "2026-08-28 14:00")
	if code != 6 {
		t.Fatalf("offline create exit = %d\n%s", code, out)
	}
	if !strings.Contains(out, "\"queued\":true") {
		t.Fatalf("queued flag missing: %s", out)
	}
	for _, ev := range env.Cal["work"].Events("primary") {
		if ev.Title == "Queued one" {
			t.Fatalf("event reached the provider despite the failure")
		}
	}
	// The event row is patched into the index optimistically, but its
	// occurrences are only materialised once the provider confirms the write
	// (sync.Engine.afterExecute), so the agenda stays empty until the outbox
	// drains. Asserted loosely on purpose: only the exit code and the
	// provider state are contractual here.
	if !strings.Contains(out, "\"id\":\"work:c:primary:") {
		t.Fatalf("queued create id = %s", out)
	}
}

func TestCalUpdate(t *testing.T) {
	env := calSeed(t)
	out := env.MustRun("cal", "update", calStandupID, "--title", "Daily standup", "--location", "Room 9")
	if !strings.Contains(out, "Daily standup") {
		t.Fatalf("update result = %s", out)
	}
	shown := env.MustRun("cal", "show", calStandupID)
	if !strings.Contains(shown, "Daily standup") || !strings.Contains(shown, "Room 9") {
		t.Fatalf("event after update = %s", shown)
	}
	// Untouched fields survive.
	if !strings.Contains(shown, "FREQ=WEEKLY;COUNT=4") {
		t.Fatalf("rrule lost on update: %s", shown)
	}
	var onProvider bool
	for _, ev := range env.Cal["work"].Events("primary") {
		if ev.RemoteID == "e1" && ev.Title == "Daily standup" {
			onProvider = true
		}
	}
	if !onProvider {
		t.Fatalf("update did not reach the provider: %+v", env.Cal["work"].Events("primary"))
	}

	// Moving the start keeps the duration.
	out = env.MustRun("cal", "update", calStandupID, "--start", "2026-08-26 10:00")
	var res struct {
		StartUTC int64 `json:"start_utc"`
		EndUTC   int64 `json:"end_utc"`
	}
	if err := json.Unmarshal([]byte(out), &res); err != nil {
		t.Fatalf("decode: %v\n%s", err, out)
	}
	if res.EndUTC-res.StartUTC != int64(30*time.Minute/time.Second) {
		t.Fatalf("duration after moving the start = %ds", res.EndUTC-res.StartUTC)
	}
	if _, _, code := env.Run("cal", "update", "work:c:primary:missing", "--title", "X"); code != 3 {
		t.Fatalf("update of a missing event exit = %d", code)
	}
}

func TestCalRespond(t *testing.T) {
	env := calSeed(t)
	out := env.MustRun("cal", "respond", calStandupID, "--accept")
	if !strings.Contains(out, "\"response\":\"accepted\"") {
		t.Fatalf("respond result = %s", out)
	}
	shown := env.MustRun("cal", "show", calStandupID)
	if !strings.Contains(shown, "\"my_response\":\"accepted\"") {
		t.Fatalf("my_response after accept: %s", shown)
	}
	var responded bool
	for _, ev := range env.Cal["work"].Events("primary") {
		if ev.RemoteID == "e1" && ev.MyResponse == model.PartAccepted {
			responded = true
		}
	}
	if !responded {
		t.Fatalf("respond did not reach the provider: %+v", env.Cal["work"].Events("primary"))
	}

	if _, _, code := env.Run("cal", "respond", calStandupID); code != 2 {
		t.Fatalf("respond without a choice exit = %d", code)
	}
	if _, _, code := env.Run("cal", "respond", calStandupID, "--accept", "--decline"); code != 2 {
		t.Fatalf("respond with two choices exit = %d", code)
	}
}

func TestCalDelete(t *testing.T) {
	env := calSeed(t)
	out := env.MustRun("cal", "delete", calOffsiteID)
	if !strings.Contains(out, "\"deleted\":true") {
		t.Fatalf("delete result = %s", out)
	}
	for _, ev := range env.Cal["work"].Events("primary") {
		if ev.RemoteID == "e2" {
			t.Fatalf("event still on the provider: %+v", ev)
		}
	}
	rows := calDecodeAgenda(t, env.MustRun("cal", "agenda", "--days", "7"))
	for _, r := range rows {
		if r.ID == calOffsiteID {
			t.Fatalf("deleted event still in the agenda: %+v", rows)
		}
	}
	if _, _, code := env.Run("cal", "delete", calOffsiteID); code != 3 {
		t.Fatalf("second delete exit = %d", code)
	}
}
