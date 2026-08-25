package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"strings"

	"github.com/lennert/emlcal/internal/model"
)

// ErrEmptyMailboxList is returned by ReplaceMailboxes when it is asked to
// replace a non-empty mailbox list with nothing. Deleting every mailbox row
// cascades away every message_mailboxes row, so one empty-but-not-an-error
// provider response would unfile the whole account; the index keeps what it
// has and the caller decides what to do (the sync engine logs and carries on).
var ErrEmptyMailboxList = errors.New("store: refusing to replace a non-empty mailbox list with an empty one")

// ReplaceMailboxes makes the stored mailbox list for an account match mbs
// exactly: rows are upserted by remote id, ParentRemote is resolved to
// parent_id, and rows whose remote id is no longer present are deleted (which
// cascades their message_mailboxes rows).
//
// An empty mbs over an account that already has mailboxes returns
// ErrEmptyMailboxList and changes nothing.
func (s *Store) ReplaceMailboxes(ctx context.Context, accountID string, mbs []model.Mailbox) error {
	return s.Tx(ctx, func(tx *Tx) error { return tx.ReplaceMailboxes(ctx, accountID, mbs) })
}

func (tx *Tx) ReplaceMailboxes(ctx context.Context, accountID string, mbs []model.Mailbox) error {
	if len(mbs) == 0 {
		existing, err := tx.mailboxIDs(ctx, accountID)
		if err != nil {
			return err
		}
		if len(existing) > 0 {
			return fmt.Errorf("%w (account %s has %d)", ErrEmptyMailboxList, accountID, len(existing))
		}
		return nil
	}

	// Pass 1: upsert everything without parents.
	for i := range mbs {
		m := &mbs[i]
		_, err := tx.q.ExecContext(ctx, `
			INSERT INTO mailboxes (account_id, remote_id, name, role, sort_order, total_count, unread_count)
			VALUES (?,?,?,?,?,?,?)
			ON CONFLICT(account_id, remote_id) DO UPDATE SET
			  name = excluded.name, role = excluded.role, sort_order = excluded.sort_order,
			  total_count = excluded.total_count, unread_count = excluded.unread_count`,
			accountID, m.RemoteID, m.Name, nullStr(string(m.Role)), m.SortOrder, m.TotalCount, m.UnreadCount)
		if err != nil {
			return fmt.Errorf("store: upsert mailbox %s/%s: %w", accountID, m.RemoteID, err)
		}
	}

	// Pass 2: resolve parents now that every row exists.
	byRemote, err := tx.mailboxIDs(ctx, accountID)
	if err != nil {
		return err
	}
	for i := range mbs {
		m := &mbs[i]
		id, ok := byRemote[m.RemoteID]
		if !ok {
			continue
		}
		m.ID = id
		m.AccountID = accountID
		var parent any
		if m.ParentRemote != "" {
			if pid, ok := byRemote[m.ParentRemote]; ok && pid != id {
				parent = pid
			} else if !ok {
				tx.warn("mailbox parent not found", "account", accountID,
					"mailbox", m.RemoteID, "parent", m.ParentRemote)
			}
		}
		if _, err := tx.q.ExecContext(ctx,
			`UPDATE mailboxes SET parent_id = ? WHERE id = ?`, parent, id); err != nil {
			return fmt.Errorf("store: set mailbox parent %s: %w", m.RemoteID, err)
		}
	}

	// Pass 3: drop what the provider no longer has.
	keep := make(map[string]bool, len(mbs))
	for _, m := range mbs {
		keep[m.RemoteID] = true
	}
	var stale []any
	for remote := range byRemote {
		if !keep[remote] {
			stale = append(stale, remote)
		}
	}
	if len(stale) > 0 {
		args := append([]any{accountID}, stale...)
		_, err := tx.q.ExecContext(ctx,
			`DELETE FROM mailboxes WHERE account_id = ? AND remote_id IN (`+placeholders(len(stale))+`)`, args...)
		if err != nil {
			return fmt.Errorf("store: delete stale mailboxes: %w", err)
		}
	}
	tx.invalidateMailboxCache(accountID)
	return nil
}

