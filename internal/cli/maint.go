package cli

import (
	"bufio"
	"bytes"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"github.com/spf13/cobra"

	"github.com/teulaert/emlcalsync/internal/model"
	"github.com/teulaert/emlcalsync/internal/output"
	"github.com/teulaert/emlcalsync/internal/store"
	"github.com/teulaert/emlcalsync/internal/sync"
)

func init() {
	Register(func(root *cobra.Command, app *App) {
		root.AddCommand(coreOutboxCmd(app))
		root.AddCommand(coreReindexCmd(app))
		root.AddCommand(coreGCCmd(app))
		root.AddCommand(coreExportCmd(app))
	})
}

// ---------------------------------------------------------------------------
// outbox

type coreOutboxRow struct {
	ID        int64       `json:"id"      table:"ID"`
	Account   string      `json:"account" table:"ACCOUNT"`
	Kind      string      `json:"kind"    table:"KIND"`
	Detail    string      `json:"detail,omitempty" table:"DETAIL"`
	Created   output.Time `json:"created_at" table:"CREATED"`
	CreatedU  int64       `json:"created_at_utc"`
	Attempts  int         `json:"attempts" table:"TRIES"`
	State     string      `json:"state"    table:"STATE"`
	LastError string      `json:"last_error,omitempty" table:"ERROR"`
}

func coreOutboxCmd(app *App) *cobra.Command {
	cmd := &cobra.Command{
		Use:   "outbox",
		Short: "Inspect and drain writes queued while offline",
		Args:  cobra.NoArgs,
		RunE:  func(c *cobra.Command, _ []string) error { return c.Help() },
	}
	cmd.AddCommand(coreOutboxListCmd(app), coreOutboxRetryCmd(app), coreOutboxDropCmd(app))
	return cmd
}

func coreOutboxListCmd(app *App) *cobra.Command {
	var all bool
	cmd := &cobra.Command{
		Use:   "list",
		Short: "List queued writes",
		Args:  cobra.NoArgs,
		RunE: func(c *cobra.Command, _ []string) error {
			st, err := app.Store()
			if err != nil {
				return err
			}
			items, err := st.ListOutbox(app.Context(), !all)
			if err != nil {
				return err
			}
			only := map[string]bool{}
			for _, n := range app.Accounts {
				only[n] = true
			}
			rows := make([]coreOutboxRow, 0, len(items))
			for _, it := range items {
				if len(only) > 0 && !only[it.AccountID] {
					continue
				}
				rows = append(rows, coreOutboxRow{
					ID: it.ID, Account: it.AccountID, Kind: it.Kind,
					Detail:    coreOutboxDetail(it),
					Created:   output.T(it.CreatedAt),
					CreatedU:  it.CreatedAt.Unix(),
					Attempts:  it.Attempts,
					State:     coreOutboxState(it),
					LastError: it.LastError,
				})
			}
			return app.Printer().Print(rows)
		},
	}
	cmd.Flags().BoolVar(&all, "all", false, "include writes that already went through")
	return cmd
}

// coreOutboxMaxAttempts mirrors the engine's give-up threshold, so `outbox
// list` can tell a row that is still being retried from one that needs a hand.
const coreOutboxMaxAttempts = 10

func coreOutboxState(it store.OutboxItem) string {
	switch {
	case it.DoneAt != nil:
		return "done"
	case it.Attempts >= coreOutboxMaxAttempts:
		return "gave-up"
	default:
		return "pending"
	}
}

// coreOutboxDetail summarises the queued operation without dumping the payload.
func coreOutboxDetail(it store.OutboxItem) string {
	var op sync.Op
	if err := json.Unmarshal(it.Payload, &op); err != nil {
		return ""
	}
	var parts []string
	if n := len(op.IDs); n == 1 {
		parts = append(parts, op.IDs[0])
	} else if n > 1 {
		parts = append(parts, fmt.Sprintf("%d messages", n))
	}
	if len(op.AddMailboxes) > 0 {
		parts = append(parts, "+"+strings.Join(op.AddMailboxes, ","))
	}
	if len(op.RemoveMailboxes) > 0 {
		parts = append(parts, "-"+strings.Join(op.RemoveMailboxes, ","))
	}
	if n := len(op.Raw); n > 0 {
		parts = append(parts, fmt.Sprintf("%d bytes", n))
	}
	if op.Event != nil {
		parts = append(parts, op.Event.Title)
	}
	if op.Response != "" {
		parts = append(parts, string(op.Response))
	}
	return strings.Join(parts, " ")
}

