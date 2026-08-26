// Package store is the SQLite index described in DESIGN.md §5: open/migrate,
// hand-written typed queries, FTS5 search helpers.
//
// The index is derived data — it can always be rebuilt from the blob archive —
// so schema changes are allowed to be destructive as long as `emlcal reindex`
// can repopulate them.
//
// Layering: every query lives on *Tx, and the *Store methods are thin wrappers
// that run the same code either directly on the *sql.DB (single-statement
// reads) or inside a transaction (anything that touches more than one table).
// This lets the sync engine batch a set of changes and advance the sync state
// atomically:
//
//	err := st.Tx(ctx, func(tx *store.Tx) error {
//	    for _, m := range batch { if _, err := tx.UpsertMessage(ctx, m, p); err != nil { return err } }
//	    return tx.SetState(ctx, "work", "mail", newState)
//	})
package store

import (
	"context"
	"database/sql"
	"embed"
	"fmt"
	"io/fs"
	"log/slog"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"time"

	_ "modernc.org/sqlite"
)

//go:embed migrations/*.sql
var migrationFS embed.FS

// Store is a handle on the SQLite index. It is safe for concurrent use.
type Store struct {
	db   *sql.DB
	path string
	log  *slog.Logger
}

// dbtx is the subset of *sql.DB / *sql.Tx the queries need.
type dbtx interface {
	ExecContext(ctx context.Context, query string, args ...any) (sql.Result, error)
	QueryContext(ctx context.Context, query string, args ...any) (*sql.Rows, error)
	QueryRowContext(ctx context.Context, query string, args ...any) *sql.Row
}

// Tx carries the query methods. A Tx obtained from Store.Tx runs inside a
// SQLite transaction; the one Store uses internally for single-statement work
// does not.
type Tx struct {
	q   dbtx
	log *slog.Logger

	// mailbox remote_id -> row id, per account. Populated lazily and only
	// valid for the lifetime of one Tx.
	mbCache map[string]map[string]int64
}

// Open opens (creating if needed) the index at path and applies all pending
// migrations. path may be ":memory:" for tests, in which case the pool is
// pinned to a single connection so the database does not vanish.
func Open(path string) (*Store, error) {
	memory := path == ":memory:" || strings.Contains(path, "mode=memory")

	var dsn string
	switch {
	case memory:
		name := path
		if path == ":memory:" {
			name = "emlcalmem" + strconv.FormatInt(time.Now().UnixNano(), 36)
		}
		dsn = "file:" + name + "?mode=memory&cache=shared" + pragmas
	default:
		abs, err := filepath.Abs(path)
		if err != nil {
			return nil, fmt.Errorf("store: %w", err)
		}
		dsn = "file:" + abs + "?" + "_txlock=immediate" + pragmas
	}

	db, err := sql.Open("sqlite", dsn)
	if err != nil {
		return nil, fmt.Errorf("store: open %s: %w", path, err)
	}
	if memory {
		db.SetMaxOpenConns(1)
	} else {
		db.SetMaxOpenConns(8)
		db.SetMaxIdleConns(4)
		db.SetConnMaxIdleTime(5 * time.Minute)
	}

	s := &Store{db: db, path: path, log: slog.Default()}
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	if err := db.PingContext(ctx); err != nil {
		db.Close()
		return nil, fmt.Errorf("store: open %s: %w", path, err)
	}
	if err := s.migrate(ctx); err != nil {
		db.Close()
		return nil, err
	}
	return s, nil
}

// pragmas is appended to every DSN; modernc applies them per connection.
const pragmas = "&_txlock=immediate&_pragma=busy_timeout(30000)" +
	"&_pragma=journal_mode(WAL)" +
	"&_pragma=synchronous(NORMAL)" +
	"&_pragma=foreign_keys(1)"

// Close closes the underlying database.
func (s *Store) Close() error {
	if s == nil || s.db == nil {
		return nil
	}
	return s.db.Close()
}

// DB exposes the raw handle. Use sparingly — for `doctor`-style diagnostics.
func (s *Store) DB() *sql.DB { return s.db }

// Path returns the path Open was called with.
func (s *Store) Path() string { return s.path }

// SetLogger replaces the logger used for non-fatal indexing warnings
// (e.g. a message referencing a mailbox we have never seen).
func (s *Store) SetLogger(l *slog.Logger) {
	if l != nil {
		s.log = l
	}
}

// tx returns a non-transactional query handle.
func (s *Store) tx() *Tx { return &Tx{q: s.db, log: s.log} }

