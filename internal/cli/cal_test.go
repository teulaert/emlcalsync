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

// TestCalCreateMeet: --meet asks a Google account's provider for a Meet room;
// the minted link is printed and indexed, and a non-Google account refuses the
// flag instead of silently dropping it.
func TestCalCreateMeet(t *testing.T) {
	env := calSeed(t)

	// work syncs calendars over CalDAV: --meet is a usage error there.
	if _, _, code := env.Run("cal", "create", "-a", "work",
		"--title", "Sync", "--start", "2026-08-28 14:00", "--meet"); code != 2 {
		t.Fatalf("--meet on a caldav account exit = %d, want 2", code)
	}

	env.Sync("home")
	out := env.MustRun("cal", "create", "-a", "home",
		"--title", "Intro call",
		"--start", "2026-08-28 14:00", "--end", "2026-08-28 14:30",
		"--attendees", "alice@example.com", "--meet")
	var res struct {
		ID      string `json:"id"`
		MeetURL string `json:"meet_url"`
	}
	if err := json.Unmarshal([]byte(out), &res); err != nil {
		t.Fatalf("decode: %v\n%s", err, out)
	}
	if !strings.HasPrefix(res.MeetURL, "https://meet.example/") {
		t.Fatalf("meet_url = %q, want the provider's minted link", res.MeetURL)
	}

	// The link reached the index: cal show prints it without another sync.
	out = env.MustRun("cal", "show", res.ID)
	var ev struct {
		ConferenceURL string `json:"conference_url"`
	}
	if err := json.Unmarshal([]byte(out), &ev); err != nil {
		t.Fatalf("decode show: %v\n%s", err, out)
	}
	if ev.ConferenceURL != res.MeetURL {
		t.Fatalf("conference_url = %q, want %q", ev.ConferenceURL, res.MeetURL)
	}
}

// calRRuleOf reads the rule the provider actually holds for an event.
func calRRuleOf(t *testing.T, env *testEnv, account, calRemote, title string) (string, bool) {
	t.Helper()
	for _, ev := range env.Cal[account].Events(calRemote) {
		if ev.Title == title {
			return ev.RRule, true
		}
	}
	return "", false
}

type calWriteResult struct {
	ID     string `json:"id"`
	Title  string `json:"title"`
	RRule  string `json:"rrule"`
	Queued bool   `json:"queued"`
}

func calDecodeWrite(t *testing.T, out string) calWriteResult {
	t.Helper()
	var res calWriteResult
	if err := json.Unmarshal([]byte(out), &res); err != nil {
		t.Fatalf("decode: %v\n%s", err, out)
	}
	return res
}

// A created series is expanded straight away: the agenda is what the person
// looks at next, and "it will appear after the next sync" is not an answer.
func TestCalCreateRecurring(t *testing.T) {
	env := calSeed(t)
	out := env.MustRun("cal", "create", "-a", "work",
		"--title", "Weekly sync", "--start", "2026-09-07 10:00", "--end", "2026-09-07 10:30",
		"--rrule", "FREQ=WEEKLY;BYDAY=MO;COUNT=3")
	res := calDecodeWrite(t, out)
	if res.Queued || res.RRule != "FREQ=WEEKLY;BYDAY=MO;COUNT=3" {
		t.Fatalf("create result = %+v", res)
	}

	if got, ok := calRRuleOf(t, env, "work", "primary", "Weekly sync"); !ok {
		t.Fatal("the event never reached the provider")
	} else if got != "FREQ=WEEKLY;BYDAY=MO;COUNT=3" {
		t.Errorf("the provider holds rrule %q", got)
	}

	rows := calDecodeAgenda(t, env.MustRun("cal", "agenda", "--from", "2026-09-07", "--to", "2026-09-28"))
	var when []string
	for _, r := range rows {
		if r.Title == "Weekly sync" {
			when = append(when, r.Start)
		}
	}
	if len(when) != 3 {
		t.Fatalf("agenda shows %d occurrences of the new series, want 3: %+v", len(when), rows)
	}
	for i, w := range []string{"2026-09-07", "2026-09-14", "2026-09-21"} {
		if !strings.HasPrefix(when[i], w) {
			t.Errorf("occurrence %d starts %q, want %s", i, when[i], w)
		}
	}
}

// Both spellings are in circulation, and the stored form is the bare value.
func TestCalCreateRecurringAcceptsThePrefix(t *testing.T) {
	env := calSeed(t)
	res := calDecodeWrite(t, env.MustRun("cal", "create", "-a", "work",
		"--title", "Prefixed", "--start", "2026-09-07 10:00",
		"--rrule", "RRULE:FREQ=DAILY;COUNT=2"))
	if res.RRule != "FREQ=DAILY;COUNT=2" {
		t.Errorf("stored rrule = %q, want the value without the prefix", res.RRule)
	}
	if got, _ := calRRuleOf(t, env, "work", "primary", "Prefixed"); got != "FREQ=DAILY;COUNT=2" {
		t.Errorf("the provider holds %q", got)
	}
}

