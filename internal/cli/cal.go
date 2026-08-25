package cli

import (
	"errors"
	"fmt"
	"io"
	"strings"
	"time"

	"github.com/spf13/cobra"

	"github.com/lennert/emlcal/internal/calendar"
	"github.com/lennert/emlcal/internal/model"
	"github.com/lennert/emlcal/internal/output"
	"github.com/lennert/emlcal/internal/store"
)

func init() {
	Register(func(root *cobra.Command, app *App) { root.AddCommand(calCmd(app)) })
}

// calCmd is the `cal` group: read commands live here, writes in cal_write.go.
func calCmd(app *App) *cobra.Command {
	cmd := &cobra.Command{
		Use:   "cal",
		Short: "Calendars, agenda, free/busy and event writes",
		Long: `Read and write the calendar side of the archive.

Agenda, show and free work entirely from the local index and therefore
offline. Writes go to the provider and are queued (exit 6) when it cannot be
reached.`,
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, args []string) error { return cmd.Help() },
	}
	cmd.AddCommand(
		calCalendarsCmd(app),
		calAgendaCmd(app),
		calShowCmd(app),
		calFreeCmd(app),
		calCreateCmd(app),
		calUpdateCmd(app),
		calDeleteCmd(app),
		calRespondCmd(app),
	)
	return cmd
}

// ---------------------------------------------------------------------------
// cal calendars

type calCalendarRow struct {
	ID         string `json:"id"          table:"ID"`
	Account    string `json:"account"     table:"ACCOUNT"`
	Name       string `json:"name"        table:"NAME"`
	Primary    bool   `json:"primary"     table:"PRIMARY"`
	AccessRole string `json:"access_role" table:"ACCESS"`
	Timezone   string `json:"timezone"    table:"TZ"`
	Color      string `json:"color,omitempty" table:"-"`
}

// calCalendarPublicID is the public id of a calendar: "<account>:c:<remote>".
func calCalendarPublicID(account, remote string) string { return account + ":c:" + remote }

func calCalendarsCmd(app *App) *cobra.Command {
	return &cobra.Command{
		Use:     "calendars",
		Short:   "List the calendars of every selected account",
		Args:    cobra.NoArgs,
		Aliases: []string{"cals"},
		RunE: func(cmd *cobra.Command, args []string) error {
			accounts, err := app.AccountIDs()
			if err != nil {
				return err
			}
			st, err := app.Store()
			if err != nil {
				return err
			}
			cals, err := st.ListCalendars(app.Context(), accounts)
			if err != nil {
				return err
			}
			rows := make([]calCalendarRow, 0, len(cals))
			for _, c := range cals {
				rows = append(rows, calCalendarRow{
					ID:         calCalendarPublicID(c.AccountID, c.RemoteID),
					Account:    c.AccountID,
					Name:       c.Name,
					Primary:    c.Primary,
					AccessRole: c.AccessRole,
					Timezone:   c.Timezone,
					Color:      c.Color,
				})
			}
			return app.Printer().Print(rows)
		},
	}
}

// ---------------------------------------------------------------------------
// calendar selection shared by agenda and free

// calSelectCalendars resolves the calendars the read commands work on: every
// calendar of the selected accounts, or the one named by --calendar (matched
// per account by name or remote id).
func calSelectCalendars(app *App, name string) ([]model.Calendar, error) {
	accounts, err := app.AccountIDs()
	if err != nil {
		return nil, err
	}
	st, err := app.Store()
	if err != nil {
		return nil, err
	}
	ctx := app.Context()
	if name == "" {
		return st.ListCalendars(ctx, accounts)
	}
	var out []model.Calendar
	for _, acct := range accounts {
		c, err := st.FindCalendar(ctx, acct, name)
		if err != nil {
			if calIsNotFound(err) {
				continue
			}
			return nil, err
		}
		out = append(out, *c)
	}
	if len(out) == 0 {
		return nil, output.Errorf(output.ExitNotFound, "no calendar matches %q: %w", name, model.ErrNotFound)
	}
	return out, nil
}