func coreOutboxRetryCmd(app *App) *cobra.Command {
	return &cobra.Command{
		Use:   "retry",
		Short: "Try every pending write again now",
		Args:  cobra.NoArgs,
		RunE: func(c *cobra.Command, _ []string) error {
			eng, err := app.Engine()
			if err != nil {
				return err
			}
			names, err := coreTargets(app)
			if err != nil {
				return err
			}
			type row struct {
				Account   string        `json:"account"   table:"ACCOUNT"`
				Pending   int           `json:"pending"   table:"PENDING"`
				Attempted int           `json:"attempted" table:"TRIED"`
				Done      int           `json:"done"      table:"DONE"`
				Failed    int           `json:"failed"    table:"FAILED"`
				Skipped   int           `json:"skipped"   table:"SKIPPED"`
				Duration  time.Duration `json:"duration"  table:"TOOK"`
			}
			ctx := app.Context()
			var rows []row
			var firstErr error
			for _, name := range names {
				rep, err := eng.RetryOutbox(ctx, name)
				if err != nil && firstErr == nil {
					firstErr = err
				}
				if rep == nil {
					continue
				}
				label := name
				if label == "" {
					label = "all"
				}
				rows = append(rows, row{label, rep.Pending, rep.Attempted, rep.Done, rep.Failed, rep.Skipped,
					rep.Duration.Round(time.Millisecond)})
			}
			if err := app.Printer().Print(rows); err != nil {
				return err
			}
			return firstErr
		},
	}
}

// coreTargets returns the accounts a maintenance command should act on: the
// single empty string ("all accounts") when --account was not given.
func coreTargets(app *App) ([]string, error) {
	if len(app.Accounts) == 0 {
		if _, err := app.Config(); err != nil {
			return nil, err
		}
		return []string{""}, nil
	}
	return app.AccountIDs()
}

func coreOutboxDropCmd(app *App) *cobra.Command {
	return &cobra.Command{
		Use:   "drop <id>",
		Short: "Forget a queued write without sending it",
		Args:  cobra.ExactArgs(1),
		RunE: func(c *cobra.Command, args []string) error {
			id, err := strconv.ParseInt(args[0], 10, 64)
			if err != nil {
				return output.Errorf(output.ExitUsage, "outbox id %q is not a number", args[0])
			}
			st, err := app.Store()
			if err != nil {
				return err
			}
			ctx := app.Context()
			it, err := st.GetOutbox(ctx, id)
			if err != nil {
				return err
			}
			if err := st.DropOutbox(ctx, id); err != nil {
				return err
			}
			return app.Printer().Print(struct {
				ID      int64  `json:"id"      table:"ID"`
				Account string `json:"account" table:"ACCOUNT"`
				Kind    string `json:"kind"    table:"KIND"`
				Dropped bool   `json:"dropped" table:"DROPPED"`
			}{id, it.AccountID, it.Kind, true})
		},
	}
}

// ---------------------------------------------------------------------------
// reindex / gc

func coreReindexCmd(app *App) *cobra.Command {
	var rethread bool
	cmd := &cobra.Command{
		Use:   "reindex",
		Short: "Rebuild the SQLite index from the blob archive",
		Long: `Re-parses every archived raw message and rewrites its index row, search text
and attachment list. The blobs are the canonical archive, so this is always
safe: nothing is fetched and nothing is deleted.

--rethread additionally discards the account's thread ids and works them out
again from the Message-ID graph. That is only useful for a backend that has no
server-side threading of its own (IMAP); on a Gmail or Fastmail account it
replaces the server's threading with a weaker guess, and a plain reindex will
not bring it back. Thread ids change, so anything holding an <account>:t:<id>
has to look it up again.`,
		Args: cobra.NoArgs,
		RunE: func(c *cobra.Command, _ []string) error {
			eng, err := app.Engine()
			if err != nil {
				return err
			}
			names, err := coreTargets(app)
			if err != nil {
				return err
			}
			type row struct {
				Accounts     string        `json:"-"            table:"ACCOUNTS"`
				AccountList  []string      `json:"accounts"     table:"-"`
				Scanned      int           `json:"scanned"      table:"SCANNED"`
				Reindexed    int           `json:"reindexed"    table:"REINDEXED"`
				MissingBlobs int           `json:"missing_blobs" table:"MISSING"`
				Skipped      int           `json:"skipped"      table:"SKIPPED"`
				Duration     time.Duration `json:"duration"     table:"TOOK"`
			}
			ctx := app.Context()
			var rows []row
			for _, name := range names {
				rep, err := eng.ReindexWith(ctx, name, sync.ReindexOptions{Rethread: rethread})
				if err != nil {
					return err
				}
				rows = append(rows, row{
					Accounts: strings.Join(rep.Accounts, ","), AccountList: rep.Accounts,
					Scanned: rep.Scanned, Reindexed: rep.Reindexed, MissingBlobs: rep.MissingBlobs,
					Skipped: rep.Skipped, Duration: rep.Duration.Round(time.Millisecond),
				})
			}
			return app.Printer().Print(rows)
		},
	}
	cmd.Flags().BoolVar(&rethread, "rethread", false,
		"also recompute threads from the Message-ID graph (IMAP accounts only)")
	return cmd
}