// A rule that cannot be expanded is refused where it is typed, rather than
// stored to produce a series with no occurrences.
func TestCalCreateRecurringRejectsBadRules(t *testing.T) {
	env := calSeed(t)
	for _, rule := range []string{
		"BYDAY=MO",                            // no FREQ
		"FREQ=FORTNIGHTLY",                    // not a frequency
		"FREQ=DAILY\nSUMMARY:Injected",        // a second property
		"FREQ=DAILY\r\nATTENDEE:mailto:x@y.z", // the same, CRLF
		"nonsense",
	} {
		_, _, code := env.Run("cal", "create", "-a", "work",
			"--title", "Bad", "--start", "2026-09-07 10:00", "--rrule", rule)
		if code != 2 {
			t.Errorf("--rrule %q exited %d, want 2", rule, code)
		}
	}
	// Nothing was written on any of those attempts.
	for _, ev := range env.Cal["work"].Events("primary") {
		if ev.Title == "Bad" {
			t.Fatal("a refused rule still created an event")
		}
	}
}

func TestCalUpdateRecurrence(t *testing.T) {
	env := calSeed(t)
	id := "work:c:primary:e1" // Standup, FREQ=WEEKLY;COUNT=4

	res := calDecodeWrite(t, env.MustRun("cal", "update", id, "--rrule", "FREQ=DAILY;COUNT=2"))
	if res.RRule != "FREQ=DAILY;COUNT=2" {
		t.Errorf("update result rrule = %q", res.RRule)
	}
	if got, _ := calRRuleOf(t, env, "work", "primary", "Standup"); got != "FREQ=DAILY;COUNT=2" {
		t.Errorf("the provider holds %q after the update", got)
	}
	rows := calDecodeAgenda(t, env.MustRun("cal", "agenda", "--from", "2026-08-26", "--to", "2026-09-30"))
	n := 0
	for _, r := range rows {
		if r.Title == "Standup" {
			n++
		}
	}
	if n != 2 {
		t.Errorf("agenda shows %d occurrences after re-ruling, want 2", n)
	}
}

// --rrule "" is the whole point of the flag being settable to empty: a series
// turned back into one event, on the provider as well as in the index.
func TestCalUpdateClearsRecurrence(t *testing.T) {
	env := calSeed(t)
	id := "work:c:primary:e1"

	res := calDecodeWrite(t, env.MustRun("cal", "update", id, "--rrule", ""))
	if res.RRule != "" {
		t.Errorf("update result still carries rrule %q", res.RRule)
	}
	if got, ok := calRRuleOf(t, env, "work", "primary", "Standup"); !ok {
		t.Fatal("the event vanished from the provider")
	} else if got != "" {
		t.Errorf("the provider still holds rrule %q — the series outlived the write", got)
	}

	rows := calDecodeAgenda(t, env.MustRun("cal", "agenda", "--from", "2026-08-26", "--to", "2026-09-30"))
	n := 0
	for _, r := range rows {
		if r.Title == "Standup" {
			n++
		}
	}
	if n != 1 {
		t.Errorf("agenda shows %d occurrences after clearing, want the one event", n)
	}
}

// Not passing the flag leaves the rule alone: an update is only what the
// flags named.
func TestCalUpdateLeavesRecurrenceAloneWhenNotAsked(t *testing.T) {
	env := calSeed(t)
	env.MustRun("cal", "update", "work:c:primary:e1", "--location", "Room 9")
	if got, _ := calRRuleOf(t, env, "work", "primary", "Standup"); got != "FREQ=WEEKLY;COUNT=4" {
		t.Errorf("rrule = %q after an unrelated update, want it untouched", got)
	}
}

func TestCalUpdateRejectsBadRule(t *testing.T) {
	env := calSeed(t)
	if _, _, code := env.Run("cal", "update", "work:c:primary:e1", "--rrule", "FREQ=NEVER"); code != 2 {
		t.Errorf("exited %d, want 2", code)
	}
	if got, _ := calRRuleOf(t, env, "work", "primary", "Standup"); got != "FREQ=WEEKLY;COUNT=4" {
		t.Errorf("a refused rule changed the stored one to %q", got)
	}
}

func TestCalCreateRecurringDryRun(t *testing.T) {
	env := calSeed(t)
	out := env.MustRun("cal", "create", "-a", "work", "--title", "Planned",
		"--start", "2026-09-07 10:00", "--rrule", "rrule:freq=weekly", "--dry-run")
	var detail struct {
		Title string `json:"title"`
		RRule string `json:"rrule"`
	}
	if err := json.Unmarshal([]byte(out), &detail); err != nil {
		t.Fatalf("decode: %v\n%s", err, out)
	}
	if detail.RRule != "FREQ=WEEKLY" {
		t.Errorf("dry run shows rrule %q, want the normalised form", detail.RRule)
	}
	for _, ev := range env.Cal["work"].Events("primary") {
		if ev.Title == "Planned" {
			t.Fatal("--dry-run created an event")
		}
	}
}