// mailboxIDs returns remote id -> row id for one account.
func (tx *Tx) mailboxIDs(ctx context.Context, accountID string) (map[string]int64, error) {
	if m, ok := tx.mbCache[accountID]; ok {
		return m, nil
	}
	rows, err := tx.q.QueryContext(ctx,
		`SELECT remote_id, id FROM mailboxes WHERE account_id = ?`, accountID)
	if err != nil {
		return nil, fmt.Errorf("store: mailbox ids %s: %w", accountID, err)
	}
	defer rows.Close()
	out := map[string]int64{}
	for rows.Next() {
		var remote string
		var id int64
		if err := rows.Scan(&remote, &id); err != nil {
			return nil, err
		}
		out[remote] = id
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	if tx.mbCache == nil {
		tx.mbCache = map[string]map[string]int64{}
	}
	tx.mbCache[accountID] = out
	return out, nil
}

func (tx *Tx) invalidateMailboxCache(accountID string) {
	delete(tx.mbCache, accountID)
}

func (tx *Tx) warn(msg string, args ...any) {
	if tx.log != nil {
		tx.log.Warn(msg, args...)
	}
}

// mailboxColumns always qualifies with the alias `m`.
const mailboxColumns = `m.id, m.account_id, m.remote_id, m.name, m.role, m.parent_id, m.sort_order, m.total_count, m.unread_count`

func scanMailbox(sc interface{ Scan(...any) error }) (model.Mailbox, error) {
	var m model.Mailbox
	var role sql.NullString
	var parent sql.NullInt64
	var sortOrder, total, unread sql.NullInt64
	err := sc.Scan(&m.ID, &m.AccountID, &m.RemoteID, &m.Name, &role, &parent, &sortOrder, &total, &unread)
	if err != nil {
		return m, err
	}
	m.Role = model.MailboxRole(role.String)
	m.SortOrder = int(sortOrder.Int64)
	m.TotalCount = int(total.Int64)
	m.UnreadCount = int(unread.Int64)
	_ = parent // parent is resolved to ParentRemote by the caller
	return m, nil
}

// ListMailboxes returns the account's mailboxes ordered by sort order, then
// name. ParentRemote is filled in from parent_id.
func (s *Store) ListMailboxes(ctx context.Context, accountID string) ([]model.Mailbox, error) {
	return s.tx().ListMailboxes(ctx, accountID)
}

func (tx *Tx) ListMailboxes(ctx context.Context, accountID string) ([]model.Mailbox, error) {
	rows, err := tx.q.QueryContext(ctx, `
		SELECT `+mailboxColumns+`, p.remote_id
		  FROM mailboxes m LEFT JOIN mailboxes p ON p.id = m.parent_id
		 WHERE m.account_id = ?
		 ORDER BY coalesce(m.sort_order, 0), m.name`, accountID)
	if err != nil {
		return nil, fmt.Errorf("store: list mailboxes %s: %w", accountID, err)
	}
	defer rows.Close()
	var out []model.Mailbox
	for rows.Next() {
		var m model.Mailbox
		var role, parentRemote sql.NullString
		var parent, sortOrder, total, unread sql.NullInt64
		if err := rows.Scan(&m.ID, &m.AccountID, &m.RemoteID, &m.Name, &role, &parent,
			&sortOrder, &total, &unread, &parentRemote); err != nil {
			return nil, err
		}
		m.Role = model.MailboxRole(role.String)
		m.SortOrder = int(sortOrder.Int64)
		m.TotalCount = int(total.Int64)
		m.UnreadCount = int(unread.Int64)
		m.ParentRemote = parentRemote.String
		out = append(out, m)
	}
	return out, rows.Err()
}

// GetMailboxByRemote looks a mailbox up by its provider id.
func (s *Store) GetMailboxByRemote(ctx context.Context, accountID, remote string) (*model.Mailbox, error) {
	return s.tx().GetMailboxByRemote(ctx, accountID, remote)
}

func (tx *Tx) GetMailboxByRemote(ctx context.Context, accountID, remote string) (*model.Mailbox, error) {
	m, err := tx.oneMailbox(ctx,
		`WHERE m.account_id = ? AND m.remote_id = ?`, accountID, remote)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return nil, notFound("mailbox %s/%s", accountID, remote)
		}
		return nil, err
	}
	return m, nil
}

