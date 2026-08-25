package store

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"strings"

	"github.com/lennert/emlcal/internal/model"
)

// maxThreadParticipants caps the denormalised participant list.
const maxThreadParticipants = 25

// refreshThread recomputes the threads summary row for one (account, thread)
// from its non-deleted messages, deleting the row when nothing is left.
func (tx *Tx) refreshThread(ctx context.Context, accountID, threadID string) error {
	if threadID == "" {
		return nil
	}
	rows, err := tx.q.QueryContext(ctx, `
		SELECT subject, from_addr, from_name, to_json, cc_json, received_utc, is_unread
		  FROM messages
		 WHERE account_id = ? AND thread_id = ? AND deleted_at IS NULL
		 ORDER BY received_utc, id`, accountID, threadID)
	if err != nil {
		return fmt.Errorf("store: refresh thread %s/%s: %w", accountID, threadID, err)
	}

	var (
		count, unread int
		first, last   int64
		subject       string
		seen          = map[string]bool{}
		participants  []model.Address
	)
	for rows.Next() {
		var subj, fromAddr, fromName, toJSON, ccJSON sql.NullString
		var received, isUnread int64
		if err := rows.Scan(&subj, &fromAddr, &fromName, &toJSON, &ccJSON, &received, &isUnread); err != nil {
			rows.Close()
			return err
		}
		if count == 0 {
			subject = subj.String
			first = received
		}
		last = received
		count++
		if isUnread != 0 {
			unread++
		}
		if subject == "" {
			subject = subj.String
		}
		addParticipant(&participants, seen, model.Address{Name: fromName.String, Email: fromAddr.String})
		for _, a := range unmarshalAddrs(toJSON) {
			addParticipant(&participants, seen, a)
		}
		for _, a := range unmarshalAddrs(ccJSON) {
			addParticipant(&participants, seen, a)
		}
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return err
	}

	if count == 0 {
		if _, err := tx.q.ExecContext(ctx,
			`DELETE FROM threads WHERE account_id = ? AND thread_id = ?`, accountID, threadID); err != nil {
			return fmt.Errorf("store: drop empty thread: %w", err)
		}
		return nil
	}

	var participantsJSON any
	if len(participants) > 0 {
		b, err := json.Marshal(participants)
		if err != nil {
			return fmt.Errorf("store: marshal participants: %w", err)
		}
		participantsJSON = string(b)
	}

	if _, err := tx.q.ExecContext(ctx, `
		INSERT INTO threads (account_id, thread_id, subject, first_utc, last_utc,
		                     message_count, unread_count, participants_json)
		VALUES (?,?,?,?,?,?,?,?)
		ON CONFLICT(account_id, thread_id) DO UPDATE SET
		  subject = excluded.subject, first_utc = excluded.first_utc, last_utc = excluded.last_utc,
		  message_count = excluded.message_count, unread_count = excluded.unread_count,
		  participants_json = excluded.participants_json`,
		accountID, threadID, nullStr(subject), first, last, count, unread, participantsJSON); err != nil {
		return fmt.Errorf("store: upsert thread %s/%s: %w", accountID, threadID, err)
	}
	return nil
}

func addParticipant(list *[]model.Address, seen map[string]bool, a model.Address) {
	if a.Email == "" || len(*list) >= maxThreadParticipants {
		return
	}
	key := strings.ToLower(a.Email)
	if seen[key] {
		return
	}
	seen[key] = true
	*list = append(*list, a)
}

// RefreshThread recomputes one thread summary. Normally maintained
// automatically; exposed for `reindex`.
func (s *Store) RefreshThread(ctx context.Context, accountID, threadID string) error {
	return s.Tx(ctx, func(tx *Tx) error { return tx.refreshThread(ctx, accountID, threadID) })
}

// RebuildThreads recomputes every thread summary for an account.
func (s *Store) RebuildThreads(ctx context.Context, accountID string) error {
	rows, err := s.db.QueryContext(ctx,
		`SELECT DISTINCT thread_id FROM messages WHERE account_id = ?`, accountID)
	if err != nil {
		return fmt.Errorf("store: rebuild threads: %w", err)
	}
	var ids []string
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			rows.Close()
			return err
		}
		ids = append(ids, id)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return err
	}
	return s.Tx(ctx, func(tx *Tx) error {
		if _, err := tx.q.ExecContext(ctx, `DELETE FROM threads WHERE account_id = ?`, accountID); err != nil {
			return err
		}
		for _, id := range ids {
			if err := tx.refreshThread(ctx, accountID, id); err != nil {
				return err
			}
		}
		return nil
	})
}

