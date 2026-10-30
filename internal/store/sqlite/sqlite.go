// Package sqlite is kb's storage backend: SQLite with FTS5 and sqlite-vec. It implements
// store.Store (index, retrieval, search log) and owns every SQL statement, PRAGMA and extension
// kb uses.
//
// Inputs: a database path and Options. Outputs: *Store. Invariants: an entry's rows in chunks,
// chunks_fts, chunks_exact and chunks_vec always move together in one transaction (asserted
// before commit); the schema version is checked in exactly one place (checkVersion, from Open and
// Artefact); migrations are an append-only migrate.Steps (migrations.go).
//
// BUILD TAG: build with -tags fts5. mattn/go-sqlite3 compiles FTS5 in only under that tag, so
// Open checks PRAGMA compile_options and fails with that instruction. sqlite-vec is registered as
// an auto-extension before the first connection.
//
// Errors: every exported method returns an *errs.Error. A damaged file or a lock held past the
// busy timeout is a Conflict, a file kb cannot open is Usage; other driver failures are Internal.
package sqlite

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"io/fs"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	sqlite_vec "github.com/asg017/sqlite-vec-go-bindings/cgo"
	sqlite3 "github.com/mattn/go-sqlite3"

	"github.com/nickstrad/kb/internal/errs"
	"github.com/nickstrad/kb/internal/migrate"
	"github.com/nickstrad/kb/internal/store"
)

// vecOnce registers the sqlite-vec auto-extension once per process, before any connection.
var vecOnce sync.Once

// driverName is the database/sql driver Open uses unless Options.driver overrides it (tests
// register a fault-injecting wrapper).
const driverName = "sqlite3"

// Options configure Open.
type Options struct {
	// ReadOnly opens an existing database without creating, migrating or writing it. A missing
	// file is NotFound; a schema version other than this build's is a Conflict.
	ReadOnly bool

	driver    string                // test seam: a registered database/sql driver name; "" = driverName
	noMigrate bool                  // Artefact: open without checking or migrating the schema version
	steps     migrate.Steps[execer] // test seam: the migration steps this build has; nil = indexSteps
}

// Schema versions on open (PLAN 1.8b, D10): a read-write open creates a missing or empty
// (version 0) database at this build's version, since that is creation, not migration. It never
// upgrades an older one: the index holds the search log, so only `kb migrate up` (Artefact.Apply,
// which backs it up first) does. Every open refuses a newer one.

// Store is one open knowledge database.
type Store struct {
	db       *sql.DB
	path     string
	readOnly bool
	steps    migrate.Steps[execer] // the build's steps; their Target is the schema version it expects
}

var _ store.Store = (*Store)(nil)

// Open opens the database at path. Read-write (the default) creates the file and its directory
// if needed, switches the file to WAL and brings a new (version 0) database to this build's
// schema; an older database is a Conflict that says "run kb migrate up". Options.ReadOnly does
// none of that.
func Open(ctx context.Context, path string, opts Options) (*Store, error) {
	s, err := open(ctx, path, opts)
	if err != nil {
		return nil, wrap(ctx, err, path, "sqlite.Open", "could not open the database %s", path)
	}
	return s, nil
}

func open(ctx context.Context, path string, opts Options) (*Store, error) {
	vecOnce.Do(sqlite_vec.Auto)
	drv := opts.driver
	if drv == "" {
		drv = driverName
	}

	// foreign_keys and busy_timeout are per-connection, so they go in the DSN: a pooled connection
	// without ON DELETE CASCADE would orphan chunks. journal_mode is per-file and must not go here
	// (see enableWAL). _txlock=immediate makes every BeginTx a BEGIN IMMEDIATE: a deferred
	// transaction that upgrades to a write lock gets SQLITE_BUSY at once, without busy_timeout,
	// when another process committed in between (knowledge: sqlite-deferred-tx-locked.md).
	params := "?_foreign_keys=ON&_busy_timeout=5000&_txlock=immediate"
	if opts.ReadOnly {
		if _, err := os.Stat(path); err != nil {
			if errors.Is(err, fs.ErrNotExist) {
				return nil, errs.Wrap(err, errs.NotFound, "sqlite.Open", "no database at %s; run `kb reindex --all` first", path)
			}
			return nil, errs.Wrap(err, errs.Usage, "sqlite.Open", "cannot read the database %s: check its permissions", path)
		}
		// mode=ro refuses to create the file; _query_only also blocks writes through ATTACH.
		params = "?mode=ro&_query_only=true&_busy_timeout=5000"
	} else if dir := filepath.Dir(path); dir != "" && dir != "." {
		if err := os.MkdirAll(dir, 0o755); err != nil {
			return nil, errs.Wrap(err, errs.Usage, "sqlite.Open", "could not create the database directory %s", dir)
		}
	}

	db, err := sql.Open(drv, (&url.URL{Scheme: "file", Path: path}).String()+params)
	if err != nil {
		return nil, err
	}
	s := &Store{db: db, path: path, readOnly: opts.ReadOnly, steps: stepsOr(opts.steps)}
	if err := s.init(ctx, opts.noMigrate); err != nil {
		db.Close()
		return nil, err
	}
	return s, nil
}

