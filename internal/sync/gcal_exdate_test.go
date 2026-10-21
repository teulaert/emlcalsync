package sync

import (
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	gosync "sync"
	"testing"
	"time"

	calendarapi "google.golang.org/api/calendar/v3"

	"github.com/teulaert/emlcalsync/internal/model"
	"github.com/teulaert/emlcalsync/internal/provider/gcal"
)

// googleServer is a scripted Google Calendar: it keeps the current version
// of every object and answers events.list the way Google does, a full
// listing without a sync token and only what changed since one with it.
type googleServer struct {
	t   *testing.T
	srv *httptest.Server

	mu      gosync.Mutex
	version int
	items   map[string]*calendarapi.Event
	changed map[string]int // id -> version it last changed at
}

func newGoogleServer(t *testing.T) *googleServer {
	t.Helper()
	g := &googleServer{t: t, items: map[string]*calendarapi.Event{}, changed: map[string]int{}}
	mux := http.NewServeMux()
	mux.HandleFunc("GET /calendar/v3/users/me/calendarList", func(w http.ResponseWriter, r *http.Request) {
		g.write(w, &calendarapi.CalendarList{Items: []*calendarapi.CalendarListEntry{
			{Id: "primary", Summary: "primary", TimeZone: "Europe/Amsterdam", AccessRole: "owner", Primary: true},
		}})
	})
	mux.HandleFunc("GET /calendar/v3/calendars/{cid}/events", g.list)
	mux.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		t.Errorf("google: unexpected request %s %s", r.Method, r.URL.Path)
		http.Error(w, "unexpected", http.StatusBadRequest)
	})
	g.srv = httptest.NewServer(mux)
	t.Cleanup(g.srv.Close)
	return g
}

func (g *googleServer) write(w http.ResponseWriter, v any) {
	w.Header().Set("Content-Type", "application/json")
	if err := json.NewEncoder(w).Encode(v); err != nil {
		g.t.Errorf("google: encode: %v", err)
	}
}

func (g *googleServer) list(w http.ResponseWriter, r *http.Request) {
	g.mu.Lock()
	defer g.mu.Unlock()
	since := -1
	if tok := r.URL.Query().Get("syncToken"); tok != "" {
		n, err := strconv.Atoi(tok)
		if err != nil {
			http.Error(w, "bad token", http.StatusBadRequest)
			return
		}
		since = n
	}
	out := &calendarapi.Events{TimeZone: "Europe/Amsterdam", NextSyncToken: strconv.Itoa(g.version)}
	for id, ev := range g.items {
		if g.changed[id] > since {
			out.Items = append(out.Items, ev)
		}
	}
	g.write(w, out)
}

// put stores (a new version of) an object.
func (g *googleServer) put(ev *calendarapi.Event) {
	g.mu.Lock()
	defer g.mu.Unlock()
	g.version++
	g.items[ev.Id] = ev
	g.changed[ev.Id] = g.version
}

func (g *googleServer) provider(t *testing.T) *gcal.Calendar {
	t.Helper()
	c, err := gcal.New(context.Background(), gcal.Options{
		HTTPClient: g.srv.Client(),
		Email:      "user@example.com",
		Endpoint:   g.srv.URL + "/calendar/v3/",
		Logger:     slog.New(slog.NewTextHandler(io.Discard, nil)),
	})
	if err != nil {
		t.Fatalf("gcal.New: %v", err)
	}
	return c
}

// weekly is a Google series of eight weekly instances, a few days out, in a
// series that declares UTC as its zone -- it stays put in UTC across any DST
// change -- while its EXDATEs are written in Amsterdam time, which is how the
// series that prompted this came from Google. Week 3 is held by Google as a
// cancelled instance object and week 4 as a moved one.
type weekly struct {
	t   *testing.T
	h   *harness
	g   *googleServer
	ams *time.Location

	w0        time.Time
	moved     time.Time // where week 4 went
	cancelled *calendarapi.Event
	override  *calendarapi.Event
}

const weeklyID = "g7ifrv1337tr318er1n3aq3uc4"

