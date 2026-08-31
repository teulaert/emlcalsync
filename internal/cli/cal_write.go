package cli

import (
	"strings"
	"time"

	"github.com/spf13/cobra"

	"github.com/teulaert/emlcalsync/internal/calendar"
	"github.com/teulaert/emlcalsync/internal/config"
	"github.com/teulaert/emlcalsync/internal/model"
	"github.com/teulaert/emlcalsync/internal/output"
	"github.com/teulaert/emlcalsync/internal/sync"
)

// calEventFlags is the flag set `cal create` and `cal update` share.
type calEventFlags struct {
	title       string
	start       string
	end         string
	allDay      bool
	calendar    string
	attendees   []string
	location    string
	description string
	meet        bool
	dryRun      bool
}

func (f *calEventFlags) bind(cmd *cobra.Command) {
	fl := cmd.Flags()
	fl.StringVar(&f.title, "title", "", "event title")
	fl.StringVar(&f.start, "start", "", "start time, e.g. \"2026-08-28 14:00\", \"tomorrow 09:30\", \"+2h\"")
	fl.StringVar(&f.end, "end", "", "end time (default: start plus one hour, or one day for --all-day)")
	fl.BoolVar(&f.allDay, "all-day", false, "an all-day event; times are snapped to midnight")
	fl.StringVarP(&f.calendar, "calendar", "c", "", "calendar to write to (name or remote id; default: primary)")
	fl.StringSliceVar(&f.attendees, "attendees", nil, "comma-separated attendee addresses")
	fl.StringVar(&f.location, "location", "", "location")
	fl.StringVar(&f.description, "description", "", "description")
	fl.BoolVar(&f.meet, "meet", false, "attach a Google Meet link (Google Calendar accounts only)")
	fl.BoolVar(&f.dryRun, "dry-run", false, "print what would be sent and exit 0 without touching the provider")
}

// calWriteAccount picks the account a write goes to: --account, else the one
// account that owns --calendar, else the send account (DESIGN.md §9.2).
func calWriteAccount(app *App, f *calEventFlags) (*config.Account, error) {
	// --account/-a is the root command's persistent flag; a write needs
	// exactly one account, so more than one is a usage error.
	var explicit string
	switch len(app.Accounts) {
	case 0:
	case 1:
		explicit = app.Accounts[0]
	default:
		return nil, output.Errorf(output.ExitUsage,
			"a write goes to one account: pass --account once, not %d times", len(app.Accounts))
	}
	if explicit != "" {
		return app.ResolveAccount(explicit)
	}
	if f.calendar != "" {
		owners, err := calCalendarOwners(app, f.calendar)
		if err != nil {
			return nil, err
		}
		switch len(owners) {
		case 0:
			return nil, output.Errorf(output.ExitNotFound, "no calendar matches %q: %w", f.calendar, model.ErrNotFound)
		case 1:
			return app.ResolveAccount(owners[0])
		default:
			return nil, output.Errorf(output.ExitUsage,
				"calendar %q exists in %s: pass --account", f.calendar, strings.Join(owners, ", "))
		}
	}
	return app.SendAccount("")
}

// calCalendarOwners lists the accounts holding a calendar with that name or
// remote id.
func calCalendarOwners(app *App, name string) ([]string, error) {
	cfg, err := app.Config()
	if err != nil {
		return nil, err
	}
	st, err := app.Store()
	if err != nil {
		return nil, err
	}
	var owners []string
	for _, acct := range cfg.AccountNames() {
		if _, err := st.FindCalendar(app.Context(), acct, name); err != nil {
			if calIsNotFound(err) {
				continue
			}
			return nil, err
		}
		owners = append(owners, acct)
	}
	return owners, nil
}

// calTargetCalendar resolves the calendar a write lands in: --calendar, else
// the account's primary calendar.
func calTargetCalendar(app *App, account, name string) (*model.Calendar, error) {
	st, err := app.Store()
	if err != nil {
		return nil, err
	}
	cal, err := st.FindCalendar(app.Context(), account, name)
	switch {
	case err == nil:
		return cal, nil
	case calIsNotFound(err) && name != "":
		return nil, output.Errorf(output.ExitNotFound, "no calendar %q in account %s: %w", name, account, model.ErrNotFound)
	case calIsNotFound(err):
		return nil, output.Errorf(output.ExitUsage,
			"account %s has no calendars yet: run `emlcal sync --account %s` first", account, account)
	}
	return nil, err
}

