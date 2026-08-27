package cli

import (
	"context"
	"encoding/json"
	"fmt"
	"net"
	"os"
	"strconv"
	"strings"
	stdsync "sync"

	"github.com/teulaert/emlcalsync/internal/config"
	"github.com/teulaert/emlcalsync/internal/model"
	"github.com/teulaert/emlcalsync/internal/output"
	"github.com/teulaert/emlcalsync/internal/provider"
	"github.com/teulaert/emlcalsync/internal/provider/caldav"
	"github.com/teulaert/emlcalsync/internal/provider/gcal"
	"github.com/teulaert/emlcalsync/internal/provider/gmail"
	imapprov "github.com/teulaert/emlcalsync/internal/provider/imap"
	"github.com/teulaert/emlcalsync/internal/provider/jmap"
	"github.com/teulaert/emlcalsync/internal/provider/oauth"
)

// Secret keys (files under the secrets dir). They are scoped by **backend**,
// not by vendor: the credential belongs to the protocol that presents it, and
// one account's two resources authenticate separately.
const (
	// JMAPTokenKeyFmt holds the raw JMAP API token.
	JMAPTokenKeyFmt = "%s.jmap.token"
	// CalDAVPasswordKeyFmt holds the CalDAV basic-auth password: a Fastmail
	// app password, or an Apple app-specific password. Calendars only work
	// once this is set.
	CalDAVPasswordKeyFmt = "%s.caldav.password"
	// IMAPPasswordKeyFmt holds the IMAP password, and the SMTP one unless a
	// separate one is stored. On iCloud and Fastmail this is the same
	// app-specific password the CalDAV half uses, kept under its own key so
	// each protocol's credential can be rotated on its own.
	IMAPPasswordKeyFmt = "%s.imap.password"
	// SMTPPasswordKeyFmt is the submission password when it differs from the
	// IMAP one, which some corporate setups insist on.
	SMTPPasswordKeyFmt = "%s.smtp.password"
	// GoogleTokenKeyFmt is the oauth.FileTokenStore key (it appends .json).
	GoogleTokenKeyFmt = "%s.google"
	// GoogleClientKey holds {"client_id":..., "client_secret":...}.
	GoogleClientKey = "google-client.json"
	// GoogleClientKeyFmt is the per-account override of GoogleClientKey, for
	// people who keep one GCP project per mailbox.
	GoogleClientKeyFmt = "%s.google-client.json"

	EnvGoogleClientID     = "EMLCAL_GOOGLE_CLIENT_ID"
	EnvGoogleClientSecret = "EMLCAL_GOOGLE_CLIENT_SECRET"
	// EnvJMAPSessionURL overrides the Fastmail session URL (tests, self-hosted JMAP).
	EnvJMAPSessionURL = "EMLCAL_JMAP_SESSION_URL"
	// EnvCalDAVBaseURL overrides the CalDAV root (tests, self-hosted CalDAV).
	EnvCalDAVBaseURL = "EMLCAL_CALDAV_BASE_URL"
	// EnvIMAPAddr overrides the IMAP host:port, and EnvSMTPAddr the submission
	// one, so the real Factory can be pointed at a fake server.
	EnvIMAPAddr = "EMLCAL_IMAP_ADDR"
	EnvSMTPAddr = "EMLCAL_SMTP_ADDR"
	// EnvIMAPInsecure allows an unencrypted connection. Tests only.
	EnvIMAPInsecure = "EMLCAL_IMAP_INSECURE"
)

// Factory builds real providers from config + secrets and caches them per
// account for the lifetime of the process.
type Factory struct {
	app *App

	mu     stdsync.Mutex
	jmap   map[string]*jmap.Client
	gmail  map[string]*gmail.Mail
	gcal   map[string]*gcal.Calendar
	caldav map[string]*caldav.Calendar
	imap   map[string]*imapprov.Mail
}

func (f *Factory) init() {
	if f.jmap == nil {
		f.jmap = map[string]*jmap.Client{}
		f.gmail = map[string]*gmail.Mail{}
		f.gcal = map[string]*gcal.Calendar{}
		f.caldav = map[string]*caldav.Calendar{}
		f.imap = map[string]*imapprov.Mail{}
	}
}

// JMAPTokenKey returns the secrets key for an account's JMAP API token.
func JMAPTokenKey(acct config.Account) string { return fmt.Sprintf(JMAPTokenKeyFmt, acct.Name) }

// CalDAVPasswordKey returns the secrets key for an account's CalDAV password.
func CalDAVPasswordKey(acct config.Account) string {
	return fmt.Sprintf(CalDAVPasswordKeyFmt, acct.Name)
}