func calIDsOf(cals []model.Calendar) []int64 {
	ids := make([]int64, 0, len(cals))
	for _, c := range cals {
		ids = append(ids, c.ID)
	}
	return ids
}

// ---------------------------------------------------------------------------
// cal agenda

type calAgendaRow struct {
	ID         string      `json:"id"                  table:"ID"`
	Start      output.Time `json:"start"               table:"START"`
	StartUTC   int64       `json:"start_utc"`
	End        output.Time `json:"end"                 table:"END"`
	EndUTC     int64       `json:"end_utc"`
	AllDay     bool        `json:"all_day"`
	Title      string      `json:"title"               table:"TITLE"`
	Calendar   string      `json:"calendar"            table:"CALENDAR"`
	Location   string      `json:"location,omitempty"  table:"LOCATION"`
	Status     string      `json:"status,omitempty"`
	MyResponse string      `json:"my_response,omitempty" table:"RSVP"`
	Recurring  bool        `json:"recurring"`
	Account    string      `json:"account"`
}

func calAgendaCmd(app *App) *cobra.Command {
	var (
		days     int
		from, to string
		calName  string
	)
	cmd := &cobra.Command{
		Use:   "agenda",
		Short: "Show the occurrences in a window, grouped by day",
		Long: `Show what is on the calendar.

The default window is today plus --days days. --from/--to accept the same
expressions everywhere else does: "2026-08-26", "tomorrow 09:00", "+3d",
"next monday".`,
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, args []string) error {
			loc := app.Location()
			start, end, err := calWindow(app, from, to, days)
			if err != nil {
				return err
			}
			cals, err := calSelectCalendars(app, calName)
			if err != nil {
				return err
			}
			st, err := app.Store()
			if err != nil {
				return err
			}
			occs, err := st.ListOccurrences(app.Context(), start, end, calIDsOf(cals))
			if err != nil {
				return err
			}
			rows := calAgendaRows(occs)
			if app.Printer().Format == output.Table {
				return calWriteAgendaTable(app.Printer().W, rows, occs, loc)
			}
			return app.Printer().Print(rows)
		},
	}
	f := cmd.Flags()
	f.IntVar(&days, "days", 7, "window length in days when --to is not given")
	f.StringVar(&from, "from", "", "window start (default today 00:00)")
	f.StringVar(&to, "to", "", "window end (default --from plus --days)")
	f.StringVarP(&calName, "calendar", "c", "", "restrict to one calendar (name or remote id)")
	return cmd
}

// calWindow resolves the --from/--to/--days trio into a half-open window.
func calWindow(app *App, from, to string, days int) (time.Time, time.Time, error) {
	loc := app.Location()
	now := app.Now()
	if days <= 0 {
		days = 7
	}
	start := now.In(loc)
	start = time.Date(start.Year(), start.Month(), start.Day(), 0, 0, 0, 0, loc)
	if from != "" {
		t, err := calendar.ParseWhen(from, now, loc)
		if err != nil {
			return time.Time{}, time.Time{}, output.Errorf(output.ExitUsage, "--from: %v", err)
		}
		start = t
	}
	end := start.AddDate(0, 0, days)
	if to != "" {
		t, err := calendar.ParseWhen(to, now, loc)
		if err != nil {
			return time.Time{}, time.Time{}, output.Errorf(output.ExitUsage, "--to: %v", err)
		}
		end = t
	}
	if !end.After(start) {
		return time.Time{}, time.Time{}, output.Errorf(output.ExitUsage,
			"empty window: %s is not before %s", start.Format(time.RFC3339), end.Format(time.RFC3339))
	}
	return start, end, nil
}

