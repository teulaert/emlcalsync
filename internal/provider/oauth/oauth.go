package oauth

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"net/url"
	"sync"

	"golang.org/x/oauth2"
	"golang.org/x/oauth2/google"
	calendar "google.golang.org/api/calendar/v3"
	gmail "google.golang.org/api/gmail/v1"

	"github.com/teulaert/emlcalsync/internal/model"
)

// DefaultScopes is what emlcal asks for: gmail.modify covers read, label
// changes, send and trash (but not permanent delete), and the calendar scope
// covers read/write on Google Calendar. See DESIGN.md §6.1.
var DefaultScopes = []string{
	gmail.GmailModifyScope,
	calendar.CalendarScope,
}

// Client id/secret of the Desktop OAuth client of the emlcal GCP project.
//
// These are deliberately empty in the source tree: they must be filled in by
// the project owner with the credentials of their own "Desktop app" OAuth
// client (they are not secret for installed applications, per Google's own
// documentation, and may be embedded in the binary). Users of their own GCP
// project override them through config.toml.
//
// The CLI must check Config.Validate and print a clear message when they are
// still empty rather than sending an empty client_id to Google.
var (
	DefaultClientID     = ""
	DefaultClientSecret = ""
)

// Config is everything needed to talk to Google's authorization server.
type Config struct {
	ClientID     string
	ClientSecret string
	// Scopes defaults to DefaultScopes when empty.
	Scopes []string
	// Endpoint overrides Google's authorization/token endpoints. Zero value
	// means google.Endpoint; tests set this to a local httptest server.
	Endpoint oauth2.Endpoint
}

// Logger receives the package's occasional warnings — chiefly a refreshed
// token that could not be persisted. Nil means slog.Default(). Set it once at
// start-up, before any token source is used.
var Logger *slog.Logger

func logger() *slog.Logger {
	if Logger != nil {
		return Logger
	}
	return slog.Default()
}

// ErrMissingClient is returned when no OAuth client id/secret is configured.
var ErrMissingClient = errors.New("no Google OAuth client configured")

// Validate reports whether the config can be used at all.
func (c Config) Validate() error {
	if c.clientID() == "" || c.clientSecret() == "" {
		return fmt.Errorf("%w: set [google] client_id/client_secret in config.toml "+
			"(create a Desktop OAuth client in your Google Cloud project)", ErrMissingClient)
	}
	return nil
}

func (c Config) clientID() string {
	if c.ClientID != "" {
		return c.ClientID
	}
	return DefaultClientID
}

func (c Config) clientSecret() string {
	if c.ClientSecret != "" {
		return c.ClientSecret
	}
	return DefaultClientSecret
}

func (c Config) scopes() []string {
	if len(c.Scopes) > 0 {
		return c.Scopes
	}
	return DefaultScopes
}

func (c Config) endpoint() oauth2.Endpoint {
	if c.Endpoint.AuthURL != "" || c.Endpoint.TokenURL != "" {
		return c.Endpoint
	}
	return google.Endpoint
}

// oauth2Config builds the x/oauth2 config. redirectURL is only needed for the
// interactive flow; refreshes ignore it.
func (c Config) oauth2Config(redirectURL string) *oauth2.Config {
	return &oauth2.Config{
		ClientID:     c.clientID(),
		ClientSecret: c.clientSecret(),
		Scopes:       c.scopes(),
		Endpoint:     c.endpoint(),
		RedirectURL:  redirectURL,
	}
}

// ErrReauthRequired means the stored refresh token is no longer usable (it was
// revoked, the consent screen is still in "testing" so it expired after seven
// days, or the password changed). The user has to run `emlcal auth login`
// again for this account.
type ErrReauthRequired struct {
	Key string
	Err error
}

func (e *ErrReauthRequired) Error() string {
	return fmt.Sprintf("re-authentication required for %q: %v", e.Key, e.Err)
}

func (e *ErrReauthRequired) Unwrap() error { return e.Err }

// IsReauthRequired reports whether err (or anything it wraps) is an
// *ErrReauthRequired.
func IsReauthRequired(err error) bool {
	var e *ErrReauthRequired
	return errors.As(err, &e)
}

