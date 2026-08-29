package tui

import (
	"context"
	"database/sql"
	"strings"
	"time"

	tea "charm.land/bubbletea/v2"

	"github.com/teulaert/emlcalsync/internal/compose"
	"github.com/teulaert/emlcalsync/internal/mime"
	"github.com/teulaert/emlcalsync/internal/model"
	"github.com/teulaert/emlcalsync/internal/store"
)

// Every store call is blocking I/O, so each one is a tea.Cmd returning one of
// these. Loads carry the seq they were issued with: holding j fires a body
// load per row and the answers can come back out of order, so a screen keeps
// the seq it last asked for and drops anything older. That is cheaper and more
// predictable than cancelling a context per keystroke.

type threadsLoaded struct {
	seq     int
	threads []model.Thread
	// append is set when the result extends the current window rather than
	// replacing it.
	append bool
	err    error
}

type threadOpened struct {
	seq      int
	thread   *model.Thread
	messages []model.Message
	err      error
}

type bodyLoaded struct {
	seq  int
	id   string // public message id the body belongs to
	msg  *model.Message
	body string
	err  error
}

type agendaLoaded struct {
	seq  int
	occs []store.OccurrenceRow
	from time.Time
	to   time.Time
	err  error
}

type eventOpened struct {
	seq   int
	event *model.Event
	err   error
}

// composeLoaded carries the message a composer is about to open on: the one
// being answered, or the draft being finished. The request comes back with it,
// so the root can say what to do when there turns out to be nothing to open.
type composeLoaded struct {
	seq int
	req composeRequest
	msg *model.Message
	// files are the attachments a forward carries, fetched with the message;
	// filesNote names the ones it could not, which is the one thing about a
	// forward that must not be found out afterwards.
	files     []mime.DraftAttachment
	filesNote string
	err       error
}

// submitted reports the outcome of a send or a draft save.
type submitted struct {
	what   string // "reply" or "draft"
	queued bool
	err    error
}

// screenClosed asks the root to take the screen on top off the stack. A
// screen cannot pop itself -- the stack belongs to the root -- so it says so
// in a message. The composer and the summary screen use it.
type screenClosed struct{}

func closeScreen() tea.Cmd { return func() tea.Msg { return screenClosed{} } }

// applied reports the outcome of one Engine.Apply.
type applied struct {
	action  string
	account string
	queued  bool
	renames map[string]string
	err     error
	// undo describes how to reverse what just happened, or is nil when the
	// action is not reversible.
	undo *undoRecord
}

// dbChanged is emitted when another connection — in practice the sync daemon —
// committed something.
type dbChanged struct{}

// tick drives the poll loop.
type tickMsg time.Time

// statusExpired clears a transient status line.
type statusExpired struct{ seq int }

// ---------------------------------------------------------------------------
// Loads

func (d Deps) loadThreads(seq int, f store.MessageFilter, query string, appendPage bool) tea.Cmd {
	return func() tea.Msg {
		ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
		defer cancel()
		if query != "" {
			hits, err := d.Store.Search(ctx, query, f)
			if err != nil {
				return threadsLoaded{seq: seq, err: err, append: appendPage}
			}
			// Search ranks messages; collapse to one row per thread, keeping
			// the order the ranking produced.
			seen := map[string]bool{}
			var out []model.Thread
			for i := range hits {
				m := &hits[i].Message
				k := m.AccountID + "\x00" + m.ThreadID
				if seen[k] {
					continue
				}
				seen[k] = true
				t, _, err := d.Store.GetThread(ctx, m.AccountID, m.ThreadID, false)
				if err != nil || t == nil {
					continue
				}
				out = append(out, *t)
			}
			return threadsLoaded{seq: seq, threads: out, append: appendPage}
		}
		th, err := d.Store.ListThreads(ctx, f)
		return threadsLoaded{seq: seq, threads: th, err: err, append: appendPage}
	}
}

func (d Deps) openThread(seq int, accountID, threadID string) tea.Cmd {
	return func() tea.Msg {
		ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
		defer cancel()
		t, msgs, err := d.Store.GetThread(ctx, accountID, threadID, false)
		return threadOpened{seq: seq, thread: t, messages: msgs, err: err}
	}
}

func (d Deps) loadBody(seq int, accountID, remote string) tea.Cmd {
	return func() tea.Msg {
		ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
		defer cancel()
		m, err := d.Store.GetMessage(ctx, accountID, remote)
		if err != nil {
			return bodyLoaded{seq: seq, id: model.MessagePublicID(accountID, remote), err: err}
		}
		return bodyLoaded{
			seq:  seq,
			id:   m.PublicID(),
			msg:  m,
			body: readableBody(ctx, d, m),
		}
	}
}

