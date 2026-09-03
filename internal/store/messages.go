package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"strings"
	"time"
	"unicode"

	"github.com/teulaert/emlcalsync/internal/mime"
	"github.com/teulaert/emlcalsync/internal/model"
)

// messageCols is the cheap projection: everything except text_body, which is
// the only column large enough to matter in list output.
const messageCols = `m.id, m.account_id, m.remote_id, m.thread_id, m.blob_sha256, m.raw_complete,
	m.message_id_hdr, m.in_reply_to, m.references_json, m.subject, m.from_addr, m.from_name,
	m.to_json, m.cc_json, m.bcc_json, m.reply_to_json, m.date_utc, m.received_utc, m.size,
	m.snippet, m.has_attachments, m.is_unread, m.is_flagged, m.is_draft, m.is_answered,
	m.deleted_at, m.indexed_at`

// messageColsBody adds text_body; used by the single-message getters.
const messageColsBody = messageCols + `, m.text_body`

type scanner interface{ Scan(dest ...any) error }

// messageRow holds the nullable raw columns so that scanMessage and Search
// (which appends rank/snippet columns) share one projection.
type messageRow struct {
	m model.Message

	blob, msgID, inReplyTo, refs, subject                 sql.NullString
	fromAddr, fromName                                    sql.NullString
	toJSON, ccJSON, bccJSON, replyToJSON                  sql.NullString
	snippet, textBody                                     sql.NullString
	size, deletedAt                                       sql.NullInt64
	dateUTC, receivedUTC, indexedAt                       int64
	rawComplete, hasAtt, unread, flagged, draft, answered int64
}

// dest returns scan targets matching messageCols (withBody=false) or
// messageColsBody (withBody=true).
func (r *messageRow) dest(withBody bool) []any {
	d := []any{
		&r.m.ID, &r.m.AccountID, &r.m.RemoteID, &r.m.ThreadID, &r.blob, &r.rawComplete,
		&r.msgID, &r.inReplyTo, &r.refs, &r.subject, &r.fromAddr, &r.fromName,
		&r.toJSON, &r.ccJSON, &r.bccJSON, &r.replyToJSON, &r.dateUTC, &r.receivedUTC, &r.size,
		&r.snippet, &r.hasAtt, &r.unread, &r.flagged, &r.draft, &r.answered,
		&r.deletedAt, &r.indexedAt,
	}
	if withBody {
		d = append(d, &r.textBody)
	}
	return d
}

// message converts the scanned row into the domain type. MailboxRemotes is
// filled separately — see attachMailboxes.
func (r *messageRow) message() model.Message {
	m := r.m
	m.BlobSHA256 = r.blob.String
	m.RawComplete = r.rawComplete != 0
	m.MessageIDHeader = r.msgID.String
	m.InReplyTo = r.inReplyTo.String
	m.References = unmarshalStrings(r.refs)
	m.Subject = r.subject.String
	m.From = model.Address{Name: r.fromName.String, Email: r.fromAddr.String}
	m.To = unmarshalAddrs(r.toJSON)
	m.Cc = unmarshalAddrs(r.ccJSON)
	m.Bcc = unmarshalAddrs(r.bccJSON)
	m.ReplyTo = unmarshalAddrs(r.replyToJSON)
	m.Date = timeOf(r.dateUTC)
	m.Received = timeOf(r.receivedUTC)
	m.Size = r.size.Int64
	m.Snippet = r.snippet.String
	m.TextBody = r.textBody.String
	m.HasAttachments = r.hasAtt != 0
	m.Flags = model.Flags{
		Unread:   r.unread != 0,
		Flagged:  r.flagged != 0,
		Draft:    r.draft != 0,
		Answered: r.answered != 0,
	}
	m.DeletedAt = timePtr(r.deletedAt)
	m.IndexedAt = timeOf(r.indexedAt)
	return m
}

// scanMessage reads one row projected with messageCols (withBody=false) or
// messageColsBody (withBody=true).
func scanMessage(sc scanner, withBody bool) (model.Message, error) {
	var r messageRow
	if err := sc.Scan(r.dest(withBody)...); err != nil {
		return model.Message{}, err
	}
	return r.message(), nil
}

// ---------------------------------------------------------------------------
// Writing

// UpsertMessage inserts or updates one message and everything derived from it:
// mailbox membership (from msg.MailboxRemotes), attachment rows (from
// parsed.Attachments), the FTS entry (via triggers) and the thread summary.
// The Store method wraps it in its own transaction; call the Tx method to
// batch many messages and the sync state advance into one.
//
// parsed may be nil (e.g. a message whose raw body we could not fetch). When
// it is non-nil it fills in every field msg left empty — subject, addresses,
// dates, body, snippet, bulk detection — so callers only have to set the
// provider-side facts (ids, blob sha, flags, mailboxes, received time).
//
// Mailbox remote ids that are not in the mailboxes table are skipped with a
// warning: the next mailbox sync will pick them up and the following delta
// re-applies membership.
func (s *Store) UpsertMessage(ctx context.Context, msg *model.Message, parsed *mime.Parsed) (int64, error) {
	var id int64
	err := s.Tx(ctx, func(tx *Tx) error {
		var err error
		id, err = tx.UpsertMessage(ctx, msg, parsed)
		return err
	})
	return id, err
}

