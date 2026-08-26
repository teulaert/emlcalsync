package sync

import (
	"context"
	"errors"
	"fmt"
	"os"
	"time"

	"github.com/teulaert/emlcalsync/internal/mime"
	"github.com/teulaert/emlcalsync/internal/model"
	"github.com/teulaert/emlcalsync/internal/provider"
	"github.com/teulaert/emlcalsync/internal/store"
)

// ReindexReport summarises a Reindex pass.
type ReindexReport struct {
	Accounts     []string      `json:"accounts"`
	Scanned      int           `json:"scanned"`
	Reindexed    int           `json:"reindexed"`
	MissingBlobs int           `json:"missing_blobs"`
	Skipped      int           `json:"skipped"` // envelope-only rows, nothing to parse
	Duration     time.Duration `json:"duration"`
}

// reindexPage is how many rows one pass reads (and one transaction writes).
const reindexPage = 100

// Reindex rebuilds the derived columns of every message from the blob archive:
// text, attachments, thread summaries and the FTS index. Provider-side facts
// (flags, mailbox membership, received time, deletion) are preserved.
// account "" covers every configured account.
func (e *Engine) Reindex(ctx context.Context, account string) (*ReindexReport, error) {
	started := time.Now()
	rep := &ReindexReport{}

	var accounts []string
	if account != "" {
		accounts = []string{account}
	} else {
		for _, a := range e.cfg.Accounts {
			accounts = append(accounts, a.Name)
		}
	}
	rep.Accounts = accounts

	for _, name := range accounts {
		offset := 0
		for {
			msgs, err := e.st.ListMessages(ctx, store.MessageFilter{
				Accounts:       []string{name},
				IncludeDeleted: true,
				Limit:          reindexPage,
				Offset:         offset,
			})
			if err != nil {
				return rep, err
			}
			if len(msgs) == 0 {
				break
			}
			offset += len(msgs)
			rep.Scanned += len(msgs)

			var batch []*pendingMsg
			for i := range msgs {
				m := &msgs[i]
				if !m.RawComplete || m.BlobSHA256 == "" {
					rep.Skipped++
					continue
				}
				raw, err := e.blobs.Get(m.BlobSHA256)
				if errors.Is(err, model.ErrNotFound) {
					rep.MissingBlobs++
					e.log.Warn("blob missing", "account", name, "message", m.RemoteID, "sha", m.BlobSHA256)
					continue
				}
				if err != nil {
					return rep, err
				}
				parsed, perr := mime.Parse(raw)
				if perr != nil {
					e.log.Warn("mime parse failed", "account", name, "message", m.RemoteID, "err", perr)
					parsed = nil
				}
				n := &model.Message{
					AccountID:      name,
					RemoteID:       m.RemoteID,
					ThreadID:       m.ThreadID,
					BlobSHA256:     m.BlobSHA256,
					RawComplete:    true,
					Received:       m.Received,
					Size:           m.Size,
					Flags:          m.Flags,
					MailboxRemotes: m.MailboxRemotes,
					DeletedAt:      m.DeletedAt,
					IndexedAt:      time.Now(),
				}
				if parsed != nil {
					n.Snippet = mime.Snippet(parsed.TextBody, 200)
					n.Date = parsed.Date
				}
				if n.Date.IsZero() {
					n.Date = m.Received
				}
				batch = append(batch, &pendingMsg{msg: n, parsed: parsed})
			}

			if len(batch) > 0 {
				err := e.st.Tx(ctx, func(tx *store.Tx) error {
					for _, p := range batch {
						if _, err := tx.UpsertMessage(ctx, p.msg, p.parsed); err != nil {
							return err
						}
					}
					return nil
				})
				if err != nil {
					return rep, err
				}
				rep.Reindexed += len(batch)
			}

			e.emit(ProgressEvent{
				Account: name, Resource: resourceMail, Phase: "reindex",
				Done: rep.Reindexed, Message: "reindexed",
			})
			if err := ctx.Err(); err != nil {
				return rep, err
			}
			if len(msgs) < reindexPage {
				break
			}
		}
	}
	rep.Duration = time.Since(started)
	return rep, nil
}

