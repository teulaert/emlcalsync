package oauth

import (
	"context"
	"crypto/rand"
	"encoding/base64"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"os/exec"
	"runtime"
	"time"

	"golang.org/x/oauth2"
)

// LoginOptions tunes the interactive flow. The zero value is usable: the
// authorization URL is printed to stdout and the user opens it themselves.
type LoginOptions struct {
	// OpenBrowser is called with the authorization URL. When nil, the URL is
	// only printed to Output. Returning an error is not fatal: the URL stays
	// on screen and the flow keeps waiting for the redirect.
	OpenBrowser func(url string) error
	// Output receives the human-readable instructions. Defaults to os.Stdout.
	Output io.Writer
	// Addr is the loopback address to listen on. Defaults to "127.0.0.1:0"
	// (any free port), which is what Google's installed-app flow expects.
	Addr string
	// Timeout bounds the wait for the redirect. Zero means "until ctx is
	// done"; when ctx has no deadline either, five minutes is used.
	Timeout time.Duration
}

// OpenSystemBrowser launches the platform's default browser. Pass it as
// LoginOptions.OpenBrowser for an interactive terminal.
func OpenSystemBrowser(url string) error {
	var cmd *exec.Cmd
	switch runtime.GOOS {
	case "darwin":
		cmd = exec.Command("open", url)
	case "windows":
		cmd = exec.Command("rundll32", "url.dll,FileProtocolHandler", url)
	default:
		cmd = exec.Command("xdg-open", url)
	}
	return cmd.Start()
}

const defaultLoginTimeout = 5 * time.Minute

// Login runs the OAuth 2.0 authorization code flow for installed apps: it
// listens on a loopback port, sends the user to Google's consent screen with
// PKCE (S256), access_type=offline and prompt=consent, and exchanges the code
// that comes back on the redirect for a token with a refresh token.
//
// It returns as soon as the token is obtained; the caller persists it with a
// TokenStore.
func Login(ctx context.Context, cfg Config, opts LoginOptions) (*oauth2.Token, error) {
	if err := cfg.Validate(); err != nil {
		return nil, err
	}
	out := opts.Output
	if out == nil {
		out = os.Stdout
	}
	addr := opts.Addr
	if addr == "" {
		addr = "127.0.0.1:0"
	}

	ln, err := net.Listen("tcp", addr)
	if err != nil {
		return nil, fmt.Errorf("listen on loopback: %w", err)
	}
	defer ln.Close()

	redirectURL, err := loopbackURL(ln.Addr())
	if err != nil {
		return nil, err
	}
	conf := cfg.oauth2Config(redirectURL)

	verifier := oauth2.GenerateVerifier()
	state, err := randomState()
	if err != nil {
		return nil, err
	}
	authURL := conf.AuthCodeURL(state,
		oauth2.AccessTypeOffline,
		oauth2.ApprovalForce, // prompt=consent, so a refresh token is always issued
		oauth2.S256ChallengeOption(verifier),
	)

	type result struct {
		code string
		err  error
	}
	results := make(chan result, 1)

	mux := http.NewServeMux()
	mux.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		q := r.URL.Query()
		if q.Get("state") != state {
			// Never report someone else's state back; just refuse.
			writePage(w, http.StatusBadRequest, "Authorisation failed",
				"The redirect did not match this login attempt. Please try again.")
			select {
			case results <- result{err: errors.New("oauth: state mismatch on redirect")}:
			default:
			}
			return
		}
		if e := q.Get("error"); e != "" {
			desc := q.Get("error_description")
			writePage(w, http.StatusOK, "Authorisation declined",
				"You can close this tab and try again.")
			select {
			case results <- result{err: fmt.Errorf("oauth: authorization denied: %s %s", e, desc)}:
			default:
			}
			return
		}
		code := q.Get("code")
		if code == "" {
			writePage(w, http.StatusBadRequest, "Authorisation failed",
				"No authorization code was returned. Please try again.")
			select {
			case results <- result{err: errors.New("oauth: redirect carried no code")}:
			default:
			}
			return
		}
		writePage(w, http.StatusOK, "emlcal is connected",
			"You can close this tab and go back to your terminal.")
		select {
		case results <- result{code: code}:
		default:
		}
	})

	srv := &http.Server{
		Handler:           mux,
		ReadHeaderTimeout: 10 * time.Second,
	}
	serveErr := make(chan error, 1)
	go func() { serveErr <- srv.Serve(ln) }()
	defer func() {
		shutCtx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
		defer cancel()
		_ = srv.Shutdown(shutCtx)
		<-serveErr
	}()

	fmt.Fprintf(out, "Opening the Google consent screen for:\n\n  %s\n\n", authURL)
	if opts.OpenBrowser != nil {
		if err := opts.OpenBrowser(authURL); err != nil {
			fmt.Fprintf(out, "Could not open a browser (%v) — open the URL above by hand.\n", err)
		}
	}
	fmt.Fprintf(out, "Waiting for the redirect on %s ...\n", redirectURL)

	waitCtx := ctx
	if opts.Timeout > 0 {
		var cancel context.CancelFunc
		waitCtx, cancel = context.WithTimeout(ctx, opts.Timeout)
		defer cancel()
	} else if _, hasDeadline := ctx.Deadline(); !hasDeadline {
		var cancel context.CancelFunc
		waitCtx, cancel = context.WithTimeout(ctx, defaultLoginTimeout)
		defer cancel()
	}

	var res result
	select {
	case res = <-results:
	case err := <-serveErr:
		return nil, fmt.Errorf("loopback server stopped: %w", err)
	case <-waitCtx.Done():
		return nil, fmt.Errorf("waiting for the OAuth redirect: %w", waitCtx.Err())
	}
	if res.err != nil {
		return nil, res.err
	}

	tok, err := conf.Exchange(ctx, res.code, oauth2.VerifierOption(verifier))
	if err != nil {
		return nil, fmt.Errorf("exchange authorization code: %w", wrapOffline(err))
	}
	if tok.RefreshToken == "" {
		fmt.Fprintln(out, "Warning: Google returned no refresh token; emlcal will "+
			"need you to log in again when the access token expires.")
	}
	return tok, nil
}

