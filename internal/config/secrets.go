package config

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"

	"github.com/teulaert/emlcalsync/internal/model"
)

// Secret backend names for general.secret_backend.
const (
	BackendFile      = "file"
	BackendLibsecret = "libsecret"
)

// Secrets stores OAuth tokens and API tokens outside config.toml. Keys are
// opaque, filesystem-safe strings; Account.SecretKey builds the canonical one.
type Secrets interface {
	// Get returns the stored value, or an error wrapping model.ErrNotFound.
	Get(key string) ([]byte, error)
	// Set writes the value, replacing any previous one.
	Set(key string, v []byte) error
	// Delete removes the value. Deleting a missing key is not an error.
	Delete(key string) error
}

// Lister is implemented by backends that can enumerate their keys; `emlcal
// doctor` uses it to spot orphaned tokens.
type Lister interface {
	List() ([]string, error)
}

// OpenSecrets returns the backend named by general.secret_backend, creating
// the storage directory for the file backend if needed.
func OpenSecrets(c *Config) (Secrets, error) {
	switch backend := c.General.SecretBackend; backend {
	case "", BackendFile:
		s := &FileSecrets{Dir: c.SecretsDir()}
		if err := s.ensureDir(); err != nil {
			return nil, err
		}
		return s, nil
	case BackendLibsecret:
		return openLibsecret(c)
	default:
		return nil, fmt.Errorf("secret_backend %q: unknown backend", backend)
	}
}

// openLibsecret is the hook for the freedesktop Secret Service backend
// (DESIGN.md §11). Wiring it up is a matter of dropping in go-keyring and
// implementing Secrets; nothing else in the tree needs to change.
func openLibsecret(*Config) (Secrets, error) {
	return nil, fmt.Errorf("secret_backend %q: not implemented yet; use %q", BackendLibsecret, BackendFile)
}

// FileSecrets stores one file per key under Dir, mode 0600 in a 0700
// directory, written atomically.
type FileSecrets struct {
	Dir string
}

var _ Secrets = (*FileSecrets)(nil)
var _ Lister = (*FileSecrets)(nil)

// ErrBadKey is returned for keys that would escape Dir or confuse the shell.
var ErrBadKey = errors.New("invalid secret key")

// validKey rejects anything that is not a plain filename. Keys come from
// account names (already [a-z0-9-]) and provider names, but the check keeps a
// hand-edited config from writing outside the secrets directory.
func validKey(key string) error {
	if key == "" || key == "." || key == ".." {
		return fmt.Errorf("%w: %q", ErrBadKey, key)
	}
	if strings.ContainsAny(key, `/\`) || strings.ContainsRune(key, 0) {
		return fmt.Errorf("%w: %q contains a path separator", ErrBadKey, key)
	}
	if strings.HasPrefix(key, ".") {
		return fmt.Errorf("%w: %q must not start with a dot", ErrBadKey, key)
	}
	return nil
}

func (f *FileSecrets) path(key string) (string, error) {
	if err := validKey(key); err != nil {
		return "", err
	}
	if f.Dir == "" {
		return "", errors.New("secrets: no directory configured")
	}
	return filepath.Join(f.Dir, key), nil
}

func (f *FileSecrets) ensureDir() error {
	if f.Dir == "" {
		return errors.New("secrets: no directory configured")
	}
	if err := os.MkdirAll(f.Dir, 0o700); err != nil {
		return err
	}
	// Tighten an existing directory that was created too permissively (for
	// example by restoring a backup with a loose umask).
	if fi, err := os.Stat(f.Dir); err == nil && fi.Mode().Perm()&0o077 != 0 {
		return os.Chmod(f.Dir, 0o700)
	}
	return nil
}

// Get reads a secret. A missing key wraps model.ErrNotFound.
func (f *FileSecrets) Get(key string) ([]byte, error) {
	p, err := f.path(key)
	if err != nil {
		return nil, err
	}
	b, err := os.ReadFile(p)
	if errors.Is(err, os.ErrNotExist) {
		return nil, fmt.Errorf("secret %q: %w", key, model.ErrNotFound)
	}
	if err != nil {
		return nil, err
	}
	return b, nil
}

// Set writes a secret atomically with mode 0600.
func (f *FileSecrets) Set(key string, v []byte) error {
	p, err := f.path(key)
	if err != nil {
		return err
	}
	if err := f.ensureDir(); err != nil {
		return err
	}
	return writeFileAtomic(p, v, 0o600)
}

// Delete removes a secret; a missing key is not an error.
func (f *FileSecrets) Delete(key string) error {
	p, err := f.path(key)
	if err != nil {
		return err
	}
	if err := os.Remove(p); err != nil && !errors.Is(err, os.ErrNotExist) {
		return err
	}
	return nil
}

// List returns the stored keys, sorted.
func (f *FileSecrets) List() ([]string, error) {
	entries, err := os.ReadDir(f.Dir)
	if errors.Is(err, os.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	var keys []string
	for _, e := range entries {
		if e.IsDir() || strings.HasPrefix(e.Name(), ".") {
			continue
		}
		keys = append(keys, e.Name())
	}
	sort.Strings(keys)
	return keys, nil
}
