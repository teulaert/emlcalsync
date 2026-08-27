package imap

import (
	"errors"
	"fmt"
	"io"
	"net"
	"strings"
	"syscall"

	imapv2 "github.com/emersion/go-imap/v2"

	"github.com/teulaert/emlcalsync/internal/model"
	"github.com/teulaert/emlcalsync/internal/provider"
)

// AuthError is a login the server refused. It names the credential the way the
// vendor does, because "authentication failed" against iCloud almost always
// means the user typed their Apple ID password instead of an app-specific one.
type AuthError struct {
	Email          string
	Detail         string
	CredentialName string
	CredentialURL  string
}

func (e *AuthError) Error() string {
	cred := e.CredentialName
	if cred == "" {
		cred = "password"
	}
	msg := fmt.Sprintf("imap: %s rejected the login for %s", cred, e.Email)
	if e.Detail != "" {
		msg += ": " + e.Detail
	}
	if e.CredentialURL != "" {
		msg += fmt.Sprintf(" (create one at %s)", e.CredentialURL)
	}
	return msg
}

// IsAuth reports whether err is a login the server refused.
func IsAuth(err error) bool {
	var a *AuthError
	return errors.As(err, &a)
}

// authError builds an AuthError from the preset's credential vocabulary.
func (m *Mail) authError(detail string) error {
	return &AuthError{
		Email:          m.opts.Email,
		Detail:         detail,
		CredentialName: m.preset.CredentialName,
		CredentialURL:  m.preset.CredentialURL,
	}
}

// isAuthResponse reports whether an IMAP error is the server rejecting the
// credential rather than anything else going wrong.
func isAuthResponse(err error) bool {
	var se *imapv2.Error
	if errors.As(err, &se) {
		switch se.Code {
		case imapv2.ResponseCodeAuthenticationFailed,
			imapv2.ResponseCodeAuthorizationFailed,
			imapv2.ResponseCodeExpired,
			imapv2.ResponseCodePrivacyRequired:
			return true
		}
		if se.Type == imapv2.StatusResponseTypeNo &&
			strings.Contains(strings.ToLower(se.Text), "authenticat") {
			return true
		}
	}
	return false
}

// wrapErr classifies an error from the wire.
//
// The distinction that matters is transport-versus-rejection: the engine rides
// out an outage and retires a rejection. A transport failure is additionally
// marked ErrNotConnected when we know the request never left the machine, which
// is what lets the outbox re-queue a send instead of reporting it as
// possibly-half-done. %w throughout — formatting a cause with %v flattens the
// chain and makes every failure look ambiguous.
func wrapErr(op string, err error) error {
	if err == nil {
		return nil
	}
	if errors.Is(err, model.ErrOffline) || IsAuth(err) {
		return err
	}
	if isTransport(err) {
		if isPreRequest(err) {
			return fmt.Errorf("imap: %s: %w: %w: %w", op, err, model.ErrOffline, provider.ErrNotConnected)
		}
		return fmt.Errorf("imap: %s: %w: %w", op, err, model.ErrOffline)
	}
	return fmt.Errorf("imap: %s: %w", op, err)
}

// dialErr is a connection that never opened: nothing was sent, so anything
// waiting on it may safely be tried again.
func dialErr(op string, err error) error {
	return fmt.Errorf("imap: %s: %w: %w: %w", op, err, model.ErrOffline, provider.ErrNotConnected)
}

// isTransport reports whether the failure was the connection rather than the
// server's considered answer. A server that says NO or BAD has answered.
func isTransport(err error) bool {
	var se *imapv2.Error
	if errors.As(err, &se) {
		return false
	}
	var ne net.Error
	return errors.As(err, &ne) ||
		errors.Is(err, io.EOF) ||
		errors.Is(err, io.ErrUnexpectedEOF) ||
		errors.Is(err, net.ErrClosed) ||
		errors.Is(err, syscall.ECONNRESET) ||
		errors.Is(err, syscall.EPIPE) ||
		isPreRequest(err)
}

// isPreRequest reports whether the connection demonstrably never opened.
// Deliberately narrow, and it mirrors provider.IsPreRequestFailure: a timeout
// or a reset can mean the request arrived and the answer was lost.
func isPreRequest(err error) bool {
	var dns *net.DNSError
	if errors.As(err, &dns) {
		return true
	}
	var op *net.OpError
	if errors.As(err, &op) && op.Op == "dial" {
		return true
	}
	return errors.Is(err, syscall.ECONNREFUSED) ||
		errors.Is(err, syscall.ENETUNREACH) ||
		errors.Is(err, syscall.EHOSTUNREACH)
}
