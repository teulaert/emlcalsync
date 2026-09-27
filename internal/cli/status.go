package cli

import (
	"context"
	"fmt"
	"os"

	"github.com/spf13/cobra"

	"github.com/teulaert/emlcalsync/internal/config"
	"github.com/teulaert/emlcalsync/internal/output"
	"github.com/teulaert/emlcalsync/internal/store"
)

func init() {
	Register(func(root *cobra.Command, app *App) {
		root.AddCommand(coreStatusCmd(app))
	})
}

// coreStatusRow is the per-account line of `emlcal status`.
type coreStatusRow struct {
	Name            string `json:"account"          table:"ACCOUNT"`
	Vendor          string `json:"vendor"           table:"VENDOR"`
	Email           string `json:"email"            table:"EMAIL"`
	Messages        int    `json:"messages"         table:"MESSAGES"`
	Unread          int    `json:"unread"           table:"UNREAD"`
	Deleted         int    `json:"deleted"          table:"DELETED"`
	BlobsIncomplete int    `json:"blobs_incomplete" table:"NO RAW"`
	// The two halves of the account are checked, and change, on their own;
	// see coreSyncTimes for what each field means and what it must not be
	// read as. LastChange is the later of the two, for the table.
	// In the table the times are relative ("3m", "2h", "yesterday"), the way
	// the mail list shows dates: freshness is what the eye is looking for.
	Mail               coreSyncTimes `json:"mail_sync"`
	Calendar           coreSyncTimes `json:"calendar_sync"`
	MailCheckedRel     string        `json:"-" table:"MAIL CHECKED"`
	CalendarCheckedRel string        `json:"-" table:"CAL CHECKED"`
	LastChange         output.Time   `json:"last_change_at"`
	LastChangeUTC      int64         `json:"last_change_at_utc"`
	LastChangeKind     string        `json:"last_change_kind,omitempty"`
	LastChangeRel      string        `json:"-" table:"LAST CHANGE"`
	SyncError          string        `json:"-" table:"ERROR,max=40"`
	Backfill           string        `json:"backfill,omitempty" table:"BACKFILL"`
	BackfillPercent    float64       `json:"backfill_percent,omitempty"`
	OutboxPending      int           `json:"outbox_pending"   table:"OUTBOX"`
	// MailBackend/CalendarBackend name each resource's backend, or "-" when
	// the account has no block for it; Disabled is the same fact spelled for
	// the table.
	MailBackend     string `json:"mail"`
	CalendarBackend string `json:"calendar"`
	Disabled        string `json:"disabled,omitempty" table:"DISABLED"`
}

// coreSyncTimes is the freshness of one resource -- mail or calendar -- of one
// account, kept apart because they are checked, and change, on their own.
//
// CheckedAt is when the resource was last looked at and found in order: the
// finish of the last pass that completed without error, whether or not it
// found anything to apply. It is the field to read for "is the archive
// current". ChangedAt is the finish of the last pass that applied something,
// which is what `emlcal status` used to call last_sync: on a quiet mailbox it
// falls hours behind CheckedAt without anything being wrong, and it must not
// be read as a pulse.
//
// FailedAt and Error are set only when the most recent completed pass failed,
// and then CheckedAt is still the last good one, so "failing since" and "last
// known good" can both be read off the row. A pass still running, or cut
// short by the process shutting down, has not completed and moves nothing.
type coreSyncTimes struct {
	CheckedAt    output.Time `json:"checked_at"`
	CheckedAtUTC int64       `json:"checked_at_utc"`
	ChangedAt    output.Time `json:"changed_at"`
	ChangedAtUTC int64       `json:"changed_at_utc"`
	ChangeKind   string      `json:"change_kind,omitempty"`
	FailedAt     output.Time `json:"failed_at"`
	Error        string      `json:"error,omitempty"`
}

