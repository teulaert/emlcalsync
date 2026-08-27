package imap

import (
	"net"
	"strings"
	"syscall"
	"testing"

	"github.com/teulaert/emlcalsync/internal/model"
	"github.com/teulaert/emlcalsync/internal/provider"
	"github.com/teulaert/emlcalsync/internal/testutil/imapfake"
)

// dialSubmit wires a provider to both an IMAP fake and an SMTP fake.
func dialSubmit(t *testing.T) (*Mail, *imapfake.Server, *imapfake.SMTPServer) {
	t.Helper()
	srv := imapfake.New(t)
	srv.CreateMailbox("Sent")
	srv.CreateMailbox("Drafts")
	srv.CreateMailbox("Trash")
	smtpSrv := imapfake.NewSMTP(t)

	host, port := splitAddr(t, smtpSrv.Addr())
	m := newProvider(t, srv, func(o *Options) {
		o.SMTPHost = host
		o.SMTPPort = port
		o.SMTPSecurity = SecNone
	})
	return m, srv, smtpSrv
}

// The reason Submit exists: Bcc is not in the message, so only an explicit
// envelope can deliver to blind recipients.
func TestSubmitDeliversToBccAndKeepsItOutOfTheMessage(t *testing.T) {
	m, _, smtpSrv := dialSubmit(t)
	raw := []byte("From: " + imapfake.Username + "\r\nTo: visible@example.com\r\n" +
		"Subject: hello\r\n\r\nbody\r\n")

	if _, err := m.Submit(testCtx(t), raw, provider.SubmitEnvelope{
		From: imapfake.Username,
		To:   []string{"visible@example.com", "blind@example.com"},
	}); err != nil {
		t.Fatalf("Submit: %v", err)
	}

	sent := smtpSrv.Sent()
	if len(sent) != 1 {
		t.Fatalf("submitted %d messages, want 1", len(sent))
	}
	if !sent[0].HasRecipient("blind@example.com") {
		t.Errorf("RCPT TO = %v, missing the blind recipient", sent[0].To)
	}
	if strings.Contains(strings.ToLower(sent[0].Body()), "bcc:") {
		t.Error("the Bcc header reached the recipients")
	}
}

// The sent copy has to be filed, or a sent message never appears in the
// archive.
func TestSubmitFilesACopyInSent(t *testing.T) {
	m, srv, _ := dialSubmit(t)
	raw := []byte(imapfake.Message("filed", "body"))

	if _, err := m.Submit(testCtx(t), raw, provider.SubmitEnvelope{
		From: imapfake.Username, To: []string{"someone@example.com"},
	}); err != nil {
		t.Fatalf("Submit: %v", err)
	}
	if srv.Count("Sent") != 1 {
		t.Errorf("Sent holds %d messages, want the copy", srv.Count("Sent"))
	}
}

// A server that files its own copy must not get a second one.
func TestSubmitSkipsTheCopyWhenTheServerFilesItself(t *testing.T) {
	m, srv, _ := dialSubmit(t)
	m.preset.SMTPAppendsToSent = true

	if _, err := m.Submit(testCtx(t), []byte(imapfake.Message("once", "body")),
		provider.SubmitEnvelope{From: imapfake.Username, To: []string{"a@example.com"}}); err != nil {
		t.Fatalf("Submit: %v", err)
	}
	if n := srv.Count("Sent"); n != 0 {
		t.Errorf("Sent holds %d; the submission server already filed one", n)
	}
}

// The message is gone by the time we try to file it. Failing the send would
// either retire it as failed (a lie) or retry it (delivering twice).
func TestSubmitSucceedsEvenIfTheSentCopyCannotBeFiled(t *testing.T) {
	srv := imapfake.New(t) // no Sent folder at all
	smtpSrv := imapfake.NewSMTP(t)
	host, port := splitAddr(t, smtpSrv.Addr())
	m := newProvider(t, srv, func(o *Options) {
		o.SMTPHost, o.SMTPPort, o.SMTPSecurity = host, port, SecNone
	})

	if _, err := m.Submit(testCtx(t), []byte(imapfake.Message("orphan", "body")),
		provider.SubmitEnvelope{From: imapfake.Username, To: []string{"a@example.com"}}); err != nil {
		t.Fatalf("Submit reported failure for a message that was delivered: %v", err)
	}
	if len(smtpSrv.Sent()) != 1 {
		t.Error("the message was not actually submitted")
	}
}

// Everything up to the last RCPT has handed over nothing, so the outbox may
// safely queue and retry it.
func TestSubmitDialFailureIsSafeToRetry(t *testing.T) {
	srv := imapfake.New(t)
	m := newProvider(t, srv, func(o *Options) {
		o.SMTPHost, o.SMTPPort, o.SMTPSecurity = "127.0.0.1", 1, SecNone
	})

	_, err := m.Submit(testCtx(t), []byte(imapfake.Message("nowhere", "body")),
		provider.SubmitEnvelope{From: imapfake.Username, To: []string{"a@example.com"}})
	if err == nil {
		t.Fatal("submitting to a dead port succeeded")
	}
	if !provider.IsOffline(err) {
		t.Errorf("error is not offline: %v", err)
	}
	if !provider.IsPreRequestFailure(err) {
		t.Errorf("a refused connection must be safe to retry, got %v", err)
	}
}