// init checks the driver and creates a new schema (read-write) or checks the version (read-only).
// noMigrate skips the version check and the migration: the Artefact adapter does those itself.
func (s *Store) init(ctx context.Context, noMigrate bool) error {
	if err := s.db.PingContext(ctx); err != nil {
		return err
	}
	if !s.readOnly && !noMigrate {
		// Refuse an older or newer file before anything is written: switching it to WAL below
		// would already change a database kb is about to leave alone.
		var version int
		if err := s.db.QueryRowContext(ctx, "PRAGMA user_version").Scan(&version); err != nil {
			return fmt.Errorf("read user_version: %w", err)
		}
		if err := s.checkVersion(version, false); err != nil {
			return err
		}
	}
	if !s.readOnly {
		if err := s.enableWAL(ctx); err != nil {
			return err
		}
	}
	if err := s.checkFTS5(ctx); err != nil {
		return err
	}
	if err := s.checkVec(ctx); err != nil {
		return err
	}
	if noMigrate {
		return nil
	}
	if s.readOnly {
		var version int
		if err := s.db.QueryRowContext(ctx, "PRAGMA user_version").Scan(&version); err != nil {
			return fmt.Errorf("read user_version: %w", err)
		}
		return s.checkVersion(version, false)
	}
	_, err := s.migrate(ctx, false)
	return err
}

// DB is the underlying handle, for tests and backend-specific diagnostics only.
func (s *Store) DB() *sql.DB { return s.db }

// Path is the file the store was opened from.
func (s *Store) Path() string { return s.path }

// ReadOnly reports whether the store was opened with Options.ReadOnly.
func (s *Store) ReadOnly() bool { return s.readOnly }

// Close closes the database.
func (s *Store) Close() error {
	if s == nil || s.db == nil {
		return nil
	}
	if err := s.db.Close(); err != nil {
		return wrap(context.TODO(), err, s.path, "sqlite.Close", "could not close the database %s", s.path)
	}
	return nil
}

// enableWAL switches the file to write-ahead logging. The journal mode is a property of the
// file, set once; entering WAL needs a brief exclusive lock and SQLite returns SQLITE_BUSY for it
// without consulting the busy handler, so concurrent first opens retry here.
func (s *Store) enableWAL(ctx context.Context) error {
	const attempts = 50
	var lastErr error
	for i := 0; i < attempts; i++ {
		var mode string
		err := s.db.QueryRowContext(ctx, "PRAGMA journal_mode = WAL").Scan(&mode)
		switch {
		case err == nil && strings.EqualFold(mode, "wal"):
			return nil
		case err == nil:
			lastErr = fmt.Errorf("journal_mode is %q after asking for WAL", mode)
		default:
			lastErr = err
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(10 * time.Millisecond):
		}
	}
	return fmt.Errorf("enable WAL on %s: %w", s.path, lastErr)
}

// checkFTS5 fails with an actionable message when the driver was built without the fts5 tag.
func (s *Store) checkFTS5(ctx context.Context) error {
	info, err := s.DriverInfo(ctx)
	if err != nil {
		return err
	}
	if !info.FTS5 {
		return errs.New(errs.Usage, "sqlite.Open", "this kb binary was built without FTS5: rebuild it with `go build -tags fts5 ./cmd/kb`")
	}
	return nil
}

// checkVec fails when the sqlite-vec extension did not load.
func (s *Store) checkVec(ctx context.Context) error {
	var version string
	if err := s.db.QueryRowContext(ctx, "SELECT vec_version()").Scan(&version); err != nil {
		return fmt.Errorf("sqlite-vec is not loaded (vec_version() failed): %w", err)
	}
	return nil
}

