package cli

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/signal"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"time"

	"github.com/spf13/cobra"

	"github.com/lennert/emlcal/internal/config"
	"github.com/lennert/emlcal/internal/output"
	"github.com/lennert/emlcal/internal/provider"
	"github.com/lennert/emlcal/internal/sync"
)

func init() {
	Register(func(root *cobra.Command, app *App) {
		root.AddCommand(coreSyncCmd(app))
	})
}

// coreSyncRow is one line of `emlcal sync` output: one resource of one account.
type coreSyncRow struct {
	Account  string        `json:"account"  table:"ACCOUNT"`
	Resource string        `json:"resource" table:"RESOURCE"`
	Kind     string        `json:"kind"     table:"KIND"`
	Added    int           `json:"added"    table:"ADDED"`
	Updated  int           `json:"updated"  table:"UPDATED"`
	Removed  int           `json:"removed"  table:"REMOVED"`
	Duration time.Duration `json:"duration" table:"TOOK"`
	Error    string        `json:"error,omitempty" table:"ERROR"`
}

func coreSyncCmd(app *App) *cobra.Command {
	var full, watch, mailOnly, calOnly bool
	cmd := &cobra.Command{
		Use:   "sync",
		Short: "Fetch new mail and calendar changes into the local archive",
		Long: `Runs one sync pass over every account (or those given with --account) and exits.

The first pass is a resumable backfill of the whole account; later passes only
apply the provider's delta. --watch turns the command into the daemon: push
streams where the provider has them, polling where it does not, plus outbox
retries. Running a manual sync while the daemon is up nudges the daemon instead
of fighting it for the lock.`,
		Args: cobra.NoArgs,
		RunE: func(c *cobra.Command, _ []string) error {
			if mailOnly && calOnly {
				return output.Errorf(output.ExitUsage, "--mail-only and --calendar-only are mutually exclusive")
			}
			opts := sync.SyncOptions{Full: full, Mail: mailOnly, Calendar: calOnly}
			if watch {
				return coreWatch(app, opts)
			}
			return coreSyncOnce(app, opts)
		},
	}
	f := cmd.Flags()
	f.BoolVar(&full, "full", false, "re-enumerate everything instead of applying the delta")
	f.BoolVar(&watch, "watch", false, "run as a daemon: push, polling and outbox retries")
	f.BoolVar(&mailOnly, "mail-only", false, "sync mail only")
	f.BoolVar(&calOnly, "calendar-only", false, "sync calendars only")
	return cmd
}

// ---------------------------------------------------------------------------
// one-shot

func coreSyncOnce(app *App, opts sync.SyncOptions) error {
	names, err := app.AccountIDs()
	if err != nil {
		return err
	}
	if len(names) == 0 {
		return output.Errorf(output.ExitUsage, "no accounts configured; run `emlcal account add`")
	}
	if app.IsTTY {
		app.Progress = coreProgressPrinter(app)
	}
	eng, err := app.Engine()
	if err != nil {
		return err
	}
	ctx := app.Context()

	var reports []*sync.Report
	if len(app.Accounts) == 0 {
		reports, err = eng.SyncAll(ctx, opts)
	} else {
		for _, name := range names {
			var r *sync.Report
			r, err = eng.SyncAccount(ctx, name, opts)
			if r != nil {
				reports = append(reports, r)
			}
			if err != nil {
				break
			}
		}
	}
	coreClearProgress(app)
	if err != nil && errors.Is(err, sync.ErrLocked) {
		return coreNudgeDaemon(app)
	}
	if err != nil && len(reports) == 0 {
		return err
	}

	rows, worst := coreSyncRows(reports)
	if perr := app.Printer().Print(rows); perr != nil {
		return perr
	}
	if worst != nil {
		if errors.Is(worst, sync.ErrLocked) {
			return coreNudgeDaemon(app)
		}
		if provider.IsOffline(worst) {
			return output.Errorf(output.ExitOffline, "sync: %v", worst)
		}
		return output.Errorf(output.ExitProvider, "sync: %v", worst)
	}
	return err
}

// coreSyncRows flattens reports into rows and returns the first error found.
func coreSyncRows(reports []*sync.Report) ([]coreSyncRow, error) {
	var rows []coreSyncRow
	var worst error
	for _, r := range reports {
		if r == nil {
			continue
		}
		if r.Err != nil && worst == nil {
			worst = fmt.Errorf("%s: %w", r.Account, r.Err)
		}
		errMsg := ""
		if r.Err != nil {
			errMsg = r.Err.Error()
		}
		for _, res := range []struct {
			name string
			rr   *sync.ResourceReport
		}{{"mail", r.Mail}, {"calendar", r.Calendar}} {
			if res.rr == nil {
				continue
			}
			rows = append(rows, coreSyncRow{
				Account: r.Account, Resource: res.name, Kind: res.rr.Kind,
				Added: res.rr.Added, Updated: res.rr.Updated, Removed: res.rr.Removed,
				Duration: res.rr.Duration.Round(time.Millisecond), Error: errMsg,
			})
			errMsg = "" // report the error once per account
		}
		if errMsg != "" {
			rows = append(rows, coreSyncRow{Account: r.Account, Resource: "-", Error: errMsg})
		}
	}
	return rows, worst
}