// calResultOut is what a create/update prints.
type calResultOut struct {
	ID       string      `json:"id"                table:"ID"`
	Queued   bool        `json:"queued"            table:"QUEUED"`
	Title    string      `json:"title"             table:"TITLE"`
	Meet     string      `json:"meet_url,omitempty" table:"MEET"`
	When     string      `json:"-"                 table:"WHEN"`
	Start    output.Time `json:"start"             table:"-"`
	StartUTC int64       `json:"start_utc"`
	End      output.Time `json:"end"               table:"-"`
	EndUTC   int64       `json:"end_utc"`
}

func calResult(ev *model.Event, account, calRemote, remoteID string, queued bool, loc *time.Location) calResultOut {
	if remoteID == "" {
		remoteID = ev.RemoteID
	}
	return calResultOut{
		ID:       model.EventPublicID(account, calRemote, remoteID),
		Queued:   queued,
		Title:    ev.Title,
		Meet:     ev.ConferenceURL,
		When:     calendar.FormatRange(ev.Start, ev.End, ev.AllDay, loc),
		Start:    output.T(ev.Start),
		StartUTC: ev.Start.Unix(),
		End:      output.T(ev.End),
		EndUTC:   ev.End.Unix(),
	}
}

// ---------------------------------------------------------------------------
// cal create

func calCreateCmd(app *App) *cobra.Command {
	var f calEventFlags
	cmd := &cobra.Command{
		Use:   "create",
		Short: "Create an event",
		Long: `Create an event on a calendar.

The event is written to the index immediately and pushed to the provider. If
the provider cannot be reached the write is queued in the outbox and the
command exits 6.`,
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, args []string) error {
			loc := app.Location()
			if strings.TrimSpace(f.title) == "" {
				return output.Errorf(output.ExitUsage, "cal create needs --title")
			}
			if f.start == "" {
				return output.Errorf(output.ExitUsage, "cal create needs --start")
			}
			acct, err := calWriteAccount(app, &f)
			if err != nil {
				return err
			}
			if f.meet {
				if err := calMeetSupported(acct); err != nil {
					return err
				}
			}
			cal, err := calTargetCalendar(app, acct.Name, f.calendar)
			if err != nil {
				return err
			}
			start, err := calendar.ParseWhen(f.start, app.Now(), loc)
			if err != nil {
				return output.Errorf(output.ExitUsage, "--start: %v", err)
			}
			var end time.Time
			if f.end != "" {
				if end, err = calendar.ParseWhen(f.end, app.Now(), loc); err != nil {
					return output.Errorf(output.ExitUsage, "--end: %v", err)
				}
			}
			start, end, err = calNormalizeTimes(start, end, f.allDay, loc)
			if err != nil {
				return err
			}
			ev := &model.Event{
				AccountID:        acct.Name,
				CalendarID:       cal.ID,
				CalendarRemote:   cal.RemoteID,
				Title:            f.title,
				Description:      f.description,
				Location:         f.location,
				Start:            start,
				End:              end,
				AllDay:           f.allDay,
				Timezone:         loc.String(),
				Attendees:        calAttendees(f.attendees),
				Status:           model.StatusConfirmed,
				CreateConference: f.meet,
			}
			if f.dryRun {
				return app.Printer().Print(calEventDetail(ev, loc))
			}
			eng, err := app.Engine()
			if err != nil {
				return err
			}
			res, err := eng.Apply(app.Context(), acct.Name, sync.Op{
				Kind:           sync.OpEventCreate,
				CalendarRemote: cal.RemoteID,
				Event:          ev,
			})
			if err != nil {
				return err
			}
			if err := app.Printer().Print(calResult(ev, acct.Name, cal.RemoteID, res.RemoteID, res.Queued, loc)); err != nil {
				return err
			}
			if res.Queued {
				return Queued(1)
			}
			return nil
		},
	}
	f.bind(cmd)
	return cmd
}

