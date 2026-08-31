// Package cli wires the cobra command tree to the store, blob archive, sync
// engine and providers. Each command group lives in its own file and
// registers itself through Register in an init func, so the tree is always
// complete regardless of which files exist.
package cli

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"time"

	"github.com/teulaert/emlcalsync/internal/ai"
	"github.com/teulaert/emlcalsync/internal/blob"
	"github.com/teulaert/emlcalsync/internal/config"
	"github.com/teulaert/emlcalsync/internal/mime"
	"github.com/teulaert/emlcalsync/internal/model"
	"github.com/teulaert/emlcalsync/internal/output"
	"github.com/teulaert/emlcalsync/internal/store"
	"github.com/teulaert/emlcalsync/internal/sync"
)

// App holds everything a command needs. Resources are opened lazily so that
// commands like `--help`, `skill` or `account add` on a fresh machine never
// touch the database.
type App struct {
	// I/O — overridable for tests.
	Stdout io.Writer
	Stderr io.Writer
	Stdin  io.Reader
	IsTTY  bool
	Now    func() time.Time
	// Factory builds providers; tests replace it with fakes.
	Factory sync.ProviderFactory
	// OpenBrowser is used by `account add gmail`; nil prints the URL only.
	OpenBrowser func(url string) error
	// Fetch pulls the pictures `mail open` folds into the page it renders.
	// Nil means webasset's own; a test stands one in so that opening a
	// message asks nobody for anything.
	Fetch mime.FetchFunc
	// Progress receives sync progress events (set by the sync command).
	Progress func(sync.ProgressEvent)
	// AIClient, when set, is the model; tests stand one in. Otherwise the
	// [ai] table decides.
	AIClient ai.Client

	// Global flags, bound in root.go.
	ConfigPath string
	FormatFlag string
	Pretty     bool
	Verbose    bool
	Accounts   []string // --account, repeatable

	cfg     *config.Config
	st      *store.Store
	blobs   *blob.Store
	eng     *sync.Engine
	printer *output.Printer
	logger  *slog.Logger
	logFile *os.File
	loc     *time.Location
}

// NewApp returns an App bound to the real process environment.
func NewApp() *App {
	a := &App{
		Stdout: os.Stdout,
		Stderr: os.Stderr,
		Stdin:  os.Stdin,
		IsTTY:  isTerminal(os.Stdout),
		Now:    time.Now,
	}
	a.Factory = &Factory{app: a}
	return a
}

func isTerminal(f *os.File) bool {
	fi, err := f.Stat()
	if err != nil {
		return false
	}
	return fi.Mode()&os.ModeCharDevice != 0
}

// Config loads, validates and caches the configuration.
func (a *App) Config() (*config.Config, error) {
	if a.cfg != nil {
		return a.cfg, nil
	}
	cfg, err := config.Load(a.ConfigPath)
	if err != nil {
		return nil, output.Errorf(output.ExitUsage, "config: %v", err)
	}
	if err := cfg.Validate(); err != nil {
		return nil, output.Errorf(output.ExitUsage, "config: %v", err)
	}
	a.cfg = cfg
	return cfg, nil
}

// SetConfig replaces the cached config (after `account add` saved a new one).
func (a *App) SetConfig(cfg *config.Config) { a.cfg = cfg }

// Location returns the configured time zone (falls back to Local).
func (a *App) Location() *time.Location {
	if a.loc != nil {
		return a.loc
	}
	a.loc = time.Local
	if cfg, err := a.Config(); err == nil {
		if l, err := cfg.Location(); err == nil && l != nil {
			a.loc = l
		}
	}
	return a.loc
}

// Store opens the SQLite index (creating directories on first use).
func (a *App) Store() (*store.Store, error) {
	if a.st != nil {
		return a.st, nil
	}
	cfg, err := a.Config()
	if err != nil {
		return nil, err
	}
	if err := cfg.EnsureDirs(); err != nil {
		return nil, err
	}
	st, err := store.Open(cfg.DBPath())
	if err != nil {
		return nil, fmt.Errorf("open index %s: %w", cfg.DBPath(), err)
	}
	st.SetLogger(a.Logger())
	a.st = st
	return st, nil
}

// Blobs opens the raw-message archive.
func (a *App) Blobs() (*blob.Store, error) {
	if a.blobs != nil {
		return a.blobs, nil
	}
	cfg, err := a.Config()
	if err != nil {
		return nil, err
	}
	if err := cfg.EnsureDirs(); err != nil {
		return nil, err
	}
	b, err := blob.Open(cfg.BlobsDir())
	if err != nil {
		return nil, fmt.Errorf("open blobs %s: %w", cfg.BlobsDir(), err)
	}
	a.blobs = b
	return b, nil
}

