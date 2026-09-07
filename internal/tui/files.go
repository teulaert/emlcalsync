package tui

import (
	"strings"

	"charm.land/bubbles/v2/key"
	tea "charm.land/bubbletea/v2"

	"github.com/teulaert/emlcalsync/internal/model"
	"github.com/teulaert/emlcalsync/internal/output"
)

// filesView is the parts of one message, or of a whole conversation, as a
// list to pick from: enter hands the one under the cursor to whatever the
// desktop opens that kind of file with, w writes it to the downloads folder.
//
// It exists because the reader can name a file but not show it. A PDF is not
// text, and the invoice, the contract, the photo -- the reason the message
// was sent -- live in it and nowhere else. `mail attachment get` is the same
// thing from the shell; this is it without leaving the screen.
//
// Inline parts are listed too, marked as such, where the reader only counts
// them. A row marked A for nothing but a signature logo is a question, and
// this is the screen that answers it.
type filesView struct {
	d Deps

	accountID string
	remote    string // empty: the whole thread
	threadID  string
	subject   string

	rows   []fileRow
	cursor int
	top    int // first visible line

	// busy names the file being fetched, so the footer can say so: a part
	// of an envelope-only message comes off the provider, which takes a
	// moment, and a second enter meanwhile would fetch it twice.
	busy string

	seq     int
	loading bool
	loadErr error
}

func newFilesView(d Deps, accountID, remote, threadID, subject string) *filesView {
	return &filesView{d: d, accountID: accountID, remote: remote, threadID: threadID, subject: subject}
}

func (f *filesView) Title() string {
	s := strings.TrimSpace(f.subject)
	if s == "" {
		s = "(no subject)"
	}
	return "files · " + s
}

func (f *filesView) Init() tea.Cmd { return f.reload() }

func (f *filesView) reload() tea.Cmd {
	f.seq++
	f.loading = true
	return f.d.loadFiles(f.seq, f.accountID, f.remote, f.threadID)
}

// targets is nil on purpose: e and d here would act on a message whose files
// are still on screen, and the screen underneath is where those keys belong.
func (f *filesView) targets() []target { return nil }

// selected is the row under the cursor, or nil while there is none.
func (f *filesView) selected() *fileRow {
	if f.cursor < 0 || f.cursor >= len(f.rows) {
		return nil
	}
	return &f.rows[f.cursor]
}

// spansThread is whether the rows come from more than one message, which is
// when each message gets a heading of its own.
func (f *filesView) spansThread() bool {
	for i := 1; i < len(f.rows); i++ {
		if f.rows[i].msg.RemoteID != f.rows[0].msg.RemoteID {
			return true
		}
	}
	return false
}

func (f *filesView) Update(msg tea.Msg, k keymap, w, h int) (screen, tea.Cmd) {
	switch msg := msg.(type) {
	case filesLoaded:
		if msg.seq != f.seq {
			return f, nil
		}
		f.loading = false
		f.loadErr = msg.err
		if msg.err == nil {
			f.rows = msg.rows
			if f.cursor >= len(f.rows) {
				f.cursor = max(len(f.rows)-1, 0)
			}
		}
		return f, nil

	case fileOpened:
		f.busy = ""
		return f, nil

	case tea.KeyPressMsg:
		switch {
		case key.Matches(msg, k.Up):
			f.cursor = max(f.cursor-1, 0)
		case key.Matches(msg, k.Down):
			f.cursor = min(f.cursor+1, max(len(f.rows)-1, 0))
		case key.Matches(msg, k.Top):
			f.cursor = 0
		case key.Matches(msg, k.Bottom):
			f.cursor = max(len(f.rows)-1, 0)
		case key.Matches(msg, k.PageUp):
			f.cursor = max(f.cursor-listRows(h), 0)
		case key.Matches(msg, k.PageDown):
			f.cursor = min(f.cursor+listRows(h), max(len(f.rows)-1, 0))
		}
		return f, nil
	}
	return f, nil
}

// fileLine is one drawn line: a row of the list, or a heading (row < 0).
type fileLine struct {
	text string
	row  int
}

func (f *filesView) View(w, h int) string {
	rows := listRows(h)
	if len(f.rows) == 0 {
		msg := "  loading…"
		if f.loadErr != nil {
			msg = "  " + f.loadErr.Error()
		} else if !f.loading {
			msg = "  (nothing attached)"
		}
		return strings.Join(append([]string{msg}, make([]string, rows-1)...), "\n")
	}
	lines := f.layout(w)
	// Keep the cursor's line in view, the way the compact thread does.
	at := 0
	for i, l := range lines {
		if l.row == f.cursor {
			at = i
			break
		}
	}
	if at < f.top {
		f.top = at
	}
	if at >= f.top+rows {
		f.top = at - rows + 1
	}
	out := make([]string, 0, rows)
	for i := f.top; i < len(lines) && len(out) < rows; i++ {
		out = append(out, lines[i].text)
	}
	for len(out) < rows {
		out = append(out, "")
	}
	return strings.Join(out, "\n")
}

// layout draws every row: name, type, size and where the part sits, which
// is what `mail attachment get` takes when the file is wanted from a shell.
func (f *filesView) layout(w int) []fileLine {
	const (
		typeW = 24
		sizeW = 8
		tailW = 18
	)
	nameW := w - 2 - typeW - sizeW - tailW - 6
	if nameW < 12 {
		nameW = 12
	}
	spans := f.spansThread()
	var lines []fileLine
	prev := ""
	for i, r := range f.rows {
		if spans && r.msg.RemoteID != prev {
			if prev != "" {
				lines = append(lines, fileLine{row: -1})
			}
			head := r.msg.From.String() + " · " + r.msg.Date.In(f.d.loc()).Format("Mon 2 Jan 15:04")
			lines = append(lines, fileLine{row: -1, text: styleFaint.Render(truncCells("  "+head, w))})
			prev = r.msg.RemoteID
		}
		tail := "part " + r.att.PartPath
		if r.att.Inline {
			tail += " · inline"
		}
		line := "  " + padCells(r.name(), nameW) + "  " +
			padCells(r.att.ContentType, typeW) + "  " +
			padCells(output.HumanSize(r.att.Size), sizeW) + "  " +
			tail
		line = padCells(line, w)
		if i == f.cursor {
			line = styleSelected.Render(line)
		}
		lines = append(lines, fileLine{row: i, text: line})
	}
	return lines
}

func (f *filesView) footer(w int) string {
	if f.busy != "" {
		return "fetching " + f.busy + "…"
	}
	id := model.ThreadPublicID(f.accountID, f.threadID)
	if f.remote != "" {
		id = model.MessagePublicID(f.accountID, f.remote)
	}
	return "enter open · w save to " + shortHome(f.d.downloadDir()) + " · " + id
}
