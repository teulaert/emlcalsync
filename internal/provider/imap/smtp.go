package imap

import (
	"context"
	"errors"
	"fmt"
	netmail "net/mail"
	"strings"
	"time"

	imapv2 "github.com/emersion/go-imap/v2"
	"github.com/emersion/go-sasl"
	"github.com/emersion/go-smtp"

	"github.com/teulaert/emlcalsync/internal/model"
	"github.com/teulaert/emlcalsync/internal/provider"
)

// Send submits a message. Prefer Submit: without an envelope the Bcc
// recipients cannot be reached, because the message deliberately omits them.
func (m *Mail) Send(ctx context.Context, raw []byte, _ string) (string, error) {
	return m.Submit(ctx, raw, provider.SubmitEnvelope{
		From: m.opts.Email,
		To:   recipientsFromHeaders(raw),
	})
}

// Submit implements provider.Submitter: SMTP submission, then a copy filed in
// Sent where the submission server does not do it for us.
//
// Order matters. Submitting first and failing to file leaves a sent message
// missing from the archive, which the next sync repairs. Filing first and
// failing to submit would leave a message in Sent that was never sent, which
// nothing repairs and which the user would reasonably believe.
func (m *Mail) Submit(ctx context.Context, raw []byte, env provider.SubmitEnvelope) (string, error) {
	if m.smtpAddr == "" {
		return "", fmt.Errorf("imap: no SMTP submission host configured for %s; "+
			"set smtp_host in [accounts.mail]", m.opts.Email)
	}
	from := env.From
	if from == "" {
		from = m.opts.Email
	}
	to := env.To
	if len(to) == 0 {
		to = recipientsFromHeaders(raw)
	}
	if len(to) == 0 {
		return "", errors.New("imap: refusing to send with no recipients")
	}

	if err := m.submitSMTP(ctx, from, to, raw); err != nil {
		return "", err
	}

	// The message is gone. Nothing from here on may fail the send.
	if m.preset.SMTPAppendsToSent {
		return "", nil
	}
	sent, err := m.roleRemote(ctx, model.RoleSent)
	if err != nil || sent == "" {
		m.log.Warn("message sent but not archived: no Sent folder found", "err", err)
		return "", nil
	}
	id, err := m.append(ctx, sent, raw, []imapv2.Flag{imapv2.FlagSeen}, time.Now())
	if err != nil {
		// Returning this error would either retire the row with a message
		// saying the send failed, or retry and deliver twice. Neither is true
		// or safe; the next sync picks the message up from Sent anyway.
		m.log.Warn("message sent but the Sent copy could not be filed",
			"mailbox", sent, "err", err)
		return "", nil
	}
	return id, nil
}

// submitSMTP performs the submission, classifying failures by how far they got.
//
// Everything up to and including the last RCPT acknowledgement has handed over
// no message, so those failures are marked pre-request and the outbox may
// safely try again. From the first byte of DATA onwards the outcome is unknown,
// so the error carries ErrOffline but never ErrNotConnected — a retry there
// could deliver the message twice.
func (m *Mail) submitSMTP(ctx context.Context, from string, to []string, raw []byte) error {
	var (
		c   *smtp.Client
		err error
	)
	switch m.smtpSec {
	case SecTLS:
		c, err = smtp.DialTLS(m.smtpAddr, m.opts.TLSConfig)
	case SecStartTLS:
		c, err = smtp.DialStartTLS(m.smtpAddr, m.opts.TLSConfig)
	case SecNone:
		c, err = smtp.Dial(m.smtpAddr)
	default:
		return fmt.Errorf("imap: unknown smtp security %q", m.smtpSec)
	}
	if err != nil {
		return dialErr("connecting to "+m.smtpAddr, err)
	}
	defer c.Close()

	pw := m.opts.SMTPPassword
	if pw == "" {
		pw = m.opts.Password
	}
	if ok, _ := c.Extension("AUTH"); ok {
		if err := c.Auth(sasl.NewPlainClient("", m.opts.SMTPUser(), pw)); err != nil {
			if isSMTPAuth(err) {
				return m.authError("SMTP: " + err.Error())
			}
			return preRequestErr("smtp auth", err)
		}
	}
	if err := c.Mail(from, nil); err != nil {
		return preRequestErr("smtp mail from", err)
	}
	for _, rcpt := range to {
		if err := c.Rcpt(rcpt, nil); err != nil {
			return preRequestErr("smtp rcpt", err)
		}
	}

	// Past here the message may have been accepted, whatever happens next.
	w, err := c.Data()
	if err != nil {
		return ambiguousErr("smtp data", err)
	}
	if _, err := w.Write(raw); err != nil {
		return ambiguousErr("smtp write", err)
	}
	if err := w.Close(); err != nil {
		return ambiguousErr("smtp commit", err)
	}
	_ = c.Quit()
	return nil
}

// preRequestErr marks a failure that happened before the message was handed
// over, so it may safely be tried again.
func preRequestErr(op string, err error) error {
	if isTransport(err) || isPreRequest(err) {
		return fmt.Errorf("imap: %s: %w: %w: %w", op, err, model.ErrOffline, provider.ErrNotConnected)
	}
	return fmt.Errorf("imap: %s: %w", op, err)
}

// ambiguousErr marks a failure whose outcome cannot be known. Never
// ErrNotConnected: the outbox must not repeat it.
func ambiguousErr(op string, err error) error {
	if isTransport(err) {
		return fmt.Errorf("imap: %s: %w: %w", op, err, model.ErrOffline)
	}
	return fmt.Errorf("imap: %s: %w", op, err)
}

// isSMTPAuth reports whether the server refused the credential (5xx 535-ish)
// rather than something else going wrong.
func isSMTPAuth(err error) bool {
	var se *smtp.SMTPError
	if errors.As(err, &se) {
		return se.Code == 535 || se.Code == 530 ||
			(se.EnhancedCode[0] == 5 && se.EnhancedCode[1] == 7)
	}
	return false
}

// recipientsFromHeaders reads To and Cc out of the message, for the Send path
// that has no envelope. It cannot recover Bcc — the header is not there, which
// is exactly why Submit exists.
func recipientsFromHeaders(raw []byte) []string {
	h := headerFields(raw, "to", "cc")
	var out []string
	seen := map[string]bool{}
	for _, key := range []string{"to", "cc"} {
		for _, a := range parseAddressList(h[key]) {
			if !seen[strings.ToLower(a)] {
				seen[strings.ToLower(a)] = true
				out = append(out, a)
			}
		}
	}
	return out
}

// parseAddressList pulls bare addresses out of a header value.
func parseAddressList(v string) []string {
	if strings.TrimSpace(v) == "" {
		return nil
	}
	list, err := netmail.ParseAddressList(v)
	if err != nil {
		return nil
	}
	out := make([]string, 0, len(list))
	for _, a := range list {
		out = append(out, a.Address)
	}
	return out
}