// IMAPPasswordKey returns the secrets key for an account's IMAP password.
func IMAPPasswordKey(acct config.Account) string {
	return fmt.Sprintf(IMAPPasswordKeyFmt, acct.Name)
}

// SMTPPasswordKey returns the secrets key for a separate submission password.
func SMTPPasswordKey(acct config.Account) string {
	return fmt.Sprintf(SMTPPasswordKeyFmt, acct.Name)
}

// GoogleTokenKey returns the oauth token-store key for an account.
func GoogleTokenKey(acct config.Account) string { return fmt.Sprintf(GoogleTokenKeyFmt, acct.Name) }

// GoogleClientKeyFor returns the per-account OAuth client key.
func GoogleClientKeyFor(acct config.Account) string {
	return fmt.Sprintf(GoogleClientKeyFmt, acct.Name)
}

// CalDAVPassword returns the stored CalDAV password, or "" when the account has
// none — in which case Factory.Calendar reports provider.ErrNotSupported and
// the sync engine skips calendars rather than failing the whole account.
func (a *App) CalDAVPassword(acct config.Account) string {
	return a.secret(CalDAVPasswordKey(acct))
}

// IMAPPassword returns the stored IMAP password, or "" when the account has
// none — in which case Factory.Mail reports provider.ErrNotSupported and the
// sync engine skips mail rather than failing the whole account.
func (a *App) IMAPPassword(acct config.Account) string {
	return a.secret(IMAPPasswordKey(acct))
}

// SMTPPassword returns a separate submission password, or "" to use the IMAP
// one — which is the normal case, since an app-specific password serves both.
func (a *App) SMTPPassword(acct config.Account) string {
	return a.secret(SMTPPasswordKey(acct))
}

// secret reads one key, treating every failure as absence: a half-configured
// account should degrade to a skipped resource with an actionable message.
func (a *App) secret(key string) string {
	sec, err := a.Secrets()
	if err != nil {
		return ""
	}
	v, err := sec.Get(key)
	if err != nil {
		return ""
	}
	return strings.TrimSpace(string(v))
}

// GoogleOAuthConfig assembles the OAuth client config for one account. The
// per-account client wins (it is the most specific thing the user configured),
// then the env vars, then the shared google-client.json secret, then whatever
// the build embedded. Each source only fills in what the previous one left
// blank, so an env var can still supply half a pair.
func (a *App) GoogleOAuthConfig(acct config.Account) (oauth.Config, error) {
	var cfg oauth.Config
	if acct.Name != "" {
		a.fillGoogleClient(&cfg, GoogleClientKeyFor(acct))
	}
	if cfg.ClientID == "" {
		cfg.ClientID = os.Getenv(EnvGoogleClientID)
	}
	if cfg.ClientSecret == "" {
		cfg.ClientSecret = os.Getenv(EnvGoogleClientSecret)
	}
	a.fillGoogleClient(&cfg, GoogleClientKey)
	if err := cfg.Validate(); err != nil {
		return cfg, output.Errorf(output.ExitUsage,
			"Google OAuth client not configured: run `emlcal account google-client --id ID --secret SECRET` "+
				"(or `emlcal account add gmail --client-id … --client-secret …` for just this account) "+
				"or set %s/%s", EnvGoogleClientID, EnvGoogleClientSecret)
	}
	return cfg, nil
}

// fillGoogleClient fills the blanks in cfg from one secrets key, ignoring a
// key that is missing or is not a client-credentials document.
func (a *App) fillGoogleClient(cfg *oauth.Config, key string) {
	if cfg.ClientID != "" && cfg.ClientSecret != "" {
		return
	}
	sec, err := a.Secrets()
	if err != nil {
		return
	}
	b, err := sec.Get(key)
	if err != nil {
		return
	}
	var v struct {
		ID     string `json:"client_id"`
		Secret string `json:"client_secret"`
	}
	if json.Unmarshal(b, &v) != nil {
		return
	}
	if cfg.ClientID == "" {
		cfg.ClientID = v.ID
	}
	if cfg.ClientSecret == "" {
		cfg.ClientSecret = v.Secret
	}
}

func (f *Factory) jmapClient(acct config.Account) (*jmap.Client, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.init()
	if c, ok := f.jmap[acct.Name]; ok {
		return c, nil
	}
	sec, err := f.app.Secrets()
	if err != nil {
		return nil, err
	}
	tok, err := sec.Get(JMAPTokenKey(acct))
	if err != nil {
		return nil, output.Errorf(output.ExitUsage,
			"no JMAP token for account %q: run `emlcal account add fastmail --name %s`", acct.Name, acct.Name)
	}
	c, err := jmap.New(jmap.Options{
		Token:      strings.TrimSpace(string(tok)),
		Email:      acct.Email,
		Logger:     f.app.Logger(),
		SessionURL: os.Getenv(EnvJMAPSessionURL),
	})
	if err != nil {
		return nil, err
	}
	f.jmap[acct.Name] = c
	return c, nil
}

