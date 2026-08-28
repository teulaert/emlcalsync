package tui

import (
	"context"
	"fmt"
	"strings"
	"time"

	"charm.land/bubbles/v2/key"
	tea "charm.land/bubbletea/v2"

	"github.com/teulaert/emlcalsync/internal/model"
	"github.com/teulaert/emlcalsync/internal/output"
	"github.com/teulaert/emlcalsync/internal/store"
)

// pageSize is how many threads one query fetches. Measured against a real
// 40k-thread archive a page costs well under a frame, so the list simply grows
// by pages as the cursor nears the end rather than trying to hold everything.
const pageSize = 100

// mailbox filters the list offers, in the order M cycles them.
var mailboxCycle = []struct {
	label string
	role  string
}{
	{"inbox", string(model.RoleInbox)},
	{"all", ""},
	{"flagged", string(model.RoleInbox)}, // narrowed further by flagged below
	{"drafts", string(model.RoleDrafts)},
	{"sent", string(model.RoleSent)},
	{"archive", string(model.RoleArchive)},
}

type mailList struct {
	d Deps

	threads []model.Thread
	cursor  int
	top     int // first visible row

	mailbox  int // index into mailboxCycle
	account  int // 0 = all, else index+1 into accounts
	accounts []string

	query     string
	searching bool
	input     string

	seq     int
	loading bool
	loadErr error
	atEnd   bool

	// resetCursor is set by the loads that change what the list is showing --
	// a different mailbox, account or query -- where starting at the top is
	// the right answer. A plain refresh leaves it clear and keeps the cursor
	// on the thread it was on.
	resetCursor bool

	// removed holds rows taken out optimistically, so a failed action can put
	// them back where they were.
	removed []removedRow
}

type removedRow struct {
	at int
	t  model.Thread
}

func newMailList(d Deps, accounts []string) *mailList {
	return &mailList{d: d, accounts: accounts}
}

func (m *mailList) Title() string {
	who := "all accounts"
	if m.account > 0 && m.account <= len(m.accounts) {
		who = m.accounts[m.account-1]
	}
	what := mailboxCycle[m.mailbox].label
	if m.query != "" {
		what = "search: " + m.query
	}
	// No "mail ·" prefix: the header's tab strip already says which stack
	// this is.
	return fmt.Sprintf("%s · %s", what, who)
}

// filter builds the store query for the current view.
func (m *mailList) filter(offset int) store.MessageFilter {
	f := store.MessageFilter{Limit: pageSize, Offset: offset}
	if m.account > 0 && m.account <= len(m.accounts) {
		f.Accounts = []string{m.accounts[m.account-1]}
	} else {
		f.Accounts = m.d.Accounts
	}
	mb := mailboxCycle[m.mailbox]
	f.MailboxRole = mb.role
	if mb.label == "flagged" {
		t := true
		f.Flagged = &t
	}
	return f
}

func (m *mailList) Init() tea.Cmd { return m.reload() }

// reload re-queries the current view. The cursor stays on the thread it was
// on: the daemon commits every couple of seconds and each commit reloads the
// visible screen, so a reload that jumped to the top would yank the selection
// away seconds after every action.
func (m *mailList) reload() tea.Cmd { return m.load(false) }

// reloadFresh is reload for a view that has just changed -- another mailbox,
// account or query -- where the old cursor means nothing.
func (m *mailList) reloadFresh() tea.Cmd { return m.load(true) }

func (m *mailList) load(reset bool) tea.Cmd {
	m.seq++
	m.loading = true
	m.atEnd = false
	m.resetCursor = reset
	return m.d.loadThreads(m.seq, m.filter(0), m.query, false)
}

func (m *mailList) loadMore() tea.Cmd {
	if m.loading || m.atEnd || m.query != "" {
		return nil
	}
	m.seq++
	m.loading = true
	return m.d.loadThreads(m.seq, m.filter(len(m.threads)), "", true)
}

func (m *mailList) targets() []target {
	t := m.selected()
	if t == nil {
		return nil
	}
	// A thread row stands for every message in it, which is what makes "e"
	// mean the same thing here as `emlcal mail archive` on each id. One
	// indexed thread lookup is sub-millisecond on a real archive, so it runs
	// inline rather than costing a round trip through the update loop.
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	_, msgs, err := m.d.Store.GetThread(ctx, t.AccountID, t.ThreadID, false)
	if err != nil || len(msgs) == 0 {
		return nil
	}
	out := make([]target, 0, len(msgs))
	for i := range msgs {
		out = append(out, targetOf(&msgs[i]))
	}
	return out
}