// FindMailbox resolves a user-supplied mailbox name: an exact
// case-insensitive role match first ("inbox", "sent", "category:promotions"),
// then a case-insensitive name match, then a unique case-insensitive prefix
// match on the name. Returns model.ErrNotFound if nothing matches.
func (s *Store) FindMailbox(ctx context.Context, accountID, nameOrRole string) (*model.Mailbox, error) {
	return s.tx().FindMailbox(ctx, accountID, nameOrRole)
}

func (tx *Tx) FindMailbox(ctx context.Context, accountID, nameOrRole string) (*model.Mailbox, error) {
	q := strings.TrimSpace(nameOrRole)
	if q == "" {
		return nil, notFound("mailbox %q", nameOrRole)
	}
	lower := strings.ToLower(q)

	if m, err := tx.oneMailbox(ctx,
		`WHERE m.account_id = ? AND lower(m.role) = ? ORDER BY m.id`, accountID, lower); err == nil {
		return m, nil
	} else if !errors.Is(err, sql.ErrNoRows) {
		return nil, err
	}

	if m, err := tx.oneMailbox(ctx,
		`WHERE m.account_id = ? AND lower(m.name) = ? ORDER BY m.id`, accountID, lower); err == nil {
		return m, nil
	} else if !errors.Is(err, sql.ErrNoRows) {
		return nil, err
	}

	// Unique prefix match, e.g. "prom" -> "Promotions".
	rows, err := tx.q.QueryContext(ctx,
		`SELECT `+mailboxColumns+` FROM mailboxes m
		  WHERE m.account_id = ? AND lower(m.name) LIKE ? ESCAPE '\' ORDER BY m.name LIMIT 2`,
		accountID, likePrefix(q))
	if err != nil {
		return nil, fmt.Errorf("store: find mailbox: %w", err)
	}
	defer rows.Close()
	var hits []model.Mailbox
	for rows.Next() {
		m, err := scanMailbox(rows)
		if err != nil {
			return nil, err
		}
		hits = append(hits, m)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	if len(hits) == 1 {
		return &hits[0], nil
	}
	if len(hits) > 1 {
		return nil, fmt.Errorf("store: mailbox %q is ambiguous in account %s", nameOrRole, accountID)
	}
	return nil, notFound("mailbox %q in account %s", nameOrRole, accountID)
}

// oneMailbox runs `SELECT <cols> FROM mailboxes m <where>` expecting one row.
func (tx *Tx) oneMailbox(ctx context.Context, where string, args ...any) (*model.Mailbox, error) {
	row := tx.q.QueryRowContext(ctx,
		`SELECT `+mailboxColumns+` FROM mailboxes m `+where+` LIMIT 1`, args...)
	m, err := scanMailbox(row)
	if err != nil {
		return nil, err
	}
	if m.ParentRemote == "" {
		// Fill ParentRemote lazily; cheap, and only for single lookups.
		var parentRemote sql.NullString
		if err := tx.q.QueryRowContext(ctx,
			`SELECT p.remote_id FROM mailboxes c JOIN mailboxes p ON p.id = c.parent_id WHERE c.id = ?`,
			m.ID).Scan(&parentRemote); err == nil {
			m.ParentRemote = parentRemote.String
		}
	}
	return &m, nil
}
