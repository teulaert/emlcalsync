package imap

import (
	"crypto/tls"
	"fmt"
	"log/slog"
	"net"
	"strconv"
	"strings"
	"sync"

	imapv2 "github.com/emersion/go-imap/v2"

	"github.com/teulaert/emlcalsync/internal/model"
	"github.com/teulaert/emlcalsync/internal/provider"
)

// Options configures a Mail provider. Host and Security default to the
// vendor's preset; an explicit Host overrides it, which is how a self-hosted
// server is configured.
type Options struct {
	// Email is the account's own address, used for its ATTENDEE line, for log
	// lines and as the default login.
	Email string
	// Username is the login when it is not Email.
	Username string
	// Password is the IMAP credential. Required.
	Password string

	// Vendor selects the preset. May be empty when Host is given.
	Vendor model.Vendor
	// Host and Port override the preset. Port defaults to 993 (implicit TLS)
	// or 143 (STARTTLS).
	Host string
	Port int
	// Security is "tls", "starttls" or "none"; empty takes the preset's, then
	// implicit TLS.
	Security Security

	// SMTP submission. Empty fields take the preset's.
	SMTPHost     string
	SMTPPort     int
	SMTPSecurity Security
	SMTPUsername string
	// SMTPPassword defaults to Password: one app-specific password normally
	// serves both halves.
	SMTPPassword string

	// IncludeSpamTrash enumerates Junk and Trash as well.
	IncludeSpamTrash bool
	// IncludeAllMail enumerates a \All mailbox. Off by default, and that is
	// load-bearing: \All holds a copy of everything, so with per-copy ids it
	// doubles or triples the archive.
	IncludeAllMail bool
	// Folders, when set, is the only folders synced (exact names).
	Folders []string
	// ExcludeFolders are never synced.
	ExcludeFolders []string
	// Role overrides for servers whose folders cannot be recognised.
	ArchiveFolder, SentFolder, TrashFolder, DraftsFolder string

	// Concurrency bounds in-flight connections, capped by the preset.
	Concurrency int

	// Insecure permits Security "none". Tests only — it allows the password to
	// cross the wire in the clear.
	Insecure bool
	// TLSConfig overrides the default. Tests point it at a self-signed cert.
	TLSConfig *tls.Config

	Logger *slog.Logger
}

// User returns the IMAP login, defaulting to the account's own address.
func (o Options) User() string {
	if s := strings.TrimSpace(o.Username); s != "" {
		return s
	}
	return o.Email
}

// SMTPUser returns the submission login, defaulting to the IMAP one.
func (o Options) SMTPUser() string {
	if s := strings.TrimSpace(o.SMTPUsername); s != "" {
		return s
	}
	return o.User()
}

// Mail is an IMAP-backed provider.MailProvider.
type Mail struct {
	opts   Options
	preset Preset
	log    *slog.Logger

	addr     string
	security Security
	smtpAddr string
	smtpSec  Security

	pool *pool

	mu sync.Mutex
	// boxes caches the last folder listing, keyed by folder name. It is what
	// role lookups and the folder policy read; refreshed by Mailboxes.
	boxes map[string]folder
	// total caches the message count hint.
	total     int
	haveTotal bool
}

var (
	_ provider.MailProvider = (*Mail)(nil)
	_ provider.Remapper     = (*Mail)(nil)
	_ provider.Submitter    = (*Mail)(nil)
	_ provider.Pusher       = (*Mail)(nil)
)

// New builds a provider. It performs no I/O: the connection is opened lazily,
// so a half-configured account fails when it is used rather than when it is
// listed.
func New(opts Options) (*Mail, error) {
	preset, _ := PresetFor(opts.Vendor)

	if strings.TrimSpace(opts.Password) == "" {
		return nil, fmt.Errorf("imap: Options.Password is required (a %s)", preset.credentialPhrase())
	}
	if strings.TrimSpace(opts.Email) == "" {
		return nil, fmt.Errorf("imap: Options.Email is required")
	}

	host := firstNonEmpty(opts.Host, preset.Host)
	if host == "" {
		return nil, fmt.Errorf("imap: no host: vendor %q has no preset, so Options.Host is required", opts.Vendor)
	}
	security := opts.Security
	if security == "" {
		security = preset.Security
	}
	if security == "" {
		security = SecTLS
	}
	if security == SecNone && !opts.Insecure {
		return nil, fmt.Errorf("imap: security %q would send the password in the clear; set Options.Insecure to mean it", security)
	}
	port := opts.Port
	if port == 0 {
		port = preset.Port
	}
	if port == 0 {
		port = defaultPort(security)
	}

	smtpHost := firstNonEmpty(opts.SMTPHost, preset.SMTPHost)
	smtpSec := opts.SMTPSecurity
	if smtpSec == "" {
		smtpSec = preset.SMTPSecurity
	}
	if smtpSec == "" {
		smtpSec = SecStartTLS
	}
	smtpPort := opts.SMTPPort
	if smtpPort == 0 {
		smtpPort = preset.SMTPPort
	}
	if smtpPort == 0 {
		smtpPort = defaultSMTPPort(smtpSec)
	}

	log := opts.Logger
	if log == nil {
		log = slog.Default()
	}

	m := &Mail{
		opts:     opts,
		preset:   preset,
		log:      log,
		addr:     net.JoinHostPort(host, strconv.Itoa(port)),
		security: security,
		smtpSec:  smtpSec,
	}
	if smtpHost != "" {
		m.smtpAddr = net.JoinHostPort(smtpHost, strconv.Itoa(smtpPort))
	}
	m.pool = newPool(m, m.maxConns())
	return m, nil
}

// maxConns is how many connections this account may hold open at once.
//
// The cap is the point: a server that rate-limits per account does not answer
// an excess by going slower, it locks the account out — taking the user's phone
// and desktop client with it. The preset's ceiling always wins.
func (m *Mail) maxConns() int {
	n := m.opts.Concurrency
	if n <= 0 {
		n = 4
	}
	if lim := m.preset.MaxConnections; lim > 0 && n > lim {
		n = lim
	}
	if n < 1 {
		n = 1
	}
	return n
}

// Close releases every pooled connection.
func (m *Mail) Close() error { return m.pool.Close() }

func defaultPort(s Security) int {
	if s == SecTLS {
		return 993
	}
	return 143
}

func defaultSMTPPort(s Security) int {
	if s == SecTLS {
		return 465
	}
	return 587
}

func firstNonEmpty(v ...string) string {
	for _, s := range v {
		if t := strings.TrimSpace(s); t != "" {
			return t
		}
	}
	return ""
}

// hasCap reports whether the server advertised a capability.
func hasCap(caps imapv2.CapSet, c imapv2.Cap) bool {
	return caps != nil && caps.Has(c)
}