func (tx *Tx) UpsertMessage(ctx context.Context, msg *model.Message, parsed *mime.Parsed) (int64, error) {
	if msg.AccountID == "" || msg.RemoteID == "" {
		return 0, fmt.Errorf("store: message needs account_id and remote_id")
	}
	m := *msg // work on a copy; only ID/IndexedAt are written back
	applyParsed(&m, parsed)

	if m.IndexedAt.IsZero() {
		m.IndexedAt = time.Now()
	}
	// Backends that thread server-side (Gmail, JMAP) supply a thread id and
	// skip all of this. IMAP supplies none, so the conversation is stitched
	// from the Message-ID graph instead — see Tx.resolveThreadID.
	refs := messageRefs(&m)
	var mergedAway []string
	if m.ThreadID == "" {
		thread, losers, err := tx.resolveThreadID(ctx, m.AccountID, refs)
		if err != nil {
			return 0, err
		}
		m.ThreadID, mergedAway = thread, losers
	}
	if m.ThreadID == "" {
		// No provider id and no usable headers — an oversize stub, or a
		// message too malformed to parse. It is at least its own thread.
		m.ThreadID = m.RemoteID
	}
	if m.Received.IsZero() {
		m.Received = m.Date
	}
	if m.Date.IsZero() {
		m.Date = m.Received
	}

	toJSON, err := marshalAddrs(m.To)
	if err != nil {
		return 0, err
	}
	ccJSON, err := marshalAddrs(m.Cc)
	if err != nil {
		return 0, err
	}
	bccJSON, err := marshalAddrs(m.Bcc)
	if err != nil {
		return 0, err
	}
	replyToJSON, err := marshalAddrs(m.ReplyTo)
	if err != nil {
		return 0, err
	}
	refsJSON, err := marshalStrings(m.References)
	if err != nil {
		return 0, err
	}

	var attNames, listID string
	var isBulk bool
	if parsed != nil {
		attNames = attachmentNames(parsed.Attachments)
		listID = parsed.ListID
		isBulk = parsed.IsBulk
	}

	// A message can move between threads (Gmail rethreads on subject edits,
	// JMAP on References changes). Remember where it was so the old summary
	// is recomputed too, otherwise it keeps a phantom message.
	var prevThread string
	if err := tx.q.QueryRowContext(ctx,
		`SELECT thread_id FROM messages WHERE account_id = ? AND remote_id = ?`,
		m.AccountID, m.RemoteID).Scan(&prevThread); err != nil && !errors.Is(err, sql.ErrNoRows) {
		return 0, fmt.Errorf("store: upsert message %s/%s: %w", m.AccountID, m.RemoteID, err)
	}

	var id int64
	err = tx.q.QueryRowContext(ctx, `
		INSERT INTO messages (
			account_id, remote_id, thread_id, blob_sha256, raw_complete,
			message_id_hdr, in_reply_to, references_json, subject, from_addr, from_name,
			to_json, cc_json, bcc_json, reply_to_json, date_utc, received_utc, size,
			snippet, text_body, attachment_names, list_id, is_bulk, has_attachments,
			is_unread, is_flagged, is_draft, is_answered, deleted_at, indexed_at)
		VALUES (?,?,?,?,?, ?,?,?,?,?,?, ?,?,?,?,?,?,?, ?,?,?,?,?,?, ?,?,?,?,?,?)
		ON CONFLICT(account_id, remote_id) DO UPDATE SET
			thread_id = excluded.thread_id,
			blob_sha256 = coalesce(excluded.blob_sha256, messages.blob_sha256),
			raw_complete = excluded.raw_complete,
			message_id_hdr = excluded.message_id_hdr,
			in_reply_to = excluded.in_reply_to,
			references_json = excluded.references_json,
			subject = excluded.subject,
			from_addr = excluded.from_addr, from_name = excluded.from_name,
			to_json = excluded.to_json, cc_json = excluded.cc_json,
			bcc_json = excluded.bcc_json, reply_to_json = excluded.reply_to_json,
			date_utc = excluded.date_utc, received_utc = excluded.received_utc,
			size = excluded.size, snippet = excluded.snippet,
			text_body = excluded.text_body, attachment_names = excluded.attachment_names,
			list_id = excluded.list_id, is_bulk = excluded.is_bulk,
			has_attachments = excluded.has_attachments,
			is_unread = excluded.is_unread, is_flagged = excluded.is_flagged,
			is_draft = excluded.is_draft, is_answered = excluded.is_answered,
			deleted_at = excluded.deleted_at, indexed_at = excluded.indexed_at
		RETURNING id`,
		m.AccountID, m.RemoteID, m.ThreadID, nullStr(m.BlobSHA256), boolInt(m.RawComplete),
		nullStr(m.MessageIDHeader), nullStr(m.InReplyTo), refsJSON, nullStr(m.Subject),
		nullStr(m.From.Email), nullStr(m.From.Name),
		toJSON, ccJSON, bccJSON, replyToJSON, unixOf(m.Date), unixOf(m.Received), m.Size,
		nullStr(m.Snippet), nullStr(m.TextBody), nullStr(attNames), nullStr(listID), boolInt(isBulk),
		boolInt(m.HasAttachments),
		boolInt(m.Flags.Unread), boolInt(m.Flags.Flagged), boolInt(m.Flags.Draft), boolInt(m.Flags.Answered),
		nullUnix(m.DeletedAt), m.IndexedAt.Unix(),
	).Scan(&id)
	if err != nil {
		return 0, fmt.Errorf("store: upsert message %s/%s: %w", m.AccountID, m.RemoteID, err)
	}

	if err := tx.replaceMemberships(ctx, id, m.AccountID, m.MailboxRemotes); err != nil {
		return 0, err
	}
	// After the memberships: whether this message sits in Sent is part of
	// what the address book records about everyone on it.
	if err := tx.replaceAddresses(ctx, id, m.AccountID); err != nil {
		return 0, err
	}
	if err := tx.recordRefs(ctx, m.AccountID, id, refs); err != nil {
		return 0, err
	}
	// Merge after the row exists, so the winner's summary counts it.
	if err := tx.mergeThreads(ctx, m.AccountID, m.ThreadID, mergedAway); err != nil {
		return 0, err
	}
	if parsed != nil {
		if err := tx.replaceAttachments(ctx, id, parsed.Attachments); err != nil {
			return 0, err
		}
	}
	if err := tx.refreshThread(ctx, m.AccountID, m.ThreadID); err != nil {
		return 0, err
	}
	if prevThread != "" && prevThread != m.ThreadID {
		if err := tx.refreshThread(ctx, m.AccountID, prevThread); err != nil {
			return 0, err
		}
	}

	msg.ID = id
	msg.IndexedAt = m.IndexedAt
	return id, nil
}