// Tx runs fn inside a transaction, committing when it returns nil and rolling
// back (preserving fn's error) otherwise. Nested calls are not supported.
func (s *Store) Tx(ctx context.Context, fn func(tx *Tx) error) error {
	sqlTx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("store: begin: %w", err)
	}
	tx := &Tx{q: sqlTx, log: s.log}
	defer func() {
		if p := recover(); p != nil {
			_ = sqlTx.Rollback()
			panic(p)
		}
	}()
	if err := fn(tx); err != nil {
		_ = sqlTx.Rollback()
		return err
	}
	if err := sqlTx.Commit(); err != nil {
		return fmt.Errorf("store: commit: %w", err)
	}
	return nil
}

// ---------------------------------------------------------------------------
// Migrations

type migration struct {
	version int
	name    string
	sql     string
}

func loadMigrations() ([]migration, error) {
	entries, err := fs.ReadDir(migrationFS, "migrations")
	if err != nil {
		return nil, err
	}
	var out []migration
	for _, e := range entries {
		if e.IsDir() || !strings.HasSuffix(e.Name(), ".sql") {
			continue
		}
		name := e.Name()
		numPart, rest, ok := strings.Cut(strings.TrimSuffix(name, ".sql"), "_")
		if !ok {
			return nil, fmt.Errorf("store: migration %q: want NNNN_name.sql", name)
		}
		v, err := strconv.Atoi(numPart)
		if err != nil {
			return nil, fmt.Errorf("store: migration %q: %w", name, err)
		}
		body, err := migrationFS.ReadFile("migrations/" + name)
		if err != nil {
			return nil, err
		}
		out = append(out, migration{version: v, name: rest, sql: string(body)})
	}
	sort.Slice(out, func(i, j int) bool { return out[i].version < out[j].version })
	for i := 1; i < len(out); i++ {
		if out[i].version == out[i-1].version {
			return nil, fmt.Errorf("store: duplicate migration version %d", out[i].version)
		}
	}
	return out, nil
}

func (s *Store) migrate(ctx context.Context) error {
	if _, err := s.db.ExecContext(ctx, `CREATE TABLE IF NOT EXISTS schema_migrations (
		version    INTEGER PRIMARY KEY,
		name       TEXT NOT NULL,
		applied_at INTEGER NOT NULL
	)`); err != nil {
		return fmt.Errorf("store: schema_migrations: %w", err)
	}

	applied := map[int]bool{}
	rows, err := s.db.QueryContext(ctx, `SELECT version FROM schema_migrations`)
	if err != nil {
		return fmt.Errorf("store: read schema_migrations: %w", err)
	}
	for rows.Next() {
		var v int
		if err := rows.Scan(&v); err != nil {
			rows.Close()
			return err
		}
		applied[v] = true
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return err
	}

	migs, err := loadMigrations()
	if err != nil {
		return err
	}
	for _, m := range migs {
		if applied[m.version] {
			continue
		}
		tx, err := s.db.BeginTx(ctx, nil)
		if err != nil {
			return fmt.Errorf("store: migration %04d: %w", m.version, err)
		}
		if _, err := tx.ExecContext(ctx, m.sql); err != nil {
			_ = tx.Rollback()
			return fmt.Errorf("store: migration %04d_%s: %w", m.version, m.name, err)
		}
		if _, err := tx.ExecContext(ctx,
			`INSERT INTO schema_migrations(version, name, applied_at) VALUES (?,?,?)`,
			m.version, m.name, time.Now().Unix()); err != nil {
			_ = tx.Rollback()
			return fmt.Errorf("store: migration %04d_%s: %w", m.version, m.name, err)
		}
		if err := tx.Commit(); err != nil {
			return fmt.Errorf("store: migration %04d_%s: %w", m.version, m.name, err)
		}
		s.log.Debug("applied migration", "version", m.version, "name", m.name)
	}
	return nil
}

// SchemaVersion returns the highest applied migration version.
func (s *Store) SchemaVersion(ctx context.Context) (int, error) {
	var v sql.NullInt64
	if err := s.db.QueryRowContext(ctx, `SELECT max(version) FROM schema_migrations`).Scan(&v); err != nil {
		return 0, err
	}
	return int(v.Int64), nil
}

// ---------------------------------------------------------------------------
// Maintenance

