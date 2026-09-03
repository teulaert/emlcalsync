package store

import (
	"context"
	"fmt"
	"sort"
	"strings"

	"github.com/teulaert/emlcalsync/internal/model"
)

// The address book is derived from the archive: message_addresses holds one
// row per address per message, contacts the summary per (account, address)
// that the composer and `contacts` read. See migrations/0006_contacts.sql for
// the shape and the reasoning.
//
// Everything here runs in SQL over the messages table rather than over the
// *model.Message in hand, so that indexing one message, rebuilding an
// account and the migration's backfill are the same statements with a
// different WHERE -- one place for the outbound rule, one for the name rule.

// outboundCase marks the messages the account wrote: a draft, one in the
// sent mailbox, or one carrying the account's own address on From. That is
// what ranks a person you write to above one who writes to you.
const outboundCase = `CASE WHEN (m.is_draft = 1
	OR lower(m.from_addr) = (SELECT lower(a.email) FROM accounts a WHERE a.id = m.account_id)
	OR EXISTS (SELECT 1 FROM message_mailboxes mm JOIN mailboxes mb ON mb.id = mm.mailbox_id
	            WHERE mm.message_id = m.id AND lower(mb.role) = 'sent')) THEN 1 ELSE 0 END`

// addressInserts are the statements that record who is on the messages
// matched by where. Reply-To is left out on purpose: that is where lists and
// ticket systems live. The From row is never outbound: on a message the
// account wrote, From is the account (or an alias of it), and writing is not
// being written to.
func addressInserts(where string) []string {
	head := `INSERT OR IGNORE INTO message_addresses (message_id, account_id, email, name, field, outbound, date_utc) `
	out := []string{head + `
		SELECT m.id, m.account_id, lower(trim(m.from_addr)), nullif(trim(m.from_name), ''), 'from',
		       0, m.date_utc
		  FROM messages m
		 WHERE ` + where + ` AND coalesce(trim(m.from_addr), '') <> ''`}
	for _, f := range []string{"to", "cc", "bcc"} {
		out = append(out, head+`
		SELECT m.id, m.account_id, lower(trim(json_extract(j.value, '$.email'))),
		       nullif(trim(json_extract(j.value, '$.name')), ''), '`+f+`',
		       `+outboundCase+`, m.date_utc
		  FROM messages m, json_each(m.`+f+`_json) j
		 WHERE `+where+` AND coalesce(trim(json_extract(j.value, '$.email')), '') <> ''`)
	}
	return out
}

// contactInsert is the summary over the fact rows matched by where. The name
// is the newest one on record, preferring what the person calls themself on
// From over what somebody typed into a To.
func contactInsert(where string) string {
	return `INSERT INTO contacts (account_id, email, name, sent_count, total_count, last_utc)
		SELECT a.account_id, a.email,
		       (SELECT n.name FROM message_addresses n
		         WHERE n.account_id = a.account_id AND n.email = a.email AND n.name IS NOT NULL
		         ORDER BY (n.field = 'from') DESC, n.date_utc DESC LIMIT 1),
		       count(DISTINCT CASE WHEN a.outbound = 1 THEN a.message_id END),
		       count(DISTINCT a.message_id),
		       max(a.date_utc)
		  FROM message_addresses a
		 WHERE ` + where + `
		 GROUP BY a.account_id, a.email`
}

// replaceAddresses makes message_addresses for one message match what the
// messages row now says, and refreshes the summary of everyone it named
// before or names now. It runs after the row and its memberships are
// written, since both feed the outbound rule.
func (tx *Tx) replaceAddresses(ctx context.Context, msgID int64, accountID string) error {
	touched, err := tx.scanEmails(ctx,
		`SELECT DISTINCT email FROM message_addresses WHERE message_id = ?`, msgID)
	if err != nil {
		return err
	}
	if _, err := tx.q.ExecContext(ctx, `DELETE FROM message_addresses WHERE message_id = ?`, msgID); err != nil {
		return fmt.Errorf("store: clear addresses: %w", err)
	}
	for _, stmt := range addressInserts(`m.id = ?`) {
		if _, err := tx.q.ExecContext(ctx, stmt, msgID); err != nil {
			return fmt.Errorf("store: record addresses: %w", err)
		}
	}
	now, err := tx.scanEmails(ctx,
		`SELECT DISTINCT email FROM message_addresses WHERE message_id = ?`, msgID)
	if err != nil {
		return err
	}
	for e := range now {
		touched[e] = true
	}
	for e := range touched {
		if _, err := tx.q.ExecContext(ctx,
			`DELETE FROM contacts WHERE account_id = ? AND email = ?`, accountID, e); err != nil {
			return fmt.Errorf("store: refresh contact: %w", err)
		}
		if _, err := tx.q.ExecContext(ctx,
			contactInsert(`a.account_id = ? AND a.email = ?`), accountID, e); err != nil {
			return fmt.Errorf("store: refresh contact: %w", err)
		}
	}
	return nil
}

