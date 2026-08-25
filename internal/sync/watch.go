package sync

import (
	"context"
	"errors"
	stdsync "sync"
	"time"

	"golang.org/x/sync/errgroup"

	"github.com/lennert/emlcal/internal/config"
	"github.com/lennert/emlcal/internal/provider"
)

// Watch timings.
const (
	// pushDebounce coalesces a burst of push hints into one delta.
	pushDebounce = 500 * time.Millisecond
	// outboxEvery is how often pending writes are retried in the daemon.
	outboxEvery = time.Minute
	// reexpandEvery slides the occurrence window once a day.
	reexpandEvery = 24 * time.Hour
	// offlineMin/offlineMax bound the connectivity backoff (DESIGN.md §12).
	offlineMin = 5 * time.Second
	offlineMax = 5 * time.Minute
	// pushRetryMin/Max bound reconnects of a push stream.
	pushRetryMin = 2 * time.Second
	pushRetryMax = 2 * time.Minute
)

// accountWatch is the wake-up channel of one account's watch loop. Hints are
// merged rather than queued, so a burst produces one pass.
type accountWatch struct {
	mu      stdsync.Mutex
	pending provider.ChangeHint
	sig     chan struct{}
}

func newAccountWatch() *accountWatch {
	return &accountWatch{sig: make(chan struct{}, 1)}
}

func (w *accountWatch) fire(h provider.ChangeHint) {
	w.mu.Lock()
	w.pending.Mail = w.pending.Mail || h.Mail
	w.pending.Calendar = w.pending.Calendar || h.Calendar
	w.mu.Unlock()
	select {
	case w.sig <- struct{}{}:
	default: // already pending
	}
}

func (w *accountWatch) take() provider.ChangeHint {
	w.mu.Lock()
	defer w.mu.Unlock()
	h := w.pending
	w.pending = provider.ChangeHint{}
	if !h.Mail && !h.Calendar {
		h = provider.ChangeHint{Mail: true, Calendar: true}
	}
	return h
}

func (e *Engine) watcher(account string) *accountWatch {
	e.watchMu.Lock()
	defer e.watchMu.Unlock()
	w, ok := e.watchers[account]
	if !ok {
		w = newAccountWatch()
		e.watchers[account] = w
	}
	return w
}

// Nudge triggers an immediate pass on every watched account. The CLI wires
// SIGUSR1 to it so `emlcal sync` on an account the daemon owns can say
// "daemon active — nudged" instead of failing.
func (e *Engine) Nudge() {
	e.watchMu.Lock()
	ws := make([]*accountWatch, 0, len(e.watchers))
	for _, w := range e.watchers {
		ws = append(ws, w)
	}
	e.watchMu.Unlock()
	for _, w := range ws {
		w.fire(provider.ChangeHint{Mail: true, Calendar: true})
	}
}

// Watch runs the daemon: one loop per account (poll timer plus push stream
// where the provider has one), outbox retries every minute and a nightly
// re-expansion of recurring events. It returns when ctx is cancelled, or
// early if an account's lock is held by another process.
func (e *Engine) Watch(ctx context.Context) error {
	// Register every account up front so Nudge works before the loops start.
	for _, acct := range e.cfg.Accounts {
		e.watcher(acct.Name)
	}

	g, gctx := errgroup.WithContext(ctx)
	for _, acct := range e.cfg.Accounts {
		acct := acct
		g.Go(func() error { return e.watchAccount(gctx, acct) })
	}
	g.Go(func() error {
		e.every(gctx, outboxEvery, func(c context.Context) {
			if _, err := e.RetryOutbox(c, ""); err != nil {
				e.log.Warn("outbox retry", "err", err)
			}
		})
		return nil
	})
	g.Go(func() error {
		e.every(gctx, reexpandEvery, func(c context.Context) {
			if err := e.ReexpandAll(c); err != nil && !errors.Is(err, context.Canceled) {
				e.log.Warn("re-expansion", "err", err)
			}
		})
		return nil
	})

	err := g.Wait()
	if errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
		return nil
	}
	return err
}

