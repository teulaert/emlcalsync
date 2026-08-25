package oauth

import (
	"context"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"errors"
	"io"
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
