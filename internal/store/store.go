// Package store owns the SQLite database: opening it with the right driver and pragmas, applying
// the schema, and (from P2.2) the transactional write helpers.
//
// BUILD TAG: this package, and therefore the kb binary, must be built with -tags fts5:
//
//	go build -tags fts5 ./...
//
// github.com/mattn/go-sqlite3 compiles FTS5 into its bundled SQLite only under that tag. Without
// it the schema's chunks_fts virtual table cannot be created, so Open checks
// PRAGMA compile_options for ENABLE_FTS5 and fails with that instruction rather than with a
// confusing "no such module: fts5".
//
// sqlite-vec is registered as an auto-extension with sqlite_vec.Auto() before the first
// sql.Open, which is what makes the vec0 module available on every connection.
package store

import (
	"context"
	"database/sql"
	_ "embed"
	"fmt"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	sqlite_vec "github.com/asg017/sqlite-vec-go-bindings/cgo"
	_ "github.com/mattn/go-sqlite3"
)

// schemaSQL is the full schema from plan.md, applied verbatim on a fresh database. It is
// migrations[0] and must never be edited once a database exists in the wild: a change to it would
// only reach brand-new databases.
//
//go:embed schema.sql
var schemaSQL string

// migrations[v] takes a database from PRAGMA user_version v to v+1. Index 0 is the whole schema,
// which is why a fresh (version 0) database is migrated by running it.
//
// TO CHANGE THE SCHEMA: append a new function to this slice — an ALTER TABLE, a CREATE INDEX, a
// backfill — and leave every earlier element alone. schemaVersion follows automatically. Do not
// edit schema.sql instead: an existing database is never re-run from the top, so an edit there
// would silently apply to new databases only. (Adding the new statements to *both* is correct
// only if schema.sql stays equivalent to replaying every migration in order.)
var migrations = []func(execer) error{
	func(db execer) error {
		_, err := db.Exec(schemaSQL)
		return err
	},
}

// schemaVersion is the PRAGMA user_version this build expects: one per migration.
var schemaVersion = len(migrations)

// execer is the subset of *sql.Conn / *sql.DB / *sql.Tx a migration needs. Migrations run on a
// connection that already holds a write transaction, so they must not BEGIN or COMMIT.
type execer interface {
	Exec(query string, args ...any) (sql.Result, error)
}

// connExecer adapts *sql.Conn, which only offers the ctx-taking methods, to execer.
type connExecer struct {
	ctx  context.Context
	conn *sql.Conn
}

func (c connExecer) Exec(query string, args ...any) (sql.Result, error) {
	return c.conn.ExecContext(c.ctx, query, args...)
}

// vecOnce registers the sqlite-vec auto-extension exactly once per process, before any connection
// is opened.
var vecOnce sync.Once

// Store is one open knowledge database.
type Store struct {
	db   *sql.DB
	path string
}

// Open opens (creating it if necessary) the database at path, sets the connection pragmas
// journal_mode=WAL, foreign_keys=ON and busy_timeout=5000, verifies that the driver has FTS5 and
// sqlite-vec, and applies the schema if the file is new.
func Open(path string) (*Store, error) {
	vecOnce.Do(sqlite_vec.Auto)

	if dir := filepath.Dir(path); dir != "" && dir != "." {
		if err := os.MkdirAll(dir, 0o755); err != nil {
			return nil, fmt.Errorf("create database directory %s: %w", dir, err)
		}
	}

	// foreign_keys and busy_timeout are per-CONNECTION settings and reset to their defaults on
	// every new connection, so they go in the DSN: database/sql hands out whichever pooled
	// connection is free, and one without ON DELETE CASCADE would silently orphan chunks.
	// journal_mode is deliberately NOT here — see enableWAL.
	uri := url.URL{Scheme: "file", Path: path}
	dsn := uri.String() + "?_foreign_keys=ON&_busy_timeout=5000"
	db, err := sql.Open("sqlite3", dsn)
	if err != nil {
		return nil, fmt.Errorf("open %s: %w", path, err)
	}
	if err := db.Ping(); err != nil {
		db.Close()
		return nil, fmt.Errorf("open %s: %w", path, err)
	}

	s := &Store{db: db, path: path}
	if err := s.enableWAL(); err != nil {
		db.Close()
		return nil, err
	}
	if err := s.checkFTS5(); err != nil {
		db.Close()
		return nil, err
	}
	if err := s.checkVec(); err != nil {
		db.Close()
		return nil, err
	}
	if err := s.migrate(); err != nil {
		db.Close()
		return nil, err
	}
	return s, nil
}

// DB exposes the underlying handle for the packages that run their own queries.
func (s *Store) DB() *sql.DB { return s.db }

// Path is the file the store was opened from.
func (s *Store) Path() string { return s.path }

// Close closes the database.
func (s *Store) Close() error {
	if s == nil || s.db == nil {
		return nil
	}
	return s.db.Close()
}

