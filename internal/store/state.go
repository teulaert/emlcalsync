package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"time"
)

// ---------------------------------------------------------------------------
// Sync state
//
// resource is "mail", "mailboxes", "calendars", or "cal:<calendar remote id>".

// GetState returns the persisted delta state for one resource, or "" when the
// resource has never been synced (which is what triggers a backfill).
func (s *Store) GetState(ctx context.Context, accountID, resource string) (string, error) {
	return s.tx().GetState(ctx, accountID, resource)
}

func (tx *Tx) GetState(ctx context.Context, accountID, resource string) (string, error) {
	var state string
	err := tx.q.QueryRowContext(ctx,
		`SELECT state FROM sync_state WHERE account_id = ? AND resource = ?`,
		accountID, resource).Scan(&state)
	if errors.Is(err, sql.ErrNoRows) {
		return "", nil
	}
	if err != nil {
		return "", fmt.Errorf("store: get state %s/%s: %w", accountID, resource, err)
	}
	return state, nil
}

// SetState persists a delta state. Call the Tx variant to advance the state in
// the same transaction that applies the changes it covers — that is what makes
// a crash replay rather than skip.
func (s *Store) SetState(ctx context.Context, accountID, resource, state string) error {
	return s.tx().SetState(ctx, accountID, resource, state)
}

func (tx *Tx) SetState(ctx context.Context, accountID, resource, state string) error {
	_, err := tx.q.ExecContext(ctx, `
		INSERT INTO sync_state (account_id, resource, state, updated_at) VALUES (?,?,?,?)
		ON CONFLICT(account_id, resource) DO UPDATE SET
		  state = excluded.state, updated_at = excluded.updated_at`,
		accountID, resource, state, time.Now().Unix())
	if err != nil {
		return fmt.Errorf("store: set state %s/%s: %w", accountID, resource, err)
	}
	return nil
}

// ClearState forgets a state, forcing a reconcile/backfill next run.
func (s *Store) ClearState(ctx context.Context, accountID, resource string) error {
	return s.tx().ClearState(ctx, accountID, resource)
}

func (tx *Tx) ClearState(ctx context.Context, accountID, resource string) error {
	_, err := tx.q.ExecContext(ctx,
		`DELETE FROM sync_state WHERE account_id = ? AND resource = ?`, accountID, resource)
	if err != nil {
		return fmt.Errorf("store: clear state %s/%s: %w", accountID, resource, err)
	}
	return nil
}

// StateEntry is one row of sync_state, for `emlcal status`.
type StateEntry struct {
	AccountID string    `json:"account"`
	Resource  string    `json:"resource"`
	State     string    `json:"state"`
	UpdatedAt time.Time `json:"updated_at"`
}

// ListStates returns every stored state for an account (all accounts when
// accountID is "").
func (s *Store) ListStates(ctx context.Context, accountID string) ([]StateEntry, error) {
	q := `SELECT account_id, resource, state, updated_at FROM sync_state`
	var args []any
	if accountID != "" {
		q += ` WHERE account_id = ?`
		args = append(args, accountID)
	}
	q += ` ORDER BY account_id, resource`
	rows, err := s.db.QueryContext(ctx, q, args...)
	if err != nil {
		return nil, fmt.Errorf("store: list states: %w", err)
	}
	defer rows.Close()
	var out []StateEntry
	for rows.Next() {
		var e StateEntry
		var updated int64
		if err := rows.Scan(&e.AccountID, &e.Resource, &e.State, &updated); err != nil {
			return nil, err
		}
		e.UpdatedAt = timeOf(updated)
		out = append(out, e)
	}
	return out, rows.Err()
}

// ---------------------------------------------------------------------------
// Backfill progress

// Backfill is the resumable progress of an initial full enumeration.
type Backfill struct {
	AccountID string `json:"account"`
	Resource  string `json:"resource"`
	// Cursor is the provider page token / JMAP position to resume from.
	Cursor string `json:"cursor"`
	// StateAtStart is the delta state captured *before* enumeration began, so
	// changes made during a multi-hour backfill are replayed afterwards.
	StateAtStart string     `json:"state_at_start"`
	TotalHint    int        `json:"total_hint"`
	Done         int        `json:"done"`
	FinishedAt   *time.Time `json:"finished_at,omitempty"`
}

// Finished reports whether the backfill has completed.
func (b *Backfill) Finished() bool { return b != nil && b.FinishedAt != nil }

// GetBackfill returns the stored progress, or model.ErrNotFound when no
// backfill has been started for this resource.
func (s *Store) GetBackfill(ctx context.Context, accountID, resource string) (*Backfill, error) {
	return s.tx().GetBackfill(ctx, accountID, resource)
}