func (m *mailList) selected() *model.Thread {
	if m.cursor < 0 || m.cursor >= len(m.threads) {
		return nil
	}
	return &m.threads[m.cursor]
}

// dropSelected removes the row under the cursor, remembering it for restore.
func (m *mailList) dropSelected() {
	if m.cursor < 0 || m.cursor >= len(m.threads) {
		return
	}
	m.removed = append(m.removed, removedRow{at: m.cursor, t: m.threads[m.cursor]})
	m.threads = append(m.threads[:m.cursor], m.threads[m.cursor+1:]...)
	if m.cursor >= len(m.threads) {
		m.cursor = len(m.threads) - 1
	}
	if m.cursor < 0 {
		m.cursor = 0
	}
}

// restore puts the last optimistically removed row back.
func (m *mailList) restore() {
	if len(m.removed) == 0 {
		return
	}
	r := m.removed[len(m.removed)-1]
	m.removed = m.removed[:len(m.removed)-1]
	if r.at > len(m.threads) {
		r.at = len(m.threads)
	}
	m.threads = append(m.threads[:r.at], append([]model.Thread{r.t}, m.threads[r.at:]...)...)
	m.cursor = r.at
}

func (m *mailList) commit() { m.removed = nil }

// anchor names the row the cursor is on, so it can be found again in the
// result of a reload.
func (m *mailList) anchor() (string, int) {
	t := m.selected()
	if t == nil {
		return "", m.cursor
	}
	return t.AccountID + "\x00" + t.ThreadID, m.cursor
}

// reanchor puts the cursor back on the thread it was on. When that thread is
// gone -- it was just archived or trashed, or the daemon moved it -- the
// cursor holds its position in the list instead, which lands it on whichever
// row took the old one's place. That is what every mail client does, and it
// is what makes deleting a run of messages possible without re-navigating
// after each one.
func (m *mailList) reanchor(key string, at int) {
	if key != "" {
		for i := range m.threads {
			if m.threads[i].AccountID+"\x00"+m.threads[i].ThreadID == key {
				m.cursor = i
				return
			}
		}
	}
	m.cursor = min(at, len(m.threads)-1)
	if m.cursor < 0 {
		m.cursor = 0
	}
}

func (m *mailList) Update(msg tea.Msg, k keymap, w, h int) (screen, tea.Cmd) {
	switch msg := msg.(type) {
	case threadsLoaded:
		if msg.seq != m.seq {
			return m, nil // a newer query is already in flight
		}
		m.loading = false
		m.loadErr = msg.err
		if msg.err != nil {
			return m, nil
		}
		if msg.append {
			if len(msg.threads) == 0 {
				m.atEnd = true
			}
			m.threads = append(m.threads, msg.threads...)
		} else {
			anchor, at := m.anchor()
			m.threads = msg.threads
			if m.resetCursor {
				m.cursor, m.top = 0, 0
			} else {
				m.reanchor(anchor, at)
				m.scroll(listRows(h))
			}
			if len(msg.threads) < pageSize {
				m.atEnd = true
			}
		}
		return m, nil

	case tea.KeyPressMsg:
		if m.searching {
			return m.searchKey(msg)
		}
		return m.navKey(msg, k, h)
	}
	return m, nil
}

func (m *mailList) searchKey(msg tea.KeyPressMsg) (screen, tea.Cmd) {
	switch msg.String() {
	case "enter":
		m.searching = false
		m.query = strings.TrimSpace(m.input)
		return m, m.reloadFresh()
	case "esc":
		m.searching = false
		m.input = ""
		return m, nil
	case "backspace":
		if r := []rune(m.input); len(r) > 0 {
			m.input = string(r[:len(r)-1])
		}
		return m, nil
	case "ctrl+u":
		m.input = ""
		return m, nil
	}
	if s := msg.String(); len([]rune(s)) == 1 {
		m.input += s
	}
	return m, nil
}