// calMeetSupported rejects --meet on an account whose calendar backend cannot
// mint a conference link. Only Google Calendar can: Meet rooms are created by
// the server on request, and CalDAV/JMAP have no equivalent to ask for.
func calMeetSupported(acct *config.Account) error {
	if acct.Calendar != nil && acct.Calendar.Backend == model.BackendGCal {
		return nil
	}
	return output.Errorf(output.ExitUsage,
		"--meet needs a Google Calendar account; %s does not sync calendars over gcal", acct.Name)
}

// calAttendees turns --attendees into model attendees that have not replied.
func calAttendees(list []string) []model.Attendee {
	var out []model.Attendee
	for _, a := range list {
		a = strings.TrimSpace(a)
		if a == "" {
			continue
		}
		out = append(out, model.Attendee{Email: a, Response: model.PartNeedsAction})
	}
	return out
}

// calNormalizeTimes fills in a missing end and snaps an all-day event to
// midnight, with an exclusive end the way both providers store it.
func calNormalizeTimes(start, end time.Time, allDay bool, loc *time.Location) (time.Time, time.Time, error) {
	if allDay {
		start = calMidnight(start, loc)
		if end.IsZero() {
			return start, start.AddDate(0, 0, 1), nil
		}
		end = calMidnight(end, loc)
		if !end.After(start) {
			end = start.AddDate(0, 0, 1)
		}
		return start, end, nil
	}
	if end.IsZero() {
		return start, start.Add(time.Hour), nil
	}
	if !end.After(start) {
		return time.Time{}, time.Time{}, output.Errorf(output.ExitUsage,
			"--end %s is not after --start %s", end.Format(time.RFC3339), start.Format(time.RFC3339))
	}
	return start, end, nil
}

func calMidnight(t time.Time, loc *time.Location) time.Time {
	t = t.In(loc)
	return time.Date(t.Year(), t.Month(), t.Day(), 0, 0, 0, 0, loc)
}

// ---------------------------------------------------------------------------
// cal update

func calUpdateCmd(app *App) *cobra.Command {
	var f calEventFlags
	cmd := &cobra.Command{
		Use:   "update <id>",
		Short: "Change an existing event",
		Long: `Change an event. Only the flags you pass are changed; everything else
keeps the value the index holds. Moving an event to another calendar is not
supported — delete it and create it again.`,
		Args: cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			loc := app.Location()
			ev, account, calRemote, remote, err := calGetEvent(app, args[0])
			if err != nil {
				return err
			}
			changed := cmd.Flags().Changed
			if changed("calendar") {
				cal, err := calTargetCalendar(app, account, f.calendar)
				if err != nil {
					return err
				}
				if cal.RemoteID != calRemote {
					return output.Errorf(output.ExitUsage,
						"cannot move %s to calendar %q: delete the event and create it again", args[0], f.calendar)
				}
			}
			if changed("title") {
				ev.Title = f.title
			}
			if changed("description") {
				ev.Description = f.description
			}
			if changed("location") {
				ev.Location = f.location
			}
			if changed("attendees") {
				ev.Attendees = calAttendees(f.attendees)
			}
			if changed("meet") && f.meet {
				cfg, err := app.Config()
				if err != nil {
					return err
				}
				acct, ok := cfg.Account(account)
				if !ok {
					return output.Errorf(output.ExitUsage, "unknown account %q", account)
				}
				if err := calMeetSupported(acct); err != nil {
					return err
				}
				ev.CreateConference = true
			}
			if changed("all-day") {
				ev.AllDay = f.allDay
			}
			if changed("start") {
				t, err := calendar.ParseWhen(f.start, app.Now(), loc)
				if err != nil {
					return output.Errorf(output.ExitUsage, "--start: %v", err)
				}
				// Keep the duration when only the start moves.
				d := ev.End.Sub(ev.Start)
				ev.Start = t
				if !changed("end") && d > 0 {
					ev.End = t.Add(d)
				}
			}
			if changed("end") {
				t, err := calendar.ParseWhen(f.end, app.Now(), loc)
				if err != nil {
					return output.Errorf(output.ExitUsage, "--end: %v", err)
				}
				ev.End = t
			}
			if changed("start") || changed("end") || changed("all-day") {
				start, end, err := calNormalizeTimes(ev.Start, ev.End, ev.AllDay, loc)
				if err != nil {
					return err
				}
				ev.Start, ev.End = start, end
			}
			if f.dryRun {
				return app.Printer().Print(calEventDetail(ev, loc))
			}
			eng, err := app.Engine()
			if err != nil {
				return err
			}
			res, err := eng.Apply(app.Context(), account, sync.Op{
				Kind:           sync.OpEventUpdate,
				CalendarRemote: calRemote,
				Event:          ev,
			})
			if err != nil {
				return err
			}
			if err := app.Printer().Print(calResult(ev, account, calRemote, remote, res.Queued, loc)); err != nil {
				return err
			}
			if res.Queued {
				return Queued(1)
			}
			return nil
		},
	}
	f.bind(cmd)
	return cmd
}

