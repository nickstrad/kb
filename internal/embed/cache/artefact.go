package cache

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"slices"
	"strings"

	sqlite3 "github.com/mattn/go-sqlite3"

	"github.com/nickstrad/kb/internal/errs"
	"github.com/nickstrad/kb/internal/migrate"
)

// ArtefactName is the cache's name in kb migrate output; its step IDs start with "cache/".
const ArtefactName = "embed-cache"

// Artefact is the cache file at a path as a migrate.Artefact. Open already migrates the cache
// (it is disposable, so it stays automatic); the adapter lets kb migrate show and verify it.
// Apply is atomic: the steps and the version stamp share one transaction.
type Artefact struct{ path string }

var (
	_ migrate.Artefact = (*Artefact)(nil)
	_ migrate.Verifier = (*Artefact)(nil)
)

// NewArtefact returns the migration adapter for the cache at path. The file need not exist.
func NewArtefact(path string) *Artefact { return &Artefact{path: path} }

// Status reads the version without changing the file; a missing file is version 0.
func (a *Artefact) Status(ctx context.Context) (migrate.Status, error) {
	const op = "cache.Artefact.Status"
	st := migrate.Status{Artefact: ArtefactName, Path: a.path, Target: SchemaVersion}
	if _, err := os.Stat(a.path); errors.Is(err, fs.ErrNotExist) {
		st.Pending = migrations.Pending(0)
		return st, nil
	}
	var v int
	err := a.with(ctx, true, op, func(db *sql.DB) error {
		return db.QueryRowContext(ctx, "PRAGMA user_version").Scan(&v)
	})
	if err != nil {
		return st, err
	}
	if v < 0 {
		return st, errs.New(errs.Conflict, op, "embedding cache %s has a negative schema version (%d); clear it with kb model cache --clear", a.path, v).
			With("path", a.path)
	}
	st.Current = v
	st.Pending = migrations.Pending(min(v, SchemaVersion))
	return st, nil
}

// Backup writes a consistent snapshot to dst (VACUUM INTO); dst must not exist.
func (a *Artefact) Backup(ctx context.Context, dst string) error {
	const op = "cache.Artefact.Backup"
	if _, err := os.Stat(a.path); err != nil {
		return errs.Wrap(err, errs.NotFound, op, "no embedding cache at %s to back up", a.path).With("path", a.path)
	}
	return a.with(ctx, false, op, func(db *sql.DB) error {
		_, err := db.ExecContext(ctx, "VACUUM INTO ?", dst)
		return err
	})
}

// Apply creates or upgrades the file and returns the steps it applied. A newer file is Conflict.
func (a *Artefact) Apply(ctx context.Context) ([]migrate.Info, error) {
	const op = "cache.Artefact.Apply"
	if err := os.MkdirAll(filepath.Dir(a.path), 0o755); err != nil {
		return nil, errs.Wrap(err, errs.Usage, op, "could not create the directory for %s", a.path).With("path", a.path)
	}
	var applied []migrate.Info
	err := a.with(ctx, false, op, func(db *sql.DB) error {
		c := &Cache{db: db, path: a.path}
		if err := c.enableWAL(ctx); err != nil {
			return err
		}
		var err error
		applied, err = c.migrate(ctx)
		return err
	})
	return applied, err
}

// Verify checks integrity_check and that the schema equals a fresh cache's at the same version.
// A failure is Conflict.
func (a *Artefact) Verify(ctx context.Context) error {
	const op = "cache.Artefact.Verify"
	if _, err := os.Stat(a.path); err != nil {
		return errs.Wrap(err, errs.NotFound, op, "no embedding cache at %s to verify", a.path).With("path", a.path)
	}
	return a.with(ctx, true, op, func(db *sql.DB) error {
		var v int
		if err := db.QueryRowContext(ctx, "PRAGMA user_version").Scan(&v); err != nil {
			return err
		}
		if v < 0 || v > SchemaVersion {
			return errs.New(errs.Conflict, op, "embedding cache %s is at schema version %d; this kb verifies 0 to %d", a.path, v, SchemaVersion).
				With("path", a.path)
		}
		var check string
		if err := db.QueryRowContext(ctx, "PRAGMA integrity_check").Scan(&check); err != nil {
			return err
		}
		if check != "ok" {
			return errs.New(errs.Conflict, op, "embedding cache %s is damaged (%s); clear it with kb model cache --clear", a.path, check).
				With("path", a.path)
		}
		got, err := schemaOf(ctx, db)
		if err != nil {
			return err
		}
		want, err := freshSchema(ctx, v)
		if err != nil {
			return err
		}
		if !slices.Equal(got, want) {
			return errs.New(errs.Conflict, op, "embedding cache %s does not match schema v%d (have %q, want %q); clear it with kb model cache --clear",
				a.path, v, got, want).With("path", a.path)
		}
		return nil
	})
}