// HTTPClient returns an *http.Client that authenticates every request with the
// token stored under key, refreshing it when it expires and writing refreshed
// tokens back to the store. Pass it to gmail.NewService /
// calendar.NewService via option.WithHTTPClient.
//
// ctx governs the lifetime of the refresh requests, so it should be the
// long-lived context of the sync process, not a per-call one.
func HTTPClient(ctx context.Context, cfg Config, store TokenStore, key string) (*http.Client, error) {
	if err := cfg.Validate(); err != nil {
		return nil, err
	}
	if store == nil {
		return nil, errors.New("oauth: nil token store")
	}
	tok, err := store.Load(key)
	if err != nil {
		return nil, err
	}
	if tok.RefreshToken == "" && !tok.Valid() {
		return nil, &ErrReauthRequired{Key: key, Err: errors.New("stored token is expired and has no refresh token")}
	}
	src := cfg.oauth2Config("").TokenSource(ctx, tok)
	return oauth2.NewClient(ctx, &persistingSource{
		src:   src,
		store: store,
		key:   key,
		last:  tok.AccessToken,
	}), nil
}

// TokenSource is HTTPClient's guts, exposed for callers that need a raw token
// (for example to print its expiry in `emlcal doctor`).
func TokenSource(ctx context.Context, cfg Config, store TokenStore, key string) (oauth2.TokenSource, error) {
	if err := cfg.Validate(); err != nil {
		return nil, err
	}
	tok, err := store.Load(key)
	if err != nil {
		return nil, err
	}
	return &persistingSource{
		src:   cfg.oauth2Config("").TokenSource(ctx, tok),
		store: store,
		key:   key,
		last:  tok.AccessToken,
	}, nil
}

// persistingSource wraps the ReuseTokenSource returned by oauth2.Config and
// saves the token whenever the access token changes, i.e. after every refresh.
// Google also rotates refresh tokens occasionally, which is exactly why the
// whole token is saved rather than just the access token.
type persistingSource struct {
	src   oauth2.TokenSource
	store TokenStore
	key   string

	mu   sync.Mutex
	last string // last access token we saved
}

func (p *persistingSource) Token() (*oauth2.Token, error) {
	tok, err := p.src.Token()
	if err != nil {
		return nil, p.classify(err)
	}
	p.mu.Lock()
	changed := tok.AccessToken != p.last
	if changed {
		p.last = tok.AccessToken
	}
	p.mu.Unlock()

	if changed {
		if err := p.store.Save(p.key, tok); err != nil {
			// A token we cannot persist still works for this process; losing
			// it only means an extra refresh next time. Do not fail the call
			// — but say so, because a store that keeps failing means the
			// rotated refresh token is being thrown away, and the user will
			// eventually be asked to log in again for no visible reason.
			logger().Warn("oauth: could not persist refreshed token",
				"key", p.key, "err", err)
			return tok, nil //nolint:nilerr // deliberate: saving is best-effort
		}
	}
	return tok, nil
}

// classify turns refresh failures into the errors the rest of emlcal knows:
// invalid_grant means the user must log in again, transport failures mean we
// are offline.
func (p *persistingSource) classify(err error) error {
	var re *oauth2.RetrieveError
	if errors.As(err, &re) {
		switch re.ErrorCode {
		case "invalid_grant", "unauthorized_client", "invalid_client":
			return &ErrReauthRequired{Key: p.key, Err: err}
		}
		// Google sometimes answers with a bare 400 whose body is not the
		// documented JSON error object, so x/oauth2 cannot fill in ErrorCode.
		// Only treat that as a dead grant when the body actually names
		// invalid_grant: other 400s (a malformed request, a bad client
		// configuration) are bugs, and telling the user to log in again would
		// send them round a loop that cannot fix anything.
		if re.Response != nil && re.Response.StatusCode == http.StatusBadRequest &&
			re.ErrorCode == "" && bytes.Contains(bytes.ToLower(re.Body), []byte("invalid_grant")) {
			return &ErrReauthRequired{Key: p.key, Err: err}
		}
		return err
	}
	return wrapOffline(err)
}

// wrapOffline tags transport-level failures with model.ErrOffline so the sync
// engine can distinguish "no network" from "the server said no" (§12).
func wrapOffline(err error) error {
	if err == nil {
		return nil
	}
	if errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
		return err
	}
	var dnsErr *net.DNSError
	var opErr *net.OpError
	var urlErr *url.Error
	var netErr net.Error
	switch {
	case errors.As(err, &dnsErr), errors.As(err, &opErr), errors.As(err, &netErr):
		return fmt.Errorf("%w: %v", model.ErrOffline, err)
	case errors.As(err, &urlErr):
		return fmt.Errorf("%w: %v", model.ErrOffline, err)
	case errors.Is(err, io.EOF), errors.Is(err, io.ErrUnexpectedEOF):
		return fmt.Errorf("%w: %v", model.ErrOffline, err)
	}
	return err
}
