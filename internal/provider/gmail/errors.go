package gmail

import (
	"context"
	"encoding/base64"
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
	maxAttempts  = 6
	baseBackoff  = 300 * time.Millisecond
	maxBackoff   = 30 * time.Second
	jitterFactor = 0.3
)

// nonIdempotent lists the calls that must never be repeated once the server
// has answered, however transient the answer looks.
//
// A 500/503/429 on messages.send or drafts.create does not prove the request
// was rejected: Gmail may well have accepted the message and failed on the way
// back, and a retry would then send or draft it a second time. Losing the id
// of a message that was in fact delivered is far cheaper than delivering it
// twice, so these calls fail fast and let the outbox decide (it can look for
// the message before trying again).
//
// Transport failures are a different story and are handled the same way for
// every call: they are not retried here either, just tagged model.ErrOffline.
var nonIdempotent = map[string]bool{
	"messages.send": true,
	"drafts.create": true,
}

// do runs one API call: it waits for quota, logs it, and retries transient
// failures (429, 403 rateLimitExceeded/userRateLimitExceeded, 5xx) with
// exponential backoff and jitter. Calls named in nonIdempotent are never
// retried after the server answered. Transport failures are not retried here —
// they are wrapped with model.ErrOffline so the sync engine can back off
// wholesale (§12).
func (m *Mail) do(ctx context.Context, name string, units int, f func() error) error {
	var lastErr error
	for attempt := 1; attempt <= maxAttempts; attempt++ {
		if err := m.wait(ctx, units); err != nil {
			return err
		}
		m.log.Debug("gmail call", "method", name, "units", units, "attempt", attempt)
		err := f()
		if err == nil {
			return nil
		}
		if ctxErr := ctx.Err(); ctxErr != nil {
			return ctxErr
		}
		lastErr = wrapErr(name, err)
		if nonIdempotent[name] {
			m.log.Debug("gmail write failed, not retrying a non-idempotent call",
				"method", name, "err", err)
			return lastErr
		}
		if !retryable(err) || attempt == maxAttempts {
			return lastErr
		}
		delay := backoffFor(attempt, err)
		m.log.Debug("gmail call failed, retrying", "method", name, "attempt", attempt, "in", delay, "err", err)
		if err := sleepCtx(ctx, delay); err != nil {
			return err
		}
	}
	return lastErr
}

// wait spends quota units from the token bucket.
func (m *Mail) wait(ctx context.Context, units int) error {
	if units <= 0 {
		units = 1
	}
	if b := m.limiter.Burst(); units > b {
		units = b
	}
	if err := m.limiter.WaitN(ctx, units); err != nil {
		return fmt.Errorf("gmail: waiting for quota: %w", err)
	}
	return nil
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
		d := time.Duration(secs) * time.Second
		if d > maxBackoff {
			d = maxBackoff
		}
		return d, true
	}
	if t, err := http.ParseTime(v); err == nil {
		if d := time.Until(t); d > 0 {
			if d > maxBackoff {
				d = maxBackoff
			}
			return d, true
		}
	}
	return 0, false
}

// rateLimitReasons are the 403 reasons Gmail uses for throttling as opposed to
// a genuine permission problem.
var rateLimitReasons = map[string]bool{
	"rateLimitExceeded":     true,
	"userRateLimitExceeded": true,
	"backendError":          true,
	"quotaExceeded":         true,
}

// retryable reports whether the call is worth repeating.
func retryable(err error) bool {
	if err == nil {
		return false
	}
	var ge *googleapi.Error
	if errors.As(err, &ge) {
		return retryableStatus(ge.Code, ge)
	}
	return false
}

// retryableStatus classifies an HTTP status (used both for whole calls and for
// individual parts of a batch response).
func retryableStatus(code int, ge *googleapi.Error) bool {
	switch {
	case code == http.StatusTooManyRequests:
		return true
	case code == http.StatusForbidden:
		if ge == nil {
			return false
		}
		for _, item := range ge.Errors {
			if rateLimitReasons[item.Reason] {
				return true
			}
		}
		return strings.Contains(ge.Message, "Rate Limit Exceeded") ||
			strings.Contains(ge.Message, "rateLimitExceeded")
	case code >= 500 && code <= 599:
		return true
	}
	return false
}

// ErrStateExpired detection: Gmail answers history.list with 404 once the
// startHistoryId is too old.
func isNotFound(err error) bool {
	var ge *googleapi.Error
	return errors.As(err, &ge) && ge.Code == http.StatusNotFound
}

// wrapErr adds the call name and tags transport failures as offline.
func wrapErr(name string, err error) error {
	if err == nil {
		return nil
	}
	if off := offlineErr(err); off != nil {
		return fmt.Errorf("gmail %s: %w", name, off)
	}
	return fmt.Errorf("gmail %s: %w", name, err)
}

// offlineErr returns a model.ErrOffline-wrapped error when err is a
// transport-level failure, or nil when the server answered.
func offlineErr(err error) error {
	if err == nil {
		return nil
	}
	if errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
		return nil
	}
	var ge *googleapi.Error
	if errors.As(err, &ge) {
		return nil // the server responded, just not happily
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

// decodeBase64URL decodes Gmail's base64url payloads, tolerating both padded
// and unpadded forms.
func decodeBase64URL(s string) ([]byte, error) {
	s = strings.TrimRight(s, "=")
	b, err := base64.RawURLEncoding.DecodeString(s)
	if err != nil {
		// Some proxies hand back the standard alphabet; try that too.
		if b2, err2 := base64.RawStdEncoding.DecodeString(s); err2 == nil {
			return b2, nil
		}
		return nil, fmt.Errorf("decode base64url payload: %w", err)
	}
	return b, nil
}

func timeFromUnixMilli(ms int64) time.Time {
	return time.UnixMilli(ms).UTC()
}