// applyParsed fills in whatever the caller left empty from the parsed MIME.
func applyParsed(m *model.Message, p *mime.Parsed) {
	if p == nil {
		return
	}
	if m.Subject == "" {
		m.Subject = p.Subject
	}
	if m.From.Email == "" && m.From.Name == "" {
		m.From = p.From
	}
	if len(m.To) == 0 {
		m.To = p.To
	}
	if len(m.Cc) == 0 {
		m.Cc = p.Cc
	}
	if len(m.Bcc) == 0 {
		m.Bcc = p.Bcc
	}
	if len(m.ReplyTo) == 0 {
		m.ReplyTo = p.ReplyTo
	}
	if m.MessageIDHeader == "" {
		m.MessageIDHeader = p.MessageID
	}
	if m.InReplyTo == "" {
		m.InReplyTo = p.InReplyTo
	}
	if len(m.References) == 0 {
		m.References = p.References
	}
	if m.Date.IsZero() {
		m.Date = p.Date
	}
	if m.TextBody == "" {
		m.TextBody = p.TextBody
	}
	if m.Snippet == "" {
		m.Snippet = snippetOf(m.TextBody, 200)
	}
	if !m.HasAttachments {
		m.HasAttachments = len(p.Attachments) > 0
	}
}

// snippetOf collapses whitespace and takes the first n runes. The mime package
// has a richer Snippet(); this keeps the store independent of it.
func snippetOf(text string, n int) string {
	var b strings.Builder
	space := true // suppress leading whitespace
	count := 0
	for _, r := range text {
		if unicode.IsSpace(r) {
			if space {
				continue
			}
			space = true
			b.WriteByte(' ')
			count++
		} else {
			space = false
			b.WriteRune(r)
			count++
		}
		if count >= n {
			break
		}
	}
	return strings.TrimSpace(b.String())
}

func attachmentNames(parts []mime.Part) string {
	var names []string
	for _, p := range parts {
		if p.Filename != "" {
			names = append(names, p.Filename)
		}
	}
	return strings.Join(names, " ")
}

// replaceMemberships makes message_mailboxes for one message match remotes.
// A nil slice means "leave membership alone"; an empty non-nil slice clears it.
func (tx *Tx) replaceMemberships(ctx context.Context, msgID int64, accountID string, remotes []string) error {
	if remotes == nil {
		return nil
	}
	byRemote, err := tx.mailboxIDs(ctx, accountID)
	if err != nil {
		return err
	}
	wanted := make(map[int64]bool, len(remotes))
	for _, r := range remotes {
		id, ok := byRemote[r]
		if !ok {
			tx.warn("unknown mailbox on message", "account", accountID, "mailbox", r)
			continue
		}
		wanted[id] = true
	}

	rows, err := tx.q.QueryContext(ctx,
		`SELECT mailbox_id FROM message_mailboxes WHERE message_id = ?`, msgID)
	if err != nil {
		return fmt.Errorf("store: read memberships: %w", err)
	}
	current := map[int64]bool{}
	for rows.Next() {
		var id int64
		if err := rows.Scan(&id); err != nil {
			rows.Close()
			return err
		}
		current[id] = true
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return err
	}

	for id := range wanted {
		if !current[id] {
			if _, err := tx.q.ExecContext(ctx,
				`INSERT OR IGNORE INTO message_mailboxes (message_id, mailbox_id) VALUES (?,?)`,
				msgID, id); err != nil {
				return fmt.Errorf("store: add membership: %w", err)
			}
		}
	}
	for id := range current {
		if !wanted[id] {
			if _, err := tx.q.ExecContext(ctx,
				`DELETE FROM message_mailboxes WHERE message_id = ? AND mailbox_id = ?`,
				msgID, id); err != nil {
				return fmt.Errorf("store: remove membership: %w", err)
			}
		}
	}
	return nil
}

