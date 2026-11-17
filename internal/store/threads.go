package store

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/base32"
	"encoding/json"
	"errors"
	"fmt"
	"strings"

	"github.com/teulaert/emlcalsync/internal/model"
)

// maxThreadParticipants caps the denormalised participant list.
const maxThreadParticipants = 25

// maxMessageRefs caps how many References entries one message contributes.
// A long-running list thread accumulates hundreds; the root and the nearest
// ancestors are what actually join a conversation, and an unbounded chain
// would let one pathological message fan out across unrelated threads.
const maxMessageRefs = 24

// messageRefs is every Message-ID a message names — its own, its parent's and
// its ancestry — normalised and deduplicated, oldest ancestor first.
//
// References is the ancestry chain in order, so References[0] is the root of
// the conversation. That ordering is what makes threadFor deterministic: two
// messages in one conversation mint the same id whichever arrives first.
func messageRefs(m *model.Message) []string {
	var out []string
	seen := map[string]bool{}
	add := func(v string) {
		v = strings.TrimSpace(strings.Trim(strings.TrimSpace(v), "<>"))
		if v == "" || seen[v] || len(out) >= maxMessageRefs {
			return
		}
		seen[v] = true
		out = append(out, v)
	}
	for _, r := range m.References {
		add(r)
	}
	add(m.InReplyTo)
	add(m.MessageIDHeader)
	return out
}

// mintThreadID derives a thread id from a conversation's root Message-ID.
//
// Hashing the root rather than an arbitrary ref is what makes the result
// independent of arrival order: a reply indexed before its parent mints the id
// the parent would have minted, so the two land in one thread without needing a
// merge. base32 keeps it free of ":", which model.ParseID splits on.
func mintThreadID(root string) string {
	sum := sha256.Sum256([]byte(root))
	enc := base32.StdEncoding.WithPadding(base32.NoPadding).EncodeToString(sum[:])
	return "r" + strings.ToLower(enc[:20])
}

// resolveThreadID assigns a message to a thread by walking the Message-ID graph
// in message_refs — the client-side threading every IMAP client does, because
// IMAP has no thread id to hand over.
//
// It is called only when the provider supplied none, so Gmail and JMAP keep
// their server-side threads and never touch this table: emlcal agreeing with
// the Gmail and Fastmail UIs about what belongs together is worth more than one
// uniform algorithm.
//
// The returned losers are threads this message proved were the same
// conversation; the caller merges them once its own row exists.
func (tx *Tx) resolveThreadID(ctx context.Context, accountID string, refs []string) (thread string, losers []string, err error) {
	if len(refs) == 0 {
		return "", nil, nil // no headers to go on; the caller falls back
	}

	args := append([]any{accountID}, anySlice(refs)...)
	rows, err := tx.q.QueryContext(ctx, `
		SELECT DISTINCT m.thread_id
		  FROM message_refs r JOIN messages m ON m.id = r.message_id
		 WHERE r.account_id = ? AND r.ref IN (`+placeholders(len(refs))+`)
		 ORDER BY m.thread_id`, args...)
	if err != nil {
		return "", nil, fmt.Errorf("store: resolve thread: %w", err)
	}
	var hits []string
	for rows.Next() {
		var t string
		if err := rows.Scan(&t); err != nil {
			rows.Close()
			return "", nil, err
		}
		if t != "" {
			hits = append(hits, t)
		}
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return "", nil, err
	}

	switch len(hits) {
	case 0:
		return mintThreadID(refs[0]), nil, nil
	case 1:
		return hits[0], nil, nil
	}
	// Several threads share this message's ancestry, so they were always one
	// conversation and we only now have the message that proves it. ORDER BY
	// above makes the winner the smallest, so the outcome does not depend on
	// which message happened to arrive last.
	return hits[0], hits[1:], nil
}

// recordRefs stores the Message-ID graph for one message. Called after the row
// exists, since message_refs points at it.
func (tx *Tx) recordRefs(ctx context.Context, accountID string, id int64, refs []string) error {
	if len(refs) == 0 {
		return nil
	}
	if _, err := tx.q.ExecContext(ctx,
		`DELETE FROM message_refs WHERE message_id = ?`, id); err != nil {
		return fmt.Errorf("store: clear message refs: %w", err)
	}
	for _, r := range refs {
		if _, err := tx.q.ExecContext(ctx,
			`INSERT OR IGNORE INTO message_refs (account_id, message_id, ref) VALUES (?,?,?)`,
			accountID, id, r); err != nil {
			return fmt.Errorf("store: record message ref: %w", err)
		}
	}
	return nil
}