func calAgendaRows(occs []store.OccurrenceRow) []calAgendaRow {
	rows := make([]calAgendaRow, 0, len(occs))
	for i := range occs {
		o := &occs[i]
		rows = append(rows, calAgendaRow{
			ID:         model.EventPublicID(o.AccountID, o.CalendarRemote, o.EventRemoteID),
			Start:      output.T(o.Start),
			StartUTC:   o.Start.Unix(),
			End:        output.T(o.End),
			EndUTC:     o.End.Unix(),
			AllDay:     o.AllDay,
			Title:      o.Title,
			Calendar:   o.CalendarName,
			Location:   o.Location,
			Status:     string(o.Status),
			MyResponse: string(o.MyResponse),
			Recurring:  o.Recurring,
			Account:    o.AccountID,
		})
	}
	return rows
}

// calWriteAgendaTable renders the agenda the way a human reads it: a day
// header, then one indented line per occurrence. The generic table renderer
// cannot group, so this is written by hand.
func calWriteAgendaTable(w io.Writer, rows []calAgendaRow, occs []store.OccurrenceRow, loc *time.Location) error {
	if len(rows) == 0 {
		return nil
	}
	type cell struct{ when, title, cal, where, rsvp string }
	cells := make([]cell, len(rows))
	for i := range rows {
		cells[i] = cell{
			when:  calTimeCell(occs[i].Start, occs[i].End, occs[i].AllDay, loc),
			title: output.Truncate(rows[i].Title, 50),
			cal:   rows[i].Calendar,
			where: output.Truncate(rows[i].Location, 30),
			rsvp:  calRSVPCell(occs[i].MyResponse),
		}
	}
	var whenW, titleW, calW, whereW int
	for _, c := range cells {
		whenW = max(whenW, len([]rune(c.when)))
		titleW = max(titleW, len([]rune(c.title)))
		calW = max(calW, len([]rune(c.cal)))
		whereW = max(whereW, len([]rune(c.where)))
	}
	day := ""
	for i := range cells {
		d := occs[i].Start.In(loc).Format("Mon 2 Jan 2006")
		if d != day {
			if day != "" {
				fmt.Fprintln(w)
			}
			fmt.Fprintln(w, d)
			day = d
		}
		line := fmt.Sprintf("  %s  %s  %s  %s  %s",
			calPad(cells[i].when, whenW), calPad(cells[i].title, titleW),
			calPad(cells[i].cal, calW), calPad(cells[i].where, whereW), cells[i].rsvp)
		fmt.Fprintln(w, strings.TrimRight(line, " "))
	}
	return nil
}

// calTimeCell is FormatRange without the leading day, which the group header
// already carries.
func calTimeCell(start, end time.Time, allDay bool, loc *time.Location) string {
	if allDay {
		return "all day"
	}
	s, e := start.In(loc), end.In(loc)
	if !e.After(s) {
		return s.Format("15:04")
	}
	if s.Year() == e.Year() && s.YearDay() == e.YearDay() {
		return s.Format("15:04") + "–" + e.Format("15:04")
	}
	return s.Format("15:04") + " – " + e.Format("Mon 2 Jan 15:04")
}

func calRSVPCell(p model.Participation) string {
	switch p {
	case model.PartAccepted:
		return "yes"
	case model.PartDeclined:
		return "no"
	case model.PartTentative:
		return "maybe"
	case model.PartNeedsAction:
		return "?"
	}
	return ""
}

func calPad(s string, n int) string {
	if d := n - len([]rune(s)); d > 0 {
		return s + strings.Repeat(" ", d)
	}
	return s
}

// ---------------------------------------------------------------------------
// cal show

type calAttendeeOut struct {
	Name     string `json:"name,omitempty"`
	Email    string `json:"email"`
	Response string `json:"response,omitempty"`
	Optional bool   `json:"optional,omitempty"`
	Self     bool   `json:"self,omitempty"`
}

