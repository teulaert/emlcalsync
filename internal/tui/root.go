package tui

import (
	"context"
	"errors"
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

// capturing is implemented by the screens that take every key press, because
// a text field is open on them. The root's own single-letter bindings must not
// fire then: the letter is being typed, not pressed.
type capturing interface{ capturingKeys() bool }

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

	// threadExpanded is the thread view's mode, kept on the root so the choice
	// survives going back to the list and opening the next thread.
	threadExpanded bool

	// composeSeq names the composer being opened, so a second r before the
	// first message has come back off disk does not push two.
	composeSeq int

	status    string
	statusSeq int
	undo      *undoRecord
	showHelp  bool
	quitting  bool

	// answers is what the model has said about conversations this session,
	// so a summary looked at twice is asked for once.
	answers *answerCache
}

func newRoot(d Deps) *root {
	accounts := d.Accounts
	r := &root{d: d, keys: defaultKeys(), threadExpanded: true, answers: newAnswerCache()}
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

	case composeLoaded:
		return r, r.onComposeLoaded(msg)

	case submitted:
		return r, r.onSubmitted(msg)

	case screenClosed:
		switch r.top().(type) {
		case *composeView, *summaryView:
			r.pop()
		}
		return r, nil

	case tea.KeyPressMsg:
		return r.onKey(msg)
	}

	return r, r.broadcast(msg)
}

