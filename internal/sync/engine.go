// Package sync is the engine described in DESIGN.md §7: it owns every
// provider→store write (backfill, delta, reconcile) and every store→provider
// write (the outbox). Nothing else in the tool talks to a provider.
//
// The three mail paths are all idempotent and resumable:
//
//	backfill   first run: capture the delta state *before* enumerating, walk
//	           every page, then replay everything that happened meanwhile
//	delta      the normal tick: apply Changes(since) and advance the state in
//	           the same transaction as the last batch, so a crash replays
//	reconcile  the expensive fallback after ErrStateExpired: diff the full id
//	           list against the index
//
// Writes go through Apply, which records an outbox row and patches the index
// optimistically inside one transaction before trying the provider. When the
// provider is unreachable the row stays pending and RetryOutbox drains it
// later; when the provider *rejects* the write the optimistic patch is rolled
// back, the row is marked permanently failed (failed_at, never retried) and
// the error is returned to the caller rather than silently queued. Sends and
// drafts are single-attempt: they are not idempotent, so anything but a
// failure from before the request was built retires the row too.
package sync

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	stdsync "sync"
	"syscall"
	"time"

	"golang.org/x/sync/errgroup"

	"github.com/teulaert/emlcalsync/internal/blob"
	"github.com/teulaert/emlcalsync/internal/config"
	"github.com/teulaert/emlcalsync/internal/model"
	"github.com/teulaert/emlcalsync/internal/provider"
	"github.com/teulaert/emlcalsync/internal/store"
)

// ProviderFactory builds the provider clients for an account. The engine
// caches whatever it returns for the lifetime of the Engine, so a factory may
// do expensive setup (OAuth token load, JMAP session fetch) per call.
type ProviderFactory interface {
	Mail(ctx context.Context, acct config.Account) (provider.MailProvider, error)
	Calendar(ctx context.Context, acct config.Account) (provider.CalendarProvider, error)
	// Pusher returns the account's push stream. ok is false when the provider
	// has none (Gmail), which is not an error.
	Pusher(ctx context.Context, acct config.Account) (provider.Pusher, bool, error)
}

// ProgressEvent is emitted at least once per page/batch so the CLI can render
// a live counter. Total is 0 when the size of the job is not known.
type ProgressEvent struct {
	Account  string `json:"account"`
	Resource string `json:"resource"` // "mail" | "calendar"
	Phase    string `json:"phase"`    // backfill|resume|delta|reconcile|reindex|gc|outbox
	Done     int    `json:"done"`
	Total    int    `json:"total,omitempty"`
	Message  string `json:"message,omitempty"`
}

// Options configures New.
type Options struct {
	Store     *store.Store
	Blobs     *blob.Store
	Config    *config.Config
	Providers ProviderFactory
	Logger    *slog.Logger
	// Progress is called (from the syncing goroutine) with backfill/delta
	// progress. Optional; it must not block for long.
	Progress func(ProgressEvent)
	// LockDir holds the per-account flock files. Empty disables file locking
	// (the in-process guard still applies).
	LockDir string
}

// Engine is the sync engine. It is safe for concurrent use.
type Engine struct {
	st        *store.Store
	blobs     *blob.Store
	cfg       *config.Config
	providers ProviderFactory
	log       *slog.Logger
	progress  func(ProgressEvent)
	lockDir   string

	mu    stdsync.Mutex
	held  map[string]bool
	mailP map[string]provider.MailProvider
	calP  map[string]provider.CalendarProvider

	retryMu stdsync.Mutex
	retryAt map[int64]time.Time

	// deltaCount counts deltas per account (for the periodic mailbox refresh).
	deltaCount map[string]int

	watchMu  stdsync.Mutex
	watchers map[string]*accountWatch
}

// tuning constants.
const (
	// enumeratePage is the page size asked of Enumerate.
	enumeratePage = 500
	// indexBatch is how many indexed messages go into one transaction.
	indexBatch = 100
	// envelopeChunk is how many ids one FetchEnvelopes call covers.
	envelopeChunk = 100
	// mailboxRefreshEvery re-syncs mailboxes on every N-th delta even when the
	// provider did not flag a change.
	mailboxRefreshEvery = 20
)