func (m *mailList) navKey(msg tea.KeyPressMsg, k keymap, h int) (screen, tea.Cmd) {
	rows := listRows(h)
	switch {
	case key.Matches(msg, k.Down):
		if m.cursor < len(m.threads)-1 {
			m.cursor++
		}
		var cmd tea.Cmd
		if m.cursor > len(m.threads)-10 {
			cmd = m.loadMore()
		}
		m.scroll(rows)
		return m, cmd
	case key.Matches(msg, k.Up):
		if m.cursor > 0 {
			m.cursor--
		}
		m.scroll(rows)
		return m, nil
	case key.Matches(msg, k.PageDown):
		m.cursor = min(m.cursor+rows, len(m.threads)-1)
		if m.cursor < 0 {
			m.cursor = 0
		}
		m.scroll(rows)
		return m, m.loadMore()
	case key.Matches(msg, k.PageUp):
		m.cursor = max(m.cursor-rows, 0)
		m.scroll(rows)
		return m, nil
	case key.Matches(msg, k.Top):
		m.cursor, m.top = 0, 0
		return m, nil
	case key.Matches(msg, k.Bottom):
		m.cursor = max(len(m.threads)-1, 0)
		m.scroll(rows)
		return m, m.loadMore()
	case key.Matches(msg, k.Search):
		m.searching = true
		m.input = m.query
		return m, nil
	case key.Matches(msg, k.Mailbox):
		m.mailbox = (m.mailbox + 1) % len(mailboxCycle)
		m.query = ""
		return m, m.reloadFresh()
	case key.Matches(msg, k.Account):
		m.account = (m.account + 1) % (len(m.accounts) + 1)
		return m, m.reloadFresh()
	}
	return m, nil
}

func (m *mailList) scroll(rows int) {
	if m.cursor < m.top {
		m.top = m.cursor
	}
	if m.cursor >= m.top+rows {
		m.top = m.cursor - rows + 1
	}
	// A reload can shrink the list under a window that was scrolled down;
	// without this the view draws blank rows below the last thread.
	if m.top > len(m.threads)-rows {
		m.top = len(m.threads) - rows
	}
	if m.top < 0 {
		m.top = 0
	}
}

// listRows is how many rows a screen may draw. The root has already taken the
// title and status line out of the height it passes down, so a screen fills
// every line it is given -- anything less and the frame comes up short.
func listRows(h int) int {
	if h < 1 {
		return 1
	}
	return h
}

func (m *mailList) View(w, h int) string {
	rows := listRows(h)
	if len(m.threads) == 0 {
		msg := "  no messages"
		if m.loading {
			msg = "  loading…"
		}
		if m.loadErr != nil {
			msg = "  " + m.loadErr.Error()
		}
		return strings.Join(append([]string{msg}, make([]string, rows-1)...), "\n")
	}

	// Column budget: account and the right-hand columns are fixed, and the
	// subject takes whatever is left, because that is the column a person
	// actually reads.
	acctW := 0
	for i := range m.threads {
		acctW = max(acctW, len(m.threads[i].AccountID))
	}
	acctW = min(acctW, 10)
	const (
		markW = 2
		cntW  = 3
		timeW = 9
		partW = 18
		gaps  = 5
	)
	subjW := w - acctW - markW - cntW - timeW - partW - gaps
	if subjW < 10 {
		// Narrow terminal: drop the participants column before the subject.
		partW2 := 0
		subjW = w - acctW - markW - cntW - timeW - partW2 - 4
		if subjW < 8 {
			subjW = 8
		}
		return m.rows(w, rows, acctW, subjW, 0, cntW, timeW)
	}
	return m.rows(w, rows, acctW, subjW, partW, cntW, timeW)
}

func (m *mailList) rows(w, rows, acctW, subjW, partW, cntW, timeW int) string {
	out := make([]string, 0, rows)
	now := m.d.now()
	for i := m.top; i < len(m.threads) && len(out) < rows; i++ {
		t := &m.threads[i]
		mark := " "
		if t.UnreadCount > 0 {
			mark = "●"
		}
		var parts []string
		for _, p := range t.Participants {
			parts = append(parts, output.ShortAddr(p))
		}
		subject := t.Subject
		if strings.TrimSpace(subject) == "" {
			subject = "(no subject)"
		}
		cnt := ""
		if t.MessageCount > 1 {
			cnt = fmt.Sprintf("%d", t.MessageCount)
		}

		line := padCells(t.AccountID, acctW) + " " + mark + " " + padCells(subject, subjW)
		if partW > 0 {
			line += " " + padCells(strings.Join(parts, ", "), partW)
		}
		line += " " + padCells(cnt, cntW) + " " + padCells(output.RelTime(t.Last, now), timeW)
		line = padCells(line, w)

		switch {
		case i == m.cursor:
			line = styleSelected.Render(line)
		case t.UnreadCount > 0:
			line = styleUnread.Render(line)
		}
		out = append(out, line)
	}
	for len(out) < rows {
		out = append(out, "")
	}
	return strings.Join(out, "\n")
}

// footer is the list's own status line contribution: search prompt or counts.
func (m *mailList) footer(w int) string {
	if m.searching {
		return padCells("/"+m.input+"█", w)
	}
	s := fmt.Sprintf("%d threads", len(m.threads))
	if !m.atEnd {
		s += "+"
	}
	if m.loading {
		s += " · loading…"
	}
	return s
}