// GCReport summarises a GC pass.
type GCReport struct {
	Blobs          int           `json:"blobs"`   // blobs walked
	Deleted        int           `json:"deleted"` // blobs removed
	Skipped        int           `json:"skipped"` // unreferenced but too young to touch
	FreedBytes     int64         `json:"freed_bytes"`
	PurgedMessages int           `json:"purged_messages"`
	Duration       time.Duration `json:"duration"`
}

// orphanGrace is how long an unreferenced blob is left alone. A sync writes
// the blob before the row that points at it, so a blob written seconds ago may
// simply belong to a batch that has not committed yet — deleting it would tear
// a hole in a running backfill.
const orphanGrace = time.Hour

// GC deletes blobs no message row references any more — normally only orphans
// left behind by a crash between the blob write and the row insert.
//
// With purgeDeleted, rows marked deleted on the server are removed first and
// their blobs go with them. That is server semantics, not archive semantics:
// the default keeps everything.
func (e *Engine) GC(ctx context.Context, purgeDeleted bool) (*GCReport, error) {
	started := time.Now()
	rep := &GCReport{}

	// Blobs whose last reference this very call removed are collected without
	// waiting out the grace window: the user asked for them to go.
	purged := map[string]bool{}
	if purgeDeleted {
		n, shas, err := e.purgeDeleted(ctx)
		if err != nil {
			return rep, err
		}
		rep.PurgedMessages, purged = n, shas
	}

	referenced := map[string]bool{}
	if err := e.st.ReferencedBlobs(ctx, func(sha string) error {
		referenced[sha] = true
		return nil
	}); err != nil {
		return rep, err
	}

	type orphan struct {
		sha  string
		size int64
	}
	cutoff := time.Now().Add(-orphanGrace)
	var orphans []orphan
	if err := e.blobs.Walk(func(sha string, size int64) error {
		rep.Blobs++
		if referenced[sha] {
			return ctx.Err()
		}
		if !purged[sha] {
			if fi, err := os.Stat(e.blobs.Path(sha)); err == nil && fi.ModTime().After(cutoff) {
				rep.Skipped++
				return ctx.Err()
			}
		}
		orphans = append(orphans, orphan{sha, size})
		return ctx.Err()
	}); err != nil {
		return rep, err
	}

	for _, o := range orphans {
		if err := e.blobs.Delete(o.sha); err != nil {
			if errors.Is(err, model.ErrNotFound) {
				continue
			}
			return rep, err
		}
		rep.Deleted++
		rep.FreedBytes += o.size
		e.emit(ProgressEvent{
			Resource: "blobs", Phase: "gc",
			Done: rep.Deleted, Total: len(orphans),
		})
	}
	if err := e.blobs.CleanTemp(); err != nil {
		e.log.Warn("clean temp blobs", "err", err)
	}
	rep.Duration = time.Since(started)
	return rep, nil
}

// purgeDeleted hard-deletes the rows the server no longer has and returns how
// many went, plus the blob shas they referenced. The FTS index and the child
// tables follow via triggers and ON DELETE CASCADE; thread summaries are
// rebuilt afterwards.
func (e *Engine) purgeDeleted(ctx context.Context) (int, map[string]bool, error) {
	shas := map[string]bool{}
	accounts, err := e.scanStrings(ctx,
		`SELECT DISTINCT account_id FROM messages WHERE deleted_at IS NOT NULL`)
	if err != nil {
		return 0, shas, err
	}
	doomed, err := e.scanStrings(ctx,
		`SELECT DISTINCT blob_sha256 FROM messages
		  WHERE deleted_at IS NOT NULL AND blob_sha256 IS NOT NULL AND blob_sha256 <> ''`)
	if err != nil {
		return 0, shas, err
	}
	for _, sha := range doomed {
		shas[sha] = true
	}

	total := 0
	for _, a := range accounts {
		res, err := e.st.DB().ExecContext(ctx,
			`DELETE FROM messages WHERE account_id = ? AND deleted_at IS NOT NULL`, a)
		if err != nil {
			return total, shas, fmt.Errorf("sync: gc: purge %s: %w", a, err)
		}
		n, _ := res.RowsAffected()
		total += int(n)
		if err := e.st.RebuildThreads(ctx, a); err != nil {
			return total, shas, err
		}
	}
	return total, shas, nil
}