// New validates o and returns an engine.
func New(o Options) (*Engine, error) {
	if o.Store == nil {
		return nil, errors.New("sync: Store is required")
	}
	if o.Blobs == nil {
		return nil, errors.New("sync: Blobs is required")
	}
	if o.Config == nil {
		return nil, errors.New("sync: Config is required")
	}
	if o.Providers == nil {
		return nil, errors.New("sync: Providers is required")
	}
	log := o.Logger
	if log == nil {
		log = slog.Default()
	}
	return &Engine{
		st:        o.Store,
		blobs:     o.Blobs,
		cfg:       o.Config,
		providers: o.Providers,
		log:       log,
		progress:  o.Progress,
		lockDir:   o.LockDir,
		held:      map[string]bool{},
		mailP:     map[string]provider.MailProvider{},
		calP:      map[string]provider.CalendarProvider{},
		retryAt:   map[int64]time.Time{},
		watchers:  map[string]*accountWatch{},
	}, nil
}

// SyncOptions selects what one pass does. Both resource flags false means
// both, which is what a bare `emlcal sync` wants.
type SyncOptions struct {
	Full     bool
	Mail     bool
	Calendar bool
}

func (o SyncOptions) resolved() SyncOptions {
	if !o.Mail && !o.Calendar {
		o.Mail, o.Calendar = true, true
	}
	return o
}

// ResourceReport summarises one resource of one pass.
type ResourceReport struct {
	Kind        string        `json:"kind"` // backfill|resume|delta|reconcile
	Added       int           `json:"added"`
	Updated     int           `json:"updated"`
	Removed     int           `json:"removed"`
	Duration    time.Duration `json:"duration"`
	StateBefore string        `json:"state_before,omitempty"`
	StateAfter  string        `json:"state_after,omitempty"`
}

func (r *ResourceReport) add(o *ResourceReport) {
	if o == nil {
		return
	}
	r.Added += o.Added
	r.Updated += o.Updated
	r.Removed += o.Removed
}

// Report is the result of syncing one account.
type Report struct {
	Account  string          `json:"account"`
	Mail     *ResourceReport `json:"mail,omitempty"`
	Calendar *ResourceReport `json:"calendar,omitempty"`
	Err      error           `json:"-"`
}

// ErrLocked is returned by SyncAccount when another sync (usually the daemon)
// holds the account's lock. The CLI turns this into "daemon active — nudged".
var ErrLocked = errors.New("account locked by another sync")

// SyncAccount runs one pass over a single account. It takes the account's
// lock for the duration and returns ErrLocked when someone else holds it.
func (e *Engine) SyncAccount(ctx context.Context, name string, o SyncOptions) (*Report, error) {
	acct, ok := e.cfg.Account(name)
	if !ok {
		return nil, fmt.Errorf("sync: unknown account %q", name)
	}
	release, err := e.lockAccount(name)
	if err != nil {
		return &Report{Account: name, Err: err}, err
	}
	defer release()
	return e.syncAccount(ctx, *acct, o)
}

// syncAccount is SyncAccount without the locking, for callers (Watch) that
// already hold the lock.
func (e *Engine) syncAccount(ctx context.Context, acct config.Account, o SyncOptions) (*Report, error) {
	o = o.resolved()
	rep := &Report{Account: acct.Name}
	if err := e.ensureAccount(ctx, acct); err != nil {
		rep.Err = err
		return rep, err
	}
	if o.Mail {
		r, err := e.syncMail(ctx, acct, o.Full)
		rep.Mail = r
		if err != nil {
			rep.Err = err
			return rep, err
		}
	}
	if o.Calendar && len(acct.Calendars) > 0 {
		r, err := e.syncCalendar(ctx, acct, o.Full)
		rep.Calendar = r
		if err != nil {
			rep.Err = err
			return rep, err
		}
	}
	return rep, nil
}

// syncAllConcurrency bounds how many accounts sync at once. Each account is
// itself sequential (mail, then calendar).
const syncAllConcurrency = 2

// SyncAll runs one pass over every configured account. An account that fails
// does not stop the others; its Report carries the error and SyncAll returns
// the first one.
func (e *Engine) SyncAll(ctx context.Context, o SyncOptions) ([]*Report, error) {
	accts := e.cfg.Accounts
	reports := make([]*Report, len(accts))

	g, gctx := errgroup.WithContext(ctx)
	g.SetLimit(syncAllConcurrency)
	for i := range accts {
		i := i
		g.Go(func() error {
			rep, err := e.SyncAccount(gctx, accts[i].Name, o)
			if rep == nil {
				rep = &Report{Account: accts[i].Name, Err: err}
			}
			reports[i] = rep
			if err != nil {
				e.log.Warn("sync failed", "account", accts[i].Name, "err", err)
			}
			// Never abort the group: one broken account must not cancel the rest.
			return nil
		})
	}
	_ = g.Wait()

	out := make([]*Report, 0, len(reports))
	var firstErr error
	for _, r := range reports {
		if r == nil {
			continue
		}
		out = append(out, r)
		if firstErr == nil && r.Err != nil {
			firstErr = r.Err
		}
	}
	return out, firstErr
}

