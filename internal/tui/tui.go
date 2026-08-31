// Package tui is the interactive terminal view over the local archive.
//
// It reads the same SQLite index every other command reads and writes only
// through the sync engine, exactly as DESIGN.md §2 requires of a second
// surface: "TUIs / Omarchy plugins read the same SQLite DB through an internal
// Go package, never through a second provider client."
//
// The package deliberately does not import internal/cli. The command is
// registered there, so the dependency has to run one way; Deps is what crosses
// the boundary instead of *cli.App.
package tui

import (
	"context"
	"log/slog"
	"time"

	tea "charm.land/bubbletea/v2"

	"github.com/teulaert/emlcalsync/internal/ai"
	"github.com/teulaert/emlcalsync/internal/config"
	"github.com/teulaert/emlcalsync/internal/mime"
	"github.com/teulaert/emlcalsync/internal/store"
	"github.com/teulaert/emlcalsync/internal/sync"
)

// Deps is everything the TUI needs from its host command.
type Deps struct {
	Store  *store.Store
	Engine *sync.Engine
	Config *config.Config
	// Accounts limits every view to these account ids. Empty means all, which
	// is what makes the default view the unified one.
	Accounts []string
	Loc      *time.Location
	Now      func() time.Time
	Logger   *slog.Logger

	// AI is the configured language model, or nil when there is none: the
	// composer's ctrl+g then says so and nothing is sent anywhere.
	AI ai.Client
	// Tools is what the model may look up while drafting -- the read
	// commands, in practice. Nil means it writes from the thread alone.
	Tools ai.Toolset

	// StatePath is the directory holding emlcal.pid, so a manual refresh can
	// nudge a running daemon. Empty disables the nudge.
	StatePath string
	// ViewDir is where o writes the page it hands the browser. Empty falls
	// back to config.ViewDir().
	ViewDir string
	// Browser opens a URL on the desktop. Nil means browser.Open; a test sets
	// it so that pressing o launches nothing.
	Browser func(url string) error
	// Fetch pulls the pictures o folds into the page, when the configuration
	// asks for them. Nil means webasset's own.
	Fetch mime.FetchFunc

	// input and output are set by tests to drive the program headlessly.
	input  ioReader
	output ioWriter
}

type (
	ioReader interface{ Read([]byte) (int, error) }
	ioWriter interface{ Write([]byte) (int, error) }
)

func (d Deps) now() time.Time {
	if d.Now != nil {
		return d.Now()
	}
	return time.Now()
}

func (d Deps) loc() *time.Location {
	if d.Loc != nil {
		return d.Loc
	}
	return time.Local
}

func (d Deps) log() *slog.Logger {
	if d.Logger != nil {
		return d.Logger
	}
	return slog.New(slog.DiscardHandler)
}

// Run starts the program and blocks until the user quits.
func Run(ctx context.Context, d Deps) error {
	opts := []tea.ProgramOption{tea.WithContext(ctx)}
	if d.input != nil {
		opts = append(opts, tea.WithInput(d.input))
	}
	if d.output != nil {
		opts = append(opts, tea.WithOutput(d.output))
	}
	_, err := tea.NewProgram(newRoot(d), opts...).Run()
	return err
}
