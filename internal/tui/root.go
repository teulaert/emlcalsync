package tui

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"time"

	"charm.land/bubbles/v2/key"
	tea "charm.land/bubbletea/v2"

	"github.com/teulaert/emlcalsync/internal/model"
)

// screen is one full-window view. The root owns a stack of them, so Esc is
// always "pop" and there is exactly one place that decides what a global key
// means.
type screen interface {
	Init() tea.Cmd
	Update(msg tea.Msg, k keymap, w, h int) (screen, tea.Cmd)
	View(w, h int) string
	Title() string
	footer(w int) string
	reload() tea.Cmd
	targets() []target
}

// triageable is implemented by the screens whose rows can be acted on, so the
// root can update them optimistically without knowing which one it holds.
type triageable interface {
	dropSelected()
	restore()
	commit()
}

type root struct {
	d    Deps
	keys keymap

	w, h int

	mail  []screen // mail stack: list → thread → reader
	cal   []screen // calendar stack: agenda → event
	onCal bool

	watcher *dbWatcher

	status    string
	statusSeq int
	undo      *undoRecord
	showHelp  bool
	quitting  bool
}

func newRoot(d Deps) *root {
	accounts := d.Accounts
	r := &root{d: d, keys: defaultKeys()}
	r.mail = []screen{newMailList(d, accounts)}
	r.cal = []screen{newAgenda(d)}
	return r
}

func (r *root) stack() []screen {
	if r.onCal {
		return r.cal
	}
	return r.mail
}

func (r *root) setStack(s []screen) {
	if r.onCal {
		r.cal = s
	} else {
		r.mail = s
	}
}

func (r *root) top() screen {
	s := r.stack()
	return s[len(s)-1]
}

func (r *root) push(s screen) tea.Cmd {
	r.setStack(append(r.stack(), s))
	return s.Init()
}

// pop returns false when there is nothing left to pop, which is the signal to
// quit.
func (r *root) pop() bool {
	s := r.stack()
	if len(s) <= 1 {
		return false
	}
	r.setStack(s[:len(s)-1])
	return true
}

func (r *root) Init() tea.Cmd {
	r.watcher = newDBWatcher(context.Background(), r.d.Store)
	return tea.Batch(r.mail[0].Init(), r.cal[0].Init(), poll())
}

func (r *root) Update(msg tea.Msg) (tea.Model, tea.Cmd) {
	switch msg := msg.(type) {
	case tea.WindowSizeMsg:
		r.w, r.h = msg.Width, msg.Height
		return r, nil

	case tickMsg:
		var cmds []tea.Cmd
		ctx, cancel := context.WithTimeout(context.Background(), time.Second)
		changed := r.watcher.changed(ctx)
		cancel()
		if changed {
			// The daemon committed something. Only the visible screen is
			// re-queried; the other stack refreshes when it is switched to.
			cmds = append(cmds, r.top().reload())
		}
		if r.undo != nil && !r.undo.live(time.Now()) {
			r.undo = nil
			r.status = ""
		}
		cmds = append(cmds, poll())
		return r, tea.Batch(cmds...)

	case statusExpired:
		if msg.seq == r.statusSeq {
			r.status = ""
		}
		return r, nil

	case applied:
		return r, r.onApplied(msg)

	case tea.KeyPressMsg:
		return r.onKey(msg)
	}

	s, cmd := r.top().Update(msg, r.keys, r.w, r.bodyHeight())
	r.replaceTop(s)
	return r, cmd
}

func (r *root) replaceTop(s screen) {
	st := r.stack()
	st[len(st)-1] = s
	r.setStack(st)
}

func (r *root) bodyHeight() int {
	h := r.h - 2 // title + status
	if h < 3 {
		return 3
	}
	return h
}

// searching reports whether the top screen is capturing text, in which case
// global single-letter keys must not fire.
func (r *root) searching() bool {
	ml, ok := r.top().(*mailList)
	return ok && ml.searching
}

