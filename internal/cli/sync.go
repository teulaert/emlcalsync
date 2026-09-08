package cli

import (
	"bytes"
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

	"github.com/teulaert/emlcalsync/internal/config"
	"github.com/teulaert/emlcalsync/internal/output"
	"github.com/teulaert/emlcalsync/internal/provider"
	"github.com/teulaert/emlcalsync/internal/sync"
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
	var full, watch, mailOnly, calOnly, quiet bool
	waitOffline := sync.DefaultWaitOffline
	cmd := &cobra.Command{
		Use:   "sync",
		Short: "Fetch new mail and calendar changes into the local archive",
		Long: `Runs one sync pass over every account (or those given with --account) and exits.

The first pass is a resumable backfill of the whole account; later passes only
apply the provider's delta. --watch turns the command into the daemon: push
streams where the provider has them, polling where it does not, plus outbox
retries. Running a manual sync while the daemon is up nudges the daemon instead
of fighting it for the lock.

A backfill of a large mailbox takes hours, so losing the network is treated as
a pause rather than a failure: the pass waits up to --wait-offline for it to
come back and then resumes from where it stopped. --wait-offline 0 restores the
old behaviour of exiting 4 on the first transport error.`,
		Args: cobra.NoArgs,
		RunE: func(c *cobra.Command, _ []string) error {
			if mailOnly && calOnly {
				return output.Errorf(output.ExitUsage, "--mail-only and --calendar-only are mutually exclusive")
			}
			if waitOffline < 0 {
				return output.Errorf(output.ExitUsage, "--wait-offline must not be negative")
			}
			opts := sync.SyncOptions{Full: full, Mail: mailOnly, Calendar: calOnly, WaitOffline: waitOffline}
			if watch {
				return coreWatch(app, opts)
			}
			return coreSyncOnce(app, opts, quiet)
		},
	}
	f := cmd.Flags()
	f.BoolVar(&full, "full", false, "re-enumerate everything instead of applying the delta")
	f.BoolVar(&watch, "watch", false, "run as a daemon: push, polling and outbox retries")
	f.BoolVar(&mailOnly, "mail-only", false, "sync mail only")
	f.BoolVar(&calOnly, "calendar-only", false, "sync calendars only")
	f.DurationVar(&waitOffline, "wait-offline", sync.DefaultWaitOffline,
		"how long to wait out a network outage before giving up (0 = fail immediately; --watch always waits)")
	f.BoolVar(&quiet, "quiet", false, "do not print the live progress line")
	return cmd
}

// ---------------------------------------------------------------------------
// one-shot

