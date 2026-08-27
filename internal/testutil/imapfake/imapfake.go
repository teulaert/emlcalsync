// Package imapfake is an in-memory IMAP server for tests.
//
// It is a thin wrapper over go-imap's own imapserver/imapmemserver rather than
// a hand-written protocol implementation — which is what makes an IMAP fake
// affordable at all. What the wrapper adds is the knobs that matter for a mail
// client: which capabilities the server admits to, and the ability to reach in
// and change the store the way a second client would.
//
// It lives outside _test.go so the provider's own tests, the CLI tests and the
// e2e suite (which needs a server the real Factory can be pointed at) can all
// use it.
package imapfake

import (
	"fmt"
	"io"
	"net"
	"strings"
	"sync"
	"testing"
	"time"

	imapv2 "github.com/emersion/go-imap/v2"
	"github.com/emersion/go-imap/v2/imapserver"
	"github.com/emersion/go-imap/v2/imapserver/imapmemserver"
)

// Username and Password are what the fake accepts.
const (
	Username = "user@example.com"
	Password = "app-specific-password"
)

// defaultCaps is what a reasonably modern server advertises. imapmemserver has
// no CONDSTORE, which is deliberate here: the no-CONDSTORE path is the harder
// one and the one most self-hosted servers take, so it is what the fake
// exercises by default.
var defaultCaps = []imapv2.Cap{
	imapv2.CapIMAP4rev1,
	imapv2.CapNamespace,
	imapv2.CapUIDPlus,
	imapv2.CapESearch,
	imapv2.CapListExtended,
	imapv2.CapListStatus,
	imapv2.CapMove,
	imapv2.CapSpecialUse,
	imapv2.CapIdle,
}

// Server is a running fake.
type Server struct {
	t    testing.TB
	ln   net.Listener
	srv  *imapserver.Server
	user *imapmemserver.User

	mu     sync.Mutex
	uidv   map[string]uint32
	script strings.Builder
}

// Option tunes a fake before it starts.
type Option func(*config)

type config struct {
	caps    []imapv2.Cap
	hidden  map[imapv2.Cap]bool
	verbose bool
	record  bool
}

// WithCaps replaces the advertised capability set outright.
func WithCaps(caps ...imapv2.Cap) Option {
	return func(c *config) { c.caps = caps }
}

// HideCaps drops capabilities from the default set, so a test can ask "what
// does this client do against a server that cannot do X?".
func HideCaps(caps ...imapv2.Cap) Option {
	return func(c *config) {
		for _, cap := range caps {
			c.hidden[cap] = true
		}
	}
}

// Verbose echoes the protocol conversation to the test log.
func Verbose() Option { return func(c *config) { c.verbose = true } }

// Record keeps the protocol conversation so a test can assert on what the
// client did or did not send. Some of what matters most about a mail client is
// a command it must never issue.
func Record() Option { return func(c *config) { c.record = true } }

// New starts a fake on loopback and registers cleanup.
func New(t testing.TB, opts ...Option) *Server {
	t.Helper()

	cfg := &config{caps: defaultCaps, hidden: map[imapv2.Cap]bool{}}
	for _, o := range opts {
		o(cfg)
	}
	caps := imapv2.CapSet{}
	for _, c := range cfg.caps {
		if !cfg.hidden[c] {
			caps[c] = struct{}{}
		}
	}

	mem := imapmemserver.New()
	user := imapmemserver.NewUser(Username, Password)
	mem.AddUser(user)

	s := &Server{uidv: map[string]uint32{}}
	var debug io.Writer
	switch {
	case cfg.verbose && cfg.record:
		debug = io.MultiWriter(testWriter{t}, (*scriptWriter)(s))
	case cfg.verbose:
		debug = testWriter{t}
	case cfg.record:
		debug = (*scriptWriter)(s)
	}
	srv := imapserver.New(&imapserver.Options{
		NewSession: func(*imapserver.Conn) (imapserver.Session, *imapserver.GreetingData, error) {
			return mem.NewSession(), nil, nil
		},
		Caps: caps,
		// Loopback, in-process, and the password is a constant in this file.
		InsecureAuth: true,
		DebugWriter:  debug,
		Logger:       nopLogger{},
	})

	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("imapfake: listen: %v", err)
	}
	go func() { _ = srv.Serve(ln) }()

	s.t, s.ln, s.srv, s.user = t, ln, srv, user
	t.Cleanup(func() { _ = srv.Close() })

	s.CreateMailbox("INBOX")
	return s
}

// Addr is the host:port to point a client at.
func (s *Server) Addr() string { return s.ln.Addr().String() }

// CreateMailbox adds a folder.
//
// The attrs are accepted but not advertised: imapmemserver takes
// CreateOptions.SpecialUse and drops it, and its LIST only emits attributes it
// has stored. So the fake exercises the name-based role fallback -- the path a
// server without SPECIAL-USE takes, and the one more likely to be wrong.
// Attribute mapping is pure logic and is unit-tested directly instead.
func (s *Server) CreateMailbox(name string, attrs ...imapv2.MailboxAttr) {
	s.t.Helper()
	err := s.user.Create(name, &imapv2.CreateOptions{SpecialUse: attrs})
	if err != nil && !strings.Contains(strings.ToLower(err.Error()), "exist") {
		s.t.Fatalf("imapfake: create %q: %v", name, err)
	}
}