type calEventOut struct {
	ID          string           `json:"id,omitempty"          table:"ID"`
	Account     string           `json:"account"               table:"Account"`
	Calendar    string           `json:"calendar"              table:"Calendar"`
	UID         string           `json:"uid,omitempty"         table:"UID"`
	Title       string           `json:"title"                 table:"Title"`
	Description string           `json:"description,omitempty" table:"Description"`
	Location    string           `json:"location,omitempty"    table:"Location"`
	Start       output.Time      `json:"start"                 table:"-"`
	StartUTC    int64            `json:"start_utc"`
	End         output.Time      `json:"end"                   table:"-"`
	EndUTC      int64            `json:"end_utc"`
	When        string           `json:"-"                     table:"When"`
	AllDay      bool             `json:"all_day"`
	Timezone    string           `json:"timezone,omitempty"    table:"Timezone"`
	RRule       string           `json:"rrule,omitempty"       table:"Repeats"`
	Status      string           `json:"status,omitempty"      table:"Status"`
	Organizer   *model.Address   `json:"organizer,omitempty"`
	OrganizerS  string           `json:"-"                     table:"Organizer"`
	Attendees   []calAttendeeOut `json:"attendees,omitempty"`
	AttendeesS  string           `json:"-"                     table:"Attendees"`
	MyResponse  string           `json:"my_response,omitempty" table:"My RSVP"`
	Updated     output.Time      `json:"updated"               table:"-"`
	UpdatedUTC  int64            `json:"updated_utc"`
}

// calEventDetail builds the presentation form of one event.
func calEventDetail(ev *model.Event, loc *time.Location) calEventOut {
	out := calEventOut{
		Account:     ev.AccountID,
		Calendar:    ev.CalendarRemote,
		UID:         ev.UID,
		Title:       ev.Title,
		Description: ev.Description,
		Location:    ev.Location,
		Start:       output.T(ev.Start),
		StartUTC:    ev.Start.Unix(),
		End:         output.T(ev.End),
		EndUTC:      ev.End.Unix(),
		When:        calendar.FormatRange(ev.Start, ev.End, ev.AllDay, loc),
		AllDay:      ev.AllDay,
		Timezone:    ev.Timezone,
		RRule:       ev.RRule,
		Status:      string(ev.Status),
		MyResponse:  string(ev.MyResponse),
		Updated:     output.T(ev.Updated),
	}
	if !ev.Updated.IsZero() {
		out.UpdatedUTC = ev.Updated.Unix()
	}
	if ev.RemoteID != "" {
		out.ID = model.EventPublicID(ev.AccountID, ev.CalendarRemote, ev.RemoteID)
	}
	if ev.Organizer.Email != "" || ev.Organizer.Name != "" {
		org := ev.Organizer
		out.Organizer = &org
		out.OrganizerS = org.String()
	}
	var names []string
	for _, a := range ev.Attendees {
		out.Attendees = append(out.Attendees, calAttendeeOut{
			Name: a.Name, Email: a.Email, Response: string(a.Response),
			Optional: a.Optional, Self: a.Self,
		})
		s := a.Email
		if r := calRSVPCell(a.Response); r != "" {
			s += " (" + r + ")"
		}
		names = append(names, s)
	}
	out.AttendeesS = strings.Join(names, ", ")
	return out
}

// calGetEvent loads an event by public id. A row the index has marked deleted
// is treated as gone, so `show`/`update`/`delete` do not resurrect it.
func calGetEvent(app *App, id string) (*model.Event, string, string, string, error) {
	account, calRemote, remote, err := app.ParseEventID(id)
	if err != nil {
		return nil, "", "", "", err
	}
	st, err := app.Store()
	if err != nil {
		return nil, "", "", "", err
	}
	ev, err := st.GetEvent(app.Context(), account, calRemote, remote)
	if err != nil {
		return nil, "", "", "", err
	}
	if ev.DeletedAt != nil {
		return nil, "", "", "", output.Errorf(output.ExitNotFound, "event %s: %w", id, model.ErrNotFound)
	}
	return ev, account, calRemote, remote, nil
}

func calShowCmd(app *App) *cobra.Command {
	return &cobra.Command{
		Use:   "show <id>",
		Short: "Show one event in full",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			ev, _, _, _, err := calGetEvent(app, args[0])
			if err != nil {
				return err
			}
			return app.Printer().Print(calEventDetail(ev, app.Location()))
		},
	}
}

// ---------------------------------------------------------------------------
// cal free