func (tx *Tx) replaceAttachments(ctx context.Context, msgID int64, parts []mime.Part) error {
	if _, err := tx.q.ExecContext(ctx, `DELETE FROM attachments WHERE message_id = ?`, msgID); err != nil {
		return fmt.Errorf("store: clear attachments: %w", err)
	}
	for _, p := range parts {
		if _, err := tx.q.ExecContext(ctx, `
			INSERT INTO attachments (message_id, part_path, filename, content_type, size, content_id, is_inline)
			VALUES (?,?,?,?,?,?,?)`,
			msgID, p.Path, nullStr(p.Filename), nullStr(p.ContentType), p.Size,
			nullStr(p.ContentID), boolInt(p.Inline)); err != nil {
			return fmt.Errorf("store: insert attachment: %w", err)
		}
	}
	return nil
}

// SetAttachmentRemoteRef records the provider-side reference used to fetch an
// attachment lazily (only needed when raw_complete = 0).
func (s *Store) SetAttachmentRemoteRef(ctx context.Context, attachmentID int64, ref string) error {
	_, err := s.db.ExecContext(ctx, `UPDATE attachments SET remote_ref = ? WHERE id = ?`, nullStr(ref), attachmentID)
	if err != nil {
		return fmt.Errorf("store: set attachment ref: %w", err)
	}
	return nil
}

// UpdateMessageState applies a flags/mailboxes-only delta: no body is
// rewritten and the FTS index is untouched. A nil mailboxRemotes leaves
// membership as it is; a non-nil (possibly empty) slice replaces it.
// Returns model.ErrNotFound if the message is not indexed yet.
func (s *Store) UpdateMessageState(ctx context.Context, accountID, remote string, flags model.Flags, mailboxRemotes []string) error {
	return s.Tx(ctx, func(tx *Tx) error {
		return tx.UpdateMessageState(ctx, accountID, remote, flags, mailboxRemotes)
	})
}

func (tx *Tx) UpdateMessageState(ctx context.Context, accountID, remote string, flags model.Flags, mailboxRemotes []string) error {
	var id int64
	var threadID string
	err := tx.q.QueryRowContext(ctx,
		`SELECT id, thread_id FROM messages WHERE account_id = ? AND remote_id = ?`,
		accountID, remote).Scan(&id, &threadID)
	if errors.Is(err, sql.ErrNoRows) {
		return notFound("message %s:%s", accountID, remote)
	}
	if err != nil {
		return fmt.Errorf("store: update state %s:%s: %w", accountID, remote, err)
	}

	if _, err := tx.q.ExecContext(ctx, `
		UPDATE messages SET is_unread = ?, is_flagged = ?, is_draft = ?, is_answered = ?
		 WHERE id = ?`,
		boolInt(flags.Unread), boolInt(flags.Flagged), boolInt(flags.Draft), boolInt(flags.Answered),
		id); err != nil {
		return fmt.Errorf("store: update flags %s:%s: %w", accountID, remote, err)
	}
	if err := tx.replaceMemberships(ctx, id, accountID, mailboxRemotes); err != nil {
		return err
	}
	return tx.refreshThread(ctx, accountID, threadID)
}

// RenameRemoteID moves a row to a new remote id, for a provider whose writes
// mint one (an IMAP COPY/MOVE, a folder rename — see provider.Rename).
//
// The row keeps its local id, so its blob, mailbox membership, attachments,
// thread and FTS entry all follow it untouched: remote_id is not one of the
// columns messages_au fires on, so the external-content index is never churned.
//
// A rename onto an id that already exists means a delta had already discovered
// the copy under its new id. The existing row is authoritative and the stale
// one is dropped, because two rows for one message is the thing the unique
// index exists to prevent.
//
// Renaming an unknown id is not an error: the message may never have been
// indexed. Returns whether a row actually moved.
func (s *Store) RenameRemoteID(ctx context.Context, accountID, oldRemote, newRemote string) (bool, error) {
	var moved bool
	err := s.Tx(ctx, func(tx *Tx) error {
		var err error
		moved, err = tx.RenameRemoteID(ctx, accountID, oldRemote, newRemote)
		return err
	})
	return moved, err
}

func (tx *Tx) RenameRemoteID(ctx context.Context, accountID, oldRemote, newRemote string) (bool, error) {
	if accountID == "" || oldRemote == "" || newRemote == "" {
		return false, fmt.Errorf("store: rename needs account_id and both remote ids")
	}
	if oldRemote == newRemote {
		return false, nil
	}

	var id int64
	var threadID string
	err := tx.q.QueryRowContext(ctx,
		`SELECT id, thread_id FROM messages WHERE account_id = ? AND remote_id = ?`,
		accountID, oldRemote).Scan(&id, &threadID)
	if errors.Is(err, sql.ErrNoRows) {
		return false, nil
	}
	if err != nil {
		return false, fmt.Errorf("store: rename %s:%s: %w", accountID, oldRemote, err)
	}

	var existing int64
	err = tx.q.QueryRowContext(ctx,
		`SELECT id FROM messages WHERE account_id = ? AND remote_id = ?`,
		accountID, newRemote).Scan(&existing)
	switch {
	case err == nil && existing != id:
		// The destination is already indexed; drop the row we were moving.
		if _, err := tx.q.ExecContext(ctx, `DELETE FROM messages WHERE id = ?`, id); err != nil {
			return false, fmt.Errorf("store: rename %s:%s: drop stale row: %w", accountID, oldRemote, err)
		}
		if err := tx.refreshThread(ctx, accountID, threadID); err != nil {
			return false, err
		}
		return false, nil
	case err == nil:
		return false, nil // already there
	case !errors.Is(err, sql.ErrNoRows):
		return false, fmt.Errorf("store: rename %s:%s: %w", accountID, newRemote, err)
	}

	if _, err := tx.q.ExecContext(ctx,
		`UPDATE messages SET remote_id = ? WHERE id = ?`, newRemote, id); err != nil {
		return false, fmt.Errorf("store: rename %s:%s -> %s: %w", accountID, oldRemote, newRemote, err)
	}
	return true, nil
}

