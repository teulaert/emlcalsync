package cli

import (
	"errors"
	"fmt"
	"os"
	"sort"
	"strings"

	"github.com/spf13/cobra"

	"github.com/teulaert/emlcalsync/internal/model"
	"github.com/teulaert/emlcalsync/internal/output"
	"github.com/teulaert/emlcalsync/internal/provider"
	"github.com/teulaert/emlcalsync/internal/sync"
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
	if err := checkUnknownCommand(root, args); err != nil {
		app.Close()
		return app.Printer().Fail("", err)
	}
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

// ---------------------------------------------------------------------------
// Unknown commands
//
// cobra parses flags before it decides the command does not exist, so
// `emlcal add fastmail --name x` used to fail with "unknown flag: --name" —
// true, but useless: the real mistake is the missing `account`. The first
// positional argument is therefore checked against the command tree before
// cobra ever sees the flags.

// checkUnknownCommand returns a usage error when the first word is not one of
// the root's subcommands.
func checkUnknownCommand(root *cobra.Command, args []string) error {
	// The built-in commands only exist once cobra has run its own setup.
	root.InitDefaultHelpCmd()
	root.InitDefaultCompletionCmd(args...)

	name := firstPositional(root, args)
	if name == "" || strings.HasPrefix(name, "__") || findChild(root, name) != nil {
		// "__complete" and friends are cobra's own completion machinery; it
		// registers them later, during Execute.
		return nil
	}
	msg := fmt.Sprintf("unknown command %q for %q", name, root.CommandPath())
	if hint := commandHint(root, name); hint != "" {
		msg += fmt.Sprintf("; did you mean `%s`?", hint)
	}
	return output.Errorf(output.ExitUsage, "%s", msg)
}

// firstPositional returns the first argument that is not a flag or a flag's
// value, mirroring how cobra itself strips flags when it looks for a
// subcommand: an unknown long flag is assumed to take a value.
func firstPositional(cmd *cobra.Command, args []string) string {
	fs := cmd.Flags()
	for i := 0; i < len(args); i++ {
		a := args[i]
		switch {
		case a == "--":
			if i+1 < len(args) {
				return args[i+1]
			}
			return ""
		case strings.HasPrefix(a, "--"):
			if strings.Contains(a, "=") {
				continue
			}
			if f := fs.Lookup(strings.TrimPrefix(a, "--")); f == nil || f.NoOptDefVal == "" {
				i++ // the value is the next argument
			}
		case strings.HasPrefix(a, "-") && a != "-":
			if strings.Contains(a, "=") {
				continue
			}
			short := strings.TrimPrefix(a, "-")
			if len(short) != 1 {
				continue // a bundle like -av: no separate value
			}
			if f := fs.ShorthandLookup(short); f == nil || f.NoOptDefVal == "" {
				i++
			}
		default:
			return a
		}
	}
	return ""
}

func findChild(cmd *cobra.Command, name string) *cobra.Command {
	for _, c := range cmd.Commands() {
		if c.Name() == name {
			return c
		}
		for _, a := range c.Aliases {
			if a == name {
				return c
			}
		}
	}
	return nil
}

// commandHint looks for the word deeper in the tree, so `emlcal add` can point
// at `emlcal account add`. The shortest match wins, which keeps the suggestion
// to the most direct spelling.
func commandHint(root *cobra.Command, name string) string {
	best := ""
	var walk func(c *cobra.Command)
	walk = func(c *cobra.Command) {
		for _, sub := range c.Commands() {
			if sub.Hidden {
				continue
			}
			if matchesCommand(sub, name) {
				path := sub.CommandPath()
				if best == "" || len(path) < len(best) {
					best = path
				}
			}
			walk(sub)
		}
	}
	walk(root)
	return best
}

// matchesCommand is the deliberately simple "close enough" test: the same
// name, one containing the other, or an alias.
func matchesCommand(cmd *cobra.Command, name string) bool {
	n := cmd.Name()
	if n == name || strings.Contains(n, name) || strings.Contains(name, n) {
		return true
	}
	for _, a := range cmd.Aliases {
		if a == name {
			return true
		}
	}
	return false
}