type calFreeRow struct {
	Start    output.Time `json:"start"`
	StartUTC int64       `json:"start_utc"`
	End      output.Time `json:"end"`
	EndUTC   int64       `json:"end_utc"`
	When     string      `json:"-"        table:"WHEN"`
	Duration string      `json:"duration" table:"DURATION"`
}

func calFreeCmd(app *App) *cobra.Command {
	var (
		from, to string
		duration string
		hours    string
		calName  string
	)
	cmd := &cobra.Command{
		Use:   "free",
		Short: "Show the gaps between meetings in a window",
		Long: `Report the free slots in a window.

Cancelled events and invitations you declined do not make you busy. --hours
restricts the search to a working day (Mon–Fri by default), so a slot never
spans a night.`,
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, args []string) error {
			loc := app.Location()
			if from == "" || to == "" {
				return output.Errorf(output.ExitUsage, "cal free needs --from and --to")
			}
			start, end, err := calWindow(app, from, to, 1)
			if err != nil {
				return err
			}
			dur, err := calendar.ParseSpan(duration)
			if err != nil || dur <= 0 {
				return output.Errorf(output.ExitUsage, "--duration %q: not a duration like 30m or 2h", duration)
			}
			wh, err := calendar.ParseWorkHours(hours, loc)
			if err != nil {
				return output.Errorf(output.ExitUsage, "--hours: %v", err)
			}
			cals, err := calSelectCalendars(app, calName)
			if err != nil {
				return err
			}
			st, err := app.Store()
			if err != nil {
				return err
			}
			occs, err := st.ListOccurrences(app.Context(), start, end, calIDsOf(cals))
			if err != nil {
				return err
			}
			var busy []calendar.Busy
			for i := range occs {
				o := &occs[i]
				if o.Status == model.StatusCancelled || o.MyResponse == model.PartDeclined {
					continue
				}
				busy = append(busy, calendar.Busy{Start: o.Start, End: o.End, Title: o.Title})
			}
			rows := []calFreeRow{}
			for _, s := range calendar.FreeSlots(busy, start, end, dur, wh) {
				rows = append(rows, calFreeRow{
					Start:    output.T(s.Start),
					StartUTC: s.Start.Unix(),
					End:      output.T(s.End),
					EndUTC:   s.End.Unix(),
					When:     calendar.FormatRange(s.Start, s.End, false, loc),
					Duration: calFormatDuration(s.Duration()),
				})
			}
			return app.Printer().Print(rows)
		},
	}
	f := cmd.Flags()
	f.StringVar(&from, "from", "", "window start (required)")
	f.StringVar(&to, "to", "", "window end (required)")
	f.StringVar(&duration, "duration", "30m", "shortest slot worth reporting")
	f.StringVar(&hours, "hours", "", "restrict to a working day, e.g. 09:00-18:00")
	f.StringVarP(&calName, "calendar", "c", "", "restrict to one calendar (name or remote id)")
	return cmd
}

// calFormatDuration renders a slot length compactly: 30m, 1h, 1h30m, 2d3h.
func calFormatDuration(d time.Duration) string {
	if d <= 0 {
		return "0m"
	}
	d = d.Round(time.Minute)
	var b strings.Builder
	if days := int(d / (24 * time.Hour)); days > 0 {
		fmt.Fprintf(&b, "%dd", days)
		d -= time.Duration(days) * 24 * time.Hour
	}
	if h := int(d / time.Hour); h > 0 {
		fmt.Fprintf(&b, "%dh", h)
		d -= time.Duration(h) * time.Hour
	}
	if m := int(d / time.Minute); m > 0 {
		fmt.Fprintf(&b, "%dm", m)
	}
	if b.Len() == 0 {
		return "0m"
	}
	return b.String()
}

// calIsNotFound reports whether err is (or wraps) the shared not-found
// sentinel; a calendar that does not exist in one account is not an error
// while others may still match.
func calIsNotFound(err error) bool { return err != nil && errors.Is(err, model.ErrNotFound) }