// mergeThreads folds losers into winner and refreshes every summary involved.
// A loser ends up with no messages, so refreshThread drops its row.
func (tx *Tx) mergeThreads(ctx context.Context, accountID, winner string, losers []string) error {
	for _, loser := range losers {
		if loser == "" || loser == winner {
			continue
		}
		if _, err := tx.q.ExecContext(ctx,
			`UPDATE messages SET thread_id = ? WHERE account_id = ? AND thread_id = ?`,
			winner, accountID, loser); err != nil {
			return fmt.Errorf("store: merge thread %s into %s: %w", loser, winner, err)
		}
		if err := tx.refreshThread(ctx, accountID, loser); err != nil {
			return err
		}
	}
	return nil
}

// byThread pins a thread's messages to the (account_id, thread_id) index.
//
// Both thread reads also order by received_utc, and without ANALYZE statistics
// SQLite prefers messages_account (account_id, received_utc) for that: it
// skips the sort but walks every message of the account to find a handful.
// On a 70k-message account that is about a gigabyte read per call, and
// refreshThread runs once per updated message inside a write transaction, so
// a large envelope refresh held the write lock for hours. A thread is small;
// sorting it is free.
const byThread = `INDEXED BY messages_thread`

// threadSummarySQL reads what refreshThread folds into a thread's summary.
const threadSummarySQL = `
		SELECT subject, from_addr, from_name, to_json, cc_json, received_utc, is_unread,
		       has_attachments, is_draft
		  FROM messages ` + byThread + `
		 WHERE account_id = ? AND thread_id = ? AND deleted_at IS NULL
		 ORDER BY received_utc, id`

// threadMessagesSQL lists a thread's messages, oldest first.
func threadMessagesSQL(includeDeleted bool) string {
	q := `SELECT ` + messageColsBody + ` FROM messages m ` + byThread + `
	       WHERE m.account_id = ? AND m.thread_id = ?`
	if !includeDeleted {
		q += ` AND m.deleted_at IS NULL`
	}
	return q + ` ORDER BY m.received_utc, m.id`
}

