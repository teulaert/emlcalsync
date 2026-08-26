package sync

import (
	"context"
	"fmt"
	"time"

	"github.com/teulaert/emlcalsync/internal/provider"
)

// Offline ride-out (DESIGN.md §12).
//
// A provider client already retries a request a handful of times before it
// gives up. What it cannot do is wait out a real outage: a laptop that loses
// wifi in the middle of a multi-hour backfill used to fail the whole pass with
// "network is unreachable (after 5 attempts)". rideOut sits above the client
// and keeps re-running the *resource sync* — which resumes from its persisted
// cursor/state — until the network is back or the budget is spent.
//
// Only transport failures qualify. A provider that rejects a request still
// fails the pass immediately: waiting would not change the answer.
const (
	// oneShotWaitMin/Max bound the backoff of `emlcal sync --wait-offline`.
	oneShotWaitMin = 5 * time.Second
	oneShotWaitMax = 60 * time.Second
	// DefaultWaitOffline is the budget `emlcal sync` uses when the flag is
	// not given.
	DefaultWaitOffline = 10 * time.Minute
	// waitForever is the budget the daemon uses: it never stops trying.
	waitForever = time.Duration(-1)
	// PhaseOffline is the ProgressEvent phase emitted while waiting for the
	// network to come back.
	PhaseOffline = "offline"
)

// offlineWait is one retry policy: how long to keep trying and how fast the
// backoff grows. A zero budget disables retrying altogether, which is the old
// behaviour and what `--wait-offline 0` asks for.
type offlineWait struct {
	budget time.Duration // 0 = no retry, <0 = forever
	min    time.Duration
	max    time.Duration
	// waitIf gates the *first* wait. `emlcal sync` on a machine with no
	// network at all must still exit 4 straight away (DESIGN.md §12) rather
	// than block its caller for the whole budget; what deserves waiting for
	// is a job already under way. Nil means always wait.
	waitIf func() bool
}

// oneShotWait is the policy of a single `emlcal sync` pass. The bounds live on
// the Engine so a test can shrink them; nothing else changes them.
func (e *Engine) oneShotWait(budget time.Duration) offlineWait {
	return offlineWait{budget: budget, min: e.waitMin, max: e.waitMax}
}

// daemonWait is the policy of `sync --watch`: keep trying forever, with the
// gentler ceiling of DESIGN.md §12 (5 s → 5 min).
func daemonWait() offlineWait {
	return offlineWait{budget: waitForever, min: offlineMin, max: offlineMax}
}

// rideOut runs attempt, and while it fails because the machine cannot reach
// the provider, waits with backoff and runs it again. It returns the last
// error when the budget runs out (or immediately, for any error that is not a
// transport failure).
func (e *Engine) rideOut(ctx context.Context, p offlineWait, account, resource string, attempt func(context.Context) error) error {
	if p.budget == 0 {
		return attempt(ctx)
	}
	var deadline time.Time
	if p.budget > 0 {
		deadline = time.Now().Add(p.budget)
	}
	wait := time.Duration(0)
	outage := false
	for {
		err := attempt(ctx)
		if err == nil {
			if outage {
				e.log.Info("network is back", "account", account, "resource", resource)
			}
			return nil
		}
		if !provider.IsOffline(err) || ctx.Err() != nil {
			return err
		}

		left := time.Duration(-1)
		if !deadline.IsZero() {
			if left = time.Until(deadline); left <= 0 {
				e.log.Warn("still offline after the whole --wait-offline budget; giving up",
					"account", account, "resource", resource, "budget", p.budget, "err", err)
				return err
			}
		}
		if !outage && p.waitIf != nil && !p.waitIf() {
			// Nothing is in flight: there is no progress to protect, so say
			// so now instead of holding the caller for the whole budget.
			e.log.Warn("offline with nothing in flight; not waiting",
				"account", account, "resource", resource, "err", err)
			return err
		}
		wait = nextBackoff(wait, p.min, p.max)
		if left >= 0 && wait > left {
			wait = left
		}
		if !outage {
			outage = true
			e.log.Warn("offline; waiting for the network",
				"account", account, "resource", resource, "budget", budgetLabel(p.budget), "err", err)
		} else {
			e.log.Debug("still offline", "account", account, "resource", resource,
				"retry_in", wait, "err", err)
		}
		e.emit(ProgressEvent{
			Account: account, Resource: resource, Phase: PhaseOffline,
			Message: offlineMessage(wait, left),
		})
		if !sleepCtx(ctx, wait) {
			// The context ended while we were waiting: report what actually
			// stopped the pass, which is the provider failure.
			return err
		}
	}
}

// offlineMessage is the progress line shown while waiting.
func offlineMessage(retryIn, left time.Duration) string {
	if left < 0 {
		return fmt.Sprintf("waiting for network (retry in %s)", shortDur(retryIn))
	}
	return fmt.Sprintf("waiting for network (retry in %s, %s left)", shortDur(retryIn), shortDur(left))
}

func budgetLabel(d time.Duration) string {
	if d < 0 {
		return "unlimited"
	}
	return shortDur(d)
}

// shortDur renders a duration the way a progress line wants it: "45s", "8m",
// "1h18m" — never "8m0s".
func shortDur(d time.Duration) string {
	if d < 0 {
		d = 0
	}
	switch {
	case d < time.Minute:
		return fmt.Sprintf("%ds", int((d + 500*time.Millisecond).Seconds()))
	case d < time.Hour:
		return fmt.Sprintf("%dm", int((d + 30*time.Second).Minutes()))
	default:
		d = d.Round(time.Minute)
		if m := int(d.Minutes()) % 60; m != 0 {
			return fmt.Sprintf("%dh%02dm", int(d.Hours()), m)
		}
		return fmt.Sprintf("%dh", int(d.Hours()))
	}
}
