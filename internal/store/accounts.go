package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"time"

	"github.com/lennert/emlcal/internal/model"
)

// ---------------------------------------------------------------------------
// Accounts

// UpsertAccount inserts or updates an account row. CreatedAt is preserved on
// update; a zero CreatedAt on insert becomes now.
func (s *Store) UpsertAccount(ctx context.Context, a *model.Account) error {
	return s.tx().UpsertAccount(ctx, a)
}

// UpsertAccount inserts or updates an account row.
func (tx *Tx) UpsertAccount(ctx context.Context, a *model.Account) error {
	if !model.ValidAccountID(a.ID) {
		return fmt.Errorf("store: invalid account id %q", a.ID)
	}
	created := a.CreatedAt
	if created.IsZero() {
		created = time.Now()
	}
	_, err := tx.q.ExecContext(ctx, `
		INSERT INTO accounts (id, provider, email, created_at) VALUES (?,?,?,?)
		ON CONFLICT(id) DO UPDATE SET provider = excluded.provider, email = excluded.email`,
		a.ID, string(a.Provider), a.Email, created.Unix())
	if err != nil {
		return fmt.Errorf("store: upsert account %s: %w", a.ID, err)
	}
	a.CreatedAt = created
	return nil
}

// GetAccount returns one account, or model.ErrNotFound.
func (s *Store) GetAccount(ctx context.Context, id string) (*model.Account, error) {
	return s.tx().GetAccount(ctx, id)
}

func (tx *Tx) GetAccount(ctx context.Context, id string) (*model.Account, error) {
	var a model.Account
	var provider string
	var created int64
	err := tx.q.QueryRowContext(ctx,
		`SELECT id, provider, email, created_at FROM accounts WHERE id = ?`, id).
		Scan(&a.ID, &provider, &a.Email, &created)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, notFound("account %q", id)
	}
	if err != nil {
		return nil, fmt.Errorf("store: get account %s: %w", id, err)
	}
	a.Provider = model.Provider(provider)
	a.CreatedAt = timeOf(created)
	return &a, nil
}

// ListAccounts returns every account ordered by id.
func (s *Store) ListAccounts(ctx context.Context) ([]model.Account, error) {
	return s.tx().ListAccounts(ctx)
}

func (tx *Tx) ListAccounts(ctx context.Context) ([]model.Account, error) {
	rows, err := tx.q.QueryContext(ctx,
		`SELECT id, provider, email, created_at FROM accounts ORDER BY id`)
	if err != nil {
		return nil, fmt.Errorf("store: list accounts: %w", err)
	}
	defer rows.Close()
	var out []model.Account
	for rows.Next() {
		var a model.Account
		var provider string
		var created int64
		if err := rows.Scan(&a.ID, &provider, &a.Email, &created); err != nil {
			return nil, err
		}
		a.Provider = model.Provider(provider)
		a.CreatedAt = timeOf(created)
		out = append(out, a)
	}
	return out, rows.Err()
}

// DeleteAccount removes an account and everything belonging to it: messages
// (and their FTS entries, mailbox memberships and attachments), mailboxes,
// threads, sync state, backfill progress, outbox rows, sync log, calendars,
// events and occurrences.
//
// Messages are deleted explicitly rather than through the accounts foreign
// key, because SQLite does not fire DELETE triggers for cascaded rows unless
// recursive_triggers is on — and those triggers are what keep the FTS index
// consistent.
func (s *Store) DeleteAccount(ctx context.Context, id string) error {
	return s.Tx(ctx, func(tx *Tx) error { return tx.DeleteAccount(ctx, id) })
}