// ---------------------------------------------------------------------------
// Accounts, providers, locking

func (e *Engine) ensureAccount(ctx context.Context, acct config.Account) error {
	a := &model.Account{ID: acct.Name, Provider: acct.Provider, Email: acct.Email}
	if err := e.st.UpsertAccount(ctx, a); err != nil {
		return fmt.Errorf("sync: %s: %w", acct.Name, err)
	}
	return nil
}

func (e *Engine) mailProvider(ctx context.Context, acct config.Account) (provider.MailProvider, error) {
	e.mu.Lock()
	p, ok := e.mailP[acct.Name]
	e.mu.Unlock()
	if ok {
		return p, nil
	}
	p, err := e.providers.Mail(ctx, acct)
	if err != nil {
		return nil, fmt.Errorf("sync: %s: mail provider: %w", acct.Name, err)
	}
	e.mu.Lock()
	if existing, ok := e.mailP[acct.Name]; ok {
		p = existing
	} else {
		e.mailP[acct.Name] = p
	}
	e.mu.Unlock()
	return p, nil
}

func (e *Engine) calendarProvider(ctx context.Context, acct config.Account) (provider.CalendarProvider, error) {
	e.mu.Lock()
	p, ok := e.calP[acct.Name]
	e.mu.Unlock()
	if ok {
		return p, nil
	}
	p, err := e.providers.Calendar(ctx, acct)
	if err != nil {
		return nil, fmt.Errorf("sync: %s: calendar provider: %w", acct.Name, err)
	}
	e.mu.Lock()
	if existing, ok := e.calP[acct.Name]; ok {
		p = existing
	} else {
		e.calP[acct.Name] = p
	}
	e.mu.Unlock()
	return p, nil
}

// lockAccount takes the in-process guard and, when LockDir is set, an
// exclusive non-blocking flock on <LockDir>/sync.<account>.lock. The returned
// function releases both.
func (e *Engine) lockAccount(name string) (release func(), err error) {
	e.mu.Lock()
	if e.held[name] {
		e.mu.Unlock()
		return nil, fmt.Errorf("sync: %s: %w", name, ErrLocked)
	}
	e.held[name] = true
	e.mu.Unlock()

	unheld := func() {
		e.mu.Lock()
		delete(e.held, name)
		e.mu.Unlock()
	}
	if e.lockDir == "" {
		return unheld, nil
	}
	if err := os.MkdirAll(e.lockDir, 0o700); err != nil {
		unheld()
		return nil, fmt.Errorf("sync: lock dir: %w", err)
	}
	path := filepath.Join(e.lockDir, "sync."+name+".lock")
	f, err := os.OpenFile(path, os.O_CREATE|os.O_RDWR, 0o600)
	if err != nil {
		unheld()
		return nil, fmt.Errorf("sync: open lock %s: %w", path, err)
	}
	if err := syscall.Flock(int(f.Fd()), syscall.LOCK_EX|syscall.LOCK_NB); err != nil {
		f.Close()
		unheld()
		return nil, fmt.Errorf("sync: %s: %w", name, ErrLocked)
	}
	return func() {
		_ = syscall.Flock(int(f.Fd()), syscall.LOCK_UN)
		f.Close()
		unheld()
	}, nil
}

// ---------------------------------------------------------------------------
// Helpers

func (e *Engine) emit(ev ProgressEvent) {
	if e.progress != nil {
		e.progress(ev)
	}
}

func (e *Engine) logSync(ctx context.Context, accountID, kind string, started time.Time, r *ResourceReport, syncErr error) {
	entry := store.SyncLogEntry{
		AccountID: accountID,
		Kind:      kind,
		Started:   started,
		Finished:  time.Now(),
	}
	if r != nil {
		entry.Added, entry.Updated, entry.Removed = r.Added, r.Updated, r.Removed
	}
	if syncErr != nil {
		entry.Error = syncErr.Error()
	}
	if _, err := e.st.AppendSyncLog(ctx, entry); err != nil {
		e.log.Warn("sync log", "account", accountID, "err", err)
	}
}

// chunk splits s into slices of at most n elements.
func chunk[T any](s []T, n int) [][]T {
	if n <= 0 || len(s) == 0 {
		if len(s) == 0 {
			return nil
		}
		return [][]T{s}
	}
	out := make([][]T, 0, (len(s)+n-1)/n)
	for i := 0; i < len(s); i += n {
		out = append(out, s[i:min(i+n, len(s))])
	}
	return out
}
