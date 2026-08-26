package cli

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"strings"
	stdsync "sync"

	"github.com/teulaert/emlcalsync/internal/config"
	"github.com/teulaert/emlcalsync/internal/model"
	"github.com/teulaert/emlcalsync/internal/output"
	"github.com/teulaert/emlcalsync/internal/provider"
	"github.com/teulaert/emlcalsync/internal/provider/caldav"
	"github.com/teulaert/emlcalsync/internal/provider/gcal"
	"github.com/teulaert/emlcalsync/internal/provider/gmail"
	"github.com/teulaert/emlcalsync/internal/provider/jmap"
	"github.com/teulaert/emlcalsync/internal/provider/oauth"
)

// Secret keys (files under the secrets dir).
const (
	// FastmailTokenKey is the secrets key holding the raw API token.
	FastmailTokenKeyFmt = "%s.fastmail.token"
	// FastmailAppPasswordKeyFmt holds a Fastmail **app password**, which is
	// what CalDAV authenticates with. API tokens have no calendar scope, so
	// calendars only work once this is set (DESIGN.md §6.4).
	FastmailAppPasswordKeyFmt = "%s.fastmail.app-password"
	// GoogleTokenKeyFmt is the oauth.FileTokenStore key (it appends .json).
	GoogleTokenKeyFmt = "%s.gmail"
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
}

func (f *Factory) init() {
	if f.jmap == nil {
		f.jmap = map[string]*jmap.Client{}
		f.gmail = map[string]*gmail.Mail{}
		f.gcal = map[string]*gcal.Calendar{}
		f.caldav = map[string]*caldav.Calendar{}
	}
}

// FastmailTokenKey returns the secrets key for an account's API token.
func FastmailTokenKey(acct config.Account) string { return fmt.Sprintf(FastmailTokenKeyFmt, acct.Name) }

// FastmailAppPasswordKey returns the secrets key for an account's CalDAV app
// password.
func FastmailAppPasswordKey(acct config.Account) string {
	return fmt.Sprintf(FastmailAppPasswordKeyFmt, acct.Name)
}

// GoogleTokenKey returns the oauth token-store key for an account.
func GoogleTokenKey(acct config.Account) string { return fmt.Sprintf(GoogleTokenKeyFmt, acct.Name) }

// GoogleClientKeyFor returns the per-account OAuth client key.
func GoogleClientKeyFor(acct config.Account) string {
	return fmt.Sprintf(GoogleClientKeyFmt, acct.Name)
}

// FastmailAppPassword returns the stored CalDAV app password, or "" when the
// account has none (in which case calendars fall back to JMAP, which reports
// provider.ErrNotSupported and is skipped).
func (a *App) FastmailAppPassword(acct config.Account) string {
	sec, err := a.Secrets()
	if err != nil {
		return ""
	}
	pw, err := sec.Get(FastmailAppPasswordKey(acct))
	if err != nil {
		return ""
	}
	return strings.TrimSpace(string(pw))
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
	tok, err := sec.Get(FastmailTokenKey(acct))
	if err != nil {
		return nil, output.Errorf(output.ExitUsage,
			"no Fastmail token for account %q: run `emlcal account add fastmail --name %s`", acct.Name, acct.Name)
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
func (f *Factory) Mail(ctx context.Context, acct config.Account) (provider.MailProvider, error) {
	switch acct.Provider {
	case model.ProviderFastmail:
		c, err := f.jmapClient(acct)
		if err != nil {
			return nil, err
		}
		return c.Mail(), nil
	case model.ProviderGmail:
		m, _, err := f.googleClients(ctx, acct)
		return m, err
	}
	return nil, fmt.Errorf("unknown provider %q", acct.Provider)
}

// caldavClient builds (and caches) the CalDAV provider for a Fastmail account.
func (f *Factory) caldavClient(acct config.Account, password string) (*caldav.Calendar, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.init()
	if c, ok := f.caldav[acct.Name]; ok {
		return c, nil
	}
	c, err := caldav.New(caldav.Options{
		Email:    acct.Email,
		Password: password,
		BaseURL:  os.Getenv(EnvCalDAVBaseURL),
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
// Fastmail calendars go over CalDAV when an app password is stored, because
// Fastmail's JMAP API tokens carry no calendars scope. Without one the JMAP
// calendar client is returned unchanged: it reports provider.ErrNotSupported
// and the sync engine skips calendars for that account.
func (f *Factory) Calendar(ctx context.Context, acct config.Account) (provider.CalendarProvider, error) {
	switch acct.Provider {
	case model.ProviderFastmail:
		if pw := f.app.FastmailAppPassword(acct); pw != "" {
			return f.caldavClient(acct, pw)
		}
		c, err := f.jmapClient(acct)
		if err != nil {
			return nil, err
		}
		return c.Calendar(), nil
	case model.ProviderGmail:
		_, c, err := f.googleClients(ctx, acct)
		return c, err
	}
	return nil, fmt.Errorf("unknown provider %q", acct.Provider)
}

// Pusher implements sync.ProviderFactory: only JMAP supports push.
func (f *Factory) Pusher(ctx context.Context, acct config.Account) (provider.Pusher, bool, error) {
	if acct.Provider != model.ProviderFastmail {
		return nil, false, nil
	}
	c, err := f.jmapClient(acct)
	if err != nil {
		return nil, false, err
	}
	return c, true, nil
}
