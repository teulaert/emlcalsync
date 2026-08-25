package oauth

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"golang.org/x/oauth2"

	"github.com/lennert/emlcal/internal/model"
)

func TestFileTokenStoreRoundTrip(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "secrets")
	store := FileTokenStore{Dir: dir}

	if _, err := store.Load("work.gmail"); !errors.Is(err, model.ErrNotFound) {
		t.Fatalf("Load of a missing token = %v, want model.ErrNotFound", err)
	}

	want := &oauth2.Token{
		AccessToken:  "at-1",
		RefreshToken: "rt-1",
		TokenType:    "Bearer",
		Expiry:       time.Now().Add(time.Hour).Truncate(time.Second),
	}
	if err := store.Save("work.gmail", want); err != nil {
		t.Fatalf("Save: %v", err)
	}
	got, err := store.Load("work.gmail")
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if got.AccessToken != want.AccessToken || got.RefreshToken != want.RefreshToken ||
		!got.Expiry.Equal(want.Expiry) {
		t.Errorf("round trip mismatch:\n got %+v\nwant %+v", got, want)
	}

	di, err := os.Stat(dir)
	if err != nil {
		t.Fatal(err)
	}
	if perm := di.Mode().Perm(); perm != 0o700 {
		t.Errorf("secrets dir mode = %o, want 700", perm)
	}
	fi, err := os.Stat(filepath.Join(dir, "work.gmail.json"))
	if err != nil {
		t.Fatal(err)
	}
	if perm := fi.Mode().Perm(); perm != 0o600 {
		t.Errorf("token file mode = %o, want 600", perm)
	}

	// Overwriting must not leave stray temp files behind.
	want.AccessToken = "at-2"
	if err := store.Save("work.gmail", want); err != nil {
		t.Fatalf("Save (overwrite): %v", err)
	}
	ents, err := os.ReadDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	if len(ents) != 1 {
		names := make([]string, len(ents))
		for i, e := range ents {
			names[i] = e.Name()
		}
		t.Errorf("secrets dir holds %v, want just the token file", names)
	}
	got, _ = store.Load("work.gmail")
	if got.AccessToken != "at-2" {
		t.Errorf("after overwrite AccessToken = %q, want at-2", got.AccessToken)
	}
}

func TestFileTokenStoreBadKey(t *testing.T) {
	store := FileTokenStore{Dir: t.TempDir()}
	for _, key := range []string{"", ".", "..", "a/b", `a\b`} {
		if _, err := store.Load(key); !errors.Is(err, ErrBadKey) {
			t.Errorf("Load(%q) = %v, want ErrBadKey", key, err)
		}
		if err := store.Save(key, &oauth2.Token{}); !errors.Is(err, ErrBadKey) {
			t.Errorf("Save(%q) = %v, want ErrBadKey", key, err)
		}
	}
}

// fakeGoogle is a minimal authorization server: it only implements the token
// endpoint, and records what the client sent.
type fakeGoogle struct {
	srv *httptest.Server

	mu           sync.Mutex
	lastForm     url.Values
	accessTokens []string
	failWith     string // when set, the token endpoint returns this OAuth error code
	refreshCount int
	nextTokenSeq int
	// failRaw* make the token endpoint answer with a body that is not the
	// documented JSON error object, as Google's front end sometimes does.
	failRawStatus int
	failRawBody   string
	failRawType   string
}

func newFakeGoogle(t *testing.T) *fakeGoogle {
	t.Helper()
	f := &fakeGoogle{}
	mux := http.NewServeMux()
	mux.HandleFunc("/token", func(w http.ResponseWriter, r *http.Request) {
		if err := r.ParseForm(); err != nil {
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		}
		f.mu.Lock()
		defer f.mu.Unlock()
		f.lastForm = r.PostForm
		if r.PostForm.Get("grant_type") == "refresh_token" {
			f.refreshCount++
		}
		if f.failRawStatus != 0 {
			ct := f.failRawType
			if ct == "" {
				ct = "text/html; charset=utf-8"
			}
			w.Header().Set("Content-Type", ct)
			w.WriteHeader(f.failRawStatus)
			io.WriteString(w, f.failRawBody)
			return
		}
		if f.failWith != "" {
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusBadRequest)
			io.WriteString(w, `{"error":"`+f.failWith+`","error_description":"Token has been expired or revoked."}`)
			return
		}
		f.nextTokenSeq++
		at := "access-" + strconv.Itoa(f.nextTokenSeq)
		f.accessTokens = append(f.accessTokens, at)
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(map[string]any{
			"access_token":  at,
			"refresh_token": "refresh-" + strconv.Itoa(f.nextTokenSeq),
			"token_type":    "Bearer",
			"expires_in":    3600,
		})
	})
	f.srv = httptest.NewServer(mux)
	t.Cleanup(f.srv.Close)
	return f
}

