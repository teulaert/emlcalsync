package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"strings"

	"github.com/lennert/emlcal/internal/model"
)

// ErrBadQuery is returned when the FTS5 expression does not parse. The CLI
// maps it to a usage error rather than a generic failure, so agents get told
// their query was malformed instead of "database error".
var ErrBadQuery = errors.New("invalid search query")

// SearchHit is one FTS5 result.
type SearchHit struct {
	Message model.Message `json:"message"`
	// Rank is the bm25 score: lower (more negative) is a better match.
	Rank float64 `json:"rank"`
	// Highlight is a snippet of the body around the match, with the matching
	// terms wrapped in the markers passed to Search (default none).
	Highlight string `json:"highlight"`
}

// searchTextColumn is the 0-based index of text_body in messages_fts.
const searchTextColumn = 4

// Search runs an FTS5 MATCH combined with the same filters ListMessages
// accepts, ordered by bm25 relevance. The query is FTS5 syntax:
//
//	invoice                      a single term
//	invoice AND (acme OR globex) boolean
//	"quarterly report"           phrase
//	invo*                        prefix (2- and 3-character prefixes indexed)
//	subject:invoice              column filter (subject, from_addr, from_name,
//	                             to_json, text_body, attachment_names)
//
// A malformed query yields ErrBadQuery. text_body is not loaded into the hits;
// use GetMessage for the full body.
func (s *Store) Search(ctx context.Context, query string, f MessageFilter) ([]SearchHit, error) {
	return s.tx().Search(ctx, query, f)
}

func (tx *Tx) Search(ctx context.Context, query string, f MessageFilter) ([]SearchHit, error) {
	return tx.SearchHighlight(ctx, query, f, "", "")
}

// SearchHighlight is Search with explicit highlight markers around matching
// terms in the returned snippet (e.g. "\x1b[1m" / "\x1b[0m", or "**" / "**").
func (s *Store) SearchHighlight(ctx context.Context, query string, f MessageFilter, open, close string) ([]SearchHit, error) {
	return s.tx().SearchHighlight(ctx, query, f, open, close)
}

func (tx *Tx) SearchHighlight(ctx context.Context, query string, f MessageFilter, open, close string) ([]SearchHit, error) {
	q := strings.TrimSpace(query)
	if q == "" {
		return nil, fmt.Errorf("%w: empty query", ErrBadQuery)
	}
	where, args := f.where()
	limit, limitArgs := f.limitClause()

	sqlArgs := make([]any, 0, len(args)+len(limitArgs)+4)
	sqlArgs = append(sqlArgs, open, close, q)
	sqlArgs = append(sqlArgs, args...)
	sqlArgs = append(sqlArgs, limitArgs...)

	rows, err := tx.q.QueryContext(ctx, fmt.Sprintf(`
		SELECT %s, bm25(messages_fts) AS rank,
		       snippet(messages_fts, %d, ?, ?, '…', 24)
		  FROM messages_fts
		  JOIN messages m ON m.id = messages_fts.rowid
		 WHERE messages_fts MATCH ? AND %s
		 ORDER BY rank, m.received_utc DESC%s`,
		messageCols, searchTextColumn, where, limit), sqlArgs...)
	if err != nil {
		return nil, classifyFTSError(err, q)
	}
	defer rows.Close()

	var out []SearchHit
	for rows.Next() {
		var (
			r         messageRow
			rank      float64
			highlight sql.NullString
		)
		dest := append(r.dest(false), &rank, &highlight)
		if err := rows.Scan(dest...); err != nil {
			return nil, classifyFTSError(err, q)
		}
		out = append(out, SearchHit{Message: r.message(), Rank: rank, Highlight: highlight.String})
	}
	if err := rows.Err(); err != nil {
		return nil, classifyFTSError(err, q)
	}

	msgs := make([]model.Message, len(out))
	for i := range out {
		msgs[i] = out[i].Message
	}
	if err := tx.attachMailboxes(ctx, msgs); err != nil {
		return nil, err
	}
	for i := range out {
		out[i].Message.MailboxRemotes = msgs[i].MailboxRemotes
	}
	return out, nil
}

// CountSearch returns how many messages match the query and filter.
func (s *Store) CountSearch(ctx context.Context, query string, f MessageFilter) (int, error) {
	q := strings.TrimSpace(query)
	if q == "" {
		return 0, fmt.Errorf("%w: empty query", ErrBadQuery)
	}
	where, args := f.where()
	sqlArgs := append([]any{q}, args...)
	var n int
	err := s.db.QueryRowContext(ctx, `
		SELECT count(*) FROM messages_fts JOIN messages m ON m.id = messages_fts.rowid
		 WHERE messages_fts MATCH ? AND `+where, sqlArgs...).Scan(&n)
	if err != nil {
		return 0, classifyFTSError(err, q)
	}
	return n, nil
}

// classifyFTSError turns SQLite's parse failures into ErrBadQuery. Anything
// that is not recognisably a query problem is passed through as-is, so real
// bugs stay visible.
func classifyFTSError(err error, query string) error {
	if err == nil {
		return nil
	}
	msg := strings.ToLower(err.Error())
	for _, marker := range []string{
		"fts5", "syntax error", "unterminated", "malformed match",
		"no such column", "unknown special query", "unrecognized token",
	} {
		if strings.Contains(msg, marker) {
			return fmt.Errorf("%w %q: %v", ErrBadQuery, query, err)
		}
	}
	return fmt.Errorf("store: search: %w", err)
}

// QuoteFTS turns arbitrary user text into a single FTS5 phrase, so callers
// that do not want to expose query syntax (or that build queries from other
// programs' output) can search literally.
func QuoteFTS(s string) string {
	return `"` + strings.ReplaceAll(s, `"`, `""`) + `"`
}
