package tui

import (
	"fmt"
	"slices"
	"strings"

	"charm.land/bubbles/v2/key"
	tea "charm.land/bubbletea/v2"

	"github.com/teulaert/emlcalsync/internal/mime"
	"github.com/teulaert/emlcalsync/internal/model"
	"github.com/teulaert/emlcalsync/internal/output"
)

// threadView shows one conversation, newest message first. The store hands
// them back oldest first — the order `emlcal mail thread` prints them in, and
// the order they were written — but on screen the newest belongs on top: it is
// the message the thread was opened for, and a long thread should not have to
// be scrolled to reach the point. `emlcal mail thread` keeps its order; agents
// parse it.
//
// It has two modes. Expanded is the default: every message's text is laid out
// inline and the whole conversation scrolls as one document, so opening a
// thread already puts the mail in front of you. Compact is the old one row per
// message index, which is the faster way to triage a long thread. `t` toggles,
// and the root remembers the choice for the next thread.
type threadView struct {
	d Deps

	accountID string
	threadID  string
	subject   string

	messages []model.Message
	cursor   int
	top      int // compact: first visible row

	expanded bool
	lines    []threadLine // expanded: the laid-out document
	starts   []int        // expanded: first line of each message
	off      int          // expanded: first visible line
	laidOut  int          // width the layout was built for; 0 = none
	dirty    bool
	placed   bool // the opening cursor position has been chosen

	// holdUnread is the message the user has just marked unread by hand, so
	// the automatic mark-read does not undo it while the cursor sits there.
	holdUnread string

	seq     int
	loading bool
	loadErr error

	removed []removedMsg
}

// threadLine is one rendered line of the expanded document, tagged with the
// message it belongs to so a scroll position can name the message being read.
type threadLine struct {
	text string
	msg  int
	head bool
}

type removedMsg struct {
	at int
	m  model.Message
}