func (f *fakeGoogle) config() Config {
	return Config{
		ClientID:     "client-id",
		ClientSecret: "client-secret",
		Scopes:       []string{"scope-a"},
		Endpoint: oauth2.Endpoint{
			AuthURL:  f.srv.URL + "/auth",
			TokenURL: f.srv.URL + "/token",
		},
	}
}

func (f *fakeGoogle) form() url.Values {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.lastForm
}

func TestLoginLoopbackFlow(t *testing.T) {
	fake := newFakeGoogle(t)

	var authQuery url.Values
	opened := make(chan struct{})
	open := func(raw string) error {
		u, err := url.Parse(raw)
		if err != nil {
			return err
		}
		authQuery = u.Query()
		close(opened)
		// Play the browser: Google redirects to the loopback listener.
		redirect := authQuery.Get("redirect_uri") + "/?" + url.Values{
			"code":  {"the-code"},
			"state": {authQuery.Get("state")},
		}.Encode()
		go func() {
			resp, err := http.Get(redirect)
			if err == nil {
				io.Copy(io.Discard, resp.Body)
				resp.Body.Close()
			}
		}()
		return nil
	}

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	tok, err := Login(ctx, fake.config(), LoginOptions{OpenBrowser: open, Output: io.Discard})
	if err != nil {
		t.Fatalf("Login: %v", err)
	}
	<-opened

	if tok.AccessToken != "access-1" || tok.RefreshToken != "refresh-1" {
		t.Errorf("token = %+v, want access-1/refresh-1", tok)
	}

	// The authorization request must be a PKCE, offline, consent-forcing one.
	if got := authQuery.Get("access_type"); got != "offline" {
		t.Errorf("access_type = %q, want offline", got)
	}
	if got := authQuery.Get("prompt"); got != "consent" {
		t.Errorf("prompt = %q, want consent", got)
	}
	if got := authQuery.Get("code_challenge_method"); got != "S256" {
		t.Errorf("code_challenge_method = %q, want S256", got)
	}
	if got := authQuery.Get("scope"); got != "scope-a" {
		t.Errorf("scope = %q, want scope-a", got)
	}
	if !strings.HasPrefix(authQuery.Get("redirect_uri"), "http://127.0.0.1:") {
		t.Errorf("redirect_uri = %q, want a loopback URL", authQuery.Get("redirect_uri"))
	}

	// ... and the exchange must prove possession of the verifier.
	form := fake.form()
	if form.Get("code") != "the-code" {
		t.Errorf("exchanged code = %q, want the-code", form.Get("code"))
	}
	sum := sha256.Sum256([]byte(form.Get("code_verifier")))
	if want := base64.RawURLEncoding.EncodeToString(sum[:]); want != authQuery.Get("code_challenge") {
		t.Errorf("code_verifier does not match code_challenge")
	}
}

func TestLoginRejectsBadState(t *testing.T) {
	fake := newFakeGoogle(t)

	open := func(raw string) error {
		u, _ := url.Parse(raw)
		redirect := u.Query().Get("redirect_uri") + "/?code=x&state=not-the-state"
		go func() {
			resp, err := http.Get(redirect)
			if err == nil {
				resp.Body.Close()
			}
		}()
		return nil
	}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	_, err := Login(ctx, fake.config(), LoginOptions{OpenBrowser: open, Output: io.Discard})
	if err == nil || !strings.Contains(err.Error(), "state mismatch") {
		t.Fatalf("Login with a wrong state = %v, want a state mismatch error", err)
	}
}

func TestLoginMissingClient(t *testing.T) {
	_, err := Login(context.Background(), Config{}, LoginOptions{Output: io.Discard})
	if !errors.Is(err, ErrMissingClient) {
		t.Fatalf("Login without a client id = %v, want ErrMissingClient", err)
	}
}