func (tx *Tx) GetBackfill(ctx context.Context, accountID, resource string) (*Backfill, error) {
	var b Backfill
	var cursor sql.NullString
	var totalHint, done, finished sql.NullInt64
	err := tx.q.QueryRowContext(ctx, `
		SELECT account_id, resource, cursor, state_at_start, total_hint, done, finished_at
		  FROM backfill_progress WHERE account_id = ? AND resource = ?`,
		accountID, resource).
		Scan(&b.AccountID, &b.Resource, &cursor, &b.StateAtStart, &totalHint, &done, &finished)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, notFound("backfill %s/%s", accountID, resource)
	}
	if err != nil {
		return nil, fmt.Errorf("store: get backfill %s/%s: %w", accountID, resource, err)
	}
	b.Cursor = cursor.String
	b.TotalHint = int(totalHint.Int64)
	b.Done = int(done.Int64)
	b.FinishedAt = timePtr(finished)
	return &b, nil
}

// SetBackfill stores (or replaces) backfill progress.
func (s *Store) SetBackfill(ctx context.Context, b *Backfill) error {
	return s.tx().SetBackfill(ctx, b)
}

func (tx *Tx) SetBackfill(ctx context.Context, b *Backfill) error {
	_, err := tx.q.ExecContext(ctx, `
		INSERT INTO backfill_progress
			(account_id, resource, cursor, state_at_start, total_hint, done, finished_at)
		VALUES (?,?,?,?,?,?,?)
		ON CONFLICT(account_id, resource) DO UPDATE SET
		  cursor = excluded.cursor, state_at_start = excluded.state_at_start,
		  total_hint = excluded.total_hint, done = excluded.done,
		  finished_at = excluded.finished_at`,
		b.AccountID, b.Resource, nullStr(b.Cursor), b.StateAtStart,
		b.TotalHint, b.Done, nullUnix(b.FinishedAt))
	if err != nil {
		return fmt.Errorf("store: set backfill %s/%s: %w", b.AccountID, b.Resource, err)
	}
	return nil
}

// ClearBackfill removes the progress row (used by `sync --full`).
func (s *Store) ClearBackfill(ctx context.Context, accountID, resource string) error {
	return s.tx().ClearBackfill(ctx, accountID, resource)
}

func (tx *Tx) ClearBackfill(ctx context.Context, accountID, resource string) error {
	_, err := tx.q.ExecContext(ctx,
		`DELETE FROM backfill_progress WHERE account_id = ? AND resource = ?`, accountID, resource)
	if err != nil {
		return fmt.Errorf("store: clear backfill %s/%s: %w", accountID, resource, err)
	}
	return nil
}

// ListBackfills returns every backfill row for an account (all when "").
func (s *Store) ListBackfills(ctx context.Context, accountID string) ([]Backfill, error) {
	q := `SELECT account_id, resource, cursor, state_at_start, total_hint, done, finished_at
	        FROM backfill_progress`
	var args []any
	if accountID != "" {
		q += ` WHERE account_id = ?`
		args = append(args, accountID)
	}
	q += ` ORDER BY account_id, resource`
	rows, err := s.db.QueryContext(ctx, q, args...)
	if err != nil {
		return nil, fmt.Errorf("store: list backfills: %w", err)
	}
	defer rows.Close()
	var out []Backfill
	for rows.Next() {
		var b Backfill
		var cursor sql.NullString
		var totalHint, done, finished sql.NullInt64
		if err := rows.Scan(&b.AccountID, &b.Resource, &cursor, &b.StateAtStart,
			&totalHint, &done, &finished); err != nil {
			return nil, err
		}
		b.Cursor = cursor.String
		b.TotalHint = int(totalHint.Int64)
		b.Done = int(done.Int64)
		b.FinishedAt = timePtr(finished)
		out = append(out, b)
	}
	return out, rows.Err()
}

// ---------------------------------------------------------------------------
// Outbox

// OutboxItem is a queued write. Payload is the JSON the sync engine needs to
// replay the operation.
type OutboxItem struct {
	ID        int64      `json:"id"`
	AccountID string     `json:"account"`
	Kind      string     `json:"kind"`
	Payload   []byte     `json:"payload"`
	CreatedAt time.Time  `json:"created_at"`
	Attempts  int        `json:"attempts"`
	LastError string     `json:"last_error,omitempty"`
	DoneAt    *time.Time `json:"done_at,omitempty"`
}