// refreshThread recomputes the threads summary row for one (account, thread)
// from its non-deleted messages, deleting the row when nothing is left.
func (tx *Tx) refreshThread(ctx context.Context, accountID, threadID string) error {
	if threadID == "" {
		return nil
	}
	rows, err := tx.q.QueryContext(ctx, threadSummarySQL, accountID, threadID)
	if err != nil {
		return fmt.Errorf("store: refresh thread %s/%s: %w", accountID, threadID, err)
	}

	var (
		count, unread        int
		first, last, lastAny int64
		hasReal              bool
		subject              string
		hasAttach            bool
		seen                 = map[string]bool{}
		participants         []model.Address
	)
	for rows.Next() {
		var subj, fromAddr, fromName, toJSON, ccJSON sql.NullString
		var received, isUnread, attach, isDraft int64
		if err := rows.Scan(&subj, &fromAddr, &fromName, &toJSON, &ccJSON, &received, &isUnread,
			&attach, &isDraft); err != nil {
			rows.Close()
			return err
		}
		if count == 0 {
			subject = subj.String
			first = received
		}
		// A draft is not activity: a conversation whose newest message is a
		// reply still being written has not moved since the last real one.
		// A thread made of drafts alone is dated by its newest draft.
		lastAny = received
		if isDraft == 0 {
			last, hasReal = received, true
		}
		count++
		if isUnread != 0 {
			unread++
		}
		// Any message with a file makes the whole thread carry one: the row
		// stands for the conversation, and a reply that adds nothing does not
		// take the invoice back out of it.
		if attach != 0 {
			hasAttach = true
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
	if !hasReal {
		last = lastAny // drafts only
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
		                     message_count, unread_count, has_attachments, participants_json)
		VALUES (?,?,?,?,?,?,?,?,?)
		ON CONFLICT(account_id, thread_id) DO UPDATE SET
		  subject = excluded.subject, first_utc = excluded.first_utc, last_utc = excluded.last_utc,
		  message_count = excluded.message_count, unread_count = excluded.unread_count,
		  has_attachments = excluded.has_attachments,
		  participants_json = excluded.participants_json`,
		accountID, threadID, nullStr(subject), first, last, count, unread,
		boolInt(hasAttach), participantsJSON); err != nil {
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

// ClearThreading discards an account's derived threading — the Message-ID
// graph, the thread ids on its messages and its thread summaries — so a reindex
// works them out again from the archived bytes.
//
// Only for a backend that threads client-side. Running it against Gmail or JMAP
// throws away the server's own threading, which is better than anything we can
// reconstruct, and a plain reindex will not bring it back.
func (s *Store) ClearThreading(ctx context.Context, accountID string) error {
	return s.Tx(ctx, func(tx *Tx) error {
		for _, q := range []string{
			`DELETE FROM message_refs WHERE account_id = ?`,
			`DELETE FROM threads WHERE account_id = ?`,
			`UPDATE messages SET thread_id = '' WHERE account_id = ?`,
		} {
			if _, err := tx.q.ExecContext(ctx, q, accountID); err != nil {
				return fmt.Errorf("store: clear threading for %s: %w", accountID, err)
			}
		}
		return nil
	})
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
	var first, last, count, unread, attach, drafts sql.NullInt64
	if err := sc.Scan(&t.AccountID, &t.ThreadID, &subject, &first, &last,
		&count, &unread, &attach, &participants, &drafts); err != nil {
		return t, err
	}
	t.Drafts = int(drafts.Int64)
	t.Subject = subject.String
	t.First = nullTime(first)
	t.Last = nullTime(last)
	t.MessageCount = int(count.Int64)
	t.UnreadCount = int(unread.Int64)
	t.HasAttachments = attach.Int64 != 0
	t.Participants = unmarshalAddrs(participants)
	return t, nil
}

const threadCols = `t.account_id, t.thread_id, t.subject, t.first_utc, t.last_utc,
	t.message_count, t.unread_count, t.has_attachments, t.participants_json, ` + threadDraftsSQL

// threadDraftsSQL counts the drafts in thread t that are still being written:
// live, and not in the trash. Gmail keeps the DRAFT label on a draft that was
// sent or discarded and adds TRASH, the same rule the drafts view applies
// (see MessageFilter.MailboxRole). It is computed when read rather than kept
// in the summary row: it only runs for the rows a page returns, and a stored
// count would need a migration and a rebuild of every thread.
const threadDraftsSQL = `(SELECT count(*) FROM messages d ` + byThread + `
	  WHERE d.account_id = t.account_id AND d.thread_id = t.thread_id
	    AND d.is_draft = 1 AND d.deleted_at IS NULL
	    AND NOT EXISTS (SELECT 1 FROM message_mailboxes dm
	                      JOIN mailboxes db ON db.id = dm.mailbox_id
	                     WHERE dm.message_id = d.id AND lower(db.role) = 'trash'))`

// ListThreads returns thread summaries whose messages match f, newest activity
// first. Filter fields that are per-message (From, mailbox, unread, …) select
// threads that contain at least one matching message; Limit/Offset apply to
// threads, not messages.
func (s *Store) ListThreads(ctx context.Context, f MessageFilter) ([]model.Thread, error) {
	return s.tx().ListThreads(ctx, f)
}

func (tx *Tx) ListThreads(ctx context.Context, f MessageFilter) ([]model.Thread, error) {
	byMailbox, err := tx.datedByMailbox(ctx, f)
	if err != nil {
		return nil, err
	}
	if byMailbox {
		return tx.listThreadsInMailbox(ctx, f)
	}
	where, args := f.where()
	// Keyset pagination: rows that leave or join the query ahead of the page
	// shift an offset, but not a key. The key goes through unixOf: a row with
	// no date stores 0 and reads back as Go's zero time, whose Unix() is far
	// below 0, so binding that would skip every other undated row. It is on t, so it belongs in the outer
	// WHERE, evaluated once per thread rather than once per message.
	paged := ""
	if f.AfterThread != "" {
		paged = ` AND (t.last_utc < ? OR (t.last_utc = ? AND
			(t.thread_id < ? OR (t.thread_id = ? AND t.account_id < ?))))`
		args = append(args, unixOf(f.AfterLast), unixOf(f.AfterLast), f.AfterThread, f.AfterThread, f.AfterAccount)
	}
	limit, limitArgs := f.limitClause()
	rows, err := tx.q.QueryContext(ctx,
		`SELECT `+threadCols+` FROM threads t
		  WHERE EXISTS (SELECT 1 FROM messages m
		                 WHERE m.account_id = t.account_id AND m.thread_id = t.thread_id
		                   AND `+where+`)`+paged+`
		  ORDER BY t.last_utc DESC, t.thread_id DESC, t.account_id DESC`+limit,
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

// datedByMailbox reports whether a view dates its conversations by their
// newest message in the view's own mailbox rather than by thread activity.
//
// Gmail's rule, and Lennert's: a conversation moves when something happens in
// it -- a reply received, a reply sent -- so Inbox and every other view order
// by thread activity (threads.last_utc, which skips drafts). Sent is the
// exception: it answers "what did I send, and when", so it shows the last
// message actually sent. Drafts likewise shows the draft, which is the item
// there.
//
// It is the mailbox's role that decides, however the view names it: the CLI
// passes a localized name ("Verzonden") as MailboxName, and that view is Sent
// all the same. A name is Sent or Drafts when a mailbox of that name in the
// selected accounts carries the role; the filter itself still matches by name.
func (tx *Tx) datedByMailbox(ctx context.Context, f MessageFilter) (bool, error) {
	dated := func(role string) bool {
		switch strings.ToLower(role) {
		case string(model.RoleSent), string(model.RoleDrafts):
			return true
		}
		return false
	}
	if f.MailboxRole != "" {
		return dated(f.MailboxRole), nil
	}
	if f.MailboxName == "" {
		return false, nil
	}
	q := `SELECT 1 FROM mailboxes WHERE lower(name) = ? AND lower(role) IN (?, ?)`
	args := []any{strings.ToLower(f.MailboxName), string(model.RoleSent), string(model.RoleDrafts)}
	if len(f.Accounts) > 0 {
		q += ` AND account_id IN (` + placeholders(len(f.Accounts)) + `)`
		args = append(args, anySlice(f.Accounts)...)
	}
	var one int
	err := tx.q.QueryRowContext(ctx, q+` LIMIT 1`, args...).Scan(&one)
	if errors.Is(err, sql.ErrNoRows) {
		return false, nil
	}
	if err != nil {
		return false, fmt.Errorf("store: mailbox role of %q: %w", f.MailboxName, err)
	}
	return true, nil
}

// listThreadsInMailbox lists the threads of one mailbox view, each dated and
// ordered by its newest message *in that view* (see datedByMailbox).
//
// A thread is in a mailbox when any of its messages is, but its summary is
// dated by its activity anywhere. In Sent that put a conversation with a
// received reply, or once an unsent reply draft, above what was sent since.
// Thread.Last carries the view's date here, and the page key is on it.
func (tx *Tx) listThreadsInMailbox(ctx context.Context, f MessageFilter) ([]model.Thread, error) {
	where, args := f.where()
	paged := ""
	if f.AfterThread != "" {
		paged = ` WHERE (v.shown < ? OR (v.shown = ? AND
			(v.thread_id < ? OR (v.thread_id = ? AND v.account_id < ?))))`
		args = append(args, unixOf(f.AfterLast), unixOf(f.AfterLast), f.AfterThread, f.AfterThread, f.AfterAccount)
	}
	limit, limitArgs := f.limitClause()
	// The page is cut before the summaries are joined, so the per-row draft
	// count runs for the rows returned and not for the whole mailbox.
	rows, err := tx.q.QueryContext(ctx, `
		WITH v AS (
		  SELECT m.account_id, m.thread_id, coalesce(max(m.received_utc), 0) AS shown
		    FROM messages m WHERE `+where+`
		   GROUP BY m.account_id, m.thread_id),
		page AS (
		  SELECT v.account_id, v.thread_id, v.shown FROM v`+paged+`
		   ORDER BY v.shown DESC, v.thread_id DESC, v.account_id DESC`+limit+`)
		SELECT `+threadCols+`, page.shown
		  FROM page JOIN threads t ON t.account_id = page.account_id AND t.thread_id = page.thread_id
		 ORDER BY page.shown DESC, t.thread_id DESC, t.account_id DESC`,
		append(args, limitArgs...)...)
	if err != nil {
		return nil, fmt.Errorf("store: list threads: %w", err)
	}
	defer rows.Close()
	var out []model.Thread
	for rows.Next() {
		var shown sql.NullInt64
		t, err := scanThread(scanWith{rows, &shown})
		if err != nil {
			return nil, err
		}
		if shown.Valid {
			t.Last = nullTime(shown)
		}
		out = append(out, t)
	}
	return out, rows.Err()
}

// scanWith scans a row that carries extra columns after a known prefix.
type scanWith struct {
	sc    scanner
	extra *sql.NullInt64
}

func (s scanWith) Scan(dest ...any) error { return s.sc.Scan(append(dest, s.extra)...) }

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

	rows, err := tx.q.QueryContext(ctx, threadMessagesSQL(includeDeleted), accountID, threadID)
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

// AnswerThread is the conversation a draft belongs to: the thread of the
// message it answers, found by its In-Reply-To, or its own thread when that
// message is not in the archive (or it answers nothing).
//
// A draft's own thread id is not always the right one. Gmail files a draft
// created without a thread id in a thread of its own, and sending it into
// that thread leaves the reply outside the conversation it answers, for good:
// the other side's next reply then threads under it, away from everything
// before. The message it answers knows where it lives.
func (s *Store) AnswerThread(ctx context.Context, m *model.Message) string {
	if m == nil {
		return ""
	}
	parent := strings.TrimSpace(strings.Trim(strings.TrimSpace(m.InReplyTo), "<>"))
	if parent == "" {
		return m.ThreadID
	}
	var thread string
	err := s.tx().q.QueryRowContext(ctx, `
		SELECT thread_id FROM messages
		 WHERE account_id = ? AND message_id_hdr = ? AND remote_id <> ? AND thread_id <> ''
		 ORDER BY date_utc LIMIT 1`,
		m.AccountID, parent, m.RemoteID).Scan(&thread)
	if err != nil || thread == "" {
		return m.ThreadID
	}
	return thread
}