func (f *Factory) googleClients(ctx context.Context, acct config.Account) (*gmail.Mail, *gcal.Calendar, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.init()
	if m, ok := f.gmail[acct.Name]; ok {
		return m, f.gcal[acct.Name], nil
	}
	cfg, err := f.app.GoogleOAuthConfig(acct)
	if err != nil {
		return nil, nil, err
	}
	appCfg, err := f.app.Config()
	if err != nil {
		return nil, nil, err
	}
	hc, err := oauth.HTTPClient(ctx, cfg, oauth.FileTokenStore{Dir: appCfg.SecretsDir()}, GoogleTokenKey(acct))
	if err != nil {
		if oauth.IsReauthRequired(err) {
			return nil, nil, output.Errorf(output.ExitProvider,
				"Google login for %q expired: run `emlcal account add gmail --name %s` again", acct.Name, acct.Name)
		}
		return nil, nil, output.Errorf(output.ExitUsage,
			"no Google login for account %q: run `emlcal account add gmail --name %s`", acct.Name, acct.Name)
	}
	m, err := gmail.New(ctx, gmail.Options{
		HTTPClient:       hc,
		Email:            acct.Email,
		IncludeSpamTrash: acct.IncludeSpamTrash,
		Logger:           f.app.Logger(),
		Concurrency:      acct.Concurrency,
	})
	if err != nil {
		return nil, nil, err
	}
	c, err := gcal.New(ctx, gcal.Options{HTTPClient: hc, Email: acct.Email, Logger: f.app.Logger()})
	if err != nil {
		return nil, nil, err
	}
	f.gmail[acct.Name] = m
	f.gcal[acct.Name] = c
	return m, c, nil
}

// Mail implements sync.ProviderFactory.
//
// An account with no [accounts.mail] block reports provider.ErrNotSupported:
// the sync engine already treats that as "skip this resource", and it is the
// honest answer for a calendar-only account such as iCloud.
func (f *Factory) Mail(ctx context.Context, acct config.Account) (provider.MailProvider, error) {
	if acct.Mail == nil {
		return nil, fmt.Errorf("account %q has no [accounts.mail] block: %w",
			acct.Name, provider.ErrNotSupported)
	}
	switch acct.Mail.Backend {
	case model.BackendJMAP:
		c, err := f.jmapClient(acct)
		if err != nil {
			return nil, err
		}
		return c.Mail(), nil
	case model.BackendGmail:
		m, _, err := f.googleClients(ctx, acct)
		return m, err
	case model.BackendIMAP:
		pw := f.app.IMAPPassword(acct)
		if pw == "" {
			return nil, fmt.Errorf(
				"account %q has no IMAP password (add one with `emlcal account imap-password --name %s`): %w",
				acct.Name, acct.Name, provider.ErrNotSupported)
		}
		return f.imapClient(acct, pw)
	}
	return nil, fmt.Errorf("account %q: unknown mail backend %q", acct.Name, acct.Mail.Backend)
}

// imapClient builds (and caches) the IMAP provider for an account.
func (f *Factory) imapClient(acct config.Account, password string) (*imapprov.Mail, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.init()
	if c, ok := f.imap[acct.Name]; ok {
		return c, nil
	}
	mb := acct.Mail
	opts := imapprov.Options{
		Email:            acct.Email,
		Username:         mb.User(acct.Email),
		Password:         password,
		SMTPUsername:     mb.SMTPUser(acct.Email),
		SMTPPassword:     f.app.SMTPPassword(acct),
		Vendor:           mb.Vendor,
		Host:             mb.Host,
		Port:             mb.Port,
		Security:         imapprov.Security(mb.Security),
		SMTPHost:         mb.SMTPHost,
		SMTPPort:         mb.SMTPPort,
		SMTPSecurity:     imapprov.Security(mb.SMTPSecurity),
		IncludeSpamTrash: acct.IncludeSpamTrash,
		IncludeAllMail:   mb.IncludeAllMail,
		Folders:          mb.Folders,
		ExcludeFolders:   mb.ExcludeFolders,
		ArchiveFolder:    mb.ArchiveFolder,
		SentFolder:       mb.SentFolder,
		TrashFolder:      mb.TrashFolder,
		DraftsFolder:     mb.DraftsFolder,
		Concurrency:      acct.Concurrency,
		Logger:           f.app.Logger(),
	}
	applyIMAPEnvOverrides(&opts)

	c, err := imapprov.New(opts)
	if err != nil {
		return nil, err
	}
	f.imap[acct.Name] = c
	return c, nil
}

