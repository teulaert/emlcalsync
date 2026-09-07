package config

import (
	"os"
	"path/filepath"
	"strings"
)

// appDir is the per-spec subdirectory name under each XDG base directory.
const appDir = "emlcal"

// Environment variables that override the config file.
const (
	EnvConfig  = "EMLCAL_CONFIG"   // full path to config.toml
	EnvDataDir = "EMLCAL_DATA_DIR" // overrides the resolved data directory
	EnvFormat  = "EMLCAL_FORMAT"   // overrides general.default_format
)

// xdgDir resolves one XDG base directory: $envVar if it is set to an absolute
// path, otherwise $HOME/fallback. The emlcal subdirectory is appended.
func xdgDir(envVar, fallback string) string {
	if v := os.Getenv(envVar); filepath.IsAbs(v) {
		return filepath.Join(v, appDir)
	}
	home, err := os.UserHomeDir()
	if err != nil || home == "" {
		// Last resort: relative to the working directory rather than "".
		return filepath.Join("."+fallback, appDir)
	}
	return filepath.Join(home, fallback, appDir)
}

// ConfigDir returns $XDG_CONFIG_HOME/emlcal (default ~/.config/emlcal).
func ConfigDir() string { return xdgDir("XDG_CONFIG_HOME", ".config") }

// DataDir returns $XDG_DATA_HOME/emlcal (default ~/.local/share/emlcal).
func DataDir() string { return xdgDir("XDG_DATA_HOME", filepath.Join(".local", "share")) }

// StateDir returns $XDG_STATE_HOME/emlcal (default ~/.local/state/emlcal).
func StateDir() string { return xdgDir("XDG_STATE_HOME", filepath.Join(".local", "state")) }

// CacheDir returns $XDG_CACHE_HOME/emlcal (default ~/.cache/emlcal). What
// lives there is disposable: deleting it costs nothing but the work to make
// it again.
func CacheDir() string { return xdgDir("XDG_CACHE_HOME", ".cache") }

// ViewDir holds the HTML pages `mail open` renders for the browser. It is
// under the cache directory because a rendered message is a copy of something
// the archive already has, kept only until the browser has read it.
func ViewDir() string { return filepath.Join(CacheDir(), "view") }

// DownloadDir is where a file saved out of the TUI lands when the
// configuration names nowhere: the desktop's own downloads folder, which is
// $XDG_DOWNLOAD_DIR, else what user-dirs.dirs says it is, else ~/Downloads.
//
// It is deliberately not one of emlcal's own directories. A saved attachment
// is something the user asked to have, to open from wherever the rest of
// their files are; a cache directory is where nobody would look for it.
func DownloadDir() string {
	if v := os.Getenv("XDG_DOWNLOAD_DIR"); filepath.IsAbs(v) {
		return filepath.Clean(v)
	}
	home, err := os.UserHomeDir()
	if err != nil || home == "" {
		return "Downloads"
	}
	if v := userDir(home, "XDG_DOWNLOAD_DIR"); v != "" {
		return v
	}
	return filepath.Join(home, "Downloads")
}

// userDir reads one entry out of user-dirs.dirs, the file the desktop keeps
// its folder choices in: lines of the form XDG_DOWNLOAD_DIR="$HOME/Downloads".
// It lives directly under the config base, not under emlcal's own directory.
// Anything unreadable, or a value that is not an absolute path, is nothing.
func userDir(home, key string) string {
	base := os.Getenv("XDG_CONFIG_HOME")
	if !filepath.IsAbs(base) {
		base = filepath.Join(home, ".config")
	}
	b, err := os.ReadFile(filepath.Join(base, "user-dirs.dirs"))
	if err != nil {
		return ""
	}
	for _, line := range strings.Split(string(b), "\n") {
		v, ok := strings.CutPrefix(strings.TrimSpace(line), key+"=")
		if !ok {
			continue
		}
		v = strings.ReplaceAll(strings.Trim(v, `"`), "$HOME", home)
		if filepath.IsAbs(v) {
			return filepath.Clean(v)
		}
	}
	return ""
}

// DefaultPath is the config file emlcal reads when no --config is given:
// $EMLCAL_CONFIG, else <config dir>/config.toml.
func DefaultPath() string {
	if p := os.Getenv(EnvConfig); p != "" {
		return p
	}
	return filepath.Join(ConfigDir(), "config.toml")
}

// Paths returns the concrete files the rest of the tool uses. The lock path is
// the process-wide lock; per-account locks come from LockPath.
func (c *Config) Paths() (db, blobs, secrets, lock, log string) {
	return c.DBPath(), c.BlobsDir(), c.SecretsDir(),
		filepath.Join(c.General.StateDir, "emlcal.lock"),
		c.LogPath()
}

// DBPath is the SQLite index.
func (c *Config) DBPath() string { return filepath.Join(c.General.DataDir, "emlcal.db") }

// BlobsDir is the content-addressed raw-message archive.
func (c *Config) BlobsDir() string { return filepath.Join(c.General.DataDir, "blobs") }

// SecretsDir holds OAuth tokens and API tokens (mode 0700).
func (c *Config) SecretsDir() string { return filepath.Join(c.General.ConfigDir, "secrets") }

// LogPath is the daemon/CLI log file.
func (c *Config) LogPath() string { return filepath.Join(c.General.StateDir, "emlcal.log") }

// LockPath is the per-account sync lock, flock'ed by the sync engine.
func (c *Config) LockPath(account string) string {
	return filepath.Join(c.General.StateDir, "sync."+account+".lock")
}

// EnsureDirs creates the data and state directories (0700) so callers can
// write without checking. The config directory is created by Save.
func (c *Config) EnsureDirs() error {
	for _, d := range []string{c.General.DataDir, c.General.StateDir, c.BlobsDir()} {
		if err := os.MkdirAll(d, 0o700); err != nil {
			return err
		}
	}
	return nil
}
