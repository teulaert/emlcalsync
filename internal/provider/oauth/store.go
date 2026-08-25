// Package oauth implements the Google OAuth 2.0 flow for installed
// applications (loopback redirect + PKCE) and persistent token storage, as
// described in DESIGN.md §6.1 and §11.
//
// Two things live here:
//
//   - [Login] runs the interactive consent flow once and returns a token.
//   - [HTTPClient] turns a stored token into an *http.Client that refreshes
//     itself and writes refreshed tokens back to the [TokenStore].
//
// Nothing in this package talks to Gmail or Calendar; it only knows about
// scopes and tokens.
package oauth

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"

	"golang.org/x/oauth2"

	"github.com/lennert/emlcal/internal/model"
)

// TokenStore persists OAuth tokens per account. key is an opaque, filesystem
// safe identifier chosen by the caller (emlcal uses "<account>.<provider>").
//
// Load returns an error wrapping model.ErrNotFound when nothing is stored.
type TokenStore interface {
	Load(key string) (*oauth2.Token, error)
	Save(key string, t *oauth2.Token) error
}

// FileTokenStore stores one JSON file per key in Dir, mode 0600 in a 0700
// directory, written atomically (temp file + rename) so a crash can never
// leave a truncated token behind. This is the default backend from §11.
type FileTokenStore struct {
	Dir string
}

var _ TokenStore = FileTokenStore{}

// ErrBadKey is returned for keys that cannot be used as a file name.
var ErrBadKey = fmt.Errorf("invalid token store key")

func validKey(key string) error {
	if key == "" || key == "." || key == ".." {
		return fmt.Errorf("%w: %q", ErrBadKey, key)
	}
	if strings.ContainsAny(key, `/\`+"\x00") {
		return fmt.Errorf("%w: %q", ErrBadKey, key)
	}
	return nil
}

// Path returns the file a key is stored in. It does not check for existence.
func (s FileTokenStore) Path(key string) (string, error) {
	if err := validKey(key); err != nil {
		return "", err
	}
	return filepath.Join(s.Dir, key+".json"), nil
}

// Load reads the token for key. A missing file yields model.ErrNotFound.
func (s FileTokenStore) Load(key string) (*oauth2.Token, error) {
	path, err := s.Path(key)
	if err != nil {
		return nil, err
	}
	b, err := os.ReadFile(path)
	if err != nil {
		if os.IsNotExist(err) {
			return nil, fmt.Errorf("%w: no stored token for %q", model.ErrNotFound, key)
		}
		return nil, fmt.Errorf("read token %s: %w", path, err)
	}
	var tok oauth2.Token
	if err := json.Unmarshal(b, &tok); err != nil {
		return nil, fmt.Errorf("parse token %s: %w", path, err)
	}
	return &tok, nil
}

// Save writes the token for key atomically with mode 0600.
func (s FileTokenStore) Save(key string, t *oauth2.Token) error {
	path, err := s.Path(key)
	if err != nil {
		return err
	}
	if t == nil {
		return fmt.Errorf("oauth: refusing to store a nil token for %q", key)
	}
	if err := os.MkdirAll(s.Dir, 0o700); err != nil {
		return fmt.Errorf("create secrets dir: %w", err)
	}
	b, err := json.MarshalIndent(t, "", "  ")
	if err != nil {
		return fmt.Errorf("encode token: %w", err)
	}
	b = append(b, '\n')

	tmp, err := os.CreateTemp(s.Dir, ".tok-*")
	if err != nil {
		return fmt.Errorf("create temp token file: %w", err)
	}
	tmpName := tmp.Name()
	defer os.Remove(tmpName) // no-op once the rename succeeded

	if err := tmp.Chmod(0o600); err != nil {
		tmp.Close()
		return fmt.Errorf("chmod token file: %w", err)
	}
	if _, err := tmp.Write(b); err != nil {
		tmp.Close()
		return fmt.Errorf("write token file: %w", err)
	}
	if err := tmp.Sync(); err != nil {
		tmp.Close()
		return fmt.Errorf("sync token file: %w", err)
	}
	if err := tmp.Close(); err != nil {
		return fmt.Errorf("close token file: %w", err)
	}
	if err := os.Rename(tmpName, path); err != nil {
		return fmt.Errorf("install token file: %w", err)
	}
	return nil
}

// MemoryTokenStore is an in-memory TokenStore, useful for tests and for
// one-shot commands that must not touch disk. It is safe for concurrent use:
// a token source is shared by every HTTP request an *http.Client makes, so
// Save can be called from several goroutines at once.
type MemoryTokenStore struct {
	mu     sync.Mutex
	tokens map[string]*oauth2.Token
}

var _ TokenStore = (*MemoryTokenStore)(nil)

func (s *MemoryTokenStore) Load(key string) (*oauth2.Token, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if t, ok := s.tokens[key]; ok {
		cp := *t
		return &cp, nil
	}
	return nil, fmt.Errorf("%w: no stored token for %q", model.ErrNotFound, key)
}

func (s *MemoryTokenStore) Save(key string, t *oauth2.Token) error {
	if t == nil {
		return fmt.Errorf("oauth: refusing to store a nil token for %q", key)
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.tokens == nil {
		s.tokens = make(map[string]*oauth2.Token)
	}
	cp := *t
	s.tokens[key] = &cp
	return nil
}