// MarkDeleted marks messages as gone on the server: deleted_at is set and
// mailbox membership is cleared (a deleted message is in no mailbox). The blob
// and the row survive — this is an archive. The FTS entry is kept too; every
// query filters on deleted_at, and removing entries from an external-content
// FTS index that may later be re-inserted is how you corrupt it.
// Returns the number of rows that changed.
func (s *Store) MarkDeleted(ctx context.Context, accountID string, remotes []string) (int, error) {
	var n int
	err := s.Tx(ctx, func(tx *Tx) error {
		var err error
		n, err = tx.MarkDeleted(ctx, accountID, remotes)
		return err
	})
	return n, err
}

func (tx *Tx) MarkDeleted(ctx context.Context, accountID string, remotes []string) (int, error) {
	if len(remotes) == 0 {
		return 0, nil
	}
	now := time.Now().Unix()
	threads := map[string]bool{}
	n := 0
	for _, remote := range remotes {
		var id int64
		var threadID string
		var already sql.NullInt64
		err := tx.q.QueryRowContext(ctx,
			`SELECT id, thread_id, deleted_at FROM messages WHERE account_id = ? AND remote_id = ?`,
			accountID, remote).Scan(&id, &threadID, &already)
		if errors.Is(err, sql.ErrNoRows) {
			continue
		}
		if err != nil {
			return n, fmt.Errorf("store: mark deleted %s:%s: %w", accountID, remote, err)
		}
		if already.Valid {
			continue
		}
		if _, err := tx.q.ExecContext(ctx,
			`UPDATE messages SET deleted_at = ? WHERE id = ?`, now, id); err != nil {
			return n, fmt.Errorf("store: mark deleted %s:%s: %w", accountID, remote, err)
		}
		if _, err := tx.q.ExecContext(ctx,
			`DELETE FROM message_mailboxes WHERE message_id = ?`, id); err != nil {
			return n, fmt.Errorf("store: clear memberships: %w", err)
		}
		threads[threadID] = true
		n++
	}
	for t := range threads {
		if err := tx.refreshThread(ctx, accountID, t); err != nil {
			return n, err
		}
	}
	return n, nil
}

// MarkUndeleted clears deleted_at (a message that came back). It restores
// nothing else: MarkDeleted cleared the message's mailbox membership, so a row
// resurrected this way is in no mailbox until an UpsertMessage or
// UpdateMessageState files it again. Callers that already know where the
// message lives should use UndeleteWithState instead — a message that is
// un-deleted but unfiled is invisible in every mailbox listing.
func (s *Store) MarkUndeleted(ctx context.Context, accountID string, remotes []string) (int, error) {
	var n int
	err := s.Tx(ctx, func(tx *Tx) error {
		var err error
		n, err = tx.MarkUndeleted(ctx, accountID, remotes)
		return err
	})
	return n, err
}

func (tx *Tx) MarkUndeleted(ctx context.Context, accountID string, remotes []string) (int, error) {
	if len(remotes) == 0 {
		return 0, nil
	}
	threads := map[string]bool{}
	n := 0
	for _, remote := range remotes {
		var threadID string
		err := tx.q.QueryRowContext(ctx,
			`SELECT thread_id FROM messages WHERE account_id = ? AND remote_id = ? AND deleted_at IS NOT NULL`,
			accountID, remote).Scan(&threadID)
		if errors.Is(err, sql.ErrNoRows) {
			continue
		}
		if err != nil {
			return n, fmt.Errorf("store: undelete %s:%s: %w", accountID, remote, err)
		}
		if _, err := tx.q.ExecContext(ctx,
			`UPDATE messages SET deleted_at = NULL WHERE account_id = ? AND remote_id = ?`,
			accountID, remote); err != nil {
			return n, fmt.Errorf("store: undelete %s:%s: %w", accountID, remote, err)
		}
		threads[threadID] = true
		n++
	}
	for t := range threads {
		if err := tx.refreshThread(ctx, accountID, t); err != nil {
			return n, err
		}
	}
	return n, nil
}

// UndeleteWithState resurrects one message and files it in one step: it clears
// deleted_at and applies flags plus mailboxRemotes the way UpdateMessageState
// does. A nil mailboxRemotes leaves membership alone (which for a row that was
// deleted means it stays in no mailbox); pass the enumeration's mailbox list to
// rebuild the membership MarkDeleted cleared.
// Returns model.ErrNotFound if the message is not indexed.
func (s *Store) UndeleteWithState(ctx context.Context, accountID, remote string, flags model.Flags, mailboxRemotes []string) error {
	return s.Tx(ctx, func(tx *Tx) error {
		return tx.UndeleteWithState(ctx, accountID, remote, flags, mailboxRemotes)
	})
}