// EnqueueOutbox appends a pending write and returns its id. Write commands
// create the row *before* trying the provider, so a crash mid-request still
// leaves a retryable record.
func (s *Store) EnqueueOutbox(ctx context.Context, accountID, kind string, payload []byte) (int64, error) {
	return s.tx().EnqueueOutbox(ctx, accountID, kind, payload)
}

func (tx *Tx) EnqueueOutbox(ctx context.Context, accountID, kind string, payload []byte) (int64, error) {
	var id int64
	err := tx.q.QueryRowContext(ctx, `
		INSERT INTO outbox (account_id, kind, payload, created_at) VALUES (?,?,?,?)
		RETURNING id`,
		accountID, kind, string(payload), time.Now().Unix()).Scan(&id)
	if err != nil {
		return 0, fmt.Errorf("store: enqueue outbox: %w", err)
	}
	return id, nil
}

// ListOutbox returns outbox rows oldest first; pending=true limits to rows
// that have not completed.
func (s *Store) ListOutbox(ctx context.Context, pending bool) ([]OutboxItem, error) {
	return s.tx().ListOutbox(ctx, pending)
}

func (tx *Tx) ListOutbox(ctx context.Context, pending bool) ([]OutboxItem, error) {
	q := `SELECT id, account_id, kind, payload, created_at, attempts, last_error, done_at FROM outbox`
	if pending {
		q += ` WHERE done_at IS NULL`
	}
	q += ` ORDER BY id`
	rows, err := tx.q.QueryContext(ctx, q)
	if err != nil {
		return nil, fmt.Errorf("store: list outbox: %w", err)
	}
	defer rows.Close()
	var out []OutboxItem
	for rows.Next() {
		var it OutboxItem
		var payload string
		var lastErr sql.NullString
		var created int64
		var done sql.NullInt64
		if err := rows.Scan(&it.ID, &it.AccountID, &it.Kind, &payload, &created,
			&it.Attempts, &lastErr, &done); err != nil {
			return nil, err
		}
		it.Payload = []byte(payload)
		it.CreatedAt = timeOf(created)
		it.LastError = lastErr.String
		it.DoneAt = timePtr(done)
		out = append(out, it)
	}
	return out, rows.Err()
}

// GetOutbox returns one outbox row.
func (s *Store) GetOutbox(ctx context.Context, id int64) (*OutboxItem, error) {
	var it OutboxItem
	var payload string
	var lastErr sql.NullString
	var created int64
	var done sql.NullInt64
	err := s.db.QueryRowContext(ctx, `
		SELECT id, account_id, kind, payload, created_at, attempts, last_error, done_at
		  FROM outbox WHERE id = ?`, id).
		Scan(&it.ID, &it.AccountID, &it.Kind, &payload, &created, &it.Attempts, &lastErr, &done)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, notFound("outbox item %d", id)
	}
	if err != nil {
		return nil, fmt.Errorf("store: get outbox %d: %w", id, err)
	}
	it.Payload = []byte(payload)
	it.CreatedAt = timeOf(created)
	it.LastError = lastErr.String
	it.DoneAt = timePtr(done)
	return &it, nil
}

// MarkOutboxDone records a successful apply.
func (s *Store) MarkOutboxDone(ctx context.Context, id int64) error {
	return s.tx().MarkOutboxDone(ctx, id)
}

func (tx *Tx) MarkOutboxDone(ctx context.Context, id int64) error {
	res, err := tx.q.ExecContext(ctx,
		`UPDATE outbox SET done_at = ?, last_error = NULL WHERE id = ?`, time.Now().Unix(), id)
	if err != nil {
		return fmt.Errorf("store: mark outbox done %d: %w", id, err)
	}
	return requireRow(res, "outbox item %d", id)
}

// MarkOutboxFailed increments the attempt counter and records the error.
func (s *Store) MarkOutboxFailed(ctx context.Context, id int64, errMsg string) error {
	return s.tx().MarkOutboxFailed(ctx, id, errMsg)
}

func (tx *Tx) MarkOutboxFailed(ctx context.Context, id int64, errMsg string) error {
	res, err := tx.q.ExecContext(ctx,
		`UPDATE outbox SET attempts = attempts + 1, last_error = ? WHERE id = ?`,
		nullStr(errMsg), id)
	if err != nil {
		return fmt.Errorf("store: mark outbox failed %d: %w", id, err)
	}
	return requireRow(res, "outbox item %d", id)
}

// DropOutbox deletes an outbox row (`emlcal outbox drop`).
func (s *Store) DropOutbox(ctx context.Context, id int64) error {
	return s.tx().DropOutbox(ctx, id)
}