// DriverInfo is what kb doctor reports about the linked SQLite.
type DriverInfo struct {
	SQLite string
	Vec    string
	FTS5   bool
}

// DriverInfo reads the SQLite and sqlite-vec versions and whether FTS5 is compiled in.
func (s *Store) DriverInfo(ctx context.Context) (DriverInfo, error) {
	var info DriverInfo
	rows, err := s.db.QueryContext(ctx, "PRAGMA compile_options")
	if err != nil {
		return info, wrap(ctx, err, s.path, "sqlite.DriverInfo", "could not read the SQLite compile options")
	}
	defer rows.Close()
	for rows.Next() {
		var opt string
		if err := rows.Scan(&opt); err != nil {
			return info, wrap(ctx, err, s.path, "sqlite.DriverInfo", "could not read the SQLite compile options")
		}
		if strings.Contains(opt, "ENABLE_FTS5") {
			info.FTS5 = true
		}
	}
	if err := rows.Err(); err != nil {
		return info, wrap(ctx, err, s.path, "sqlite.DriverInfo", "could not read the SQLite compile options")
	}
	if err := s.db.QueryRowContext(ctx, "SELECT sqlite_version(), vec_version()").Scan(&info.SQLite, &info.Vec); err != nil {
		return info, wrap(ctx, err, s.path, "sqlite.DriverInfo", "could not read the SQLite and sqlite-vec versions")
	}
	return info, nil
}

// migrate brings the database up to s.steps.Target() and returns the steps it applied. A database
// that already has a schema (version > 0) is upgraded only when upgrade is set (Artefact.Apply);
// otherwise an older one is a Conflict. The
// read-decide-apply sequence runs inside one BEGIN IMMEDIATE on a pinned connection, so a second
// opener of a fresh database blocks on the lock (busy_timeout) and then sees the winner's work
// instead of failing halfway. Every pending step and its version stamp share that transaction:
// a failure or a cancel rolls back to the starting version.
func (s *Store) migrate(ctx context.Context, upgrade bool) ([]migrate.Info, error) {
	conn, err := s.db.Conn(ctx)
	if err != nil {
		return nil, fmt.Errorf("open migration connection: %w", err)
	}
	defer conn.Close()

	version, err := userVersion(ctx, conn)
	if err != nil {
		return nil, err
	}
	if err := s.checkVersion(version, upgrade); err != nil {
		return nil, err
	}
	if version == s.steps.Target() {
		return nil, nil
	}

	if _, err := conn.ExecContext(ctx, "BEGIN IMMEDIATE"); err != nil {
		return nil, fmt.Errorf("lock %s for migration: %w", s.path, err)
	}
	committed := false
	defer func() {
		if !committed {
			// Rollback on a fresh context: the migration must not stay open if ctx was cancelled.
			_, _ = conn.ExecContext(context.WithoutCancel(ctx), "ROLLBACK") // the original error is what matters
		}
	}()

	// Re-read under the write lock: another opener may have migrated while we waited for it.
	version, err = userVersion(ctx, conn)
	if err != nil {
		return nil, err
	}
	if err := s.checkVersion(version, upgrade); err != nil {
		return nil, err
	}
	applied, err := s.steps.Up(ctx, execer(conn), version, stampVersion)
	if err != nil {
		return nil, fmt.Errorf("migrate %s: %w", s.path, err)
	}
	// Neither *sql.Conn nor the driver refuses a statement on an already-cancelled ctx (the
	// interrupt only races a running one), so a cancel during the last step would still COMMIT.
	if err := ctx.Err(); err != nil {
		return nil, fmt.Errorf("migrate %s: %w", s.path, err)
	}
	if _, err := conn.ExecContext(ctx, "COMMIT"); err != nil {
		return nil, fmt.Errorf("commit migration of %s: %w", s.path, err)
	}
	committed = true
	return applied, nil
}

// stampVersion sets PRAGMA user_version inside the migration transaction, so the version cannot
// drift from the statements before it. PRAGMA takes no parameters; v is an int.
func stampVersion(ctx context.Context, db execer, v int) error {
	_, err := db.ExecContext(ctx, fmt.Sprintf("PRAGMA user_version = %d", v))
	return err
}