// every calls fn on a ticker until ctx is done.
func (e *Engine) every(ctx context.Context, d time.Duration, fn func(context.Context)) {
	t := time.NewTicker(d)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			fn(ctx)
		}
	}
}

// watchAccount owns one account for the lifetime of the daemon: it holds the
// account's lock, polls, listens to the push stream, and never dies of a
// provider error.
func (e *Engine) watchAccount(ctx context.Context, acct config.Account) error {
	release, err := e.lockAccount(acct.Name)
	if err != nil {
		return err
	}
	defer release()

	w := e.watcher(acct.Name)

	if acct.Push {
		if p, ok, perr := e.providers.Pusher(ctx, acct); perr != nil {
			e.log.Warn("push unavailable", "account", acct.Name, "err", perr)
		} else if ok && p != nil {
			go e.pushLoop(ctx, acct, p, w)
		}
	}

	poll := acct.Poll.Duration()
	if poll <= 0 {
		poll = time.Minute
	}
	ticker := time.NewTicker(poll)
	defer ticker.Stop()

	offline := time.Duration(0)
	first := true
	for {
		var o SyncOptions
		if first {
			first = false // the initial pass covers everything
		} else {
			select {
			case <-ctx.Done():
				return nil
			case <-ticker.C:
				// A poll tick covers everything.
			case <-w.sig:
				if !debounce(ctx, w, pushDebounce) {
					return nil
				}
				h := w.take()
				o = SyncOptions{Mail: h.Mail, Calendar: h.Calendar}
			}
		}

		if err := e.pass(ctx, acct, o); err != nil && provider.IsOffline(err) {
			offline = nextBackoff(offline, offlineMin, offlineMax)
			e.log.Warn("offline, backing off", "account", acct.Name, "retry_in", offline)
			if !sleepCtx(ctx, offline) {
				return nil
			}
			continue
		}
		offline = 0
	}
}

// debounce waits d, swallowing further wake-ups, so a burst of push hints
// produces one pass. It returns false when ctx ended.
func debounce(ctx context.Context, w *accountWatch, d time.Duration) bool {
	timer := time.NewTimer(d)
	defer timer.Stop()
	for {
		select {
		case <-ctx.Done():
			return false
		case <-w.sig:
			// Merged into pending already; keep waiting for the timer.
		case <-timer.C:
			return true
		}
	}
}

// pass runs one sync for an account whose lock this goroutine already holds,
// then drains that account's outbox. Provider errors are logged, not returned
// upward, so the daemon stays alive.
func (e *Engine) pass(ctx context.Context, acct config.Account, o SyncOptions) error {
	rep, err := e.syncAccount(ctx, acct, o)
	if err != nil {
		if errors.Is(err, context.Canceled) {
			return err
		}
		e.log.Warn("sync failed", "account", acct.Name, "err", err)
		return err
	}
	_ = rep
	if _, err := e.RetryOutbox(ctx, acct.Name); err != nil {
		e.log.Warn("outbox retry", "account", acct.Name, "err", err)
	}
	return nil
}

// pushLoop keeps the provider's push stream connected and turns every hint
// into a wake-up for the account loop.
func (e *Engine) pushLoop(ctx context.Context, acct config.Account, p provider.Pusher, w *accountWatch) {
	wait := time.Duration(0)
	for ctx.Err() == nil {
		err := p.Watch(ctx, func(h provider.ChangeHint) { w.fire(h) })
		if ctx.Err() != nil {
			return
		}
		if err != nil {
			e.log.Warn("push stream dropped", "account", acct.Name, "err", err)
		}
		wait = nextBackoff(wait, pushRetryMin, pushRetryMax)
		if !sleepCtx(ctx, wait) {
			return
		}
	}
}

func nextBackoff(cur, min, max time.Duration) time.Duration {
	if cur <= 0 {
		return min
	}
	cur *= 2
	if cur > max {
		return max
	}
	return cur
}

// sleepCtx sleeps for d, returning false if ctx ended first.
func sleepCtx(ctx context.Context, d time.Duration) bool {
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-ctx.Done():
		return false
	case <-t.C:
		return true
	}
}
