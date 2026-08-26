package gcal

import (
	"context"
	"errors"
	"fmt"
	"io"
	"math/rand/v2"
	"net"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"

	"google.golang.org/api/googleapi"

	"github.com/teulaert/emlcalsync/internal/model"
)

const (
	maxAttempts  = 5
	baseBackoff  = 300 * time.Millisecond
	maxBackoff   = 30 * time.Second
	jitterFactor = 0.3
)

// do runs one API call, retrying transient failures (429, 403 rate limits,
// 5xx) with exponential backoff and jitter, and tagging transport failures
// with model.ErrOffline.
func (c *Calendar) do(ctx context.Context, name string, f func() error) error {
	var lastErr error
	for attempt := 1; attempt <= maxAttempts; attempt++ {
		if err := ctx.Err(); err != nil {
			return err
		}
		c.log.Debug("gcal call", "method", name, "attempt", attempt)
		err := f()
		if err == nil {
			return nil
		}
		if ctxErr := ctx.Err(); ctxErr != nil {
			return ctxErr
		}
		lastErr = wrapErr(name, err)
		if !retryable(err) || attempt == maxAttempts {
			return lastErr
		}
		delay := backoffFor(attempt, err)
		c.log.Debug("gcal call failed, retrying", "method", name, "attempt", attempt, "in", delay, "err", err)
		if err := sleepCtx(ctx, delay); err != nil {
			return err
		}
	}
	return lastErr
}

func sleepCtx(ctx context.Context, d time.Duration) error {
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-t.C:
		return nil
	}
}

func backoffFor(attempt int, err error) time.Duration {
	if d, ok := retryAfter(err); ok {
		return d
	}
	d := baseBackoff * (1 << (attempt - 1))
	if d > maxBackoff {
		d = maxBackoff
	}
	jitter := time.Duration(rand.Float64() * jitterFactor * float64(d))
	return d - time.Duration(jitterFactor*float64(d)/2) + jitter
}

func retryAfter(err error) (time.Duration, bool) {
	var ge *googleapi.Error
	if !errors.As(err, &ge) || ge.Header == nil {
		return 0, false
	}
	v := ge.Header.Get("Retry-After")
	if v == "" {
		return 0, false
	}
	if secs, err := strconv.Atoi(v); err == nil && secs >= 0 {
		return min(time.Duration(secs)*time.Second, maxBackoff), true
	}
	if t, err := http.ParseTime(v); err == nil {
		if d := time.Until(t); d > 0 {
			return min(d, maxBackoff), true
		}
	}
	return 0, false
}

var rateLimitReasons = map[string]bool{
	"rateLimitExceeded":     true,
	"userRateLimitExceeded": true,
	"backendError":          true,
	"quotaExceeded":         true,
}

func retryable(err error) bool {
	var ge *googleapi.Error
	if !errors.As(err, &ge) {
		return false
	}
	switch {
	case ge.Code == http.StatusTooManyRequests:
		return true
	case ge.Code == http.StatusForbidden:
		for _, item := range ge.Errors {
			if rateLimitReasons[item.Reason] {
				return true
			}
		}
		return strings.Contains(ge.Message, "Rate Limit Exceeded")
	case ge.Code >= 500 && ge.Code <= 599:
		return true
	}
	return false
}

// isGone reports the 410 that Google returns for an expired sync token.
func isGone(err error) bool {
	var ge *googleapi.Error
	return errors.As(err, &ge) && ge.Code == http.StatusGone
}

func isNotFound(err error) bool {
	var ge *googleapi.Error
	return errors.As(err, &ge) && (ge.Code == http.StatusNotFound || ge.Code == http.StatusGone)
}

func wrapErr(name string, err error) error {
	if err == nil {
		return nil
	}
	if off := offlineErr(err); off != nil {
		return fmt.Errorf("gcal %s: %w", name, off)
	}
	return fmt.Errorf("gcal %s: %w", name, err)
}

// offlineErr returns a model.ErrOffline-wrapped error for transport-level
// failures, or nil when the server answered.
func offlineErr(err error) error {
	if err == nil {
		return nil
	}
	if errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
		return nil
	}
	var ge *googleapi.Error
	if errors.As(err, &ge) {
		return nil
	}
	var dnsErr *net.DNSError
	var opErr *net.OpError
	var urlErr *url.Error
	var netErr net.Error
	switch {
	case errors.As(err, &dnsErr), errors.As(err, &opErr), errors.As(err, &netErr), errors.As(err, &urlErr):
		return fmt.Errorf("%w: %v", model.ErrOffline, err)
	case errors.Is(err, io.EOF), errors.Is(err, io.ErrUnexpectedEOF):
		return fmt.Errorf("%w: %v", model.ErrOffline, err)
	}
	return nil
}