func (tx *Tx) DropOutbox(ctx context.Context, id int64) error {
	res, err := tx.q.ExecContext(ctx, `DELETE FROM outbox WHERE id = ?`, id)
	if err != nil {
		return fmt.Errorf("store: drop outbox %d: %w", id, err)
	}
	return requireRow(res, "outbox item %d", id)
}

// PruneOutbox deletes completed rows older than before. Returns rows removed.
func (s *Store) PruneOutbox(ctx context.Context, before time.Time) (int, error) {
	res, err := s.db.ExecContext(ctx,
		`DELETE FROM outbox WHERE done_at IS NOT NULL AND done_at < ?`, before.Unix())
	if err != nil {
		return 0, fmt.Errorf("store: prune outbox: %w", err)
	}
	n, _ := res.RowsAffected()
	return int(n), nil
}

func requireRow(res sql.Result, what string, args ...any) error {
	n, err := res.RowsAffected()
	if err != nil {
		return nil // driver without RowsAffected support: assume success
	}
	if n == 0 {
		return notFound(what, args...)
	}
	return nil
}

// ---------------------------------------------------------------------------
// Sync log

// SyncLogEntry is one recorded sync pass.
type SyncLogEntry struct {
	ID        int64     `json:"id"`
	AccountID string    `json:"account"`
	Kind      string    `json:"kind"` // backfill|delta|reconcile|outbox|calendar
	Started   time.Time `json:"started"`
	Finished  time.Time `json:"finished"`
	Added     int       `json:"added"`
	Updated   int       `json:"updated"`
	Removed   int       `json:"removed"`
	Error     string    `json:"error,omitempty"`
}

// AppendSyncLog records a completed (or failed) sync pass and returns its id.
func (s *Store) AppendSyncLog(ctx context.Context, e SyncLogEntry) (int64, error) {
	return s.tx().AppendSyncLog(ctx, e)
}

func (tx *Tx) AppendSyncLog(ctx context.Context, e SyncLogEntry) (int64, error) {
	var id int64
	err := tx.q.QueryRowContext(ctx, `
		INSERT INTO sync_log (account_id, kind, started_at, finished_at, added, updated, removed, error)
		VALUES (?,?,?,?,?,?,?,?) RETURNING id`,
		e.AccountID, e.Kind, unixOf(e.Started), unixOf(e.Finished),
		e.Added, e.Updated, e.Removed, nullStr(e.Error)).Scan(&id)
	if err != nil {
		return 0, fmt.Errorf("store: append sync log: %w", err)
	}
	return id, nil
}

// RecentSyncLog returns the n most recent entries, newest first. accountID ""
// covers every account.
func (s *Store) RecentSyncLog(ctx context.Context, accountID string, n int) ([]SyncLogEntry, error) {
	if n <= 0 {
		n = 20
	}
	q := `SELECT id, account_id, kind, started_at, finished_at, added, updated, removed, error
	        FROM sync_log`
	var args []any
	if accountID != "" {
		q += ` WHERE account_id = ?`
		args = append(args, accountID)
	}
	q += ` ORDER BY id DESC LIMIT ?`
	args = append(args, n)

	rows, err := s.db.QueryContext(ctx, q, args...)
	if err != nil {
		return nil, fmt.Errorf("store: recent sync log: %w", err)
	}
	defer rows.Close()
	var out []SyncLogEntry
	for rows.Next() {
		var e SyncLogEntry
		var acct, kind, errMsg sql.NullString
		var started, finished, added, updated, removed sql.NullInt64
		if err := rows.Scan(&e.ID, &acct, &kind, &started, &finished,
			&added, &updated, &removed, &errMsg); err != nil {
			return nil, err
		}
		e.AccountID = acct.String
		e.Kind = kind.String
		e.Started = nullTime(started)
		e.Finished = nullTime(finished)
		e.Added = int(added.Int64)
		e.Updated = int(updated.Int64)
		e.Removed = int(removed.Int64)
		e.Error = errMsg.String
		out = append(out, e)
	}
	return out, rows.Err()
}

// PruneSyncLog keeps only the newest keep entries per account.
func (s *Store) PruneSyncLog(ctx context.Context, keep int) error {
	if keep <= 0 {
		keep = 200
	}
	_, err := s.db.ExecContext(ctx, `
		DELETE FROM sync_log WHERE id IN (
			SELECT id FROM (
				SELECT id, row_number() OVER (PARTITION BY account_id ORDER BY id DESC) AS rn
				  FROM sync_log
			) WHERE rn > ?)`, keep)
	if err != nil {
		return fmt.Errorf("store: prune sync log: %w", err)
	}
	return nil
}