// coreSyncTimesOf reads one resource's times off the store: the check row for
// when it was looked at, the sync log for when it last changed.
func coreSyncTimesOf(ctx context.Context, st *store.Store, account, resource string) coreSyncTimes {
	var t coreSyncTimes
	if c, err := st.GetSyncCheck(ctx, account, resource); err == nil && c != nil {
		t.CheckedAt = output.T(c.CheckedAt)
		t.CheckedAtUTC = t.CheckedAt.Unix()
		if c.Failed() {
			t.FailedAt = output.T(c.AttemptedAt)
			t.Error = c.Error
		}
	}
	if e, err := st.LastSyncChange(ctx, account, resource); err == nil && e != nil {
		when := e.Finished
		if when.IsZero() {
			when = e.Started
		}
		t.ChangedAt = output.T(when)
		t.ChangedAtUTC = t.ChangedAt.Unix()
		t.ChangeKind = e.Kind
	}
	return t
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
		Short: "Per-account counts, when each resource was last checked and last changed, backfill progress and daemon state",
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
	now := app.Now()
	out := coreStatusOut{Accounts: []coreStatusRow{}, DB: cfg.DBPath()}

	st := coreOpenStoreIfExists(app)
	for _, name := range names {
		acct, err := app.ResolveAccount(name)
		if err != nil {
			return err
		}
		row := coreStatusRow{
			Name: name, Vendor: string(acct.Vendor()), Email: acct.Email,
			MailBackend:     coreBackendLabel(acct.Mail != nil, func() string { return string(acct.Mail.Backend) }),
			CalendarBackend: coreBackendLabel(acct.Calendar != nil, func() string { return string(acct.Calendar.Backend) }),
			Disabled:        coreDisabledLabel(acct),
		}
		if st != nil {
			if s, err := st.AccountStats(ctx, name); err == nil {
				row.Messages = s.Messages
				row.Unread = s.Unread
				row.Deleted = s.Deleted
				row.BlobsIncomplete = s.BlobsIncomplete
				row.OutboxPending = s.OutboxPending
			}
			row.Mail = coreSyncTimesOf(ctx, st, name, "mail")
			row.Calendar = coreSyncTimesOf(ctx, st, name, "calendar")
			row.LastChange, row.LastChangeKind = row.Mail.ChangedAt, row.Mail.ChangeKind
			if row.Calendar.ChangedAt.After(row.LastChange.Time) {
				row.LastChange, row.LastChangeKind = row.Calendar.ChangedAt, row.Calendar.ChangeKind
			}
			row.LastChangeUTC = row.LastChange.Unix()
			row.MailCheckedRel = output.RelTime(row.Mail.CheckedAt.Time, now)
			row.CalendarCheckedRel = output.RelTime(row.Calendar.CheckedAt.Time, now)
			row.LastChangeRel = output.RelTime(row.LastChange.Time, now)
			row.SyncError = coreSyncErrorCell(row.Mail.Error, row.Calendar.Error)
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

// coreSyncErrorCell is the table's one ERROR column for two resources: the
// failing one is named when there is only one, both are when both are.
func coreSyncErrorCell(mail, cal string) string {
	switch {
	case mail != "" && cal != "":
		return "mail: " + mail + "; calendar: " + cal
	case mail != "":
		return "mail: " + mail
	case cal != "":
		return "calendar: " + cal
	}
	return ""
}

// coreDisabledLabel spells out a switched-off half of an account. Both off is
// not a configuration Validate allows, so it can only come from a Config built
// in code; it is still worth showing rather than silently picking one.
func coreDisabledLabel(acct *config.Account) string {
	switch {
	case !acct.SyncsMail() && !acct.SyncsCalendar():
		return "mail off, calendar off"
	case !acct.SyncsMail():
		return "mail off"
	case !acct.SyncsCalendar():
		return "calendar off"
	}
	return ""
}

// coreBackendLabel names a configured backend, or "-" when the block is absent.
func coreBackendLabel(present bool, name func() string) string {
	if !present {
		return "-"
	}
	return name()
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
	rec, err := coreReadPid(app)
	if err != nil {
		return info
	}
	info.PID = rec.PID
	info.Running = rec.alive()
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