func (d Deps) loadAgenda(seq int, from, to time.Time) tea.Cmd {
	return func() tea.Msg {
		ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
		defer cancel()
		cals, err := d.Store.ListCalendars(ctx, d.Accounts)
		if err != nil {
			return agendaLoaded{seq: seq, err: err, from: from, to: to}
		}
		ids := make([]int64, 0, len(cals))
		for _, c := range cals {
			ids = append(ids, c.ID)
		}
		if len(ids) == 0 {
			return agendaLoaded{seq: seq, from: from, to: to}
		}
		occs, err := d.Store.ListOccurrences(ctx, from, to, ids)
		return agendaLoaded{seq: seq, occs: occs, from: from, to: to, err: err}
	}
}

func (d Deps) openEvent(seq int, accountID, calRemote, remote string) tea.Cmd {
	return func() tea.Msg {
		ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
		defer cancel()
		ev, err := d.Store.GetEvent(ctx, accountID, calRemote, remote)
		return eventOpened{seq: seq, event: ev, err: err}
	}
}

// loadCompose fetches the message a composer is about to open on, off the
// update loop like every other store call.
//
// remote names it outright, which is what the thread view and the reader have;
// a list row instead names a whole thread, and the message to answer is the
// newest one in it that was actually sent.
func (d Deps) loadCompose(seq int, req composeRequest) tea.Cmd {
	return func() tea.Msg {
		// A forward carries the files, and those can be a download rather
		// than a read off disk: the budget is the one a fetch gets, not the
		// one a row lookup gets.
		timeout := 15 * time.Second
		if req.forward {
			timeout = 90 * time.Second
		}
		ctx, cancel := context.WithTimeout(context.Background(), timeout)
		defer cancel()
		m, resolved, err := resolveCompose(ctx, d, req)
		if err != nil {
			return composeLoaded{seq: seq, req: req, err: err}
		}
		ensureText(ctx, d, m)
		out := composeLoaded{seq: seq, req: resolved, msg: m}
		if resolved.forward {
			out.files, out.filesNote = d.forwardFiles(ctx, m)
		}
		return out
	}
}

// forwardFiles fetches the attachments the forward carries, and says which
// ones it could not.
//
// The bytes come through the engine, which reads them out of the archived raw
// message when it has one and downloads them from the provider when it does
// not -- the same path `mail attachment` takes. A file that will not come is
// named rather than dropped: forwarding is mostly done *for* the attachment,
// so "it went without them" is the one outcome nobody may discover at the
// other end.
func (d Deps) forwardFiles(ctx context.Context, m *model.Message) ([]mime.DraftAttachment, string) {
	if !m.HasAttachments || d.Store == nil || d.Engine == nil {
		return nil, ""
	}
	atts, err := d.Store.ListAttachments(ctx, m.ID)
	if err != nil {
		d.log().Warn("forward: list attachments", "id", m.PublicID(), "err", err)
		return nil, "the attachments could not be read: " + err.Error()
	}
	var (
		out   []mime.DraftAttachment
		left  = int64(compose.MaxForwardBytes)
		short []string
	)
	for _, a := range compose.ForwardAttachments(atts) {
		name := a.Filename
		if name == "" {
			name = a.PartPath
		}
		if a.Size > left {
			short = append(short, name+" (too large)")
			continue
		}
		ref := a.RemoteRef
		if ref == "" {
			ref = a.PartPath
		}
		data, err := d.Engine.FetchAttachment(ctx, m.AccountID, m.RemoteID, ref)
		if err != nil {
			d.log().Warn("forward: fetch attachment", "id", m.PublicID(), "part", a.PartPath, "err", err)
			short = append(short, name)
			continue
		}
		if int64(len(data)) > left {
			short = append(short, name+" (too large)")
			continue
		}
		left -= int64(len(data))
		out = append(out, mime.DraftAttachment{
			Filename: name, ContentType: a.ContentType, Data: data,
		})
	}
	if len(short) > 0 {
		return out, "not carried: " + strings.Join(short, ", ")
	}
	return out, ""
}

// resolveCompose finds the message the composer opens on, and says which kind
// of composer that turns out to be.
//
// A reply to a conversation that already has an answer under way continues
// that answer instead of starting a second one beside it: the draft is where
// the earlier words are, and two half-written replies to one thread is not
// something anybody meant to have. That is why the answer comes back with the
// request it resolved to rather than the one that was asked -- and why the
// header then reads "draft ·" rather than "reply ·", which is what tells you
// the words already on screen are your own from earlier.
func resolveCompose(ctx context.Context, d Deps, req composeRequest) (*model.Message, composeRequest, error) {
	byRemote := func() (*model.Message, composeRequest, error) {
		m, err := d.Store.GetMessage(ctx, req.account, req.remote)
		return m, req, err
	}
	if req.draft {
		if req.remote != "" {
			return byRemote()
		}
		m, err := newestDraft(ctx, d, req.account, req.thread)
		return m, req, err
	}
	// A forward is never the draft in the thread: f says "send this message
	// on", and an unfinished answer is neither that message nor somewhere to
	// write a forward.
	if req.thread != "" && !req.forward {
		if m, err := newestDraft(ctx, d, req.account, req.thread); err == nil {
			req.draft = true
			return m, req, nil
		}
	}
	if req.remote != "" {
		return byRemote()
	}
	m, err := newestSent(ctx, d, req.account, req.thread)
	return m, req, err
}