func coreGCCmd(app *App) *cobra.Command {
	var purge bool
	cmd := &cobra.Command{
		Use:   "gc",
		Short: "Remove blobs no index row references",
		Long: `Walks the blob archive and deletes files nothing points at any more.

By default messages deleted on the server keep their blobs — the archive
outlives the mailbox. --purge-deleted drops those rows and their blobs too,
which is the one operation in emlcal that loses mail permanently.`,
		Args: cobra.NoArgs,
		RunE: func(c *cobra.Command, _ []string) error {
			eng, err := app.Engine()
			if err != nil {
				return err
			}
			rep, err := eng.GC(app.Context(), purge)
			if err != nil {
				return err
			}
			return app.Printer().Print(struct {
				Blobs          int           `json:"blobs"           table:"WALKED"`
				Deleted        int           `json:"deleted"         table:"DELETED"`
				Skipped        int           `json:"skipped"         table:"SKIPPED"`
				FreedBytes     int64         `json:"freed_bytes"     table:"-"`
				Freed          string        `json:"freed"           table:"FREED"`
				PurgedMessages int           `json:"purged_messages" table:"PURGED"`
				Duration       time.Duration `json:"duration"        table:"TOOK"`
			}{rep.Blobs, rep.Deleted, rep.Skipped, rep.FreedBytes, coreHumanBytes(rep.FreedBytes),
				rep.PurgedMessages, rep.Duration.Round(time.Millisecond)})
		},
	}
	cmd.Flags().BoolVar(&purge, "purge-deleted", false, "also drop messages deleted on the server (irreversible)")
	return cmd
}

// ---------------------------------------------------------------------------
// export

// coreExportPage is how many index rows are read per query.
const coreExportPage = 500

type coreExportSummary struct {
	Format   string `json:"format"   table:"FORMAT"`
	Target   string `json:"target"   table:"TARGET"`
	Exported int    `json:"exported" table:"EXPORTED"`
	Skipped  int    `json:"skipped"  table:"SKIPPED"`
}

func coreExportCmd(app *App) *cobra.Command {
	var mbox, maildir string
	cmd := &cobra.Command{
		Use:   "export",
		Short: "Write the archive out as an mbox file or a Maildir",
		Long: `Exports every archived message of the selected accounts.

Messages whose raw form was never stored in full (raw_max_size was in effect,
or the fetch is still pending) are counted as skipped rather than written half.`,
		Args: cobra.NoArgs,
		RunE: func(c *cobra.Command, _ []string) error {
			switch {
			case mbox == "" && maildir == "":
				return output.Errorf(output.ExitUsage, "pass --mbox FILE or --maildir DIR")
			case mbox != "" && maildir != "":
				return output.Errorf(output.ExitUsage, "--mbox and --maildir are mutually exclusive")
			}
			names, err := app.AccountIDs()
			if err != nil {
				return err
			}
			if mbox != "" {
				return coreExportMbox(app, names, mbox)
			}
			return coreExportMaildir(app, names, maildir)
		},
	}
	cmd.Flags().StringVar(&mbox, "mbox", "", "write a single mbox file")
	cmd.Flags().StringVar(&maildir, "maildir", "", "write a Maildir (cur/new/tmp) directory")
	return cmd
}