// enableWAL switches the database file to write-ahead logging.
//
// Unlike foreign_keys and busy_timeout, the journal mode is a persistent property of the FILE,
// recorded in its header, so it has to be set once rather than on every connection. It must not
// go in the DSN either: entering WAL mode needs a brief exclusive lock, and SQLite returns
// SQLITE_BUSY for that transition *without consulting the busy handler*, so several processes
// opening the same brand-new database at once would fail at connect time with "database is
// locked" — busy_timeout cannot help. Retrying here is what makes concurrent first opens safe;
// once the file is in WAL mode the pragma is a lock-free no-op for everyone else.
func (s *Store) enableWAL() error {
	const attempts = 50
	var lastErr error
	for i := 0; i < attempts; i++ {
		var mode string
		err := s.db.QueryRow("PRAGMA journal_mode = WAL").Scan(&mode)
		switch {
		case err == nil && strings.EqualFold(mode, "wal"):
			return nil
		case err == nil:
			lastErr = fmt.Errorf("journal_mode is %q after asking for WAL", mode)
		default:
			lastErr = err
		}
		time.Sleep(10 * time.Millisecond)
	}
	return fmt.Errorf("enable WAL on %s: %w", s.path, lastErr)
}

// checkFTS5 fails with an actionable message when the driver was built without the fts5 tag.
func (s *Store) checkFTS5() error {
	rows, err := s.db.Query("PRAGMA compile_options")
	if err != nil {
		return fmt.Errorf("read compile options: %w", err)
	}
	defer rows.Close()
	for rows.Next() {
		var opt string
		if err := rows.Scan(&opt); err != nil {
			return fmt.Errorf("read compile options: %w", err)
		}
		if strings.Contains(opt, "ENABLE_FTS5") {
			return rows.Err()
		}
	}
	if err := rows.Err(); err != nil {
		return fmt.Errorf("read compile options: %w", err)
	}
	return fmt.Errorf("this kb binary was built without FTS5: rebuild it with `go build -tags fts5 ./cmd/kb`")
}

// checkVec fails when the sqlite-vec extension did not load.
func (s *Store) checkVec() error {
	var version string
	if err := s.db.QueryRow("SELECT vec_version()").Scan(&version); err != nil {
		return fmt.Errorf("sqlite-vec is not loaded (vec_version() failed): %w", err)
	}
	return nil
}

// migrate brings the database up to schemaVersion by running the outstanding migrations.
//
// Concurrency: two processes (or two goroutines) opening the same fresh database race to create
// it, so the whole read-decide-apply sequence happens inside a single BEGIN IMMEDIATE transaction
// on one pinned connection. IMMEDIATE takes the write lock up front instead of at the first
// write, so the loser blocks here (busy_timeout=5000) rather than failing with SQLITE_BUSY
// halfway through; user_version is then re-read under that lock, at which point the loser sees
// the winner's work and does nothing. Reading user_version before the transaction is only a
// fast path for the overwhelmingly common case of an already-current database.
//
// A database at a newer version is refused rather than silently misread.
func (s *Store) migrate() error {
	ctx := context.Background()
	conn, err := s.db.Conn(ctx)
	if err != nil {
		return fmt.Errorf("open migration connection: %w", err)
	}
	defer conn.Close()

	version, err := userVersion(ctx, conn)
	if err != nil {
		return err
	}
	if err := s.checkVersion(version); err != nil {
		return err
	}
	if version == schemaVersion {
		return nil
	}

	if _, err := conn.ExecContext(ctx, "BEGIN IMMEDIATE"); err != nil {
		return fmt.Errorf("lock %s for migration: %w", s.path, err)
	}
	committed := false
	defer func() {
		if !committed {
			conn.ExecContext(ctx, "ROLLBACK")
		}
	}()

	// Re-read under the write lock: another opener may have migrated while we waited for it.
	version, err = userVersion(ctx, conn)
	if err != nil {
		return err
	}
	if err := s.checkVersion(version); err != nil {
		return err
	}
	db := connExecer{ctx: ctx, conn: conn}
	for v := version; v < schemaVersion; v++ {
		if err := migrations[v](db); err != nil {
			return fmt.Errorf("migrate %s from schema version %d to %d: %w", s.path, v, v+1, err)
		}
		// user_version lives in the database header and is part of the transaction, so it
		// cannot drift from the statements above. It takes no parameters.
		if _, err := conn.ExecContext(ctx, fmt.Sprintf("PRAGMA user_version = %d", v+1)); err != nil {
			return fmt.Errorf("stamp schema version %d: %w", v+1, err)
		}
	}
	if _, err := conn.ExecContext(ctx, "COMMIT"); err != nil {
		return fmt.Errorf("commit migration of %s: %w", s.path, err)
	}
	committed = true
	return nil
}

// checkVersion refuses a database written by a newer kb.
func (s *Store) checkVersion(version int) error {
	if version > schemaVersion {
		return fmt.Errorf("database %s is at schema version %d, this kb only understands %d", s.path, version, schemaVersion)
	}
	return nil
}

// userVersion reads PRAGMA user_version on a specific connection.
func userVersion(ctx context.Context, conn *sql.Conn) (int, error) {
	var version int
	if err := conn.QueryRowContext(ctx, "PRAGMA user_version").Scan(&version); err != nil {
		return 0, fmt.Errorf("read user_version: %w", err)
	}
	return version, nil
}

// The write helpers — ReplaceEntryChunks, DeleteEntry, the embed_meta check and the entry/chunk
// read helpers — live in entries.go.