// ReferencedBlobs calls fn once for every distinct blob sha referenced by a
// message row (including soft-deleted ones). `emlcal gc` diffs this against
// blob.Store.Walk.
func (s *Store) ReferencedBlobs(ctx context.Context, fn func(sha string) error) error {
	rows, err := s.db.QueryContext(ctx,
		`SELECT DISTINCT blob_sha256 FROM messages WHERE blob_sha256 IS NOT NULL AND blob_sha256 <> ''`)
	if err != nil {
		return err
	}
	defer rows.Close()
	for rows.Next() {
		var sha string
		if err := rows.Scan(&sha); err != nil {
			return err
		}
		if err := fn(sha); err != nil {
			return err
		}
	}
	return rows.Err()
}

// LiveBlobs is like ReferencedBlobs but skips rows marked deleted; it is what
// `gc --purge-deleted` uses.
func (s *Store) LiveBlobs(ctx context.Context, fn func(sha string) error) error {
	rows, err := s.db.QueryContext(ctx,
		`SELECT DISTINCT blob_sha256 FROM messages
		  WHERE blob_sha256 IS NOT NULL AND blob_sha256 <> '' AND deleted_at IS NULL`)
	if err != nil {
		return err
	}
	defer rows.Close()
	for rows.Next() {
		var sha string
		if err := rows.Scan(&sha); err != nil {
			return err
		}
		if err := fn(sha); err != nil {
			return err
		}
	}
	return rows.Err()
}

// Vacuum compacts the database file. It cannot run inside a transaction.
func (s *Store) Vacuum(ctx context.Context) error {
	if _, err := s.db.ExecContext(ctx, `VACUUM`); err != nil {
		return fmt.Errorf("store: vacuum: %w", err)
	}
	if _, err := s.db.ExecContext(ctx, `PRAGMA wal_checkpoint(TRUNCATE)`); err != nil {
		return fmt.Errorf("store: checkpoint: %w", err)
	}
	return nil
}

// Optimize runs SQLite's recommended pre-close maintenance plus an FTS5 merge.
func (s *Store) Optimize(ctx context.Context) error {
	if _, err := s.db.ExecContext(ctx, `INSERT INTO messages_fts(messages_fts) VALUES('optimize')`); err != nil {
		return fmt.Errorf("store: fts optimize: %w", err)
	}
	if _, err := s.db.ExecContext(ctx, `PRAGMA optimize`); err != nil {
		return fmt.Errorf("store: optimize: %w", err)
	}
	return nil
}

// IntegrityCheck runs PRAGMA integrity_check, PRAGMA foreign_key_check and the
// FTS5 integrity check. It returns "ok" when everything is clean, otherwise a
// human-readable multi-line report (with a nil error — the check succeeded,
// its findings are the result).
func (s *Store) IntegrityCheck(ctx context.Context) (string, error) {
	var problems []string

	rows, err := s.db.QueryContext(ctx, `PRAGMA integrity_check`)
	if err != nil {
		return "", fmt.Errorf("store: integrity_check: %w", err)
	}
	for rows.Next() {
		var line string
		if err := rows.Scan(&line); err != nil {
			rows.Close()
			return "", err
		}
		if line != "ok" {
			problems = append(problems, "integrity_check: "+line)
		}
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return "", err
	}

	fkRows, err := s.db.QueryContext(ctx, `PRAGMA foreign_key_check`)
	if err != nil {
		return "", fmt.Errorf("store: foreign_key_check: %w", err)
	}
	for fkRows.Next() {
		var table, parent sql.NullString
		var rowid sql.NullInt64
		var fkid sql.NullInt64
		if err := fkRows.Scan(&table, &rowid, &parent, &fkid); err != nil {
			fkRows.Close()
			return "", err
		}
		problems = append(problems, fmt.Sprintf("foreign_key_check: %s rowid %d -> %s",
			table.String, rowid.Int64, parent.String))
	}
	fkRows.Close()
	if err := fkRows.Err(); err != nil {
		return "", err
	}

	if _, err := s.db.ExecContext(ctx, `INSERT INTO messages_fts(messages_fts) VALUES('integrity-check')`); err != nil {
		problems = append(problems, "messages_fts: "+err.Error())
	}

	if len(problems) == 0 {
		return "ok", nil
	}
	return strings.Join(problems, "\n"), nil
}

// Reindex clears and rebuilds the FTS index from the messages table. Cheaper
// than a full `emlcal reindex` when only the search index is suspect.
func (s *Store) RebuildFTS(ctx context.Context) error {
	if _, err := s.db.ExecContext(ctx, `INSERT INTO messages_fts(messages_fts) VALUES('rebuild')`); err != nil {
		return fmt.Errorf("store: fts rebuild: %w", err)
	}
	return nil
}