// ErrSchemaOlder is wrapped by the Conflict an open returns for an index at an older schema
// version, so a caller can add its own "how to migrate" hint (errors.Is) without matching text.
var ErrSchemaOlder = errors.New("index schema is older than this kb")

// checkVersion is the one schema-version check. A newer database is refused. An older one with a
// schema (version > 0) is refused unless upgrade is set, which only Artefact.Apply does: every
// other open says to run kb migrate up. Version 0 is a new database and passes. A negative
// version is damage.
func (s *Store) checkVersion(version int, upgrade bool) error {
	schemaVersion := s.steps.Target()
	switch {
	case version < 0:
		return errs.New(errs.Conflict, "sqlite.Open",
			"database %s has a negative schema version (%d); it is damaged: move it aside and run kb reindex --all", s.path, version).
			With("db", s.path)
	case version > schemaVersion:
		return errs.New(errs.Conflict, "sqlite.Open",
			"database %s is at schema version %d, this kb only understands %d; use a newer kb", s.path, version, schemaVersion).
			With("db", s.path)
	case !upgrade && version > 0 && version < schemaVersion:
		return errs.Wrap(ErrSchemaOlder, errs.Conflict, "sqlite.Open",
			"store database %s is at schema v%d; this kb needs v%d. Run: kb migrate up (with the same --store, if any)", s.path, version, schemaVersion).
			With("db", s.path)
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

// wrap turns an error from inside this package into the boundary *errs.Error. An error that is
// already well-formed passes through with the db added; a driver error is classified (classify)
// and gets the friendly message. A cancelled ctx makes any failure Interrupted: after a cancel,
// database/sql rolls the transaction back in the background, so the error that surfaces may be
// sql.ErrTxDone rather than the cancel itself. ctx may be nil (Close).
func wrap(ctx context.Context, err error, path, op, format string, args ...any) error {
	var e *errs.Error
	if errors.As(err, &e) {
		if e.Ctx["db"] == nil {
			e.With("db", path)
		}
		return err
	}
	k := classify(err)
	if ctx != nil && ctx.Err() != nil {
		k = errs.Interrupted
	}
	switch k {
	case errs.Interrupted:
		return errs.Wrap(err, k, op, "interrupted").With("db", path)
	case errs.Conflict:
		var se sqlite3.Error
		if errors.As(err, &se) && (se.Code == sqlite3.ErrBusy || se.Code == sqlite3.ErrLocked) {
			return errs.Wrap(err, k, op, "the database %s stayed locked by another kb process; try again", path).With("db", path)
		}
		return errs.Wrap(err, k, op, "the database %s is damaged or is not a kb database; move it aside and run kb reindex --all", path).
			With("db", path)
	case errs.Usage:
		var se sqlite3.Error
		if errors.As(err, &se) && (se.Code == sqlite3.ErrFull || se.Code == sqlite3.ErrIoErr) {
			return errs.Wrap(err, k, op, "disk full or unwritable: %s; free space and retry", path).With("db", path)
		}
		return errs.Wrap(err, k, op, "cannot open or write the database %s: check the file and its permissions", path).With("db", path)
	default:
		return errs.Wrap(err, k, op, format, args...).With("db", path)
	}
}

// classify maps the failures that are known edge cases to a Kind. A cancelled ctx is Interrupted
// whichever way it surfaces: as ctx.Err() from database/sql, or as SQLITE_INTERRUPT when the
// driver interrupts a running statement. A full or failing disk is Usage (CODING_STYLE §1.1's
// "a disk write fails"). Everything else is Internal.
func classify(err error) errs.Kind {
	if errors.Is(err, context.Canceled) {
		return errs.Interrupted
	}
	var se sqlite3.Error
	if !errors.As(err, &se) {
		return errs.Internal
	}
	switch se.Code {
	case sqlite3.ErrInterrupt:
		return errs.Interrupted
	case sqlite3.ErrCorrupt, sqlite3.ErrNotADB, sqlite3.ErrBusy, sqlite3.ErrLocked:
		return errs.Conflict
	case sqlite3.ErrCantOpen, sqlite3.ErrPerm, sqlite3.ErrReadonly:
		return errs.Usage
	case sqlite3.ErrFull, sqlite3.ErrIoErr:
		// Environmental and fixed by the user (free space, fix the disk), like the permission
		// codes above: Usage, not Conflict (which means another process or damaged data).
		return errs.Usage
	}
	return errs.Internal
}