func coreSyncOnce(app *App, opts sync.SyncOptions, quiet bool) error {
	names, err := app.AccountIDs()
	if err != nil {
		return err
	}
	if len(names) == 0 {
		return output.Errorf(output.ExitUsage, "no accounts configured; run `emlcal account add`")
	}
	if app.IsTTY && !quiet {
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

// coreProgressPrinter renders a single self-overwriting line on stderr:
//
//	work mail backfill 1 234/52 000 · 48/s · ~18m
//
// The engine composes the counts, rate and ETA into Message; only an event
// without one (an older phase, or the outbox) falls back to the raw numbers.
func coreProgressPrinter(app *App) func(sync.ProgressEvent) {
	var last int
	return func(ev sync.ProgressEvent) {
		line := strings.TrimSpace(fmt.Sprintf("%s %s %s", ev.Account, ev.Resource, ev.Phase))
		switch {
		case ev.Message != "":
			line += " " + ev.Message
		case ev.Total > 0:
			line += fmt.Sprintf(" %d/%d", ev.Done, ev.Total)
		default:
			line += fmt.Sprintf(" %d", ev.Done)
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
	fmt.Fprintf(app.Stderr, "\r%s\r", strings.Repeat(" ", 100))
}

// coreNudgeDaemon signals a running daemon to sync now. Not finding one is an
// error: the lock was held by something, and silently exiting 0 would lie.
func coreNudgeDaemon(app *App) error {
	rec, err := coreReadPid(app)
	if err != nil || rec.PID <= 0 {
		return output.Errorf(output.ExitGeneric,
			"another sync holds the lock but no daemon pid file was found; retry in a moment")
	}
	// Never signal a pid the record no longer vouches for: the number may
	// have been handed to some unrelated process since the daemon died.
	if !rec.alive() {
		return output.Errorf(output.ExitGeneric,
			"another sync holds the lock but the daemon pid file is stale (pid %d is a different process); retry in a moment", rec.PID)
	}
	if err := syscall.Kill(rec.PID, syscall.SIGUSR1); err != nil {
		return output.Errorf(output.ExitGeneric, "another sync holds the lock (pid %d): %v", rec.PID, err)
	}
	fmt.Fprintf(app.Stdout, "daemon active — nudged (pid %d)\n", rec.PID)
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
	if rec, err := coreReadPid(app); err == nil && rec.PID != os.Getpid() && rec.alive() {
		return output.Errorf(output.ExitGeneric, "a daemon is already running (pid %d)", rec.PID)
	}
	if err := os.WriteFile(path, coreProcRecord(os.Getpid()).bytes(), 0o600); err != nil {
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

// coreReadPid returns the record left by a `sync --watch` process.
func coreReadPid(app *App) (corePidRecord, error) {
	cfg, err := app.Config()
	if err != nil {
		return corePidRecord{}, err
	}
	path := corePidPathOf(cfg)
	b, err := os.ReadFile(path)
	if err != nil {
		return corePidRecord{}, err
	}
	return coreParsePid(b, path)
}

// corePidRecord is what the daemon leaves behind: its pid, and enough of the
// kernel's own bookkeeping to tell that process apart from whatever inherits
// the number later. A pid alone proves nothing across a reboot -- the daemon's
// old number came back as the keyring daemon, and a bare liveness probe read
// that as a daemon still running and locked `sync --watch` out for good.
type corePidRecord struct {
	PID   int
	Boot  string // kernel boot id: a different boot is a different pid space
	Start string // start time in jiffies since boot, /proc/<pid>/stat field 22
}

// identified says whether the record carries more than a bare pid. Pid files
// written before the daemon recorded an identity do not.
func (r corePidRecord) identified() bool { return r.Boot != "" && r.Start != "" }

// alive reports whether the process the record describes is still the one
// holding that pid. The signal-0 probe only proves the number is taken, so
// when both records carry an identity they have to agree as well. EPERM means
// the process exists but belongs to someone else, which still counts as taken.
func (r corePidRecord) alive() bool {
	if r.PID <= 0 {
		return false
	}
	if err := syscall.Kill(r.PID, 0); err != nil && !errors.Is(err, syscall.EPERM) {
		return false
	}
	live := coreProcRecord(r.PID)
	if !r.identified() || !live.identified() {
		return true // nothing to compare; the probe is all there is
	}
	return live.Boot == r.Boot && live.Start == r.Start
}

// coreProcRecord reads the identity the kernel keeps for a live pid. It comes
// back bare when /proc does not answer, which callers read as "cannot tell".
func coreProcRecord(pid int) corePidRecord {
	boot, err := os.ReadFile("/proc/sys/kernel/random/boot_id")
	if err != nil {
		return corePidRecord{PID: pid}
	}
	start, err := coreProcStart(pid)
	if err != nil {
		return corePidRecord{PID: pid}
	}
	return corePidRecord{PID: pid, Boot: strings.TrimSpace(string(boot)), Start: start}
}

// coreProcStart is field 22 of /proc/<pid>/stat, the jiffies since boot at
// which the process started. Paired with the boot id it pins a pid to one
// process: a recycled number always starts later than the one it replaced.
func coreProcStart(pid int) (string, error) {
	b, err := os.ReadFile(fmt.Sprintf("/proc/%d/stat", pid))
	if err != nil {
		return "", err
	}
	// The comm field is parenthesised and may itself hold spaces and
	// parentheses, so the fixed columns only start after its closing brace.
	i := bytes.LastIndexByte(b, ')')
	if i < 0 {
		return "", fmt.Errorf("proc stat %d: no comm field", pid)
	}
	fields := strings.Fields(string(b[i+1:]))
	const startCol = 22 - 3 // fields[0] is state, column 3 of the stat line
	if len(fields) <= startCol {
		return "", fmt.Errorf("proc stat %d: %d columns after comm", pid, len(fields))
	}
	return fields[startCol], nil
}

// bytes renders the record for the pid file: the pid on its own first line, so
// anything that only wants the number still reads it, then the identity.
func (r corePidRecord) bytes() []byte {
	var b strings.Builder
	fmt.Fprintf(&b, "%d\n", r.PID)
	if r.Boot != "" {
		fmt.Fprintf(&b, "boot %s\n", r.Boot)
	}
	if r.Start != "" {
		fmt.Fprintf(&b, "start %s\n", r.Start)
	}
	return []byte(b.String())
}

// coreParsePid reads a pid file. A file holding nothing but a pid is one an
// older build wrote, and parses into a record with no identity.
func coreParsePid(b []byte, path string) (corePidRecord, error) {
	lines := strings.Split(strings.TrimSpace(string(b)), "\n")
	pid, err := strconv.Atoi(strings.TrimSpace(lines[0]))
	if err != nil {
		return corePidRecord{}, fmt.Errorf("pid file %s: %w", path, err)
	}
	r := corePidRecord{PID: pid}
	for _, line := range lines[1:] {
		key, val, ok := strings.Cut(strings.TrimSpace(line), " ")
		if !ok {
			continue
		}
		switch key {
		case "boot":
			r.Boot = val
		case "start":
			r.Start = val
		}
	}
	return r, nil
}
