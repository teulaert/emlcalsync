package cli

import (
	"fmt"
	"os"

	"github.com/spf13/cobra"

	"github.com/teulaert/emlcalsync/internal/output"
)

func init() {
	Register(func(root *cobra.Command, app *App) {
		root.AddCommand(coreStatusCmd(app))
	})
}

// coreStatusRow is the per-account line of `emlcal status`.
type coreStatusRow struct {
	Name            string      `json:"account"          table:"ACCOUNT"`
	Provider        string      `json:"provider"         table:"PROVIDER"`
	Email           string      `json:"email"            table:"EMAIL"`
	Messages        int         `json:"messages"         table:"MESSAGES"`
	Unread          int         `json:"unread"           table:"UNREAD"`
	Deleted         int         `json:"deleted"          table:"DELETED"`
	BlobsIncomplete int         `json:"blobs_incomplete" table:"NO RAW"`
	LastSync        output.Time `json:"last_sync"        table:"LAST SYNC"`
	LastSyncUTC     int64       `json:"last_sync_utc"`
	LastSyncKind    string      `json:"last_sync_kind,omitempty" table:"KIND"`
	LastSyncError   string      `json:"last_sync_error,omitempty" table:"-"`
	Backfill        string      `json:"backfill,omitempty" table:"BACKFILL"`
	BackfillPercent float64     `json:"backfill_percent,omitempty"`
	OutboxPending   int         `json:"outbox_pending"   table:"OUTBOX"`
}

type coreDaemonInfo struct {
	Running bool   `json:"running"`
	PID     int    `json:"pid,omitempty"`
	PidFile string `json:"pid_file,omitempty"`
}

type coreBlobInfo struct {
	Count int    `json:"count"`
	Bytes int64  `json:"bytes"`
	Dir   string `json:"dir"`
}

// coreStatusOut is the whole of `emlcal status` in JSON: the account rows plus
// the process-wide summary an agent needs to judge whether data is fresh.
type coreStatusOut struct {
	Accounts []coreStatusRow `json:"accounts"`
	Daemon   coreDaemonInfo  `json:"daemon"`
	Blobs    coreBlobInfo    `json:"blobs"`
	DB       string          `json:"db"`
}

func coreStatusCmd(app *App) *cobra.Command {
	return &cobra.Command{
		Use:   "status",
		Short: "Per-account counts, last sync, backfill progress and daemon state",
		Args:  cobra.NoArgs,
		RunE:  func(c *cobra.Command, _ []string) error { return coreStatus(app) },
	}
}

func coreStatus(app *App) error {
	cfg, err := app.Config()
	if err != nil {
		return err
	}
	names, err := app.AccountIDs()
	if err != nil {
		return err
	}
	ctx := app.Context()
	out := coreStatusOut{Accounts: []coreStatusRow{}, DB: cfg.DBPath()}

	st := coreOpenStoreIfExists(app)
	for _, name := range names {
		acct, err := app.ResolveAccount(name)
		if err != nil {
			return err
		}
		row := coreStatusRow{Name: name, Provider: string(acct.Provider), Email: acct.Email}
		if st != nil {
			if s, err := st.AccountStats(ctx, name); err == nil {
				row.Messages = s.Messages
				row.Unread = s.Unread
				row.Deleted = s.Deleted
				row.BlobsIncomplete = s.BlobsIncomplete
				row.OutboxPending = s.OutboxPending
			}
			if entries, err := st.RecentSyncLog(ctx, name, 1); err == nil && len(entries) > 0 {
				e := entries[0]
				when := e.Finished
				if when.IsZero() {
					when = e.Started
				}
				row.LastSync = output.T(when)
				row.LastSyncUTC = row.LastSync.Unix()
				row.LastSyncKind = e.Kind
				row.LastSyncError = e.Error
			}
			if b, err := st.GetBackfill(ctx, name, "mail"); err == nil && b != nil && !b.Finished() {
				row.BackfillPercent = corePercent(b.Done, b.TotalHint)
				if b.TotalHint > 0 {
					row.Backfill = fmt.Sprintf("%.0f%% (%d/%d)", row.BackfillPercent, b.Done, b.TotalHint)
				} else {
					row.Backfill = fmt.Sprintf("%d messages", b.Done)
				}
			}
		}
		out.Accounts = append(out.Accounts, row)
	}

	out.Daemon = coreDaemonState(app)
	out.Blobs = coreBlobState(app)

	p := app.Printer()
	if p.Format == output.JSON || p.Format == output.Auto {
		return p.Print(out)
	}
	if err := p.Print(out.Accounts); err != nil {
		return err
	}
	daemon := "daemon: not running"
	if out.Daemon.Running {
		daemon = fmt.Sprintf("daemon: running (pid %d)", out.Daemon.PID)
	}
	fmt.Fprintf(app.Stdout, "%s\n", daemon)
	fmt.Fprintf(app.Stdout, "blobs: %d (%s) in %s; index %s\n",
		out.Blobs.Count, coreHumanBytes(out.Blobs.Bytes), out.Blobs.Dir, out.DB)
	return nil
}

func corePercent(done, total int) float64 {
	if total <= 0 {
		return 0
	}
	pct := float64(done) / float64(total) * 100
	if pct > 100 {
		return 100
	}
	return pct
}

func coreDaemonState(app *App) coreDaemonInfo {
	cfg, err := app.Config()
	if err != nil {
		return coreDaemonInfo{}
	}
	info := coreDaemonInfo{PidFile: corePidPathOf(cfg)}
	pid, err := coreReadPid(app)
	if err != nil {
		return info
	}
	info.PID = pid
	info.Running = coreDaemonRunning(pid)
	if !info.Running {
		info.PID = 0
	}
	return info
}

func coreBlobState(app *App) coreBlobInfo {
	cfg, err := app.Config()
	if err != nil {
		return coreBlobInfo{}
	}
	info := coreBlobInfo{Dir: cfg.BlobsDir()}
	if _, err := os.Stat(cfg.BlobsDir()); err != nil {
		return info
	}
	bl, err := app.Blobs()
	if err != nil {
		return info
	}
	if n, bytes, err := bl.Stats(); err == nil {
		info.Count, info.Bytes = n, bytes
	}
	return info
}

// coreHumanBytes renders a byte count for the table footer.
func coreHumanBytes(n int64) string {
	const unit = 1024
	if n < unit {
		return fmt.Sprintf("%d B", n)
	}
	div, exp := int64(unit), 0
	for m := n / unit; m >= unit && exp < 4; m /= unit {
		div *= unit
		exp++
	}
	return fmt.Sprintf("%.1f %ciB", float64(n)/float64(div), "KMGTP"[exp])
}