func newWeekly(t *testing.T) *weekly {
	t.Helper()
	ams, err := time.LoadLocation("Europe/Amsterdam")
	if err != nil {
		t.Fatal(err)
	}
	s := &weekly{t: t, h: newHarness(t), g: newGoogleServer(t), ams: ams}
	s.h.fact.cal = s.g.provider(t)
	s.w0 = time.Now().UTC().Add(72 * time.Hour).Truncate(time.Hour)

	original := func(n int) *calendarapi.EventDateTime {
		return &calendarapi.EventDateTime{DateTime: s.week(n).In(ams).Format(time.RFC3339), TimeZone: "UTC"}
	}
	// Listed in full, iCalUID and all, which is how Google lists one.
	s.cancelled = &calendarapi.Event{
		Id: s.instanceID(3), ICalUID: weeklyID + "@google.com", Summary: "Weekly", Status: "cancelled",
		RecurringEventId: weeklyID, OriginalStartTime: original(3),
	}
	s.moved = s.week(4).Add(24 * time.Hour)
	s.override = &calendarapi.Event{
		Id: s.instanceID(4), ICalUID: weeklyID + "@google.com", Summary: "Weekly (moved)",
		Status: "confirmed", RecurringEventId: weeklyID, OriginalStartTime: original(4),
		Start:   &calendarapi.EventDateTime{DateTime: s.moved.Format(time.RFC3339), TimeZone: "UTC"},
		End:     &calendarapi.EventDateTime{DateTime: s.moved.Add(30 * time.Minute).Format(time.RFC3339), TimeZone: "UTC"},
		Updated: s.w0.AddDate(0, 0, -20).Format(time.RFC3339),
	}
	return s
}

func (s *weekly) week(i int) time.Time { return s.w0.AddDate(0, 0, 7*i) }

func (s *weekly) instanceID(i int) string {
	return weeklyID + "_" + s.week(i).Format("20060102T150405Z")
}

// master is revision rev of the series' master, excluding the weeks named.
func (s *weekly) master(rev int, excluded ...int) *calendarapi.Event {
	rec := []string{"RRULE:FREQ=WEEKLY;COUNT=8"}
	if len(excluded) > 0 {
		vals := make([]string, len(excluded))
		for i, n := range excluded {
			vals[i] = s.week(n).In(s.ams).Format("20060102T150405")
		}
		rec = append([]string{"EXDATE;TZID=Europe/Amsterdam:" + strings.Join(vals, ",")}, rec...)
	}
	return &calendarapi.Event{
		Id: weeklyID, ICalUID: weeklyID + "@google.com", Summary: "Weekly", Status: "confirmed",
		Start:      &calendarapi.EventDateTime{DateTime: s.w0.In(s.ams).Format(time.RFC3339), TimeZone: "UTC"},
		End:        &calendarapi.EventDateTime{DateTime: s.w0.Add(30 * time.Minute).In(s.ams).Format(time.RFC3339), TimeZone: "UTC"},
		Recurrence: rec,
		Updated:    s.w0.AddDate(0, 0, -30+rev).Format(time.RFC3339),
	}
}

func (s *weekly) sync(full bool) { s.h.sync(SyncOptions{Calendar: true, Full: full}) }

// occurrences checks the occurrence cache the agenda and free/busy read.
func (s *weekly) occurrences(step string, want ...time.Time) {
	s.t.Helper()
	rows, err := s.h.st.ListOccurrences(context.Background(), s.w0.Add(-time.Hour), s.week(9), nil)
	if err != nil {
		s.t.Fatalf("%s: ListOccurrences: %v", step, err)
	}
	var got []time.Time
	for _, r := range rows {
		got = append(got, r.Start.UTC())
		if r.Start.Equal(s.moved) {
			if r.Title != "Weekly (moved)" {
				s.t.Errorf("%s: the moved instance shows as %q", step, r.Title)
			}
		} else if r.EventRemoteID != weeklyID {
			s.t.Errorf("%s: occurrence at %v is filed under %q, want the master", step, r.Start, r.EventRemoteID)
		}
	}
	ok := len(got) == len(want)
	for i := 0; ok && i < len(want); i++ {
		ok = got[i].Equal(want[i])
	}
	if !ok {
		s.t.Fatalf("%s: occurrences\n got %v\nwant %v", step, got, want)
	}
}

// googleInstances checks that Google's own instance objects are as Google
// sent them: the moved one moved, the cancelled one cancelled.
func (s *weekly) googleInstances(step string) {
	s.t.Helper()
	ctx := context.Background()
	ov, err := s.h.st.GetEvent(ctx, "work", "primary", s.override.Id)
	if err != nil {
		s.t.Fatalf("%s: the moved instance is gone: %v", step, err)
	}
	if ov.Status != model.StatusConfirmed || !ov.Start.Equal(s.moved) || ov.DeletedAt != nil {
		s.t.Errorf("%s: moved instance = %s at %v (deleted %v), want confirmed at %v",
			step, ov.Status, ov.Start, ov.DeletedAt, s.moved)
	}
	cx, err := s.h.st.GetEvent(ctx, "work", "primary", s.cancelled.Id)
	if err != nil {
		s.t.Fatalf("%s: the cancelled instance is gone: %v", step, err)
	}
	if cx.Status != model.StatusCancelled {
		s.t.Errorf("%s: cancelled instance status = %s", step, cx.Status)
	}
}