func (tx *Tx) UndeleteWithState(ctx context.Context, accountID, remote string, flags model.Flags, mailboxRemotes []string) error {
	res, err := tx.q.ExecContext(ctx,
		`UPDATE messages SET deleted_at = NULL WHERE account_id = ? AND remote_id = ?`,
		accountID, remote)
	if err != nil {
		return fmt.Errorf("store: undelete %s:%s: %w", accountID, remote, err)
	}
	if err := requireRow(res, "message %s:%s", accountID, remote); err != nil {
		return err
	}
	// UpdateMessageState refreshes the thread summary, which is what makes the
	// message count as live again.
	return tx.UpdateMessageState(ctx, accountID, remote, flags, mailboxRemotes)
}

// ---------------------------------------------------------------------------
// Reading

// GetMessage returns one message including its text body and mailbox
// membership, or model.ErrNotFound.
func (s *Store) GetMessage(ctx context.Context, accountID, remote string) (*model.Message, error) {
	return s.tx().GetMessage(ctx, accountID, remote)
}

func (tx *Tx) GetMessage(ctx context.Context, accountID, remote string) (*model.Message, error) {
	row := tx.q.QueryRowContext(ctx,
		`SELECT `+messageColsBody+` FROM messages m WHERE m.account_id = ? AND m.remote_id = ?`,
		accountID, remote)
	m, err := scanMessage(row, true)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, notFound("message %s:%s", accountID, remote)
	}
	if err != nil {
		return nil, fmt.Errorf("store: get message %s:%s: %w", accountID, remote, err)
	}
	out := []model.Message{m}
	if err := tx.attachMailboxes(ctx, out); err != nil {
		return nil, err
	}
	return &out[0], nil
}

// GetMessageByID is GetMessage by local row id.
func (s *Store) GetMessageByID(ctx context.Context, id int64) (*model.Message, error) {
	return s.tx().GetMessageByID(ctx, id)
}

func (tx *Tx) GetMessageByID(ctx context.Context, id int64) (*model.Message, error) {
	row := tx.q.QueryRowContext(ctx,
		`SELECT `+messageColsBody+` FROM messages m WHERE m.id = ?`, id)
	m, err := scanMessage(row, true)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, notFound("message #%d", id)
	}
	if err != nil {
		return nil, fmt.Errorf("store: get message #%d: %w", id, err)
	}
	out := []model.Message{m}
	if err := tx.attachMailboxes(ctx, out); err != nil {
		return nil, err
	}
	return &out[0], nil
}

// IndexState is what the index knows about one remote id.
type IndexState struct {
	// Exists is true when a row is present, deleted or not.
	Exists bool
	// RawComplete is true when the full raw bytes are archived (i.e. the row
	// is not the envelope-only stub written for an oversize message).
	RawComplete bool
	// Deleted is true when the row is soft-deleted (deleted_at set), which
	// also means MarkDeleted cleared its mailbox membership.
	Deleted bool
}

// HasMessage reports whether a message is indexed and whether its raw bytes
// were stored in full. Used by the backfill to skip work. It says nothing
// about deletion — use MessageIndexState when that matters.
func (s *Store) HasMessage(ctx context.Context, accountID, remote string) (exists bool, rawComplete bool, err error) {
	return s.tx().HasMessage(ctx, accountID, remote)
}

func (tx *Tx) HasMessage(ctx context.Context, accountID, remote string) (bool, bool, error) {
	st, err := tx.MessageIndexState(ctx, accountID, remote)
	return st.Exists, st.RawComplete, err
}

// MessageIndexState reports presence, completeness and deletion in one query,
// so an enumeration can tell "already indexed, skip it" from "already indexed
// but locally deleted, resurrect it".
func (s *Store) MessageIndexState(ctx context.Context, accountID, remote string) (IndexState, error) {
	return s.tx().MessageIndexState(ctx, accountID, remote)
}

func (tx *Tx) MessageIndexState(ctx context.Context, accountID, remote string) (IndexState, error) {
	var complete int64
	var blob sql.NullString
	var deleted sql.NullInt64
	err := tx.q.QueryRowContext(ctx,
		`SELECT raw_complete, blob_sha256, deleted_at FROM messages
		  WHERE account_id = ? AND remote_id = ?`,
		accountID, remote).Scan(&complete, &blob, &deleted)
	if errors.Is(err, sql.ErrNoRows) {
		return IndexState{}, nil
	}
	if err != nil {
		return IndexState{}, fmt.Errorf("store: has message %s:%s: %w", accountID, remote, err)
	}
	return IndexState{
		Exists:      true,
		RawComplete: complete != 0 && blob.Valid && blob.String != "",
		Deleted:     deleted.Valid,
	}, nil
}

// ListRemoteIDs returns every remote id known for an account. Reconcile diffs
// this against the provider's id list.
func (s *Store) ListRemoteIDs(ctx context.Context, accountID string, includeDeleted bool) ([]string, error) {
	return s.tx().ListRemoteIDs(ctx, accountID, includeDeleted)
}