func scanThread(sc scanner) (model.Thread, error) {
	var t model.Thread
	var subject, participants sql.NullString
	var first, last, count, unread sql.NullInt64
	if err := sc.Scan(&t.AccountID, &t.ThreadID, &subject, &first, &last,
		&count, &unread, &participants); err != nil {
		return t, err
	}
	t.Subject = subject.String
	t.First = nullTime(first)
	t.Last = nullTime(last)
	t.MessageCount = int(count.Int64)
	t.UnreadCount = int(unread.Int64)
	t.Participants = unmarshalAddrs(participants)
	return t, nil
}

const threadCols = `t.account_id, t.thread_id, t.subject, t.first_utc, t.last_utc,
	t.message_count, t.unread_count, t.participants_json`

// ListThreads returns thread summaries whose messages match f, newest activity
// first. Filter fields that are per-message (From, mailbox, unread, …) select
// threads that contain at least one matching message; Limit/Offset apply to
// threads, not messages.
func (s *Store) ListThreads(ctx context.Context, f MessageFilter) ([]model.Thread, error) {
	return s.tx().ListThreads(ctx, f)
}

func (tx *Tx) ListThreads(ctx context.Context, f MessageFilter) ([]model.Thread, error) {
	where, args := f.where()
	limit, limitArgs := f.limitClause()
	rows, err := tx.q.QueryContext(ctx,
		`SELECT `+threadCols+` FROM threads t
		  WHERE EXISTS (SELECT 1 FROM messages m
		                 WHERE m.account_id = t.account_id AND m.thread_id = t.thread_id
		                   AND `+where+`)
		  ORDER BY t.last_utc DESC, t.thread_id DESC`+limit,
		append(args, limitArgs...)...)
	if err != nil {
		return nil, fmt.Errorf("store: list threads: %w", err)
	}
	defer rows.Close()
	var out []model.Thread
	for rows.Next() {
		t, err := scanThread(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, t)
	}
	return out, rows.Err()
}

// GetThread returns a thread summary and its messages, oldest first, with
// bodies loaded. Deleted messages are included only when includeDeleted is set.
func (s *Store) GetThread(ctx context.Context, accountID, threadID string, includeDeleted bool) (*model.Thread, []model.Message, error) {
	return s.tx().GetThread(ctx, accountID, threadID, includeDeleted)
}

func (tx *Tx) GetThread(ctx context.Context, accountID, threadID string, includeDeleted bool) (*model.Thread, []model.Message, error) {
	row := tx.q.QueryRowContext(ctx,
		`SELECT `+threadCols+` FROM threads t WHERE t.account_id = ? AND t.thread_id = ?`,
		accountID, threadID)
	t, err := scanThread(row)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil, notFound("thread %s:t:%s", accountID, threadID)
	}
	if err != nil {
		return nil, nil, fmt.Errorf("store: get thread %s:%s: %w", accountID, threadID, err)
	}

	q := `SELECT ` + messageColsBody + ` FROM messages m
	       WHERE m.account_id = ? AND m.thread_id = ?`
	if !includeDeleted {
		q += ` AND m.deleted_at IS NULL`
	}
	q += ` ORDER BY m.received_utc, m.id`
	rows, err := tx.q.QueryContext(ctx, q, accountID, threadID)
	if err != nil {
		return nil, nil, fmt.Errorf("store: get thread messages: %w", err)
	}
	defer rows.Close()
	var msgs []model.Message
	for rows.Next() {
		m, err := scanMessage(rows, true)
		if err != nil {
			return nil, nil, err
		}
		msgs = append(msgs, m)
	}
	if err := rows.Err(); err != nil {
		return nil, nil, err
	}
	if err := tx.attachMailboxes(ctx, msgs); err != nil {
		return nil, nil, err
	}
	return &t, msgs, nil
}