func (r *root) onKey(msg tea.KeyPressMsg) (tea.Model, tea.Cmd) {
	if r.showHelp {
		r.showHelp = false
		return r, nil
	}
	if r.searching() {
		s, cmd := r.top().Update(msg, r.keys, r.w, r.bodyHeight())
		r.replaceTop(s)
		return r, cmd
	}

	switch {
	case key.Matches(msg, r.keys.Help):
		r.showHelp = true
		return r, nil

	case key.Matches(msg, r.keys.Quit):
		if msg.String() == "ctrl+c" || !r.pop() {
			r.quitting = true
			r.watcher.Close()
			return r, tea.Quit
		}
		return r, nil

	case key.Matches(msg, r.keys.Back):
		if !r.pop() {
			r.quitting = true
			r.watcher.Close()
			return r, tea.Quit
		}
		return r, nil

	case key.Matches(msg, r.keys.Mail):
		r.onCal = false
		return r, nil

	case key.Matches(msg, r.keys.Calendar):
		r.onCal = true
		return r, nil

	case key.Matches(msg, r.keys.Switch):
		r.onCal = !r.onCal
		return r, nil

	case key.Matches(msg, r.keys.Refresh):
		r.nudgeDaemon()
		r.note("refreshing…")
		return r, r.top().reload()

	case key.Matches(msg, r.keys.Undo):
		return r, r.applyUndo()

	case key.Matches(msg, r.keys.Open):
		return r, r.open()

	case key.Matches(msg, r.keys.Archive):
		return r, r.triage("archive")

	case key.Matches(msg, r.keys.Trash):
		return r, r.triage("trash")

	case key.Matches(msg, r.keys.ToggleRead):
		return r, r.toggleFlag("unread")

	case key.Matches(msg, r.keys.Star):
		return r, r.toggleFlag("flagged")
	}

	// RSVP only means something on an event.
	if ev, ok := r.top().(*eventView); ok && ev.ev != nil {
		if p, ok := ev.rsvp(msg.String()); ok {
			r.note("sending RSVP…")
			return r, r.d.apply(string(p), respondOp(ev.accountID, ev.calRemote, ev.remote, p), nil)
		}
	}

	s, cmd := r.top().Update(msg, r.keys, r.w, r.bodyHeight())
	r.replaceTop(s)
	return r, cmd
}

// open descends one level in the current stack.
func (r *root) open() tea.Cmd {
	switch s := r.top().(type) {
	case *mailList:
		t := s.selected()
		if t == nil {
			return nil
		}
		return r.push(newThreadView(r.d, t.AccountID, t.ThreadID, t.Subject))
	case *threadView:
		m := s.selected()
		if m == nil {
			return nil
		}
		// Opening a message marks it read, the way every mail client does.
		var cmds []tea.Cmd
		cmds = append(cmds, r.push(newReader(r.d, m.AccountID, m.RemoteID)))
		if m.Flags.Unread {
			ops, _ := flagOps([]target{targetOf(m)}, "unread", false)
			m.Flags.Unread = false
			cmds = append(cmds, r.d.apply("read", ops, nil))
		}
		return tea.Batch(cmds...)
	case *agenda:
		o := s.selectedOcc()
		if o == nil {
			return nil
		}
		return r.push(newEventView(r.d, o.AccountID, o.CalendarRemote, o.CalendarName, o.EventRemoteID))
	}
	return nil
}