// loopbackURL builds the redirect URI Google is given, and the one it must
// match exactly when the browser comes back.
//
// Only the port is taken from the listener: its address prints as "[::]:8080"
// or "0.0.0.0:8080" whenever the caller asked to listen on every interface,
// and neither is a URL Google's installed-app flow accepts. The redirect
// itself always arrives over loopback, so 127.0.0.1 is both correct and the
// literal form Google documents (it needs no DNS and needs no registration,
// unlike "localhost").
func loopbackURL(addr net.Addr) (string, error) {
	if a, ok := addr.(*net.TCPAddr); ok {
		return fmt.Sprintf("http://127.0.0.1:%d", a.Port), nil
	}
	_, port, err := net.SplitHostPort(addr.String())
	if err != nil {
		return "", fmt.Errorf("loopback listener address %q: %w", addr, err)
	}
	return "http://127.0.0.1:" + port, nil
}

func randomState() (string, error) {
	b := make([]byte, 32)
	if _, err := rand.Read(b); err != nil {
		return "", fmt.Errorf("generate state: %w", err)
	}
	return base64.RawURLEncoding.EncodeToString(b), nil
}

func writePage(w http.ResponseWriter, status int, title, body string) {
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.WriteHeader(status)
	fmt.Fprintf(w, `<!doctype html>
<html lang="en"><head><meta charset="utf-8"><title>%s</title>
<style>body{font:16px/1.5 system-ui,sans-serif;margin:6rem auto;max-width:30rem;padding:0 1rem}
h1{font-size:1.25rem}</style></head>
<body><h1>%s</h1><p>%s</p></body></html>
`, title, title, body)
}