// composeRequest is what the root worked out from the screen in focus before
// the load went off.
type composeRequest struct {
	account string
	// remote names the message outright. Empty means "work it out from the
	// thread": the newest draft in it when draft is set, else the newest
	// message that was actually sent.
	remote  string
	thread  string
	draft   bool
	forward bool
	all     bool
}

// newestSent is the message in a thread that a reply belongs under: the last
// one that actually went somewhere.
//
// resolveCompose has already taken any draft out of the running by the time
// this is reached -- a thread with one in it is continued, not replied to --
// so in practice this is the newest message. The draft check stays because it
// is what makes that sentence true wherever this is called from.
func newestSent(ctx context.Context, d Deps, accountID, threadID string) (*model.Message, error) {
	_, msgs, err := d.Store.GetThread(ctx, accountID, threadID, false)
	if err != nil {
		return nil, err
	}
	if len(msgs) == 0 {
		return nil, model.ErrNotFound
	}
	// GetThread hands them back oldest first.
	for i := len(msgs) - 1; i >= 0; i-- {
		if !msgs[i].Flags.Draft {
			return &msgs[i], nil
		}
	}
	return &msgs[len(msgs)-1], nil
}

// newestDraft is the unsent message a drafts row stands for. A row there is a
// thread like any other -- the conversation the draft belongs to -- but what
// enter is asking for is the draft in it, not the mail it answers.
func newestDraft(ctx context.Context, d Deps, accountID, threadID string) (*model.Message, error) {
	_, msgs, err := d.Store.GetThread(ctx, accountID, threadID, false)
	if err != nil {
		return nil, err
	}
	// GetThread hands them back oldest first.
	for i := len(msgs) - 1; i >= 0; i-- {
		if msgs[i].Flags.Draft {
			return &msgs[i], nil
		}
	}
	return nil, model.ErrNotFound
}

// sendFrom is the address a reply from this account goes out as. There is no
// --from here: `mail reply` sends from the account that received the message,
// and so does the composer.
func (d Deps) sendFrom(account string) model.Address {
	if d.Config != nil {
		if a, ok := d.Config.Account(account); ok && strings.TrimSpace(a.Email) != "" {
			return model.Address{Email: a.Email}
		}
	}
	// The index carries the address too, which is what keeps this working
	// against a store opened without a config -- in tests, and for an account
	// whose name in config.toml has moved on.
	if d.Store == nil {
		return model.Address{}
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if a, err := d.Store.GetAccount(ctx, account); err == nil && a != nil {
		return model.Address{Email: a.Email}
	}
	return model.Address{}
}

// ---------------------------------------------------------------------------
// Change detection

// dbWatcher notices commits made by another process — normally `sync --watch`,
// which on this machine runs as a systemd unit against the very same file.
//
// PRAGMA data_version is a counter SQLite bumps when a *different connection*
// commits, which is exactly the question being asked, and it costs nothing.
// It is per-connection, so the watcher pins one connection for its whole life:
// asking through the pool would compare counters from different connections
// and report a change on every other poll.
type dbWatcher struct {
	conn *sql.Conn
	last int64
}

func newDBWatcher(ctx context.Context, st *store.Store) *dbWatcher {
	if st == nil {
		return nil
	}
	conn, err := st.DB().Conn(ctx)
	if err != nil {
		return nil
	}
	w := &dbWatcher{conn: conn}
	w.last, _ = w.read(ctx)
	return w
}

func (w *dbWatcher) read(ctx context.Context) (int64, error) {
	var v int64
	err := w.conn.QueryRowContext(ctx, "PRAGMA data_version").Scan(&v)
	return v, err
}

// changed reports whether another connection has committed since the last call.
func (w *dbWatcher) changed(ctx context.Context) bool {
	if w == nil || w.conn == nil {
		return false
	}
	v, err := w.read(ctx)
	if err != nil || v == w.last {
		return false
	}
	w.last = v
	return true
}

func (w *dbWatcher) Close() {
	if w != nil && w.conn != nil {
		_ = w.conn.Close()
		w.conn = nil
	}
}

const pollInterval = 2 * time.Second

func poll() tea.Cmd {
	return tea.Tick(pollInterval, func(t time.Time) tea.Msg { return tickMsg(t) })
}