// Engine builds the sync engine on top of Store/Blobs/Factory.
func (a *App) Engine() (*sync.Engine, error) {
	if a.eng != nil {
		return a.eng, nil
	}
	cfg, err := a.Config()
	if err != nil {
		return nil, err
	}
	st, err := a.Store()
	if err != nil {
		return nil, err
	}
	bl, err := a.Blobs()
	if err != nil {
		return nil, err
	}
	_, _, _, lockPath, _ := cfg.Paths()
	eng, err := sync.New(sync.Options{
		Store:     st,
		Blobs:     bl,
		Config:    cfg,
		Providers: a.Factory,
		Logger:    a.Logger(),
		Progress: func(ev sync.ProgressEvent) {
			if a.Progress != nil {
				a.Progress(ev)
			}
		},
		LockDir: filepath.Dir(lockPath),
	})
	if err != nil {
		return nil, err
	}
	a.eng = eng
	return eng, nil
}

// Secrets opens the secret backend.
func (a *App) Secrets() (config.Secrets, error) {
	cfg, err := a.Config()
	if err != nil {
		return nil, err
	}
	if err := cfg.EnsureDirs(); err != nil {
		return nil, err
	}
	return config.OpenSecrets(cfg)
}

// Printer returns the output printer for the resolved format.
func (a *App) Printer() *output.Printer {
	if a.printer != nil {
		return a.printer
	}
	f := output.Auto
	src := a.FormatFlag
	if src == "" {
		src = os.Getenv(config.EnvFormat)
	}
	if src == "" {
		if cfg, err := a.Config(); err == nil && cfg.General.DefaultFormat != "" {
			src = cfg.General.DefaultFormat
		}
	}
	if src != "" {
		if pf, err := output.ParseFormat(src); err == nil {
			f = pf
		}
	}
	a.printer = &output.Printer{
		W:      a.Stdout,
		ErrW:   a.Stderr,
		Format: output.Resolve(f, a.IsTTY),
		Pretty: a.Pretty,
		Color:  a.IsTTY,
	}
	return a.printer
}

// Logger logs to <state>/emlcal.log at Info (Debug with --verbose), and also
// to stderr when --verbose is set.
func (a *App) Logger() *slog.Logger {
	if a.logger != nil {
		return a.logger
	}
	level := slog.LevelInfo
	if a.Verbose {
		level = slog.LevelDebug
	}
	var w io.Writer = io.Discard
	if cfg, err := a.Config(); err == nil {
		if err := cfg.EnsureDirs(); err == nil {
			if f, err := os.OpenFile(cfg.LogPath(), os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0o600); err == nil {
				a.logFile = f
				w = f
			}
		}
	}
	if a.Verbose {
		w = io.MultiWriter(w, a.Stderr)
	}
	a.logger = slog.New(slog.NewTextHandler(w, &slog.HandlerOptions{Level: level}))
	return a.logger
}

// Close releases the store and log file.
func (a *App) Close() error {
	var errs []error
	if a.st != nil {
		errs = append(errs, a.st.Close())
		a.st = nil
	}
	if a.logFile != nil {
		errs = append(errs, a.logFile.Close())
		a.logFile = nil
	}
	return errors.Join(errs...)
}

// AccountIDs returns the accounts selected by --account, or all configured
// accounts. Unknown names are an error (exit 3).
func (a *App) AccountIDs() ([]string, error) {
	cfg, err := a.Config()
	if err != nil {
		return nil, err
	}
	if len(a.Accounts) == 0 {
		return cfg.AccountNames(), nil
	}
	var out []string
	for _, n := range a.Accounts {
		if _, ok := cfg.Account(n); !ok {
			return nil, output.Errorf(output.ExitNotFound, "unknown account %q: %w", n, model.ErrNotFound)
		}
		out = append(out, n)
	}
	return out, nil
}

// ResolveAccount returns the configured account with that name.
func (a *App) ResolveAccount(name string) (*config.Account, error) {
	cfg, err := a.Config()
	if err != nil {
		return nil, err
	}
	acct, ok := cfg.Account(name)
	if !ok {
		return nil, output.Errorf(output.ExitNotFound, "unknown account %q: %w", name, model.ErrNotFound)
	}
	return acct, nil
}

// SendAccount picks the account for `mail send`/`draft` per DESIGN.md:
// explicit --account, else general.default_account, else the only account.
func (a *App) SendAccount(explicit string) (*config.Account, error) {
	cfg, err := a.Config()
	if err != nil {
		return nil, err
	}
	switch {
	case explicit != "":
		return a.ResolveAccount(explicit)
	case cfg.General.DefaultAccount != "":
		return a.ResolveAccount(cfg.General.DefaultAccount)
	case len(cfg.Accounts) == 1:
		return &cfg.Accounts[0], nil
	case len(cfg.Accounts) == 0:
		return nil, output.Errorf(output.ExitUsage, "no accounts configured; run `emlcal account add`")
	}
	return nil, output.Errorf(output.ExitUsage, "several accounts configured: pass --account or set general.default_account")
}

// Context returns a base context for a command.
func (a *App) Context() context.Context { return context.Background() }