func (tx *Tx) ListRemoteIDs(ctx context.Context, accountID string, includeDeleted bool) ([]string, error) {
	q := `SELECT remote_id FROM messages WHERE account_id = ?`
	if !includeDeleted {
		q += ` AND deleted_at IS NULL`
	}
	rows, err := tx.q.QueryContext(ctx, q, accountID)
	if err != nil {
		return nil, fmt.Errorf("store: list remote ids %s: %w", accountID, err)
	}
	defer rows.Close()
	var out []string
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			return nil, err
		}
		out = append(out, id)
	}
	return out, rows.Err()
}

// membershipChunk bounds how many message ids go into one IN (...) clause, so
// an unbounded ListMessages cannot exceed SQLite's variable limit.
const membershipChunk = 400

// attachMailboxes fills MailboxRemotes on a batch of messages, one query per
// chunk of messages.
func (tx *Tx) attachMailboxes(ctx context.Context, msgs []model.Message) error {
	if len(msgs) == 0 {
		return nil
	}
	byID := make(map[int64]*model.Message, len(msgs))
	for i := range msgs {
		byID[msgs[i].ID] = &msgs[i]
	}
	for start := 0; start < len(msgs); start += membershipChunk {
		end := min(start+membershipChunk, len(msgs))
		ids := make([]any, 0, end-start)
		for _, m := range msgs[start:end] {
			ids = append(ids, m.ID)
		}
		if err := tx.attachMailboxChunk(ctx, byID, ids); err != nil {
			return err
		}
	}
	return nil
}

func (tx *Tx) attachMailboxChunk(ctx context.Context, byID map[int64]*model.Message, ids []any) error {
	rows, err := tx.q.QueryContext(ctx, `
		SELECT mm.message_id, mb.remote_id
		  FROM message_mailboxes mm JOIN mailboxes mb ON mb.id = mm.mailbox_id
		 WHERE mm.message_id IN (`+placeholders(len(ids))+`)
		 ORDER BY mm.message_id, mb.sort_order, mb.name`, ids...)
	if err != nil {
		return fmt.Errorf("store: attach mailboxes: %w", err)
	}
	defer rows.Close()
	for rows.Next() {
		var msgID int64
		var remote string
		if err := rows.Scan(&msgID, &remote); err != nil {
			return err
		}
		if m := byID[msgID]; m != nil {
			m.MailboxRemotes = append(m.MailboxRemotes, remote)
		}
	}
	return rows.Err()
}

// ---------------------------------------------------------------------------
// Filtering

// MessageFilter is the shared filter for list, search, thread and count
// queries. The zero value matches every non-deleted message.
type MessageFilter struct {
	// Accounts limits the query to these account ids; empty means all.
	Accounts []string
	// MailboxRole matches mailboxes.role exactly (case-insensitive):
	// "inbox", "sent", "category:promotions", …
	MailboxRole string
	// MailboxName matches mailboxes.name exactly (case-insensitive).
	MailboxName string
	// Unread / Flagged are tri-state: nil means "don't care".
	Unread  *bool
	Flagged *bool
	// From matches the sender address or display name (substring, case-insensitive).
	From string
	// To matches any To or Cc recipient (substring, case-insensitive).
	To string
	// Since/Until bound received time: [Since, Until).
	Since, Until time.Time
	// NoBulk excludes list mail, auto-submitted mail and bulk precedence.
	NoBulk bool
	// ThreadID limits to one provider thread.
	ThreadID string
	// IncludeDeleted keeps messages that are gone on the server.
	IncludeDeleted bool
	// Limit <= 0 means unlimited. Offset is applied after ordering.
	Limit, Offset int
}

// where builds the SQL predicate for the filter over the alias `m`.
func (f MessageFilter) where() (string, []any) {
	var cond []string
	var args []any

	if len(f.Accounts) > 0 {
		cond = append(cond, `m.account_id IN (`+placeholders(len(f.Accounts))+`)`)
		args = append(args, anySlice(f.Accounts)...)
	}
	if !f.IncludeDeleted {
		cond = append(cond, `m.deleted_at IS NULL`)
	}
	if f.MailboxRole != "" {
		cond = append(cond, `EXISTS (SELECT 1 FROM message_mailboxes mm
			JOIN mailboxes mb ON mb.id = mm.mailbox_id
			WHERE mm.message_id = m.id AND lower(mb.role) = ?)`)
		args = append(args, strings.ToLower(f.MailboxRole))
	}
	if f.MailboxName != "" {
		cond = append(cond, `EXISTS (SELECT 1 FROM message_mailboxes mm2
			JOIN mailboxes mb2 ON mb2.id = mm2.mailbox_id
			WHERE mm2.message_id = m.id AND lower(mb2.name) = ?)`)
		args = append(args, strings.ToLower(f.MailboxName))
	}
	if f.Unread != nil {
		cond = append(cond, `m.is_unread = ?`)
		args = append(args, boolInt(*f.Unread))
	}
	if f.Flagged != nil {
		cond = append(cond, `m.is_flagged = ?`)
		args = append(args, boolInt(*f.Flagged))
	}
	if f.From != "" {
		cond = append(cond, `(lower(coalesce(m.from_addr,'')) LIKE ? ESCAPE '\'
			OR lower(coalesce(m.from_name,'')) LIKE ? ESCAPE '\')`)
		args = append(args, likeContains(f.From), likeContains(f.From))
	}
	if f.To != "" {
		cond = append(cond, `(lower(coalesce(m.to_json,'')) LIKE ? ESCAPE '\'
			OR lower(coalesce(m.cc_json,'')) LIKE ? ESCAPE '\')`)
		args = append(args, likeContains(f.To), likeContains(f.To))
	}
	if !f.Since.IsZero() {
		cond = append(cond, `m.received_utc >= ?`)
		args = append(args, f.Since.Unix())
	}
	if !f.Until.IsZero() {
		cond = append(cond, `m.received_utc < ?`)
		args = append(args, f.Until.Unix())
	}
	if f.NoBulk {
		cond = append(cond, `m.is_bulk = 0`)
	}
	if f.ThreadID != "" {
		cond = append(cond, `m.thread_id = ?`)
		args = append(args, f.ThreadID)
	}
	if len(cond) == 0 {
		return "1=1", nil
	}
	return strings.Join(cond, " AND "), args
}