func newThreadView(d Deps, accountID, threadID, subject string, expanded bool) *threadView {
	return &threadView{d: d, accountID: accountID, threadID: threadID, subject: subject, expanded: expanded}
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

// setExpanded switches mode, keeping the message being read under the cursor.
func (t *threadView) setExpanded(v bool) tea.Cmd {
	if t.expanded == v {
		return nil
	}
	t.expanded = v
	t.dirty = true
	if v {
		t.off = -1 // View re-anchors on the current message
		return t.markRead()
	}
	return nil
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
	t.dirty, t.off = true, -1
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
	t.dirty, t.off = true, -1
}

func (t *threadView) commit() { t.removed = nil }

// keepUnread stops the expanded view from marking one message read again. The
// user has just marked it unread by hand and the cursor is still on it, so the
// next keystroke would otherwise undo them.
func (t *threadView) keepUnread(remote string) { t.holdUnread = remote }

func (t *threadView) Update(msg tea.Msg, k keymap, w, h int) (screen, tea.Cmd) {
	switch msg := msg.(type) {
	case threadOpened:
		if msg.seq != t.seq {
			return t, nil
		}
		t.loading = false
		t.loadErr = msg.err
		if msg.err != nil {
			return t, nil
		}
		// A reload lands while the thread is being read — the daemon commits
		// every couple of seconds — so hold the reading position rather than
		// jumping back to the top: same message, same offset into it.
		anchor, delta := "", 0
		if m := t.selected(); m != nil {
			anchor, delta = m.RemoteID, t.off-t.startOf(t.cursor)
		}
		t.messages = msg.messages
		slices.Reverse(t.messages)
		if msg.thread != nil {
			t.subject = msg.thread.Subject
		}
		t.dirty = true
		t.layout(w)
		switch {
		case !t.placed:
			t.place()
			t.placed = true
			t.off = -1 // View anchors it, at the size the terminal really is
		case anchor != "":
			if i := t.indexOf(anchor); i >= 0 {
				t.cursor = i
			}
			t.off = t.startOf(t.cursor) + delta
		}
		t.cursor = min(t.cursor, max(len(t.messages)-1, 0))
		if t.off >= 0 {
			t.clamp(listRows(h))
		}
		return t, t.markRead()

	case tea.KeyPressMsg:
		if t.expanded {
			return t, t.expandedKey(msg, k, w, h)
		}
		return t, t.compactKey(msg, k, h)
	}
	return t, nil
}

// place chooses where reading starts: the newest unread message, or the newest
// one of all when the whole thread has been read. Both are at or near the top,
// the messages being newest first.
func (t *threadView) place() {
	for i := range t.messages {
		if t.messages[i].Flags.Unread {
			t.cursor = i
			return
		}
	}
	t.cursor = 0
}

func (t *threadView) indexOf(remote string) int {
	for i := range t.messages {
		if t.messages[i].RemoteID == remote {
			return i
		}
	}
	return -1
}

// markRead marks the message under the cursor read, because in expanded mode
// having the cursor on a message means its text is on screen. Compact mode
// leaves the flag alone; there it is only a row.
func (t *threadView) markRead() tea.Cmd {
	if !t.expanded || t.d.Engine == nil {
		return nil
	}
	m := t.selected()
	if m == nil || !m.Flags.Unread {
		return nil
	}
	if m.RemoteID == t.holdUnread {
		return nil
	}
	t.holdUnread = "" // the cursor has moved on
	ops, _ := flagOps([]target{targetOf(m)}, "unread", false)
	m.Flags.Unread = false
	t.dirty = true
	return t.d.apply("read", ops, nil)
}

func (t *threadView) compactKey(msg tea.KeyPressMsg, k keymap, h int) tea.Cmd {
	rows := listRows(h)
	switch {
	case key.Matches(msg, k.Down):
		if t.cursor < len(t.messages)-1 {
			t.cursor++
		}
	case key.Matches(msg, k.Up):
		if t.cursor > 0 {
			t.cursor--
		}
	case key.Matches(msg, k.Top):
		t.cursor, t.top = 0, 0
	case key.Matches(msg, k.Bottom):
		t.cursor = max(len(t.messages)-1, 0)
	default:
		return nil
	}
	t.scrollCompact(rows)
	return nil
}

func (t *threadView) expandedKey(msg tea.KeyPressMsg, k keymap, w, h int) tea.Cmd {
	t.ensure(w)
	rows := listRows(h)
	switch {
	// j/k move between messages here, as they do on every other screen. They
	// used to scroll a line and let the cursor fall out of whichever message
	// the top of the window showed, which meant they did nothing at all once
	// the document was clamped at its end — and a thread that fits on one
	// screen is clamped from the moment it opens.
	case key.Matches(msg, k.Down):
		return t.toMessage(t.cursor+1, rows)
	case key.Matches(msg, k.Up):
		return t.toMessage(t.cursor-1, rows)
	case key.Matches(msg, k.Top):
		return t.toMessage(0, rows)
	case key.Matches(msg, k.Bottom):
		return t.toMessage(len(t.messages)-1, rows)
	case key.Matches(msg, k.PageDown):
		t.off += rows - 1
	case key.Matches(msg, k.PageUp):
		t.off -= rows - 1
	case key.Matches(msg, k.LineDown):
		t.off++
	case key.Matches(msg, k.LineUp):
		t.off--
	default:
		return nil
	}
	// Reading through a message longer than the window eventually crosses
	// into the next one; the cursor follows what the window is showing.
	t.clamp(rows)
	if t.off < len(t.lines) {
		t.cursor = t.lines[t.off].msg
	}
	return t.markRead()
}

// toMessage moves the cursor to message i and scrolls its header into view.
// The cursor is set outright rather than read back off the scroll position: a
// thread shorter than the window cannot scroll at all, and one scrolled to its
// end cannot scroll further, so a cursor that only followed the scroll would
// sit still in both.
func (t *threadView) toMessage(i, rows int) tea.Cmd {
	t.cursor = min(max(i, 0), max(len(t.messages)-1, 0))
	t.off = t.startOf(t.cursor)
	t.clamp(rows)
	return t.markRead()
}

// clamp keeps the expanded offset inside the document.
func (t *threadView) clamp(rows int) {
	if maxOff := len(t.lines) - rows; t.off > maxOff {
		t.off = maxOff
	}
	if t.off < 0 {
		t.off = 0
	}
}

func (t *threadView) scrollCompact(rows int) {
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

// startOf is the first line of message i, clamped to the document.
func (t *threadView) startOf(i int) int {
	if i < 0 || len(t.starts) == 0 {
		return 0
	}
	if i >= len(t.starts) {
		return t.starts[len(t.starts)-1]
	}
	return t.starts[i]
}

func (t *threadView) ensure(w int) {
	if t.dirty || t.laidOut != w {
		t.layout(w)
	}
}

// layout renders the whole conversation into lines once, so scrolling costs
// nothing and every line knows which message it came from.
func (t *threadView) layout(w int) {
	t.laidOut, t.dirty = w, false
	t.lines, t.starts = t.lines[:0], t.starts[:0]
	if !t.expanded || w <= 0 {
		return
	}
	for i := range t.messages {
		m := &t.messages[i]
		t.starts = append(t.starts, len(t.lines))
		if i > 0 {
			t.lines = append(t.lines, threadLine{msg: i})
		}
		t.lines = append(t.lines, threadLine{msg: i, head: true, text: t.headerText(m, w)})
		for _, l := range wrapCells(threadBody(m), w-2) {
			if l != "" {
				l = "  " + l
			}
			t.lines = append(t.lines, threadLine{msg: i, text: l})
		}
	}
}

func (t *threadView) headerText(m *model.Message, w int) string {
	mark := "  "
	if m.Flags.Unread {
		mark = "● "
	}
	s := mark + m.From.String() + " · " + m.Date.In(t.d.loc()).Format("Mon 2 Jan 15:04")
	if f := output.MailFlags(m.Flags, m.HasAttachments); f != "" {
		s += " · " + f
	}
	return padCells(s, w)
}

// threadBody is the text the expanded view shows. GetThread already carries
// the bodies, so there is nothing to load; what it cannot carry is a message
// stored as an envelope-only stub (DESIGN.md §16), which has no body until the
// raw bytes are fetched. Enter opens the reader, which does fetch.
func threadBody(m *model.Message) string {
	if s := strings.TrimSpace(m.TextBody); s != "" {
		return mime.StripQuotes(m.TextBody)
	}
	if !m.RawComplete {
		return "(too large to archive in full — enter to fetch and read it)"
	}
	return "(no text)"
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
	if t.expanded {
		return t.viewExpanded(w, rows)
	}
	return t.viewCompact(w, rows)
}

func (t *threadView) viewExpanded(w, rows int) string {
	t.ensure(w)
	if t.off < 0 { // a mode switch left the offset to be re-anchored
		t.off = t.startOf(t.cursor)
	}
	t.clamp(rows)
	out := make([]string, 0, rows)
	for i := t.off; i < len(t.lines) && len(out) < rows; i++ {
		l := t.lines[i]
		line := l.text
		switch {
		case !l.head:
		case l.msg == t.cursor:
			line = styleSelected.Render(line)
		case t.messages[l.msg].Flags.Unread:
			line = styleUnread.Render(line)
		default:
			line = styleFaint.Render(line)
		}
		out = append(out, line)
	}
	for len(out) < rows {
		out = append(out, "")
	}
	return strings.Join(out, "\n")
}

func (t *threadView) viewCompact(w, rows int) string {
	// The cursor may have been placed, or carried over from the expanded
	// view, without a key press to scroll the window to it.
	t.scrollCompact(rows)
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
	if t.expanded {
		return fmt.Sprintf("message %d of %d · j/k moves · t collapses",
			min(t.cursor+1, len(t.messages)), len(t.messages))
	}
	return fmt.Sprintf("%d messages · enter to read · t expands", len(t.messages))
}
