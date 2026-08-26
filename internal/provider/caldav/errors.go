package caldav

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"strings"

	"github.com/teulaert/emlcalsync/internal/model"
	"github.com/teulaert/emlcalsync/internal/provider"
)

// AuthError is returned when the server rejects the credentials (401, or the
// 403 a server sends when the app password exists but may not touch
// calendars). It is a distinct type so the CLI can tell "wrong password" from
// "server said no to this particular request".
type AuthError struct {
	Email  string
	Status int
	Detail string
}

func (e *AuthError) Error() string {
	msg := fmt.Sprintf("caldav: %s rejected the app password (HTTP %d)", e.Email, e.Status)
	if e.Detail != "" {
		msg += ": " + e.Detail
	}
	return msg + "; create one with access \"Calendars (CalDAV)\" at https://app.fastmail.com/settings/security/devices"
}

// IsAuth reports whether err is an authentication failure.
func IsAuth(err error) bool {
	var ae *AuthError
	return errors.As(err, &ae)
}

// httpError is a non-2xx answer from the server. The body is kept (truncated)
// because CalDAV puts its machine-readable preconditions there.
type httpError struct {
	Method string
	Path   string
	Code   int
	Body   string
}

func (e *httpError) Error() string {
	s := fmt.Sprintf("caldav: %s %s: %d %s", e.Method, e.Path, e.Code, http.StatusText(e.Code))
	if e.Body != "" {
		s += ": " + e.Body
	}
	return s
}

// statusOf returns the HTTP status carried by err, or 0.
func statusOf(err error) int {
	var he *httpError
	if errors.As(err, &he) {
		return he.Code
	}
	return 0
}

func isNotFound(err error) bool {
	c := statusOf(err)
	return c == http.StatusNotFound || c == http.StatusGone
}

// isInvalidSyncToken reports the RFC 6578 DAV:valid-sync-token precondition,
// which servers signal with 403 (and, in the wild, 409) plus that element in
// the error body. A 507 with DAV:number-of-matches-within-limits means the
// delta is larger than the server will report and also requires a full
// listing, so it is folded in here.
func isInvalidSyncToken(err error) bool {
	var he *httpError
	if !errors.As(err, &he) {
		return false
	}
	body := strings.ToLower(he.Body)
	switch he.Code {
	case http.StatusForbidden, http.StatusConflict, http.StatusBadRequest:
		return strings.Contains(body, "valid-sync-token")
	case http.StatusInsufficientStorage:
		return true
	}
	return false
}

// offlineErr wraps transport-level failures with model.ErrOffline (and, when
// the request demonstrably never left the machine, provider.ErrNotConnected)
// so the sync engine can tell "no network" from "the server said no".
func offlineErr(err error) error {
	if err == nil {
		return nil
	}
	if errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
		return nil
	}
	var he *httpError
	if errors.As(err, &he) {
		return nil // the server answered
	}
	var dnsErr *net.DNSError
	var opErr *net.OpError
	var urlErr *url.Error
	var netErr net.Error
	switch {
	case errors.As(err, &dnsErr), errors.As(err, &opErr), errors.As(err, &netErr), errors.As(err, &urlErr):
	case errors.Is(err, io.EOF), errors.Is(err, io.ErrUnexpectedEOF):
	default:
		return nil
	}
	if provider.IsPreRequestFailure(err) {
		return fmt.Errorf("%w: %w: %v", model.ErrOffline, provider.ErrNotConnected, err)
	}
	return fmt.Errorf("%w: %v", model.ErrOffline, err)
}

// wrapErr tags an error with the operation that produced it, turning
// transport failures into model.ErrOffline on the way through.
func wrapErr(op string, err error) error {
	if err == nil {
		return nil
	}
	if off := offlineErr(err); off != nil {
		return fmt.Errorf("caldav %s: %w", op, off)
	}
	return fmt.Errorf("caldav %s: %w", op, err)
}