// triage archives or trashes what the top screen has selected.
func (r *root) triage(what string) tea.Cmd {
	ts := r.top().targets()
	if len(ts) == 0 {
		return nil
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	var ops []accountOp
	var undo *undoRecord
	switch what {
	case "archive":
		ops, undo = archiveOps(ctx, r.d.Store, ts)
	case "trash":
		ops, undo = trashOps(ctx, r.d.Store, ts)
	}
	if t, ok := r.top().(triageable); ok {
		t.dropSelected()
	}
	r.note(what + "…")
	return r.d.apply(what, ops, undo)
}

func (r *root) toggleFlag(flag string) tea.Cmd {
	ts := r.top().targets()
	if len(ts) == 0 {
		return nil
	}
	// The whole selection follows the first row: if it is unread, mark all
	// read, and vice versa. Anything else makes a mixed thread flip-flop.
	var cur bool
	switch flag {
	case "unread":
		cur = ts[0].flags.Unread
	case "flagged":
		cur = ts[0].flags.Flagged
	}
	ops, undo := flagOps(ts, flag, !cur)
	label := flag
	if cur {
		label = "un" + flag
	}
	r.note(label + "…")
	return r.d.apply(label, ops, undo)
}

func (r *root) applyUndo() tea.Cmd {
	if !r.undo.live(time.Now()) {
		r.note("nothing to undo")
		return nil
	}
	u := r.undo
	r.undo = nil
	r.note("undoing " + u.label + "…")
	// Put the row back straight away; the reload that follows confirms it.
	if t, ok := r.top().(triageable); ok {
		t.restore()
	}
	return tea.Batch(r.d.apply("undo "+u.label, u.ops, nil), r.top().reload())
}

func (r *root) onApplied(a applied) tea.Cmd {
	if a.err != nil {
		if t, ok := r.top().(triageable); ok {
			t.restore()
		}
		r.note(a.action + " failed: " + a.err.Error())
		r.undo = nil
		return nil
	}
	if t, ok := r.top().(triageable); ok {
		t.commit()
	}
	switch {
	case a.queued:
		r.note(a.action + " queued — offline, it will go out on the next sync")
	case a.undo != nil:
		r.undo = a.undo
		r.note(a.action + " · z to undo")
	default:
		r.note(a.action)
	}
	return nil
}

func (r *root) note(s string) {
	r.status = s
	r.statusSeq++
}

// nudgeDaemon asks a running `sync --watch` to do a pass now, the same SIGUSR1
// handshake the sync command uses. No daemon means nothing to nudge, which is
// not an error: R still re-queries what is already on disk.
func (r *root) nudgeDaemon() {
	if r.d.StatePath == "" {
		return
	}
	b, err := os.ReadFile(filepath.Join(r.d.StatePath, "emlcal.pid"))
	if err != nil {
		return
	}
	pid, err := strconv.Atoi(strings.TrimSpace(string(b)))
	if err != nil || pid <= 0 {
		return
	}
	p, err := os.FindProcess(pid)
	if err != nil {
		return
	}
	_ = p.Signal(syscall.SIGUSR1)
}

func (r *root) View() tea.View {
	v := tea.NewView(r.render())
	v.AltScreen = true
	v.WindowTitle = "emlcal"
	return v
}

func (r *root) render() string {
	if r.w == 0 || r.h == 0 {
		return ""
	}
	if r.quitting {
		return ""
	}
	if r.showHelp {
		return r.helpView()
	}
	top := r.top()
	title := styleHeader.Render(padCells(" "+top.Title(), r.w))
	body := top.View(r.w, r.bodyHeight())
	status := r.status
	if status == "" {
		status = top.footer(r.w)
	}
	hint := "? help"
	pad := r.w - len(status) - len(hint) - 2
	if pad < 1 {
		pad = 1
	}
	line := " " + status + strings.Repeat(" ", pad) + hint
	return title + "\n" + body + "\n" + styleFaint.Render(padCells(line, r.w))
}

func (r *root) helpView() string {
	var b strings.Builder
	b.WriteString(styleHeader.Render(padCells(" emlcal — keys", r.w)))
	b.WriteString("\n")
	n := 1
	for _, l := range r.keys.helpLines() {
		if n >= r.h-1 {
			break
		}
		if l[0] == "" {
			b.WriteString("\n")
		} else {
			b.WriteString("  " + padCells(l[0], 18) + l[1] + "\n")
		}
		n++
	}
	for ; n < r.h-1; n++ {
		b.WriteString("\n")
	}
	b.WriteString(styleFaint.Render(padCells(" any key to close", r.w)))
	return b.String()
}

// unusedModelCheck keeps the compiler honest about the tea.Model contract.
var _ tea.Model = (*root)(nil)

var _ = fmt.Sprintf
var _ = model.RoleInbox
