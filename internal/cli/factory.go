package cli

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"strings"
	stdsync "sync"

	"github.com/lennert/emlcal/internal/config"
	"github.com/lennert/emlcal/internal/model"
	"github.com/lennert/emlcal/internal/output"
	"github.com/lennert/emlcal/internal/provider"
	"github.com/lennert/emlcal/internal/provider/gcal"
	"github.com/lennert/emlcal/internal/provider/gmail"
	"github.com/lennert/emlcal/internal/provider/jmap"
	"github.com/lennert/emlcal/internal/provider/oauth"
)

// Secret keys (files under the secrets dir).
const (
	// FastmailTokenKey is the secrets key holding the raw API token.
	FastmailTokenKeyFmt = "%s.fastmail.token"
	// GoogleTokenKeyFmt is the oauth.FileTokenStore key (it appends .json).
	GoogleTokenKeyFmt = "%s.gmail"
	// GoogleClientKey holds {"client_id":..., "client_secret":...}.
	GoogleClientKey = "google-client.json"

	EnvGoogleClientID     = "EMLCAL_GOOGLE_CLIENT_ID"
	EnvGoogleClientSecret = "EMLCAL_GOOGLE_CLIENT_SECRET"
)

// Factory builds real providers from config + secrets and caches them per
// account for the lifetime of the process.
type Factory struct {
	app *App

	mu    stdsync.Mutex
	jmap  map[string]*jmap.Client
	gmail map[string]*gmail.Mail
	gcal  map[string]*gcal.Calendar
}

func (f *Factory) init() {
	if f.jmap == nil {
		f.jmap = map[string]*jmap.Client{}
		f.gmail = map[string]*gmail.Mail{}
		f.gcal = map[string]*gcal.Calendar{}
	}
}

// FastmailTokenKey returns the secrets key for an account's API token.
func FastmailTokenKey(acct config.Account) string { return fmt.Sprintf(FastmailTokenKeyFmt, acct.Name) }

// GoogleTokenKey returns the oauth token-store key for an account.
func GoogleTokenKey(acct config.Account) string { return fmt.Sprintf(GoogleTokenKeyFmt, acct.Name) }

// GoogleOAuthConfig assembles the OAuth client config: env vars win, then
// the google-client.json secret, then the compiled-in defaults.
func (a *App) GoogleOAuthConfig() (oauth.Config, error) {
	cfg := oauth.Config{
		ClientID:     os.Getenv(EnvGoogleClientID),
		ClientSecret: os.Getenv(EnvGoogleClientSecret),
	}
	if cfg.ClientID == "" || cfg.ClientSecret == "" {
		if sec, err := a.Secrets(); err == nil {
			if b, err := sec.Get(GoogleClientKey); err == nil {
				var v struct {
					ID     string `json:"client_id"`
					Secret string `json:"client_secret"`
				}
				if json.Unmarshal(b, &v) == nil {
					if cfg.ClientID == "" {
						cfg.ClientID = v.ID
					}
					if cfg.ClientSecret == "" {
						cfg.ClientSecret = v.Secret
					}
				}
			}
		}
	}
	if err := cfg.Validate(); err != nil {
		return cfg, output.Errorf(output.ExitUsage,
			"Google OAuth client not configured: run `emlcal account google-client --id ID --secret SECRET` "+
				"or set %s/%s", EnvGoogleClientID, EnvGoogleClientSecret)
	}
	return cfg, nil
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
		Token:  strings.TrimSpace(string(tok)),
		Email:  acct.Email,
		Logger: f.app.Logger(),
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
	cfg, err := f.app.GoogleOAuthConfig()
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

// Calendar implements sync.ProviderFactory.
func (f *Factory) Calendar(ctx context.Context, acct config.Account) (provider.CalendarProvider, error) {
	switch acct.Provider {
	case model.ProviderFastmail:
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
