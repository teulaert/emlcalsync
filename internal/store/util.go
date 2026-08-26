package store

import (
	"database/sql"
	"encoding/json"
	"fmt"
	"strings"
	"time"

	"github.com/teulaert/emlcalsync/internal/model"
)

// ---------------------------------------------------------------------------
// Time. Everything is unix seconds; the zero time.Time round-trips as 0.

func unixOf(t time.Time) int64 {
	if t.IsZero() {
		return 0
	}
	return t.Unix()
}

func timeOf(sec int64) time.Time {
	if sec == 0 {
		return time.Time{}
	}
	return time.Unix(sec, 0).UTC()
}

func nullUnix(t *time.Time) any {
	if t == nil || t.IsZero() {
		return nil
	}
	return t.Unix()
}

func timePtr(n sql.NullInt64) *time.Time {
	if !n.Valid {
		return nil
	}
	t := time.Unix(n.Int64, 0).UTC()
	return &t
}

func nullTime(n sql.NullInt64) time.Time {
	if !n.Valid {
		return time.Time{}
	}
	return timeOf(n.Int64)
}

// ---------------------------------------------------------------------------
// Strings / JSON

func nullStr(s string) any {
	if s == "" {
		return nil
	}
	return s
}

func boolInt(b bool) int64 {
	if b {
		return 1
	}
	return 0
}

// marshalAddrs encodes an address list, returning NULL for an empty list.
func marshalAddrs(a []model.Address) (any, error) {
	if len(a) == 0 {
		return nil, nil
	}
	b, err := json.Marshal(a)
	if err != nil {
		return nil, fmt.Errorf("store: marshal addresses: %w", err)
	}
	return string(b), nil
}

func unmarshalAddrs(n sql.NullString) []model.Address {
	if !n.Valid || n.String == "" {
		return nil
	}
	var out []model.Address
	if err := json.Unmarshal([]byte(n.String), &out); err != nil {
		return nil
	}
	return out
}

func marshalStrings(v []string) (any, error) {
	if len(v) == 0 {
		return nil, nil
	}
	b, err := json.Marshal(v)
	if err != nil {
		return nil, fmt.Errorf("store: marshal strings: %w", err)
	}
	return string(b), nil
}

func unmarshalStrings(n sql.NullString) []string {
	if !n.Valid || n.String == "" {
		return nil
	}
	var out []string
	if err := json.Unmarshal([]byte(n.String), &out); err != nil {
		return nil
	}
	return out
}

// ---------------------------------------------------------------------------
// SQL fragment helpers

// placeholders returns "?,?,?" for n.
func placeholders(n int) string {
	if n <= 0 {
		return ""
	}
	return strings.TrimSuffix(strings.Repeat("?,", n), ",")
}

func anySlice[T any](v []T) []any {
	out := make([]any, len(v))
	for i, x := range v {
		out[i] = x
	}
	return out
}

// likeContains builds the argument for a case-insensitive substring match.
// SQLite's LIKE is already case-insensitive for ASCII; lower() on both sides
// extends that to the rest of Latin-1 as far as SQLite goes.
func likeContains(s string) string {
	s = strings.ReplaceAll(s, `\`, `\\`)
	s = strings.ReplaceAll(s, `%`, `\%`)
	s = strings.ReplaceAll(s, `_`, `\_`)
	return "%" + strings.ToLower(s) + "%"
}

// likePrefix builds the argument for a case-insensitive prefix match.
func likePrefix(s string) string {
	return strings.TrimSuffix(strings.TrimPrefix(likeContains(s), "%"), "%") + "%"
}

// lowerTrim normalises user-supplied names for exact, case-insensitive matching.
func lowerTrim(s string) string { return strings.ToLower(strings.TrimSpace(s)) }

// notFound decorates model.ErrNotFound with what was being looked for.
func notFound(format string, args ...any) error {
	return fmt.Errorf("%s: %w", fmt.Sprintf(format, args...), model.ErrNotFound)
}
