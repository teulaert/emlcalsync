package tui

import (
	"context"
	"database/sql"
	"time"

	tea "charm.land/bubbletea/v2"

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
