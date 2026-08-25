package cli

import (
	"errors"
	"fmt"
	"os"
	"sort"
	"strings"

	"github.com/spf13/cobra"

	"github.com/lennert/emlcal/internal/model"
	"github.com/lennert/emlcal/internal/output"
	"github.com/lennert/emlcal/internal/provider"
	"github.com/lennert/emlcal/internal/sync"
)

// Registrar adds a command group to the root. Command files call Register in
// an init func so the tree is assembled without a central list.
type Registrar func(root *cobra.Command, app *App)

var registrars []Registrar

// Register queues a command group for NewRoot.
func Register(r Registrar) { registrars = append(registrars, r) }

// NewRoot builds the full command tree bound to app.
func NewRoot(app *App) *cobra.Command {
	root := &cobra.Command{
		Use:   "emlcal",
		Short: "Local mail & calendar archive with an agent-friendly CLI",
		Long: `emlcal keeps a complete local archive of your mail and calendar accounts
and lets you (or an AI agent) read, search and act on it from the terminal.

Read commands work fully offline. Write commands go to the provider and are
queued when offline (exit code 6). Output is JSON when stdout is not a TTY.`,
		Args:              cobra.NoArgs,
		RunE:              func(cmd *cobra.Command, args []string) error { return cmd.Help() },
		SilenceUsage:      true,
		SilenceErrors:     true,
		CompletionOptions: cobra.CompletionOptions{DisableDefaultCmd: false},
	}
	pf := root.PersistentFlags()
	pf.StringVar(&app.ConfigPath, "config", "", "config file (default $XDG_CONFIG_HOME/emlcal/config.toml)")
	pf.StringVarP(&app.FormatFlag, "format", "o", "", "output format: json|table|plain (default: table on a TTY, json otherwise)")
	pf.BoolVar(&app.Pretty, "pretty", false, "indent JSON output")
	pf.BoolVarP(&app.Verbose, "verbose", "v", false, "debug logging to stderr")
	pf.StringArrayVarP(&app.Accounts, "account", "a", nil, "restrict to account (repeatable; default all)")
	root.SetOut(app.Stdout)
	root.SetErr(app.Stderr)
	root.SetIn(strings.NewReader(""))
	if app.Stdin != nil {
		if r, ok := app.Stdin.(interface{ Read([]byte) (int, error) }); ok {
			root.SetIn(r)
		}
	}
	for _, r := range registrars {
		r(root, app)
	}
	sortCommands(root)
	return root
}

func sortCommands(c *cobra.Command) {
	cmds := c.Commands()
	sort.Slice(cmds, func(i, j int) bool { return cmds[i].Name() < cmds[j].Name() })
	for _, sc := range cmds {
		sortCommands(sc)
	}
}

// Execute runs args against app and returns the process exit code. Errors
// are rendered through the printer (JSON envelope in JSON mode).
func Execute(args []string, app *App) int {
	root := NewRoot(app)
	root.SetArgs(args)
	err := root.Execute()
	closeErr := app.Close()
	if err == nil {
		if closeErr != nil {
			fmt.Fprintf(app.Stderr, "emlcal: close: %v\n", closeErr)
		}
		return output.ExitOK
	}
	var ee *output.ExitError
	if !errors.As(err, &ee) {
		err = &output.ExitError{Code: ExitCode(err), Err: err}
	}
	return app.Printer().Fail("", err)
}

// ExitCode maps an error to the DESIGN.md exit code table.
func ExitCode(err error) int {
	var ee *output.ExitError
	switch {
	case err == nil:
		return output.ExitOK
	case errors.As(err, &ee):
		return ee.Code
	case errors.Is(err, model.ErrNotFound):
		return output.ExitNotFound
	case provider.IsOffline(err):
		return output.ExitOffline
	case errors.Is(err, sync.ErrLocked):
		return output.ExitGeneric
	case isUsageError(err):
		return output.ExitUsage
	}
	return output.ExitGeneric
}

func isUsageError(err error) bool {
	s := err.Error()
	for _, p := range []string{"unknown flag", "unknown shorthand flag", "unknown command", "required flag", "accepts ", "invalid argument", "flag needs an argument"} {
		if strings.HasPrefix(s, p) || strings.Contains(s, ": "+p) {
			return true
		}
	}
	return false
}

// Queued builds the exit-6 error used after a write was accepted locally but
// could not reach the provider.
func Queued(n int) error {
	return output.Errorf(output.ExitQueued, "%d operation(s) queued in the outbox (offline); run `emlcal outbox retry` or wait for the daemon", n)
}

// stdinIsPipe reports whether real stdin has data piped in (used by --body -).
func stdinIsPipe() bool {
	fi, err := os.Stdin.Stat()
	return err == nil && fi.Mode()&os.ModeCharDevice == 0
}