func (tx *Tx) scanEmails(ctx context.Context, query string, args ...any) (map[string]bool, error) {
	rows, err := tx.q.QueryContext(ctx, query, args...)
	if err != nil {
		return nil, fmt.Errorf("store: read addresses: %w", err)
	}
	defer rows.Close()
	out := map[string]bool{}
	for rows.Next() {
		var e string
		if err := rows.Scan(&e); err != nil {
			return nil, err
		}
		out[e] = true
	}
	return out, rows.Err()
}

// RebuildContacts recomputes an account's address book from its messages.
// Indexing keeps the book current on its own; this is for after a purge,
// where rows leave the messages table without passing through UpsertMessage.
func (s *Store) RebuildContacts(ctx context.Context, accountID string) error {
	return s.Tx(ctx, func(tx *Tx) error {
		for _, stmt := range []string{
			`DELETE FROM contacts WHERE account_id = ?`,
			`DELETE FROM message_addresses WHERE account_id = ?`,
		} {
			if _, err := tx.q.ExecContext(ctx, stmt, accountID); err != nil {
				return fmt.Errorf("store: rebuild contacts: %w", err)
			}
		}
		for _, stmt := range addressInserts(`m.account_id = ?`) {
			if _, err := tx.q.ExecContext(ctx, stmt, accountID); err != nil {
				return fmt.Errorf("store: rebuild contacts: %w", err)
			}
		}
		if _, err := tx.q.ExecContext(ctx, contactInsert(`a.account_id = ?`), accountID); err != nil {
			return fmt.Errorf("store: rebuild contacts: %w", err)
		}
		return nil
	})
}

// ---------------------------------------------------------------------------
// Reading

// ContactFilter narrows SearchContacts. The zero value is the whole book.
type ContactFilter struct {
	// Accounts limits the query to these account ids; empty means all. The
	// book is merged across whichever are selected: one person, however
	// many accounts know them.
	Accounts []string
	// Query matches the address or the name (substring, case-insensitive).
	Query string
	// Limit <= 0 means unlimited.
	Limit int
}

// robotLocalParts are the senders nobody writes back to. They are kept out
// at read time rather than at indexing, so the list can change without a
// reindex. Each is matched as a prefix of the local part: noreply-1234@ is
// still noreply.
var robotLocalParts = []string{
	"noreply", "no-reply", "no_reply", "donotreply", "do-not-reply", "do_not_reply",
	"mailer-daemon", "postmaster", "bounce", "notification", "alerts", "newsletter",
}

// SearchContacts is the address book, ranked: the people the account writes
// to first, then by how often they turn up, then by how recently. The
// account's own addresses and the robots are left out.
func (s *Store) SearchContacts(ctx context.Context, f ContactFilter) ([]model.Contact, error) {
	return s.tx().SearchContacts(ctx, f)
}

func (tx *Tx) SearchContacts(ctx context.Context, f ContactFilter) ([]model.Contact, error) {
	cond := []string{`c.email NOT IN (SELECT lower(trim(email)) FROM accounts)`}
	var args []any
	if len(f.Accounts) > 0 {
		cond = append(cond, `c.account_id IN (`+placeholders(len(f.Accounts))+`)`)
		args = append(args, anySlice(f.Accounts)...)
	}
	for _, r := range robotLocalParts {
		cond = append(cond, `c.email NOT LIKE ? ESCAPE '\'`)
		args = append(args, likePrefix(r))
	}
	if q := strings.TrimSpace(f.Query); q != "" {
		cond = append(cond, `(c.email LIKE ? ESCAPE '\' OR lower(coalesce(c.name, '')) LIKE ? ESCAPE '\')`)
		args = append(args, likeContains(q), likeContains(q))
	}
	limit := ""
	if f.Limit > 0 {
		limit = ` LIMIT ?`
		args = append(args, f.Limit)
	}
	// name is a bare column beside a single max(): SQLite hands it back from
	// the row that holds the max, so the name is the one the most recently
	// active account has.
	rows, err := tx.q.QueryContext(ctx, `
		SELECT c.email, coalesce(c.name, ''), group_concat(c.account_id),
		       sum(c.sent_count), sum(c.total_count), max(c.last_utc)
		  FROM contacts c
		 WHERE `+strings.Join(cond, " AND ")+`
		 GROUP BY c.email
		 ORDER BY sum(c.sent_count) DESC, sum(c.total_count) DESC, max(c.last_utc) DESC, c.email`+limit,
		args...)
	if err != nil {
		return nil, fmt.Errorf("store: search contacts: %w", err)
	}
	defer rows.Close()
	var out []model.Contact
	for rows.Next() {
		var c model.Contact
		var accounts string
		var last int64
		if err := rows.Scan(&c.Email, &c.Name, &accounts, &c.SentCount, &c.Count, &last); err != nil {
			return nil, err
		}
		c.Accounts = strings.Split(accounts, ",")
		sort.Strings(c.Accounts)
		c.Last = timeOf(last)
		out = append(out, c)
	}
	return out, rows.Err()
}