// From the first byte of DATA the outcome is unknowable, so it must NOT be
// marked safe to retry -- the alternative is delivering the message twice.
func TestSubmitFailureAfterDataIsNotMarkedRetryable(t *testing.T) {
	m, _, smtpSrv := dialSubmit(t)
	smtpSrv.RejectAfterData = true

	_, err := m.Submit(testCtx(t), []byte(imapfake.Message("rejected", "body")),
		provider.SubmitEnvelope{From: imapfake.Username, To: []string{"a@example.com"}})
	if err == nil {
		t.Fatal("a rejected message reported success")
	}
	if provider.IsPreRequestFailure(err) {
		t.Errorf("a post-DATA failure was marked safe to retry, which risks sending twice: %v", err)
	}
}

// The classification itself, tested directly -- the integration test above
// cannot reach the dangerous case, because a server that answers 554 has
// answered, and it is the connection dying mid-DATA that leaves the outcome
// genuinely unknown.
//
// This is the single most consequential rule in the package: mark a post-DATA
// failure retryable and the outbox will send the message a second time.
func TestSubmitErrorClassificationByPhase(t *testing.T) {
	// A dropped connection: indistinguishable, from here, between "never
	// arrived" and "arrived and the acknowledgement was lost".
	dropped := &net.OpError{Op: "write", Err: syscall.ECONNRESET}

	before := preRequestErr("smtp rcpt", dropped)
	if !provider.IsOffline(before) {
		t.Errorf("a dropped connection before DATA is not offline: %v", before)
	}
	if !provider.IsPreRequestFailure(before) {
		t.Errorf("a failure before DATA must be safe to retry: %v", before)
	}

	after := ambiguousErr("smtp write", dropped)
	if !provider.IsOffline(after) {
		t.Errorf("a dropped connection during DATA is not offline: %v", after)
	}
	if provider.IsPreRequestFailure(after) {
		t.Error("a connection dropped during DATA was marked safe to retry; " +
			"the outbox would send the message twice")
	}
}

// A refused credential must name the credential, not just say "auth failed".
func TestSubmitAuthFailureIsAnAuthError(t *testing.T) {
	m, _, smtpSrv := dialSubmit(t)
	smtpSrv.RejectAuth = true
	m.preset.CredentialName = "app-specific password"
	m.preset.CredentialURL = "https://example.invalid/passwords"

	_, err := m.Submit(testCtx(t), []byte(imapfake.Message("nope", "body")),
		provider.SubmitEnvelope{From: imapfake.Username, To: []string{"a@example.com"}})
	if err == nil {
		t.Fatal("submitting with a rejected credential succeeded")
	}
	if !IsAuth(err) {
		t.Fatalf("not reported as an auth failure: %v", err)
	}
	if !strings.Contains(err.Error(), "app-specific password") {
		t.Errorf("error does not name the credential: %v", err)
	}
}

// Send has no envelope, so it can only reach the visible recipients. That is
// exactly why the engine prefers Submit.
func TestSendFallsBackToTheHeaders(t *testing.T) {
	m, _, smtpSrv := dialSubmit(t)
	raw := []byte("From: " + imapfake.Username + "\r\nTo: a@example.com\r\nCc: b@example.com\r\n" +
		"Subject: hi\r\n\r\nbody\r\n")

	if _, err := m.Send(testCtx(t), raw, ""); err != nil {
		t.Fatalf("Send: %v", err)
	}
	sent := smtpSrv.Sent()
	if len(sent) != 1 {
		t.Fatalf("submitted %d, want 1", len(sent))
	}
	if !sent[0].HasRecipient("a@example.com") || !sent[0].HasRecipient("b@example.com") {
		t.Errorf("RCPT TO = %v, want both To and Cc", sent[0].To)
	}
}

func TestSubmitWithNoSMTPHostSaysSo(t *testing.T) {
	m, _ := dial(t) // no SMTP configured
	_, err := m.Submit(testCtx(t), []byte("x"), provider.SubmitEnvelope{
		From: "a@example.com", To: []string{"b@example.com"},
	})
	if err == nil || !strings.Contains(err.Error(), "smtp_host") {
		t.Errorf("error does not say what to configure: %v", err)
	}
}

func TestFetchAttachmentIsNotFound(t *testing.T) {
	m, _ := dial(t)
	_, err := m.FetchAttachment(testCtx(t), "id", "ref")
	if err == nil || !strings.Contains(err.Error(), "archived message") {
		t.Errorf("unexpected error: %v", err)
	}
	if !strings.Contains(err.Error(), model.ErrNotFound.Error()) {
		t.Errorf("should wrap ErrNotFound: %v", err)
	}
}