// ---------------------------------------------------------------------------
// cal delete

type calDeleteOut struct {
	ID      string `json:"id"      table:"ID"`
	Deleted bool   `json:"deleted" table:"DELETED"`
	Queued  bool   `json:"queued"  table:"QUEUED"`
	Title   string `json:"title"   table:"TITLE"`
}

func calDeleteCmd(app *App) *cobra.Command {
	var dryRun bool
	cmd := &cobra.Command{
		Use:     "delete <id>",
		Short:   "Delete an event",
		Aliases: []string{"rm"},
		Args:    cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			ev, account, calRemote, remote, err := calGetEvent(app, args[0])
			if err != nil {
				return err
			}
			out := calDeleteOut{ID: args[0], Deleted: true, Title: ev.Title}
			if dryRun {
				out.Deleted = false
				return app.Printer().Print(out)
			}
			eng, err := app.Engine()
			if err != nil {
				return err
			}
			res, err := eng.Apply(app.Context(), account, sync.Op{
				Kind:           sync.OpEventDelete,
				CalendarRemote: calRemote,
				IDs:            []string{remote},
			})
			if err != nil {
				return err
			}
			out.Queued = res.Queued
			if err := app.Printer().Print(out); err != nil {
				return err
			}
			if res.Queued {
				return Queued(1)
			}
			return nil
		},
	}
	cmd.Flags().BoolVar(&dryRun, "dry-run", false, "print what would be sent and exit 0")
	return cmd
}

// ---------------------------------------------------------------------------
// cal respond

type calRespondOut struct {
	ID       string `json:"id"       table:"ID"`
	Response string `json:"response" table:"RESPONSE"`
	Queued   bool   `json:"queued"   table:"QUEUED"`
	Title    string `json:"title"    table:"TITLE"`
}

func calRespondCmd(app *App) *cobra.Command {
	var accept, decline, tentative, dryRun bool
	cmd := &cobra.Command{
		Use:   "respond <id> --accept|--decline|--tentative",
		Short: "RSVP to an invitation",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			var resp model.Participation
			n := 0
			for _, c := range []struct {
				on bool
				p  model.Participation
			}{{accept, model.PartAccepted}, {decline, model.PartDeclined}, {tentative, model.PartTentative}} {
				if c.on {
					resp = c.p
					n++
				}
			}
			if n != 1 {
				return output.Errorf(output.ExitUsage, "cal respond needs exactly one of --accept, --decline, --tentative")
			}
			ev, account, calRemote, remote, err := calGetEvent(app, args[0])
			if err != nil {
				return err
			}
			out := calRespondOut{ID: args[0], Response: string(resp), Title: ev.Title}
			if dryRun {
				return app.Printer().Print(out)
			}
			eng, err := app.Engine()
			if err != nil {
				return err
			}
			res, err := eng.Apply(app.Context(), account, sync.Op{
				Kind:           sync.OpEventRespond,
				CalendarRemote: calRemote,
				IDs:            []string{remote},
				Response:       resp,
			})
			if err != nil {
				return err
			}
			out.Queued = res.Queued
			if err := app.Printer().Print(out); err != nil {
				return err
			}
			if res.Queued {
				return Queued(1)
			}
			return nil
		},
	}
	f := cmd.Flags()
	f.BoolVar(&accept, "accept", false, "accept the invitation")
	f.BoolVar(&decline, "decline", false, "decline the invitation")
	f.BoolVar(&tentative, "tentative", false, "answer tentatively")
	f.BoolVar(&dryRun, "dry-run", false, "print what would be sent and exit 0")
	return cmd
}
