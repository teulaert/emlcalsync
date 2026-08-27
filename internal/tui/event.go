package tui

import (
	"strings"

	"charm.land/bubbles/v2/viewport"
	tea "charm.land/bubbletea/v2"

	"github.com/teulaert/emlcalsync/internal/calendar"
	"github.com/teulaert/emlcalsync/internal/model"
	"github.com/teulaert/emlcalsync/internal/output"
)

// eventView is one event in full, and the place an invitation is answered.
type eventView struct {
	d Deps

	accountID string
	calRemote string
	calName   string
	remote    string

	ev *model.Event

	vp      viewport.Model
	ready   bool
	seq     int
	loading bool
	loadErr error
}

func newEventView(d Deps, accountID, calRemote, calName, remote string) *eventView {
	return &eventView{
		d: d, accountID: accountID, calRemote: calRemote, calName: calName, remote: remote,
	}
}

func (e *eventView) Title() string {
	if e.ev == nil {
		return "event"
	}
	return e.ev.Title
}

func (e *eventView) Init() tea.Cmd { return e.reload() }

func (e *eventView) reload() tea.Cmd {
	e.seq++
	e.loading = true
	return e.d.openEvent(e.seq, e.accountID, e.calRemote, e.remote)
}

func (e *eventView) targets() []target { return nil }

// rsvp is the RSVP the user just pressed, if any.
func (e *eventView) rsvp(k string) (model.Participation, bool) {
	switch k {
	case "y":
		return model.PartAccepted, true
	case "n":
		return model.PartDeclined, true
	case "t":
		return model.PartTentative, true
	}
	return "", false
}

func (e *eventView) Update(msg tea.Msg, k keymap, w, h int) (screen, tea.Cmd) {
	switch msg := msg.(type) {
	case eventOpened:
		if msg.seq != e.seq {
			return e, nil
		}
		e.loading = false
		e.loadErr = msg.err
		if msg.err == nil {
			e.ev = msg.event
			e.ensure(w, h)
			e.vp.SetContent(e.detail(w))
			e.vp.GotoTop()
		}
		return e, nil

	case tea.KeyPressMsg:
		e.ensure(w, h)
		var cmd tea.Cmd
		e.vp, cmd = e.vp.Update(msg)
		return e, cmd
	}
	return e, nil
}

func (e *eventView) ensure(w, h int) {
	vh := listRows(h)
	if vh < 1 {
		vh = 1
	}
	if !e.ready {
		e.vp = viewport.New(viewport.WithWidth(w), viewport.WithHeight(vh))
		e.vp.SoftWrap = true
		e.ready = true
		return
	}
	if e.vp.Width() != w || e.vp.Height() != vh {
		e.vp.SetWidth(w)
		e.vp.SetHeight(vh)
	}
}

func (e *eventView) detail(w int) string {
	if e.ev == nil {
		return ""
	}
	ev := e.ev
	var b strings.Builder
	add := func(k, v string) {
		if strings.TrimSpace(v) == "" {
			return
		}
		b.WriteString(styleFaint.Render(padCells(k, 11)))
		b.WriteString(v)
		b.WriteString("\n")
	}
	add("Title", ev.Title)
	add("When", calendar.FormatRange(ev.Start, ev.End, ev.AllDay, e.d.loc()))
	add("Where", ev.Location)
	add("Calendar", e.calName)
	add("Account", ev.AccountID)
	add("Status", string(ev.Status))
	add("RSVP", output.RSVP(ev.MyResponse))
	if ev.Organizer.Email != "" {
		add("Organizer", ev.Organizer.String())
	}
	if len(ev.Attendees) > 0 {
		var who []string
		for _, a := range ev.Attendees {
			s := a.Email
			if r := output.RSVP(a.Response); r != "" {
				s += " (" + r + ")"
			}
			who = append(who, s)
		}
		add("Attendees", strings.Join(who, ", "))
	}
	if ev.RRule != "" {
		add("Repeats", ev.RRule)
	}
	if strings.TrimSpace(ev.Description) != "" {
		b.WriteString("\n")
		b.WriteString(ev.Description)
		b.WriteString("\n")
	}
	return b.String()
}

func (e *eventView) View(w, h int) string {
	rows := listRows(h)
	if e.ev == nil {
		msg := "  loading…"
		if e.loadErr != nil {
			msg = "  " + e.loadErr.Error()
		} else if !e.loading {
			msg = "  (not found)"
		}
		return strings.Join(append([]string{msg}, make([]string, rows-1)...), "\n")
	}
	e.ensure(w, h)
	out := strings.Split(e.vp.View(), "\n")
	for len(out) < rows {
		out = append(out, "")
	}
	return strings.Join(out[:rows], "\n")
}

func (e *eventView) footer(w int) string {
	return "y accept · n decline · t tentative"
}