// coreProgressPrinter renders a single self-overwriting line on stderr.
func coreProgressPrinter(app *App) func(sync.ProgressEvent) {
	var last int
	return func(ev sync.ProgressEvent) {
		line := fmt.Sprintf("%s %s %s %d", ev.Account, ev.Resource, ev.Phase, ev.Done)
		if ev.Total > 0 {
			line = fmt.Sprintf("%s %s %s %d/%d", ev.Account, ev.Resource, ev.Phase, ev.Done, ev.Total)
		}
		if ev.Message != "" {
			line += " " + ev.Message
		}
		pad := ""
		if n := last - len([]rune(line)); n > 0 {
			pad = strings.Repeat(" ", n)
		}
		last = len([]rune(line))
		fmt.Fprintf(app.Stderr, "\r%s%s", line, pad)
	}
}

func coreClearProgress(app *App) {
	if app.Progress == nil {
		return
	}
	app.Progress = nil
	fmt.Fprintf(app.Stderr, "\r%s\r", strings.Repeat(" ", 72))
}

// coreNudgeDaemon signals a running daemon to sync now. Not finding one is an
// error: the lock was held by something, and silently exiting 0 would lie.
func coreNudgeDaemon(app *App) error {
	pid, err := coreReadPid(app)
	if err != nil || pid <= 0 {
		return output.Errorf(output.ExitGeneric,
			"another sync holds the lock but no daemon pid file was found; retry in a moment")
	}
	if err := syscall.Kill(pid, syscall.SIGUSR1); err != nil {
		return output.Errorf(output.ExitGeneric, "another sync holds the lock (pid %d): %v", pid, err)
	}
	fmt.Fprintf(app.Stdout, "daemon active — nudged (pid %d)\n", pid)
	return nil
}

// ---------------------------------------------------------------------------
// watch

func coreWatch(app *App, opts sync.SyncOptions) error {
	eng, err := app.Engine()
	if err != nil {
		return err
	}
	path, err := corePidPath(app)
	if err != nil {
		return err
	}
	if pid, err := coreReadPid(app); err == nil && pid > 0 && pid != os.Getpid() && coreDaemonRunning(pid) {
		return output.Errorf(output.ExitGeneric, "a daemon is already running (pid %d)", pid)
	}
	if err := os.WriteFile(path, []byte(strconv.Itoa(os.Getpid())+"\n"), 0o600); err != nil {
		return fmt.Errorf("write pid file %s: %w", path, err)
	}
	defer os.Remove(path)

	ctx, stop := signal.NotifyContext(app.Context(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()

	nudges := make(chan os.Signal, 4)
	signal.Notify(nudges, syscall.SIGUSR1)
	defer signal.Stop(nudges)
	go func() {
		for {
			select {
			case <-ctx.Done():
				return
			case <-nudges:
				eng.Nudge()
			}
		}
	}()

	fmt.Fprintf(app.Stderr, "emlcal watching (pid %d); SIGUSR1 forces a pass\n", os.Getpid())
	if err := eng.Watch(ctx); err != nil && !errors.Is(err, context.Canceled) {
		return err
	}
	return nil
}

// ---------------------------------------------------------------------------
// pid file helpers, shared with `status` and `doctor`

// corePidPath is <state dir>/emlcal.pid.
func corePidPath(app *App) (string, error) {
	cfg, err := app.Config()
	if err != nil {
		return "", err
	}
	if err := cfg.EnsureDirs(); err != nil {
		return "", err
	}
	return corePidPathOf(cfg), nil
}

func corePidPathOf(cfg *config.Config) string {
	_, _, _, lock, _ := cfg.Paths()
	return filepath.Join(filepath.Dir(lock), "emlcal.pid")
}

// coreReadPid returns the pid recorded by a `sync --watch` process.
func coreReadPid(app *App) (int, error) {
	cfg, err := app.Config()
	if err != nil {
		return 0, err
	}
	b, err := os.ReadFile(corePidPathOf(cfg))
	if err != nil {
		return 0, err
	}
	pid, err := strconv.Atoi(strings.TrimSpace(string(b)))
	if err != nil {
		return 0, fmt.Errorf("pid file %s: %w", corePidPathOf(cfg), err)
	}
	return pid, nil
}

// coreDaemonRunning reports whether that pid is alive (signal 0 probe).
// EPERM means the process exists but belongs to someone else, which still
// counts as running.
func coreDaemonRunning(pid int) bool {
	if pid <= 0 {
		return false
	}
	err := syscall.Kill(pid, 0)
	return err == nil || errors.Is(err, syscall.EPERM)
}