func TestLoginContextCancelled(t *testing.T) {
	fake := newFakeGoogle(t)
	ctx, cancel := context.WithCancel(context.Background())
	go func() {
		time.Sleep(50 * time.Millisecond)
		cancel()
	}()
	_, err := Login(ctx, fake.config(), LoginOptions{Output: io.Discard})
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("Login after cancel = %v, want context.Canceled", err)
	}
}

func TestPersistingTokenSourceSavesRefresh(t *testing.T) {
	fake := newFakeGoogle(t)
	store := &MemoryTokenStore{}
	if err := store.Save("work.gmail", &oauth2.Token{
		AccessToken:  "stale",
		RefreshToken: "refresh-0",
		TokenType:    "Bearer",
		Expiry:       time.Now().Add(-time.Minute), // already expired: forces a refresh
	}); err != nil {
		t.Fatal(err)
	}

	src, err := TokenSource(context.Background(), fake.config(), store, "work.gmail")
	if err != nil {
		t.Fatalf("TokenSource: %v", err)
	}
	tok, err := src.Token()
	if err != nil {
		t.Fatalf("Token: %v", err)
	}
	if tok.AccessToken != "access-1" {
		t.Fatalf("refreshed access token = %q, want access-1", tok.AccessToken)
	}
	saved, err := store.Load("work.gmail")
	if err != nil {
		t.Fatalf("Load after refresh: %v", err)
	}
	if saved.AccessToken != "access-1" || saved.RefreshToken != "refresh-1" {
		t.Errorf("stored token = %+v, want the refreshed one", saved)
	}

	// A second call is served from the cache: no extra refresh, no extra save.
	if _, err := src.Token(); err != nil {
		t.Fatalf("Token (cached): %v", err)
	}
	fake.mu.Lock()
	refreshes := fake.refreshCount
	fake.mu.Unlock()
	if refreshes != 1 {
		t.Errorf("token endpoint hit %d times, want 1", refreshes)
	}
}

func TestPersistingTokenSourceReauth(t *testing.T) {
	fake := newFakeGoogle(t)
	fake.failWith = "invalid_grant"
	store := &MemoryTokenStore{}
	store.Save("work.gmail", &oauth2.Token{
		AccessToken:  "stale",
		RefreshToken: "revoked",
		Expiry:       time.Now().Add(-time.Minute),
	})

	src, err := TokenSource(context.Background(), fake.config(), store, "work.gmail")
	if err != nil {
		t.Fatalf("TokenSource: %v", err)
	}
	_, err = src.Token()
	if !IsReauthRequired(err) {
		t.Fatalf("Token after invalid_grant = %v, want *ErrReauthRequired", err)
	}
	var re *ErrReauthRequired
	errors.As(err, &re)
	if re.Key != "work.gmail" {
		t.Errorf("ErrReauthRequired.Key = %q, want work.gmail", re.Key)
	}
}

func TestHTTPClientOffline(t *testing.T) {
	cfg := Config{
		ClientID:     "id",
		ClientSecret: "secret",
		Endpoint: oauth2.Endpoint{
			AuthURL:  "http://127.0.0.1:1/auth",
			TokenURL: "http://127.0.0.1:1/token", // nothing listens here
		},
	}
	store := &MemoryTokenStore{}
	store.Save("k", &oauth2.Token{AccessToken: "a", RefreshToken: "r", Expiry: time.Now().Add(-time.Minute)})

	src, err := TokenSource(context.Background(), cfg, store, "k")
	if err != nil {
		t.Fatal(err)
	}
	_, err = src.Token()
	if !errors.Is(err, model.ErrOffline) {
		t.Fatalf("Token with a dead token endpoint = %v, want model.ErrOffline", err)
	}
}

// ---------------------------------------------------------------------------
// Redirect URL

type stringAddr string

func (a stringAddr) Network() string { return "tcp" }
func (a stringAddr) String() string  { return string(a) }

