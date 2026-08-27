package tui

import (
	"fmt"
	"strings"
	"time"

	"charm.land/bubbles/v2/key"
	tea "charm.land/bubbletea/v2"

	"github.com/teulaert/emlcalsync/internal/output"
	"github.com/teulaert/emlcalsync/internal/store"
)

// agendaDays is the window the calendar screen opens on. [ and ] page it.
const agendaDays = 14

// agenda is `cal agenda` made navigable: a day header, then one indented line
// per occurrence, merged across every calendar of every account.
//
// Rows and headers share one slice so the cursor can skip headers without the
// view and the model disagreeing about what row 7 is.
type agenda struct {
	d Deps

	from, to time.Time
	occs     []store.OccurrenceRow
	lines    []agendaLine
	cursor   int
	top      int

	seq     int
	loading bool
	loadErr error
}

type agendaLine struct {
	header string // non-empty for a day header
	occ    int    // index into occs, when header is empty
}

func newAgenda(d Deps) *agenda {
	now := d.now().In(d.loc())
	from := time.Date(now.Year(), now.Month(), now.Day(), 0, 0, 0, 0, d.loc())
	return &agenda{d: d, from: from, to: from.AddDate(0, 0, agendaDays)}
}

func (a *agenda) Title() string {
	return fmt.Sprintf("calendar · %s – %s",
		a.from.Format("Mon 2 Jan"), a.to.Add(-time.Second).Format("Mon 2 Jan"))
}

func (a *agenda) Init() tea.Cmd { return a.reload() }

func (a *agenda) reload() tea.Cmd {
	a.seq++
	a.loading = true
	return a.d.loadAgenda(a.seq, a.from, a.to)
}

func (a *agenda) targets() []target { return nil }

func (a *agenda) selectedOcc() *store.OccurrenceRow {
	if a.cursor < 0 || a.cursor >= len(a.lines) {
		return nil
	}
	l := a.lines[a.cursor]
	if l.header != "" {
		return nil
	}
	return &a.occs[l.occ]
}

func (a *agenda) build() {
	a.lines = nil
	day := ""
	for i := range a.occs {
		d := a.occs[i].Start.In(a.d.loc()).Format("Mon 2 Jan 2006")
		if d != day {
			a.lines = append(a.lines, agendaLine{header: d})
			day = d
		}
		a.lines = append(a.lines, agendaLine{occ: i})
	}
	if a.cursor >= len(a.lines) {
		a.cursor = max(len(a.lines)-1, 0)
	}
	// Never rest on a header.
	a.skipHeader(1)
}

// skipHeader moves the cursor off a day header in direction dir.
func (a *agenda) skipHeader(dir int) {
	for a.cursor >= 0 && a.cursor < len(a.lines) && a.lines[a.cursor].header != "" {
		a.cursor += dir
	}
	if a.cursor < 0 {
		a.cursor = 0
	}
	if a.cursor >= len(a.lines) {
		a.cursor = max(len(a.lines)-1, 0)
	}
}

func (a *agenda) Update(msg tea.Msg, k keymap, w, h int) (screen, tea.Cmd) {
	switch msg := msg.(type) {
	case agendaLoaded:
		if msg.seq != a.seq {
			return a, nil
		}
		a.loading = false
		a.loadErr = msg.err
		if msg.err == nil {
			a.occs = msg.occs
			a.build()
		}
		return a, nil

	case tea.KeyPressMsg:
		rows := listRows(h)
		switch {
		case key.Matches(msg, k.Down):
			if a.cursor < len(a.lines)-1 {
				a.cursor++
				a.skipHeader(1)
			}
			a.scroll(rows)
		case key.Matches(msg, k.Up):
			if a.cursor > 0 {
				a.cursor--
				a.skipHeader(-1)
			}
			a.scroll(rows)
		case key.Matches(msg, k.Top):
			a.cursor, a.top = 0, 0
			a.skipHeader(1)
		case key.Matches(msg, k.Bottom):
			a.cursor = max(len(a.lines)-1, 0)
			a.skipHeader(-1)
			a.scroll(rows)
		default:
			switch msg.String() {
			case "]":
				a.from = a.to
				a.to = a.from.AddDate(0, 0, agendaDays)
				a.cursor, a.top = 0, 0
				return a, a.reload()
			case "[":
				a.to = a.from
				a.from = a.to.AddDate(0, 0, -agendaDays)
				a.cursor, a.top = 0, 0
				return a, a.reload()
			}
		}
		return a, nil
	}
	return a, nil
}

func (a *agenda) scroll(rows int) {
	if a.cursor < a.top {
		a.top = a.cursor
	}
	if a.cursor >= a.top+rows {
		a.top = a.cursor - rows + 1
	}
	if a.top < 0 {
		a.top = 0
	}
	// Keep the day header above the first visible row when possible, so a row
	// is never shown without the day it belongs to.
	if a.top > 0 && a.lines[a.top].header == "" {
		if a.top-1 >= 0 && a.lines[a.top-1].header != "" {
			a.top--
		}
	}
}

func (a *agenda) View(w, h int) string {
	rows := listRows(h)
	if len(a.lines) == 0 {
		msg := "  nothing scheduled"
		if a.loading {
			msg = "  loading…"
		}
		if a.loadErr != nil {
			msg = "  " + a.loadErr.Error()
		}
		return strings.Join(append([]string{msg}, make([]string, rows-1)...), "\n")
	}
	const (
		whenW = 13
		calW  = 12
		rsvpW = 5
	)
	titleW := w - whenW - calW - rsvpW - 8
	if titleW < 10 {
		titleW = 10
	}
	out := make([]string, 0, rows)
	for i := a.top; i < len(a.lines) && len(out) < rows; i++ {
		l := a.lines[i]
		if l.header != "" {
			out = append(out, styleHeader.Render(padCells(l.header, w)))
			continue
		}
		o := &a.occs[l.occ]
		line := "  " + padCells(output.TimeCell(o.Start, o.End, o.AllDay, a.d.loc()), whenW) +
			" " + padCells(o.Title, titleW) +
			" " + padCells(o.CalendarName, calW) +
			" " + padCells(output.RSVP(o.MyResponse), rsvpW)
		line = padCells(line, w)
		if i == a.cursor {
			line = styleSelected.Render(line)
		}
		out = append(out, line)
	}
	for len(out) < rows {
		out = append(out, "")
	}
	return strings.Join(out, "\n")
}

func (a *agenda) footer(w int) string {
	return fmt.Sprintf("%d events · [ ] to page weeks · enter for detail", len(a.occs))
}