func (f MessageFilter) limitClause() (string, []any) {
	switch {
	case f.Limit > 0 && f.Offset > 0:
		return " LIMIT ? OFFSET ?", []any{f.Limit, f.Offset}
	case f.Limit > 0:
		return " LIMIT ?", []any{f.Limit}
	case f.Offset > 0:
		return " LIMIT -1 OFFSET ?", []any{f.Offset}
	}
	return "", nil
}

// ListMessages returns messages matching f, newest first. text_body is not
// loaded — use GetMessage for that.
func (s *Store) ListMessages(ctx context.Context, f MessageFilter) ([]model.Message, error) {
	return s.tx().ListMessages(ctx, f)
}

func (tx *Tx) ListMessages(ctx context.Context, f MessageFilter) ([]model.Message, error) {
	where, args := f.where()
	limit, limitArgs := f.limitClause()
	rows, err := tx.q.QueryContext(ctx,
		`SELECT `+messageCols+` FROM messages m WHERE `+where+
			` ORDER BY m.received_utc DESC, m.id DESC`+limit,
		append(args, limitArgs...)...)
	if err != nil {
		return nil, fmt.Errorf("store: list messages: %w", err)
	}
	defer rows.Close()
	var out []model.Message
	for rows.Next() {
		m, err := scanMessage(rows, false)
		if err != nil {
			return nil, err
		}
		out = append(out, m)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	if err := tx.attachMailboxes(ctx, out); err != nil {
		return nil, err
	}
	return out, nil
}

// CountMessages returns how many messages match f (ignoring Limit/Offset).
func (s *Store) CountMessages(ctx context.Context, f MessageFilter) (int, error) {
	return s.tx().CountMessages(ctx, f)
}

func (tx *Tx) CountMessages(ctx context.Context, f MessageFilter) (int, error) {
	where, args := f.where()
	var n int
	if err := tx.q.QueryRowContext(ctx,
		`SELECT count(*) FROM messages m WHERE `+where, args...).Scan(&n); err != nil {
		return 0, fmt.Errorf("store: count messages: %w", err)
	}
	return n, nil
}

// ---------------------------------------------------------------------------
// Attachments

// ListAttachments returns the attachment rows of one message (by local row id).
func (s *Store) ListAttachments(ctx context.Context, messageRowID int64) ([]model.Attachment, error) {
	return s.tx().ListAttachments(ctx, messageRowID)
}

func (tx *Tx) ListAttachments(ctx context.Context, messageRowID int64) ([]model.Attachment, error) {
	rows, err := tx.q.QueryContext(ctx, `
		SELECT id, message_id, part_path, filename, content_type, size, content_id, is_inline, remote_ref
		  FROM attachments WHERE message_id = ? ORDER BY id`, messageRowID)
	if err != nil {
		return nil, fmt.Errorf("store: list attachments: %w", err)
	}
	defer rows.Close()
	var out []model.Attachment
	for rows.Next() {
		var a model.Attachment
		var filename, ctype, cid, ref sql.NullString
		var size sql.NullInt64
		var inline int64
		if err := rows.Scan(&a.ID, &a.MessageID, &a.PartPath, &filename, &ctype, &size,
			&cid, &inline, &ref); err != nil {
			return nil, err
		}
		a.Filename = filename.String
		a.ContentType = ctype.String
		a.Size = size.Int64
		a.ContentID = cid.String
		a.Inline = inline != 0
		a.RemoteRef = ref.String
		out = append(out, a)
	}
	return out, rows.Err()
}

// SenderName is the display name an account's own mail goes out with, read
// off its newest sent message: what a model writing as the person should
// sign, rather than a surname guessed from the address. It is "" when the
// account has sent nothing, or signs with the bare address.
func (s *Store) SenderName(ctx context.Context, accountID, email string) string {
	for _, f := range []MessageFilter{
		{Accounts: []string{accountID}, MailboxRole: "sent", Limit: 10},
		{Accounts: []string{accountID}, From: email, Limit: 10},
	} {
		msgs, err := s.ListMessages(ctx, f)
		if err != nil {
			return ""
		}
		for i := range msgs {
			from := msgs[i].From
			if strings.EqualFold(strings.TrimSpace(from.Email), strings.TrimSpace(email)) && strings.TrimSpace(from.Name) != "" {
				return strings.TrimSpace(from.Name)
			}
		}
	}
	return ""
}