// derived checks how many exception rows there are besides Google's own.
func (s *weekly) derived(step string, want int) {
	s.t.Helper()
	ctx := context.Background()
	cals, err := s.h.st.ListCalendars(ctx, []string{"work"})
	if err != nil || len(cals) != 1 {
		s.t.Fatalf("%s: calendars = %v, %v", step, cals, err)
	}
	ex, err := s.h.st.ListEventExceptions(ctx, cals[0].ID)
	if err != nil {
		s.t.Fatalf("%s: ListEventExceptions: %v", step, err)
	}
	n := 0
	for _, e := range ex {
		if e.RemoteID != s.cancelled.Id && e.RemoteID != s.override.Id {
			n++
		}
	}
	if n != want {
		s.t.Errorf("%s: %d exclusion rows besides Google's own instances, want %d: %+v", step, n, want, ex)
	}
}

// An EXDATE excludes an instance only for as long as the master carries it.
// Lifting one brings the meeting back and changing the set moves the hole,
// over full listings and deltas alike.
func TestGoogleExdateComesAndGoesWithTheMaster(t *testing.T) {
	s := newWeekly(t)
	week, moved := s.week, s.moved

	// Weeks 1 and 2 are excluded by EXDATE, 3 cancelled, 4 moved.
	s.g.put(s.master(0, 1, 2))
	s.g.put(s.cancelled)
	s.g.put(s.override)
	s.sync(false)
	s.occurrences("full listing", week(0), moved, week(5), week(6), week(7))
	s.googleInstances("full listing")
	s.derived("full listing", 2)

	// The same listing again adds nothing.
	s.sync(true)
	s.occurrences("second full listing", week(0), moved, week(5), week(6), week(7))
	s.derived("second full listing", 2)

	// Week 2 is reinstated: the delta holds the master alone.
	s.g.put(s.master(1, 1))
	s.sync(false)
	s.occurrences("week 2 reinstated", week(0), week(2), moved, week(5), week(6), week(7))
	s.googleInstances("week 2 reinstated")
	s.derived("week 2 reinstated", 1)

	// Every EXDATE lifted: week 1 is back too.
	s.g.put(s.master(2))
	s.sync(false)
	s.occurrences("all exclusions lifted", week(0), week(1), week(2), moved, week(5), week(6), week(7))
	s.derived("all exclusions lifted", 0)

	// A different exclusion, then a full listing over it: the hole is at
	// week 6 now and stays there.
	s.g.put(s.master(3, 6))
	s.sync(false)
	s.occurrences("exclusion changed", week(0), week(1), week(2), moved, week(5), week(7))
	s.sync(true)
	s.occurrences("exclusion changed, full listing", week(0), week(1), week(2), moved, week(5), week(7))
	s.googleInstances("exclusion changed, full listing")
	s.derived("exclusion changed, full listing", 1)

	// The series is deleted, which Google lists as the master and its moved
	// instance cancelled: nothing of it is left, derived rows included, and
	// the cancelled instances do not turn into meetings of their own.
	gone := s.master(4)
	gone.Status = "cancelled"
	s.g.put(gone)
	movedGone := *s.override
	movedGone.Status = "cancelled"
	s.g.put(&movedGone)
	s.sync(false)
	s.occurrences("series deleted")
	s.derived("series deleted", 0)
}

// An EXDATE on a slot Google also holds an instance object for neither
// overwrites that object nor, when it is lifted again, takes it along: a
// cancelled instance is never revived and a moved one never discarded.
func TestGoogleExdateNeverTouchesGoogleInstances(t *testing.T) {
	s := newWeekly(t)
	week, moved := s.week, s.moved

	s.g.put(s.master(0))
	s.g.put(s.cancelled)
	s.g.put(s.override)
	s.sync(false)
	all := []time.Time{week(0), week(1), week(2), moved, week(5), week(6), week(7)}
	s.occurrences("full listing", all...)
	s.googleInstances("full listing")

	// The master comes alone in the delta, excluding both slots.
	s.g.put(s.master(1, 3, 4))
	s.sync(false)
	s.occurrences("excluded over Google's instances", all...)
	s.googleInstances("excluded over Google's instances")
	s.derived("excluded over Google's instances", 2)

	// And alone again, with the exclusions lifted.
	s.g.put(s.master(2))
	s.sync(false)
	s.occurrences("exclusions lifted", all...)
	s.googleInstances("exclusions lifted")
	s.derived("exclusions lifted", 0)

	s.sync(true)
	s.occurrences("full listing after", all...)
	s.googleInstances("full listing after")
}
