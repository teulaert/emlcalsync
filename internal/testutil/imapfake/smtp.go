package imapfake

import (
	"io"
	"net"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/emersion/go-sasl"
	"github.com/emersion/go-smtp"
)

// Submission is one message the fake SMTP server accepted.
type Submission struct {
	From string
	To   []string
	Raw  []byte
}

// SMTPServer is an in-memory submission server. It records what it is given so
// a test can assert on the envelope — which is the whole point, since Bcc
// recipients exist only in the envelope and never in the message.
type SMTPServer struct {
	ln  net.Listener
	srv *smtp.Server

	mu   sync.Mutex
	sent []Submission
	// RejectAuth makes every AUTH fail, for the credential-error path.
	RejectAuth bool
	// RejectAfterData makes the server accept the envelope and then refuse the
	// message, which is the ambiguous case the outbox must not retry.
	RejectAfterData bool
}

// NewSMTP starts a submission server on loopback.
func NewSMTP(t testing.TB) *SMTPServer {
	t.Helper()
	s := &SMTPServer{}
	s.srv = smtp.NewServer(&smtpBackend{s: s})
	s.srv.AllowInsecureAuth = true
	s.srv.Domain = "localhost"
	s.srv.ReadTimeout = 10 * time.Second
	s.srv.WriteTimeout = 10 * time.Second

	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("imapfake: smtp listen: %v", err)
	}
	s.ln = ln
	go func() { _ = s.srv.Serve(ln) }()
	t.Cleanup(func() { _ = s.srv.Close() })
	return s
}

// Addr is the host:port to submit to.
func (s *SMTPServer) Addr() string { return s.ln.Addr().String() }

// Sent is everything the server accepted, in order.
func (s *SMTPServer) Sent() []Submission {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]Submission(nil), s.sent...)
}

type smtpBackend struct{ s *SMTPServer }

func (b *smtpBackend) NewSession(_ *smtp.Conn) (smtp.Session, error) {
	return &smtpSession{s: b.s}, nil
}

type smtpSession struct {
	s    *SMTPServer
	from string
	to   []string
}

func (ss *smtpSession) AuthMechanisms() []string { return []string{"PLAIN"} }

func (ss *smtpSession) Auth(mech string) (sasl.Server, error) {
	return sasl.NewPlainServer(func(identity, username, password string) error {
		if ss.s.RejectAuth {
			return &smtp.SMTPError{
				Code: 535, EnhancedCode: smtp.EnhancedCode{5, 7, 8},
				Message: "Authentication credentials invalid",
			}
		}
		return nil
	}), nil
}

func (ss *smtpSession) Mail(from string, _ *smtp.MailOptions) error {
	ss.from, ss.to = from, nil
	return nil
}

func (ss *smtpSession) Rcpt(to string, _ *smtp.RcptOptions) error {
	ss.to = append(ss.to, to)
	return nil
}

func (ss *smtpSession) Data(r io.Reader) error {
	raw, err := io.ReadAll(r)
	if err != nil {
		return err
	}
	if ss.s.RejectAfterData {
		return &smtp.SMTPError{
			Code: 554, EnhancedCode: smtp.EnhancedCode{5, 6, 0},
			Message: "Message content rejected",
		}
	}
	ss.s.mu.Lock()
	ss.s.sent = append(ss.s.sent, Submission{From: ss.from, To: ss.to, Raw: raw})
	ss.s.mu.Unlock()
	return nil
}

func (ss *smtpSession) Reset()        { ss.from, ss.to = "", nil }
func (ss *smtpSession) Logout() error { return nil }

// Body is the submission's message as a string.
func (s Submission) Body() string { return string(s.Raw) }

// HasRecipient reports whether the envelope named an address.
func (s Submission) HasRecipient(addr string) bool {
	for _, t := range s.To {
		if strings.EqualFold(t, addr) {
			return true
		}
	}
	return false
}
