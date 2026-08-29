package tui

import (
	"fmt"
	"strings"

	"charm.land/bubbles/v2/viewport"
	tea "charm.land/bubbletea/v2"

	"github.com/teulaert/emlcalsync/internal/model"
	"github.com/teulaert/emlcalsync/internal/output"
)

// reader shows one message. The body goes through a viewport because wrapping
// and scrolling long mail is exactly the fiddly work worth borrowing.
type reader struct {
	d Deps

	accountID string
	remote    string

	msg  *model.Message
	body string

	vp      viewport.Model
	ready   bool
	seq     int
	loading bool
	loadErr error
}

func newReader(d Deps, accountID, remote string) *reader {
	return &reader{d: d, accountID: accountID, remote: remote}
}

func (r *reader) Title() string {
	if r.msg == nil {
		return "message"
	}
	s := r.msg.Subject
	if strings.TrimSpace(s) == "" {
		s = "(no subject)"
	}
	return s
}

func (r *reader) Init() tea.Cmd { return r.reload() }

func (r *reader) reload() tea.Cmd {
	r.seq++
	r.loading = true
	return r.d.loadBody(r.seq, r.accountID, r.remote)
}

// show swaps the reader onto another message, keeping the screen where it is.
// Following a triage to the next message in the thread is the one thing that
// changes what an open reader shows; everything else pushes a new one.
func (r *reader) show(accountID, remote string) tea.Cmd {
	r.accountID, r.remote = accountID, remote
	r.msg, r.body, r.loadErr = nil, "", nil
	if r.ready {
		r.vp.SetContent("")
		r.vp.GotoTop()
	}
	return r.reload()
}

func (r *reader) targets() []target {
	if r.msg == nil {
		return nil
	}
	return []target{targetOf(r.msg)}
}

func (r *reader) Update(msg tea.Msg, k keymap, w, h int) (screen, tea.Cmd) {
	switch msg := msg.(type) {
	case bodyLoaded:
		if msg.seq != r.seq {
			return r, nil // a newer body is on its way; do not paint this one
		}
		r.loading = false
		r.loadErr = msg.err
		if msg.err == nil {
			r.msg = msg.msg
			r.body = msg.body
			r.setContent(w, h)
		}
		return r, nil

	case tea.KeyPressMsg:
		r.ensure(w, h)
		var cmd tea.Cmd
		r.vp, cmd = r.vp.Update(msg)
		return r, cmd
	}
	return r, nil
}

func (r *reader) ensure(w, h int) {
	body := listRows(h)
	head := len(r.headerLines(w))
	vh := body - head - 1
	if vh < 1 {
		vh = 1
	}
	if !r.ready {
		r.vp = viewport.New(viewport.WithWidth(w), viewport.WithHeight(vh))
		r.vp.SoftWrap = true
		r.ready = true
		r.vp.SetContent(r.body)
		return
	}
	if r.vp.Width() != w || r.vp.Height() != vh {
		r.vp.SetWidth(w)
		r.vp.SetHeight(vh)
	}
}

func (r *reader) setContent(w, h int) {
	r.ensure(w, h)
	r.vp.SetContent(r.body)
	r.vp.GotoTop()
}

func (r *reader) headerLines(w int) []string {
	if r.msg == nil {
		return []string{""}
	}
	m := r.msg
	var to []string
	for _, a := range m.To {
		to = append(to, a.String())
	}
	// The id first: it is what anything else that reads the archive -- an
	// agent with the skill -- needs to be told, and y copies it.
	lines := []string{
		truncCells("Id:      "+m.PublicID(), w),
		truncCells("From:    "+m.From.String(), w),
	}
	if len(to) > 0 {
		lines = append(lines, truncCells("To:      "+strings.Join(to, ", "), w))
	}
	lines = append(lines,
		truncCells("Date:    "+m.Date.In(r.d.loc()).Format("Mon, 02 Jan 2006 15:04"), w),
		truncCells("Subject: "+m.Subject, w),
	)
	if m.HasAttachments {
		lines = append(lines, truncCells("Attach:  yes", w))
	}
	for i, l := range lines {
		lines[i] = styleFaint.Render(l)
	}
	return lines
}

func (r *reader) View(w, h int) string {
	rows := listRows(h)
	if r.msg == nil {
		msg := "  loading…"
		if r.loadErr != nil {
			msg = "  " + r.loadErr.Error()
		} else if !r.loading {
			msg = "  (not found)"
		}
		return strings.Join(append([]string{msg}, make([]string, rows-1)...), "\n")
	}
	r.ensure(w, h)
	out := r.headerLines(w)
	out = append(out, "")
	out = append(out, strings.Split(r.vp.View(), "\n")...)
	for len(out) < rows {
		out = append(out, "")
	}
	return strings.Join(out[:rows], "\n")
}

func (r *reader) footer(w int) string {
	if r.msg == nil {
		return ""
	}
	f := output.MailFlags(r.msg.Flags, r.msg.HasAttachments)
	if f == "" {
		f = "-"
	}
	return fmt.Sprintf("%s · %s · %d%%", r.msg.PublicID(), f, int(r.vp.ScrollPercent()*100))
}