// Google's installed-app flow only accepts a loopback redirect URI, and the
// one sent to the authorization server must match the one the browser is sent
// to. A listener bound to every interface prints as "[::]:port" or
// "0.0.0.0:port", neither of which is usable, so only the port is taken.
func TestLoopbackURL(t *testing.T) {
	cases := []struct {
		name string
		addr net.Addr
		want string
	}{
		{"ipv4 loopback", &net.TCPAddr{IP: net.IPv4(127, 0, 0, 1), Port: 5000}, "http://127.0.0.1:5000"},
		{"ipv4 wildcard", &net.TCPAddr{IP: net.IPv4zero, Port: 5001}, "http://127.0.0.1:5001"},
		{"ipv6 wildcard", &net.TCPAddr{IP: net.IPv6zero, Port: 5002}, "http://127.0.0.1:5002"},
		{"ipv6 loopback", &net.TCPAddr{IP: net.IPv6loopback, Port: 5003}, "http://127.0.0.1:5003"},
		{"stringly ipv6 wildcard", stringAddr("[::]:5004"), "http://127.0.0.1:5004"},
		{"stringly ipv4 wildcard", stringAddr("0.0.0.0:5005"), "http://127.0.0.1:5005"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got, err := loopbackURL(tc.addr)
			if err != nil {
				t.Fatalf("loopbackURL(%v): %v", tc.addr, err)
			}
			if got != tc.want {
				t.Errorf("loopbackURL(%v) = %q, want %q", tc.addr, got, tc.want)
			}
		})
	}

	if _, err := loopbackURL(stringAddr("not-an-address")); err == nil {
		t.Error("loopbackURL of an unparsable address succeeded, want an error")
	}
}

// End to end: listening on every interface must still advertise 127.0.0.1.
func TestLoginRedirectURLOnWildcardListener(t *testing.T) {
	fake := newFakeGoogle(t)

	var authQuery url.Values
	opened := make(chan struct{})
	open := func(raw string) error {
		u, err := url.Parse(raw)
		if err != nil {
			return err
		}
		authQuery = u.Query()
		close(opened)
		return nil
	}

	done := make(chan error, 1)
	go func() {
		_, err := Login(context.Background(), fake.config(), LoginOptions{
			Addr:        "0.0.0.0:0",
			OpenBrowser: open,
			Output:      io.Discard,
			Timeout:     10 * time.Second,
		})
		done <- err
	}()

	select {
	case <-opened:
	case err := <-done:
		t.Fatalf("Login returned before opening a browser: %v", err)
	case <-time.After(10 * time.Second):
		t.Fatal("timed out waiting for the authorization URL")
	}

	redirect := authQuery.Get("redirect_uri")
	if !strings.HasPrefix(redirect, "http://127.0.0.1:") {
		t.Fatalf("redirect_uri = %q, want a http://127.0.0.1:<port> URL", redirect)
	}
	u, err := url.Parse(redirect)
	if err != nil {
		t.Fatalf("redirect_uri is not a URL: %v", err)
	}
	if u.Port() == "" || u.Port() == "0" {
		t.Fatalf("redirect_uri %q carries no real port", redirect)
	}

	// The browser really can reach it, which is the point of the exercise.
	resp, err := http.Get(redirect + "/?state=" + authQuery.Get("state") + "&code=the-code")
	if err != nil {
		t.Fatalf("GET %s: %v", redirect, err)
	}
	resp.Body.Close()

	if err := <-done; err != nil {
		t.Fatalf("Login: %v", err)
	}
	if got := fake.form().Get("redirect_uri"); got != redirect {
		t.Errorf("token exchange redirect_uri = %q, want %q", got, redirect)
	}
}

// ---------------------------------------------------------------------------
// Error classification

// A 400 that is not an invalid_grant is a bug on our side, not a dead
// credential: sending the user through the consent screen cannot fix it.
func TestBadRequestWithoutInvalidGrantIsNotReauth(t *testing.T) {
	fake := newFakeGoogle(t)
	fake.failRawStatus = http.StatusBadRequest
	fake.failRawBody = "<html><body>Bad Request</body></html>"

	store := &MemoryTokenStore{}
	if err := store.Save("k", &oauth2.Token{
		AccessToken: "stale", RefreshToken: "r", Expiry: time.Now().Add(-time.Minute),
	}); err != nil {
		t.Fatal(err)
	}
	src, err := TokenSource(context.Background(), fake.config(), store, "k")
	if err != nil {
		t.Fatal(err)
	}
	_, err = src.Token()
	if err == nil {
		t.Fatal("Token succeeded, want the 400 surfaced")
	}
	if IsReauthRequired(err) {
		t.Errorf("a bare 400 was reported as *ErrReauthRequired: %v", err)
	}
	if errors.Is(err, model.ErrOffline) {
		t.Errorf("a bare 400 was reported as offline: %v", err)
	}
}