// applyIMAPEnvOverrides points the provider at a fake server, so the CLI and
// e2e tests exercise the real Factory rather than a stub of it.
func applyIMAPEnvOverrides(opts *imapprov.Options) {
	if addr := os.Getenv(EnvIMAPAddr); addr != "" {
		if host, port, ok := splitHostPort(addr); ok {
			opts.Host, opts.Port = host, port
		}
	}
	if addr := os.Getenv(EnvSMTPAddr); addr != "" {
		if host, port, ok := splitHostPort(addr); ok {
			opts.SMTPHost, opts.SMTPPort = host, port
		}
	}
	if os.Getenv(EnvIMAPInsecure) == "1" {
		opts.Insecure = true
		opts.Security = imapprov.SecNone
		opts.SMTPSecurity = imapprov.SecNone
	}
}

func splitHostPort(addr string) (string, int, bool) {
	host, portStr, err := net.SplitHostPort(addr)
	if err != nil {
		return "", 0, false
	}
	port, err := strconv.Atoi(portStr)
	if err != nil {
		return "", 0, false
	}
	return host, port, true
}

// forgetIMAP drops the cached IMAP provider for an account, so the next build
// picks up a credential that was just written. Providers are cached per account
// for the life of the process, which is right for a sync and wrong immediately
// after `account imap-password`.
func (f *Factory) forgetIMAP(name string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	delete(f.imap, name)
}

// caldavClient builds (and caches) the CalDAV provider for an account.
func (f *Factory) caldavClient(acct config.Account, password string) (*caldav.Calendar, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.init()
	if c, ok := f.caldav[acct.Name]; ok {
		return c, nil
	}
	cb := acct.Calendar
	base := cb.BaseURL
	if env := os.Getenv(EnvCalDAVBaseURL); env != "" {
		base = env
	}
	c, err := caldav.New(caldav.Options{
		Email:    acct.Email,
		Username: cb.User(acct.Email),
		Password: password,
		Vendor:   cb.Vendor,
		BaseURL:  base,
		Logger:   f.app.Logger(),
	})
	if err != nil {
		return nil, err
	}
	f.caldav[acct.Name] = c
	return c, nil
}

// Calendar implements sync.ProviderFactory.
//
// A CalDAV backend with no stored password reports provider.ErrNotSupported
// rather than failing: a half-configured account should skip its calendars
// with an actionable message, not break the whole sync.
func (f *Factory) Calendar(ctx context.Context, acct config.Account) (provider.CalendarProvider, error) {
	if acct.Calendar == nil {
		return nil, fmt.Errorf("account %q has no [accounts.calendar] block: %w",
			acct.Name, provider.ErrNotSupported)
	}
	switch acct.Calendar.Backend {
	case model.BackendCalDAV:
		pw := f.app.CalDAVPassword(acct)
		if pw == "" {
			return nil, fmt.Errorf(
				"account %q has no CalDAV password (add one with `emlcal account caldav-password --name %s`): %w",
				acct.Name, acct.Name, provider.ErrNotSupported)
		}
		return f.caldavClient(acct, pw)
	case model.BackendGCal:
		_, c, err := f.googleClients(ctx, acct)
		return c, err
	case model.BackendJMAP:
		c, err := f.jmapClient(acct)
		if err != nil {
			return nil, err
		}
		return c.Calendar(), nil
	}
	return nil, fmt.Errorf("account %q: unknown calendar backend %q", acct.Name, acct.Calendar.Backend)
}

// Pusher implements sync.ProviderFactory: JMAP's event stream, or IMAP IDLE.
func (f *Factory) Pusher(ctx context.Context, acct config.Account) (provider.Pusher, bool, error) {
	if acct.Mail == nil {
		return nil, false, nil
	}
	switch acct.Mail.Backend {
	case model.BackendJMAP:
		c, err := f.jmapClient(acct)
		if err != nil {
			return nil, false, err
		}
		return c, true, nil
	case model.BackendIMAP:
		pw := f.app.IMAPPassword(acct)
		if pw == "" {
			return nil, false, nil
		}
		c, err := f.imapClient(acct, pw)
		if err != nil {
			return nil, false, err
		}
		return c, true, nil
	}
	return nil, false, nil
}
