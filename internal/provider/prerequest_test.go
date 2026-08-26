package provider

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"net/url"
	"os"
	"syscall"
	"testing"

	"github.com/teulaert/emlcalsync/internal/model"
)

// TestIsPreRequestFailure pins the classification the outbox relies on to
// decide whether a send may be retried. A false positive here delivers a
// message twice, so the ambiguous cases matter as much as the clear ones.
func TestIsPreRequestFailure(t *testing.T) {
	dialRefused := &net.OpError{
		Op:  "dial",
		Net: "tcp",
		Err: &os.SyscallError{Syscall: "connect", Err: syscall.ECONNREFUSED},
	}

	cases := []struct {
		name string
		err  error
		want bool
	}{
		{"nil", nil, false},
		{"ErrNotConnected", ErrNotConnected, true},
		{"ErrNotConnected wrapped twice", fmt.Errorf("send: %w: %w", ErrNotConnected, model.ErrOffline), true},
		{"DNS failure", &net.DNSError{Err: "no such host", Name: "api.example.com"}, true},
		{"DNS failure behind url.Error", &url.Error{
			Op: "Post", URL: "https://api.example.com/",
			Err: &net.DNSError{Err: "no such host", Name: "api.example.com"},
		}, true},
		{"dial refused", dialRefused, true},
		{"dial refused behind url.Error", &url.Error{Op: "Post", URL: "https://x/", Err: dialRefused}, true},
		{"bare ECONNREFUSED", syscall.ECONNREFUSED, true},
		{"ENETUNREACH", fmt.Errorf("connect: %w", syscall.ENETUNREACH), true},
		{"EHOSTUNREACH", fmt.Errorf("connect: %w", syscall.EHOSTUNREACH), true},

		// Everything below may mean the server already acted on the request.
		{"read timeout", &net.OpError{Op: "read", Net: "tcp", Err: os.ErrDeadlineExceeded}, false},
		{"timeout behind url.Error", &url.Error{
			Op: "Post", URL: "https://x/", Err: os.ErrDeadlineExceeded,
		}, false},
		{"EOF", io.EOF, false},
		{"unexpected EOF", io.ErrUnexpectedEOF, false},
		{"connection reset", fmt.Errorf("read tcp: %w", syscall.ECONNRESET), false},
		{"context deadline", context.DeadlineExceeded, false},
		{"context cancelled", context.Canceled, false},
		{"plain offline", fmt.Errorf("provider: %w", model.ErrOffline), false},
		{"HTTP 500", errors.New("unexpected status 500"), false},
		{"HTTP 403", errors.New("403 permission denied"), false},
	}

	for _, c := range cases {
		if got := IsPreRequestFailure(c.err); got != c.want {
			t.Errorf("%s: IsPreRequestFailure(%v) = %v, want %v", c.name, c.err, got, c.want)
		}
	}
}

// TestPreRequestFailureNeedsAnIntactChain documents the one way a real
// provider loses this: formatting the cause with %v instead of %w flattens the
// error chain, and a genuine dial failure then reads as ambiguous. That errs
// toward not retrying a send, never toward sending it twice.
func TestPreRequestFailureNeedsAnIntactChain(t *testing.T) {
	cause := &net.OpError{Op: "dial", Net: "tcp", Err: syscall.ECONNREFUSED}

	kept := fmt.Errorf("%w: %w", model.ErrOffline, cause)
	if !IsPreRequestFailure(kept) {
		t.Error("a dial failure wrapped with %w was not recognised")
	}

	flattened := fmt.Errorf("%w: %v", model.ErrOffline, cause)
	if IsPreRequestFailure(flattened) {
		t.Error("a flattened chain must not be guessed at from the message text")
	}
	if !IsOffline(flattened) {
		t.Error("the offline sentinel is still expected to survive")
	}
}