// with opens the file (read-only or read-write, never migrating), runs fn and closes it,
// turning any failure into one *errs.Error.
func (a *Artefact) with(ctx context.Context, readOnly bool, op string, fn func(*sql.DB) error) (err error) {
	db, err := sql.Open("sqlite3", dsn(a.path, readOnly))
	if err != nil {
		return wrap(ctx, err, op, a.path)
	}
	defer func() {
		if cerr := db.Close(); cerr != nil && err == nil {
			err = wrap(ctx, cerr, op, a.path)
		}
	}()
	if err := fn(db); err != nil {
		return wrap(ctx, err, op, a.path)
	}
	return nil
}

// wrap classifies a failure once: an *errs.Error passes through, a cancel is Interrupted, a newer
// or damaged file or a held lock is Conflict, an unwritable file Usage, anything else Internal.
func wrap(ctx context.Context, err error, op, path string) error {
	var e *errs.Error
	if errors.As(err, &e) {
		return err
	}
	var se sqlite3.Error
	switch {
	case errors.Is(ctx.Err(), context.Canceled) || errors.Is(err, context.Canceled) ||
		errors.As(err, &se) && se.Code == sqlite3.ErrInterrupt:
		return errs.Wrap(err, errs.Interrupted, op, "interrupted").With("path", path)
	case errors.Is(err, ErrNewerSchema):
		return errs.Wrap(err, errs.Conflict, op, "embedding cache %s was written by a newer kb; upgrade kb", path).With("path", path)
	case errors.As(err, &se) && (se.Code == sqlite3.ErrBusy || se.Code == sqlite3.ErrLocked):
		return errs.Wrap(err, errs.Conflict, op, "embedding cache %s stayed locked by another kb process; try again", path).With("path", path)
	case errors.Is(err, errNegativeVersion) || errors.As(err, &se) && (se.Code == sqlite3.ErrCorrupt || se.Code == sqlite3.ErrNotADB):
		return errs.Wrap(err, errs.Conflict, op, "embedding cache %s is damaged; clear it with kb model cache --clear", path).With("path", path)
	case errors.As(err, &se) && (se.Code == sqlite3.ErrCantOpen || se.Code == sqlite3.ErrPerm || se.Code == sqlite3.ErrReadonly ||
		se.Code == sqlite3.ErrFull || se.Code == sqlite3.ErrIoErr):
		return errs.Wrap(err, errs.Usage, op, "cannot open or write the embedding cache %s: check the file, its permissions and free space", path).
			With("path", path)
	}
	return errs.Wrap(err, errs.Internal, op, "%s failed on the embedding cache %s", op, path).With("path", path)
}

// schemaOf is the file's schema, one normalised line per object, sorted.
func schemaOf(ctx context.Context, db interface {
	QueryContext(context.Context, string, ...any) (*sql.Rows, error)
}) ([]string, error) {
	rows, err := db.QueryContext(ctx, "SELECT type, name, coalesce(sql, '') FROM sqlite_schema WHERE name NOT LIKE 'sqlite_%'")
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []string
	for rows.Next() {
		var typ, name, text string
		if err := rows.Scan(&typ, &name, &text); err != nil {
			return nil, err
		}
		out = append(out, fmt.Sprintf("%s %s: %s", typ, name, strings.Join(strings.Fields(text), " ")))
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	slices.Sort(out)
	return out, nil
}

// freshSchema is schemaOf a new in-memory cache after the first v steps.
func freshSchema(ctx context.Context, v int) ([]string, error) {
	db, err := sql.Open("sqlite3", ":memory:")
	if err != nil {
		return nil, err
	}
	defer db.Close()
	conn, err := db.Conn(ctx) // one connection: each :memory: connection is its own database
	if err != nil {
		return nil, err
	}
	defer conn.Close()
	if _, err := migrations[:v].Up(ctx, execer(conn), 0, stampVersion); err != nil {
		return nil, fmt.Errorf("build a reference cache at v%d: %w", v, err)
	}
	return schemaOf(ctx, conn)
}
