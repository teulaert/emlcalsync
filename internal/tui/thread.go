package tui

import (
	"fmt"
	"strings"

	"charm.land/bubbles/v2/key"
	tea "charm.land/bubbletea/v2"

	"github.com/teulaert/emlcalsync/internal/model"
	"github.com/teulaert/emlcalsync/internal/output"
)

// threadView lists the messages of one conversation, oldest first — the order
// `emlcal mail thread` prints them in, which is the order they were written.
type threadView struct {
	d Deps

	accountID string
	threadID  string
	subject   string

	messages []model.Message
	cursor   int
	top      int

	seq     int
	loading bool
	loadErr error

	removed []removedMsg
}

type removedMsg struct {
	at int
	m  model.Message
}

func newThreadView(d Deps, accountID, threadID, subject string) *threadView {
	return &threadView{d: d, accountID: accountID, threadID: threadID, subject: subject}
}

func (t *threadView) Title() string {
	s := t.subject
	if strings.TrimSpace(s) == "" {
		s = "(no subject)"
	}
	return "thread · " + s
}

func (t *threadView) Init() tea.Cmd { return t.reload() }

func (t *threadView) reload() tea.Cmd {
	t.seq++
	t.loading = true
	return t.d.openThread(t.seq, t.accountID, t.threadID)
}

func (t *threadView) selected() *model.Message {
	if t.cursor < 0 || t.cursor >= len(t.messages) {
		return nil
	}
	return &t.messages[t.cursor]
}

func (t *threadView) targets() []target {
	m := t.selected()
	if m == nil {
		return nil
	}
	return []target{targetOf(m)}
}

func (t *threadView) dropSelected() {
	if t.cursor < 0 || t.cursor >= len(t.messages) {
		return
	}
	t.removed = append(t.removed, removedMsg{at: t.cursor, m: t.messages[t.cursor]})
	t.messages = append(t.messages[:t.cursor], t.messages[t.cursor+1:]...)
	if t.cursor >= len(t.messages) {
		t.cursor = len(t.messages) - 1
	}
	if t.cursor < 0 {
		t.cursor = 0
	}
}

func (t *threadView) restore() {
	if len(t.removed) == 0 {
		return
	}
	r := t.removed[len(t.removed)-1]
	t.removed = t.removed[:len(t.removed)-1]
	if r.at > len(t.messages) {
		r.at = len(t.messages)
	}
	t.messages = append(t.messages[:r.at], append([]model.Message{r.m}, t.messages[r.at:]...)...)
	t.cursor = r.at
}

func (t *threadView) commit() { t.removed = nil }

func (t *threadView) Update(msg tea.Msg, k keymap, w, h int) (screen, tea.Cmd) {
	switch msg := msg.(type) {
	case threadOpened:
		if msg.seq != t.seq {
			return t, nil
		}
		t.loading = false
		t.loadErr = msg.err
		if msg.err == nil {
			t.messages = msg.messages
			if msg.thread != nil {
				t.subject = msg.thread.Subject
			}
			if t.cursor >= len(t.messages) {
				t.cursor = max(len(t.messages)-1, 0)
			}
		}
		return t, nil

	case tea.KeyPressMsg:
		rows := listRows(h)
		switch {
		case key.Matches(msg, k.Down):
			if t.cursor < len(t.messages)-1 {
				t.cursor++
			}
			t.scroll(rows)
		case key.Matches(msg, k.Up):
			if t.cursor > 0 {
				t.cursor--
			}
			t.scroll(rows)
		case key.Matches(msg, k.Top):
			t.cursor, t.top = 0, 0
		case key.Matches(msg, k.Bottom):
			t.cursor = max(len(t.messages)-1, 0)
			t.scroll(rows)
		}
		return t, nil
	}
	return t, nil
}

func (t *threadView) scroll(rows int) {
	if t.cursor < t.top {
		t.top = t.cursor
	}
	if t.cursor >= t.top+rows {
		t.top = t.cursor - rows + 1
	}
	if t.top < 0 {
		t.top = 0
	}
}

func (t *threadView) View(w, h int) string {
	rows := listRows(h)
	if len(t.messages) == 0 {
		msg := "  empty thread"
		if t.loading {
			msg = "  loading…"
		}
		if t.loadErr != nil {
			msg = "  " + t.loadErr.Error()
		}
		return strings.Join(append([]string{msg}, make([]string, rows-1)...), "\n")
	}
	now := t.d.now()
	const (
		markW = 2
		timeW = 9
		fromW = 22
	)
	snipW := w - markW - timeW - fromW - 4
	if snipW < 8 {
		snipW = 8
	}
	out := make([]string, 0, rows)
	for i := t.top; i < len(t.messages) && len(out) < rows; i++ {
		m := &t.messages[i]
		mark := " "
		if m.Flags.Unread {
			mark = "●"
		}
		flags := output.MailFlags(m.Flags, m.HasAttachments)
		snippet := m.Snippet
		if strings.TrimSpace(snippet) == "" {
			snippet = flags
		}
		line := mark + " " + padCells(output.ShortAddr(m.From), fromW) +
			" " + padCells(output.RelTime(m.Received, now), timeW) +
			" " + padCells(snippet, snipW)
		line = padCells(line, w)
		switch {
		case i == t.cursor:
			line = styleSelected.Render(line)
		case m.Flags.Unread:
			line = styleUnread.Render(line)
		}
		out = append(out, line)
	}
	for len(out) < rows {
		out = append(out, "")
	}
	return strings.Join(out, "\n")
}

func (t *threadView) footer(w int) string {
	return fmt.Sprintf("%d messages · enter to read", len(t.messages))
}