// coreEachMessage walks the archive page by page, handing each message and its
// raw bytes to fn. Messages without complete raw bytes are counted, not passed.
func coreEachMessage(app *App, accounts []string, fn func(m model.Message, raw []byte) error) (exported, skipped int, err error) {
	st, err := app.Store()
	if err != nil {
		return 0, 0, err
	}
	bl, err := app.Blobs()
	if err != nil {
		return 0, 0, err
	}
	ctx := app.Context()
	offset := 0
	for {
		msgs, err := st.ListMessages(ctx, store.MessageFilter{
			Accounts: accounts,
			Limit:    coreExportPage,
			Offset:   offset,
		})
		if err != nil {
			return exported, skipped, err
		}
		if len(msgs) == 0 {
			return exported, skipped, nil
		}
		for i := range msgs {
			m := msgs[i]
			if !m.RawComplete || m.BlobSHA256 == "" {
				skipped++
				continue
			}
			raw, err := bl.Get(m.BlobSHA256)
			if err != nil {
				skipped++
				continue
			}
			if err := fn(m, raw); err != nil {
				return exported, skipped, err
			}
			exported++
		}
		if len(msgs) < coreExportPage {
			return exported, skipped, nil
		}
		offset += len(msgs)
	}
}

func coreExportMbox(app *App, accounts []string, path string) error {
	f, err := os.Create(path)
	if err != nil {
		return fmt.Errorf("create %s: %w", path, err)
	}
	w := bufio.NewWriterSize(f, 1<<20)
	exported, skipped, walkErr := coreEachMessage(app, accounts, func(m model.Message, raw []byte) error {
		when := m.Received
		if when.IsZero() {
			when = m.Date
		}
		if when.IsZero() {
			when = coreNow(app)
		}
		if _, err := fmt.Fprintf(w, "From MAILER-DAEMON %s\n", when.UTC().Format(time.ANSIC)); err != nil {
			return err
		}
		if err := coreWriteEscaped(w, raw); err != nil {
			return err
		}
		_, err := w.WriteString("\n")
		return err
	})
	if err := w.Flush(); err != nil && walkErr == nil {
		walkErr = err
	}
	if err := f.Close(); err != nil && walkErr == nil {
		walkErr = err
	}
	if walkErr != nil {
		return walkErr
	}
	return app.Printer().Print(coreExportSummary{"mbox", path, exported, skipped})
}

// coreWriteEscaped writes raw with mbox "From " escaping and a trailing newline.
func coreWriteEscaped(w *bufio.Writer, raw []byte) error {
	for len(raw) > 0 {
		line := raw
		if i := bytes.IndexByte(raw, '\n'); i >= 0 {
			line, raw = raw[:i+1], raw[i+1:]
		} else {
			raw = nil
		}
		if bytes.HasPrefix(line, []byte("From ")) {
			if _, err := w.WriteString(">"); err != nil {
				return err
			}
		}
		if _, err := w.Write(line); err != nil {
			return err
		}
		if len(line) > 0 && line[len(line)-1] != '\n' && len(raw) == 0 {
			if _, err := w.WriteString("\n"); err != nil {
				return err
			}
		}
	}
	return nil
}

func coreExportMaildir(app *App, accounts []string, dir string) error {
	for _, sub := range []string{"cur", "new", "tmp"} {
		if err := os.MkdirAll(filepath.Join(dir, sub), 0o700); err != nil {
			return fmt.Errorf("create maildir: %w", err)
		}
	}
	n := 0
	exported, skipped, err := coreEachMessage(app, accounts, func(m model.Message, raw []byte) error {
		n++
		when := m.Received
		if when.IsZero() {
			when = m.Date
		}
		if when.IsZero() {
			when = coreNow(app)
		}
		name := fmt.Sprintf("%d.%d.emlcal:2,%s", when.Unix(), n, coreMaildirFlags(m.Flags))
		return os.WriteFile(filepath.Join(dir, "cur", name), raw, 0o600)
	})
	if err != nil {
		return err
	}
	return app.Printer().Print(coreExportSummary{"maildir", dir, exported, skipped})
}

// coreMaildirFlags renders the info suffix flags, which must be sorted.
func coreMaildirFlags(f model.Flags) string {
	var b strings.Builder
	if f.Flagged {
		b.WriteByte('F')
	}
	if f.Answered {
		b.WriteByte('R')
	}
	if !f.Unread {
		b.WriteByte('S')
	}
	return b.String()
}