func (tx *Tx) DeleteAccount(ctx context.Context, id string) error {
	stmts := []string{
		`DELETE FROM messages WHERE account_id = ?`, // fires FTS triggers, cascades children
		`DELETE FROM mailboxes WHERE account_id = ?`,
		`DELETE FROM threads WHERE account_id = ?`,
		`DELETE FROM sync_state WHERE account_id = ?`,
		`DELETE FROM backfill_progress WHERE account_id = ?`,
		`DELETE FROM outbox WHERE account_id = ?`,
		`DELETE FROM sync_log WHERE account_id = ?`,
		`DELETE FROM event_occurrences WHERE event_id IN
			(SELECT e.id FROM events e JOIN calendars c ON c.id = e.calendar_id WHERE c.account_id = ?)`,
		`DELETE FROM events WHERE calendar_id IN (SELECT id FROM calendars WHERE account_id = ?)`,
		`DELETE FROM calendars WHERE account_id = ?`,
		`DELETE FROM accounts WHERE id = ?`,
	}
	for _, q := range stmts {
		if _, err := tx.q.ExecContext(ctx, q, id); err != nil {
			return fmt.Errorf("store: delete account %s: %w", id, err)
		}
	}
	return nil
}

// ---------------------------------------------------------------------------
// Stats

// Stats is the per-account summary shown by `emlcal status`.
type Stats struct {
	Messages        int       `json:"messages"`
	Unread          int       `json:"unread"`
	Flagged         int       `json:"flagged"`
	Deleted         int       `json:"deleted"`
	Threads         int       `json:"threads"`
	Mailboxes       int       `json:"mailboxes"`
	BlobsIncomplete int       `json:"blobs_incomplete"` // raw_complete = 0
	Attachments     int       `json:"attachments"`
	Events          int       `json:"events"`
	LastReceived    time.Time `json:"last_received"`
	LastIndexed     time.Time `json:"last_indexed"`
	OutboxPending   int       `json:"outbox_pending"`
}

// AccountStats collects the counters `emlcal status` prints for one account.
func (s *Store) AccountStats(ctx context.Context, accountID string) (Stats, error) {
	return s.tx().AccountStats(ctx, accountID)
}

func (tx *Tx) AccountStats(ctx context.Context, accountID string) (Stats, error) {
	var st Stats
	var lastRecv, lastIdx sql.NullInt64
	err := tx.q.QueryRowContext(ctx, `
		SELECT
		  count(*),
		  coalesce(sum(CASE WHEN is_unread = 1 AND deleted_at IS NULL THEN 1 ELSE 0 END), 0),
		  coalesce(sum(CASE WHEN is_flagged = 1 AND deleted_at IS NULL THEN 1 ELSE 0 END), 0),
		  coalesce(sum(CASE WHEN deleted_at IS NOT NULL THEN 1 ELSE 0 END), 0),
		  coalesce(sum(CASE WHEN raw_complete = 0 THEN 1 ELSE 0 END), 0),
		  max(CASE WHEN deleted_at IS NULL THEN received_utc END),
		  max(indexed_at)
		FROM messages WHERE account_id = ?`, accountID).
		Scan(&st.Messages, &st.Unread, &st.Flagged, &st.Deleted, &st.BlobsIncomplete, &lastRecv, &lastIdx)
	if err != nil {
		return st, fmt.Errorf("store: stats %s: %w", accountID, err)
	}
	st.LastReceived = nullTime(lastRecv)
	st.LastIndexed = nullTime(lastIdx)

	counts := []struct {
		query string
		dst   *int
	}{
		{`SELECT count(*) FROM threads WHERE account_id = ?`, &st.Threads},
		{`SELECT count(*) FROM mailboxes WHERE account_id = ?`, &st.Mailboxes},
		{`SELECT count(*) FROM attachments a JOIN messages m ON m.id = a.message_id WHERE m.account_id = ?`, &st.Attachments},
		{`SELECT count(*) FROM events e JOIN calendars c ON c.id = e.calendar_id
		    WHERE c.account_id = ? AND e.deleted_at IS NULL`, &st.Events},
		{`SELECT count(*) FROM outbox WHERE account_id = ? AND done_at IS NULL`, &st.OutboxPending},
	}
	for _, c := range counts {
		if err := tx.q.QueryRowContext(ctx, c.query, accountID).Scan(c.dst); err != nil {
			return st, fmt.Errorf("store: stats %s: %w", accountID, err)
		}
	}
	return st, nil
}