// A 400 whose body names invalid_grant is a dead grant even when the response
// is not the documented JSON object and x/oauth2 cannot fill in ErrorCode.
func TestBadRequestNamingInvalidGrantIsReauth(t *testing.T) {
	fake := newFakeGoogle(t)
	fake.failRawStatus = http.StatusBadRequest
	fake.failRawBody = "<html><body>error: Invalid_Grant (token revoked)</body></html>"

	store := &MemoryTokenStore{}
	if err := store.Save("k", &oauth2.Token{
		AccessToken: "stale", RefreshToken: "r", Expiry: time.Now().Add(-time.Minute),
	}); err != nil {
		t.Fatal(err)
	}
	src, err := TokenSource(context.Background(), fake.config(), store, "k")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := src.Token(); !IsReauthRequired(err) {
		t.Fatalf("Token after an invalid_grant body = %v, want *ErrReauthRequired", err)
	}
}

// ---------------------------------------------------------------------------
// Store failures and concurrency

// failingStore accepts nothing.
type failingStore struct {
	tok *oauth2.Token
}

func (s failingStore) Load(string) (*oauth2.Token, error) { return s.tok, nil }
func (s failingStore) Save(string, *oauth2.Token) error {
	return errors.New("disk is full")
}

// A token that cannot be persisted still works for this process, but silence
// would hide a store that is throwing away every rotated refresh token.
func TestSaveFailureIsWarnedAboutAndTokenReturned(t *testing.T) {
	fake := newFakeGoogle(t)
	var buf bytes.Buffer
	var mu sync.Mutex
	Logger = slog.New(slog.NewTextHandler(&syncWriter{w: &buf, mu: &mu}, &slog.HandlerOptions{Level: slog.LevelDebug}))
	t.Cleanup(func() { Logger = nil })

	store := failingStore{tok: &oauth2.Token{
		AccessToken: "stale", RefreshToken: "r", Expiry: time.Now().Add(-time.Minute),
	}}
	src, err := TokenSource(context.Background(), fake.config(), store, "work.gmail")
	if err != nil {
		t.Fatal(err)
	}
	tok, err := src.Token()
	if err != nil {
		t.Fatalf("Token = %v, want the refreshed token despite the store failing", err)
	}
	if tok.AccessToken != "access-1" {
		t.Errorf("access token = %q, want access-1", tok.AccessToken)
	}

	mu.Lock()
	logged := buf.String()
	mu.Unlock()
	if !strings.Contains(logged, "level=WARN") {
		t.Errorf("store failure was not logged at WARN:\n%s", logged)
	}
	if !strings.Contains(logged, "work.gmail") || !strings.Contains(logged, "disk is full") {
		t.Errorf("warning does not name the key and the cause:\n%s", logged)
	}
}

type syncWriter struct {
	w  io.Writer
	mu *sync.Mutex
}

func (s *syncWriter) Write(p []byte) (int, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.w.Write(p)
}

// One token source is shared by every request an *http.Client makes, so the
// store is written from several goroutines at once.
func TestMemoryTokenStoreConcurrent(t *testing.T) {
	store := &MemoryTokenStore{}
	var wg sync.WaitGroup
	for i := range 16 {
		wg.Add(2)
		go func() {
			defer wg.Done()
			if err := store.Save("k", &oauth2.Token{AccessToken: "a" + strconv.Itoa(i)}); err != nil {
				t.Errorf("Save: %v", err)
			}
		}()
		go func() {
			defer wg.Done()
			if _, err := store.Load("k"); err != nil && !errors.Is(err, model.ErrNotFound) {
				t.Errorf("Load: %v", err)
			}
		}()
	}
	wg.Wait()
	if _, err := store.Load("k"); err != nil {
		t.Fatalf("Load after the storm: %v", err)
	}
	if err := store.Save("k", nil); err == nil {
		t.Error("Save of a nil token succeeded, want an error instead of a panic")
	}
}