// scanStrings runs a single-column query against the index.
func (e *Engine) scanStrings(ctx context.Context, query string, args ...any) ([]string, error) {
	rows, err := e.st.DB().QueryContext(ctx, query, args...)
	if err != nil {
		return nil, fmt.Errorf("sync: gc: %w", err)
	}
	defer rows.Close()
	var out []string
	for rows.Next() {
		var v string
		if err := rows.Scan(&v); err != nil {
			return nil, err
		}
		out = append(out, v)
	}
	return out, rows.Err()
}

// ---------------------------------------------------------------------------
// On-demand fetches

// EnsureRaw returns the complete raw message. When the row is envelope-only
// (raw_max_size skipped it) or its blob went missing, the message is fetched
// from the provider, stored and re-indexed first.
func (e *Engine) EnsureRaw(ctx context.Context, account, messageRemote string) ([]byte, error) {
	msg, err := e.st.GetMessage(ctx, account, messageRemote)
	if err != nil {
		return nil, err
	}
	if msg.RawComplete && msg.BlobSHA256 != "" {
		raw, err := e.blobs.Get(msg.BlobSHA256)
		if err == nil {
			return raw, nil
		}
		if !errors.Is(err, model.ErrNotFound) {
			return nil, err
		}
		e.log.Warn("blob missing, refetching", "account", account, "message", messageRemote)
	}

	acct, ok := e.cfg.Account(account)
	if !ok {
		return nil, fmt.Errorf("sync: unknown account %q", account)
	}
	mp, err := e.mailProvider(ctx, *acct)
	if err != nil {
		return nil, err
	}
	r := &mailRun{e: e, acct: *acct, mp: mp, unresolved: map[string]bool{}}
	if err := r.loadMailboxes(ctx); err != nil {
		return nil, err
	}

	var out []byte
	err = mp.FetchRaw(ctx, []string{messageRemote}, func(rm provider.RawMessage) error {
		p, err := r.prepare(rm)
		if err != nil {
			return err
		}
		out = rm.Raw
		return r.writeBatch(ctx, []*pendingMsg{p}, nil)
	})
	if err != nil {
		return nil, fmt.Errorf("sync: %s: fetch %s: %w", account, messageRemote, err)
	}
	if out == nil {
		if _, err := e.st.MarkDeleted(ctx, account, []string{messageRemote}); err != nil {
			return nil, err
		}
		return nil, fmt.Errorf("message %s: %w", model.MessagePublicID(account, messageRemote), model.ErrNotFound)
	}
	return out, nil
}

// FetchAttachment returns one attachment's bytes. ref is the part path when
// the raw message is archived, or the provider's attachment reference for
// rows that were never fetched in full.
func (e *Engine) FetchAttachment(ctx context.Context, account, messageRemote, ref string) ([]byte, error) {
	msg, err := e.st.GetMessage(ctx, account, messageRemote)
	if err != nil {
		return nil, err
	}
	if msg.RawComplete && msg.BlobSHA256 != "" {
		atts, err := e.st.ListAttachments(ctx, msg.ID)
		if err != nil {
			return nil, err
		}
		for _, a := range atts {
			if a.PartPath != ref && (a.RemoteRef == "" || a.RemoteRef != ref) {
				continue
			}
			raw, err := e.blobs.Get(msg.BlobSHA256)
			if err != nil {
				break // fall through to the provider
			}
			data, _, _, err := mime.PartContent(raw, a.PartPath)
			if err != nil {
				return nil, err
			}
			return data, nil
		}
	}

	acct, ok := e.cfg.Account(account)
	if !ok {
		return nil, fmt.Errorf("sync: unknown account %q", account)
	}
	mp, err := e.mailProvider(ctx, *acct)
	if err != nil {
		return nil, err
	}
	data, err := mp.FetchAttachment(ctx, messageRemote, ref)
	if err != nil {
		return nil, fmt.Errorf("sync: %s: attachment %s: %w", account, ref, err)
	}
	return data, nil
}