// broadcast hands a message to every screen in both stacks.
//
// A load started by a screen that is not currently on top still has to reach
// it: both stacks are initialised at startup, so the agenda's first result
// arrives while the mail list is showing. Routing only to r.top() silently
// dropped it, and the calendar came up empty. Screens already ignore results
// whose seq is stale, so delivering widely is safe.
func (r *root) broadcast(msg tea.Msg) tea.Cmd {
	var cmds []tea.Cmd
	for _, st := range [][]screen{r.mail, r.cal} {
		for i, s := range st {
			next, cmd := s.Update(msg, r.keys, r.w, r.bodyHeight())
			st[i] = next
			if cmd != nil {
				cmds = append(cmds, cmd)
			}
		}
	}
	return tea.Batch(cmds...)
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

// capturing reports whether the top screen is taking the keys itself.
func (r *root) capturing() bool {
	c, ok := r.top().(capturing)
	return ok && c.capturingKeys()
}

func (r *root) onKey(msg tea.KeyPressMsg) (tea.Model, tea.Cmd) {
	if r.showHelp {
		r.showHelp = false
		return r, nil
	}
	// ctrl+c quits from anywhere, ahead of the capture gate below: a screen
	// that is taking every key -- the composer, the search prompt -- must not
	// be able to hold the program, least of all while a send is in flight and
	// nothing else is being accepted.
	if msg.String() == "ctrl+c" {
		r.quitting = true
		r.watcher.Close()
		return r, tea.Quit
	}
	if r.capturing() {
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

	case key.Matches(msg, r.keys.Expand):
		if tv, ok := r.top().(*threadView); ok {
			r.threadExpanded = !r.threadExpanded
			return r, tv.setExpanded(r.threadExpanded)
		}
		return r, nil

	case key.Matches(msg, r.keys.Archive):
		return r, r.triage("archive")

	case key.Matches(msg, r.keys.Trash):
		return r, r.triage("trash")

	case key.Matches(msg, r.keys.ToggleRead):
		return r, r.toggleFlag("unread")

	case key.Matches(msg, r.keys.Star):
		return r, r.toggleFlag("flagged")

	case key.Matches(msg, r.keys.Reply):
		return r, r.startReply(false)

	case key.Matches(msg, r.keys.ReplyAll):
		return r, r.startReply(true)

	case key.Matches(msg, r.keys.AI):
		return r, r.startSummary()
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
		// In the drafts mailbox a row stands for something unsent, not a
		// conversation to read, so enter goes straight into the composer.
		// Going by way of the thread put two keys between the list and the
		// draft -- and landed on the wrong one of them whenever the mail the
		// draft answers was still unread, because that is where a thread
		// opens.
		if s.showingDrafts() {
			return r.startCompose(composeRequest{
				account: t.AccountID, thread: t.ThreadID, draft: true,
			})
		}
		return r.push(newThreadView(r.d, t.AccountID, t.ThreadID, t.Subject, r.threadExpanded))
	case *threadView:
		m := s.selected()
		if m == nil {
			return nil
		}
		// A draft is not a message to read, it is one to finish. Enter on it
		// reopens the composer where it was left off.
		if m.Flags.Draft {
			return r.startCompose(composeRequest{
				account: m.AccountID, remote: m.RemoteID, draft: true,
			})
		}
		// Opening a message marks it read, the way every mail client does.
		return tea.Batch(r.push(newReader(r.d, m.AccountID, m.RemoteID)), r.markRead(m))
	case *agenda:
		o := s.selectedOcc()
		if o == nil {
			return nil
		}
		return r.push(newEventView(r.d, o.AccountID, o.CalendarRemote, o.CalendarName, o.EventRemoteID))
	}
	return nil
}

// startReply opens the composer on whatever message the screen has in focus.
// Nothing on the calendar side answers to it, and neither does the composer
// itself -- r there is a letter being typed, which the capture gate above has
// already dealt with.
func (r *root) startReply(all bool) tea.Cmd {
	req := composeRequest{all: all}
	switch s := r.top().(type) {
	case *mailList:
		t := s.selected()
		if t == nil {
			return nil
		}
		req.account, req.thread = t.AccountID, t.ThreadID
	case *threadView:
		m := s.selected()
		if m == nil {
			return nil
		}
		req.account, req.remote, req.thread = m.AccountID, m.RemoteID, m.ThreadID
	case *reader:
		if s.msg == nil {
			return nil
		}
		req.account, req.remote, req.thread = s.msg.AccountID, s.msg.RemoteID, s.msg.ThreadID
	case *summaryView:
		// The point of the summary: read four lines, answer the thread.
		req.account, req.thread = s.account, s.threadID
	default:
		return nil
	}
	return r.startCompose(req)
}

// startSummary is ctrl+g anywhere but the composer: it asks the model about
// the conversation in focus -- a summary, or a question typed at the prompt
// -- on a screen of its own. On the summary screen itself it opens the
// prompt again.
func (r *root) startSummary() tea.Cmd {
	if s, ok := r.top().(*summaryView); ok {
		s.ask()
		return nil
	}
	if r.d.AI == nil {
		r.note(errNoAI.Error())
		return nil
	}
	var account, thread, subject string
	switch s := r.top().(type) {
	case *mailList:
		t := s.selected()
		if t == nil {
			return nil
		}
		account, thread, subject = t.AccountID, t.ThreadID, t.Subject
	case *threadView:
		account, thread, subject = s.accountID, s.threadID, s.subject
	case *reader:
		if s.msg == nil {
			return nil
		}
		account, thread, subject = s.msg.AccountID, s.msg.ThreadID, s.msg.Subject
	default:
		return nil
	}
	r.note("")
	return r.push(newSummaryView(r.d, r.answers, account, thread, subject))
}

// startCompose loads the message and opens a composer on it.
func (r *root) startCompose(req composeRequest) tea.Cmd {
	r.composeSeq++
	return r.d.loadCompose(r.composeSeq, req)
}

// onComposeLoaded pushes the composer once its message is in hand.
func (r *root) onComposeLoaded(msg composeLoaded) tea.Cmd {
	if msg.seq != r.composeSeq {
		return nil // a newer r has been pressed since
	}
	if msg.err != nil {
		// The draft a row stood for is gone -- sent from somewhere else, or
		// synced away between the keypress and the load. The row is still a
		// conversation, so open that instead of saying nothing happened.
		if msg.req.draft && msg.req.remote == "" && errors.Is(msg.err, model.ErrNotFound) {
			r.onCal = false
			return r.push(newThreadView(r.d, msg.req.account, msg.req.thread, "", r.threadExpanded))
		}
		r.note("compose: " + msg.err.Error())
		return nil
	}
	// It was started from a mail screen, so that is the stack it goes on,
	// whichever one is showing by the time it comes back off disk.
	r.onCal = false
	// A status left over from the last action would sit where the composer's
	// own key hints belong; the composer is a new thing to be doing.
	r.note("")
	if msg.req.draft {
		return r.push(newDraftCompose(r.d, msg.msg))
	}
	return r.push(newReplyCompose(r.d, msg.msg, msg.req.all))
}

// onSubmitted closes the composer once the message is away, and leaves it open
// with the error when it is not.
func (r *root) onSubmitted(s submitted) tea.Cmd {
	c, onTop := r.top().(*composeView)
	if s.err != nil {
		if onTop {
			c.sending, c.err = false, s.err
		}
		r.note(s.what + " failed: " + s.err.Error())
		return nil
	}
	if onTop {
		r.pop()
	}
	switch {
	case s.queued:
		r.note(s.what + " queued — offline, it will go out on the next sync")
	case s.what == "draft":
		r.note("draft saved")
	case s.what == "send":
		r.note("sent")
	case s.what == "delete":
		r.note("draft deleted — it is in the trash")
	default:
		r.note("reply sent")
	}
	// The message is on the server but not yet in the archive; the daemon is
	// what puts it there, so ask it to look now rather than in a minute.
	r.nudgeDaemon()
	return r.top().reload()
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
	// The reader is the one screen with no rows of its own: the drop belongs
	// to the thread underneath, which then says what to read next.
	var follow tea.Cmd
	switch s := r.top().(type) {
	case *reader:
		follow = r.followOn(s)
	case triageable:
		s.dropSelected()
		if tv, ok := s.(*threadView); ok && len(tv.messages) == 0 {
			follow = r.closeThread()
		}
	}
	r.note(what + "…")
	return tea.Batch(r.d.apply(what, ops, undo), follow)
}

// followOn moves on after the message being read has been archived or trashed.
// The thread underneath drops it and the reader takes whichever message is now
// under the thread's cursor -- the next one down, the thread being newest
// first -- so a run of messages can be read and cleared without going back out
// after each one. When the thread has nothing left, the reader and the thread
// both close and the row goes with them, which is where a one-message thread
// ends up: back on the list, one row shorter.
func (r *root) followOn(rd *reader) tea.Cmd {
	st := r.stack()
	tv, _ := st[max(len(st)-2, 0)].(*threadView)
	if tv == nil {
		r.pop()
		return nil
	}
	tv.dropMessage(rd.remote)
	if m := tv.selected(); m != nil {
		return tea.Batch(rd.show(m.AccountID, m.RemoteID), r.markRead(m))
	}
	r.pop() // out of the reader
	return r.closeThread()
}

// closeThread leaves a thread with nothing left to show. Its last message has
// just been archived or trashed, so the row on the list underneath goes too --
// what a thread of one message amounts to, which is most of them, is that the
// list is where the next thing to read is.
func (r *root) closeThread() tea.Cmd {
	r.pop()
	if t, ok := r.top().(triageable); ok {
		t.dropSelected()
	}
	return nil
}

// markRead is the mark-read that comes with putting a message on screen. The
// engine is absent in tests that only draw screens; there is nothing to write
// to then.
func (r *root) markRead(m *model.Message) tea.Cmd {
	if !m.Flags.Unread || r.d.Engine == nil {
		return nil
	}
	ops, _ := flagOps([]target{targetOf(m)}, "unread", false)
	m.Flags.Unread = false
	return r.d.apply("read", ops, nil)
}

// triageScreen is the screen an action's row belongs to. That is the top one,
// except in the reader: reading is a level below the thread the message is a
// row of, so an optimistic drop -- and the restore or commit that follows --
// lands one screen down.
func (r *root) triageScreen() triageable {
	st := r.stack()
	if _, ok := st[len(st)-1].(*reader); ok && len(st) > 1 {
		t, _ := st[len(st)-2].(triageable)
		return t
	}
	t, _ := st[len(st)-1].(triageable)
	return t
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
	if tv, ok := r.top().(*threadView); ok && flag == "unread" && !cur {
		tv.keepUnread(ts[0].remote)
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
	if t := r.triageScreen(); t != nil {
		t.restore()
	}
	return tea.Batch(r.d.apply("undo "+u.label, u.ops, nil), r.top().reload())
}

func (r *root) onApplied(a applied) tea.Cmd {
	if a.err != nil {
		if t := r.triageScreen(); t != nil {
			t.restore()
		}
		r.note(a.action + " failed: " + a.err.Error())
		r.undo = nil
		return nil
	}
	if t := r.triageScreen(); t != nil {
		t.commit()
	}
	switch {
	case a.queued:
		r.note(a.action + " queued — offline, it will go out on the next sync")
	case a.undo != nil:
		r.undo = a.undo
		r.note(a.action + " · z to undo")
	case a.action == "read" && r.undo.live(time.Now()):
		// The mark-read that comes with landing on the next message must not
		// wipe the undo offer the trash before it just put up.
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
	tabs, tw := tabStrip(r.onCal)
	title := tabs + styleHeader.Render(padCells(" "+top.Title(), max(r.w-tw, 0)))
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

var _ tea.Model = (*root)(nil)