// AddMessage appends a message and returns the uid it landed under.
func (s *Server) AddMessage(mailbox string, raw []byte, flags ...imapv2.Flag) imapv2.UID {
	s.t.Helper()
	data, err := s.user.Append(mailbox, literal{strings.NewReader(string(raw)), int64(len(raw))},
		&imapv2.AppendOptions{Flags: flags, Time: time.Now()})
	if err != nil {
		s.t.Fatalf("imapfake: append to %q: %v", mailbox, err)
	}
	s.mu.Lock()
	s.uidv[mailbox] = data.UIDValidity
	s.mu.Unlock()
	return data.UID
}

// Mail builds and appends a small RFC 822 message, returning its uid.
func (s *Server) Mail(mailbox, subject, body string, flags ...imapv2.Flag) imapv2.UID {
	s.t.Helper()
	return s.AddMessage(mailbox, []byte(Message(subject, body)), flags...)
}

// Message renders a minimal RFC 822 message with a unique Message-ID.
func Message(subject, body string) string {
	return fmt.Sprintf(
		"From: Someone <someone@example.com>\r\n"+
			"To: %s\r\n"+
			"Subject: %s\r\n"+
			"Message-ID: <%s@example.com>\r\n"+
			"Date: Mon, 02 Mar 2026 10:00:00 +0000\r\n"+
			"\r\n%s\r\n",
		Username, subject, sanitiseID(subject), body)
}

// Reply renders a message that answers another, so threading has something to
// work with.
func Reply(subject, body, inReplyTo string) string {
	return fmt.Sprintf(
		"From: Someone <someone@example.com>\r\n"+
			"To: %s\r\n"+
			"Subject: Re: %s\r\n"+
			"Message-ID: <%s@example.com>\r\n"+
			"In-Reply-To: <%s@example.com>\r\n"+
			"References: <%s@example.com>\r\n"+
			"Date: Mon, 02 Mar 2026 11:00:00 +0000\r\n"+
			"\r\n%s\r\n",
		Username, subject, sanitiseID(subject)+"-reply", inReplyTo, inReplyTo, body)
}

func sanitiseID(s string) string {
	return strings.NewReplacer(" ", "-", "<", "", ">", "", "@", "-").Replace(strings.ToLower(s))
}

// UIDValidity is the folder's current UIDVALIDITY.
func (s *Server) UIDValidity(mailbox string) uint32 {
	s.t.Helper()
	st, err := s.user.Status(mailbox, &imapv2.StatusOptions{UIDValidity: true})
	if err != nil {
		s.t.Fatalf("imapfake: status %q: %v", mailbox, err)
	}
	return st.UIDValidity
}

// Count is how many messages a folder holds.
func (s *Server) Count(mailbox string) int {
	s.t.Helper()
	st, err := s.user.Status(mailbox, &imapv2.StatusOptions{NumMessages: true})
	if err != nil {
		s.t.Fatalf("imapfake: status %q: %v", mailbox, err)
	}
	if st.NumMessages == nil {
		return 0
	}
	return int(*st.NumMessages)
}

// DeleteMailbox removes a folder, as a second client would.
func (s *Server) DeleteMailbox(name string) {
	s.t.Helper()
	if err := s.user.Delete(name); err != nil {
		s.t.Fatalf("imapfake: delete %q: %v", name, err)
	}
}

// RenameMailbox renames a folder, keeping its messages and their uids.
func (s *Server) RenameMailbox(old, new string) {
	s.t.Helper()
	if err := s.user.Rename(old, new, nil); err != nil {
		s.t.Fatalf("imapfake: rename %q: %v", old, err)
	}
}

// literal adapts a reader to imap.LiteralReader.
type literal struct {
	io.Reader
	n int64
}

func (l literal) Size() int64 { return l.n }

type nopLogger struct{}

func (nopLogger) Printf(string, ...any) {}

// Transcript is everything that crossed the wire, when Record was set.
func (s *Server) Transcript() string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.script.String()
}

// Sent reports whether the client ever sent a command matching a substring.
// Case-insensitive, because IMAP verbs are not case-sensitive on the wire.
func (s *Server) Sent(substr string) bool {
	return strings.Contains(strings.ToUpper(s.Transcript()), strings.ToUpper(substr))
}

type scriptWriter Server

func (w *scriptWriter) Write(p []byte) (int, error) {
	s := (*Server)(w)
	s.mu.Lock()
	s.script.Write(p)
	s.mu.Unlock()
	return len(p), nil
}

type testWriter struct{ t testing.TB }

func (w testWriter) Write(p []byte) (int, error) {
	w.t.Logf("imap: %s", strings.TrimRight(string(p), "\r\n"))
	return len(p), nil
}
