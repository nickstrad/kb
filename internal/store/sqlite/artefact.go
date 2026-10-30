package sqlite

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"slices"
	"strings"

	sqlite_vec "github.com/asg017/sqlite-vec-go-bindings/cgo"

	"github.com/nickstrad/kb/internal/errs"
	"github.com/nickstrad/kb/internal/migrate"
)

// Artefact is the index DB at a path as a migrate.Artefact. Unlike Open it never migrates by
// itself: each method opens the file without the version check, does one thing and closes it.
// Apply is atomic: every pending step and the version stamp run in one BEGIN IMMEDIATE, so a
// failure or a cancel leaves the starting version.
type Artefact struct {
	path   string
	driver string // test seam, as Options.driver
}

var (
	_ migrate.Artefact = (*Artefact)(nil)
	_ migrate.Verifier = (*Artefact)(nil)
)

// NewArtefact returns the migration adapter for the index DB at path. The file need not exist.
func NewArtefact(path string) *Artefact { return &Artefact{path: path} }

// Status reads the file's schema version without changing it; a missing file is version 0.
func (a *Artefact) Status(ctx context.Context) (migrate.Status, error) {
	st := migrate.Status{Artefact: artefactName, Path: a.path, Target: schemaVersion}
	v, err := a.version(ctx)
	if err != nil {
		return st, err
	}
	st.Current = v
	st.Pending = migrations.Pending(min(v, schemaVersion))
	return st, nil
}

// version is the file's user_version, 0 for a missing file. A negative one is damage (Conflict).
func (a *Artefact) version(ctx context.Context) (int, error) {
	const op = "sqlite.Artefact.Status"
	if _, err := os.Stat(a.path); errors.Is(err, fs.ErrNotExist) {
		return 0, nil
	}
	var v int
	err := a.with(ctx, true, op, func(s *Store) error {
		return s.db.QueryRowContext(ctx, "PRAGMA user_version").Scan(&v)
	})
	if err != nil {
		return 0, err
	}
	if v < 0 {
		return 0, errs.New(errs.Conflict, op,
			"database %s has a negative schema version (%d); it is damaged: move it aside and run kb reindex --all", a.path, v).
			With("db", a.path)
	}
	return v, nil
}

// Backup writes a consistent snapshot to dst with VACUUM INTO, which readers do not block. dst
// must not exist.
func (a *Artefact) Backup(ctx context.Context, dst string) error {
	const op = "sqlite.Artefact.Backup"
	if _, err := os.Stat(a.path); err != nil {
		return errs.Wrap(err, errs.NotFound, op, "no database at %s to back up", a.path).With("db", a.path)
	}
	// Read-write: VACUUM INTO is refused on a query_only connection.
	return a.with(ctx, false, op, func(s *Store) error {
		_, err := s.db.ExecContext(ctx, "VACUUM INTO ?", dst)
		return err
	})
}

// Apply runs every pending step (creating the file at version 0) and returns those it applied.
func (a *Artefact) Apply(ctx context.Context) ([]migrate.Info, error) {
	var applied []migrate.Info
	err := a.with(ctx, false, "sqlite.Artefact.Apply", func(s *Store) error {
		var err error
		applied, err = s.migrate(ctx)
		return err
	})
	return applied, err
}

// Verify checks the file: PRAGMA integrity_check and foreign_key_check pass, and its schema
// equals a fresh database's at the same version (the vector table that EnsureEmbedMeta creates
// at the model's width is not part of the steps from v3 and is left out). A failure is Conflict.
func (a *Artefact) Verify(ctx context.Context) error {
	const op = "sqlite.Artefact.Verify"
	if _, err := os.Stat(a.path); err != nil {
		return errs.Wrap(err, errs.NotFound, op, "no database at %s to verify", a.path).With("db", a.path)
	}
	return a.with(ctx, true, op, func(s *Store) error {
		var v int
		if err := s.db.QueryRowContext(ctx, "PRAGMA user_version").Scan(&v); err != nil {
			return err
		}
		if v < 0 || v > schemaVersion {
			return errs.New(errs.Conflict, op, "database %s is at schema version %d; this kb verifies 0 to %d", a.path, v, schemaVersion).
				With("db", a.path)
		}
		if err := integrity(ctx, s.db); err != nil {
			return errs.Wrap(err, errs.Conflict, op, "database %s is damaged: %v; restore a backup or move it aside and run kb reindex --all", a.path, err).
				With("db", a.path)
		}
		got, err := schemaOf(ctx, s.db)
		if err != nil {
			return err
		}
		want, err := freshSchema(ctx, v)
		if err != nil {
			return err
		}
		if !hasVecTable(want) {
			got = slices.DeleteFunc(got, isVecObject)
		}
		if d := diffSchema(want, got); d != "" {
			return errs.New(errs.Conflict, op, "database %s does not match schema v%d (%s); restore a backup or move it aside and run kb reindex --all", a.path, v, d).
				With("db", a.path)
		}
		return nil
	})
}

// with opens the file without migrating, runs fn and closes it, wrapping any failure once.
func (a *Artefact) with(ctx context.Context, readOnly bool, op string, fn func(*Store) error) (err error) {
	s, err := open(ctx, a.path, Options{ReadOnly: readOnly, driver: a.driver, noMigrate: true})
	if err != nil {
		return wrap(ctx, err, a.path, op, "could not open the database %s", a.path)
	}
	defer func() {
		if cerr := s.db.Close(); cerr != nil && err == nil {
			err = wrap(ctx, cerr, a.path, op, "could not close the database %s", a.path)
		}
	}()
	if err := fn(s); err != nil {
		return wrap(ctx, err, a.path, op, "%s failed on %s", op, a.path)
	}
	return nil
}

// queryer is what the schema helpers read through: *sql.DB and *sql.Conn.
type queryer interface {
	QueryContext(ctx context.Context, query string, args ...any) (*sql.Rows, error)
}

// integrity runs integrity_check (every row "ok") and foreign_key_check (no rows).
func integrity(ctx context.Context, db queryer) error {
	bad, err := strings1(ctx, db, "PRAGMA integrity_check")
	if err != nil {
		return err
	}
	if len(bad) != 1 || bad[0] != "ok" {
		return fmt.Errorf("integrity_check: %s", strings.Join(bad, "; "))
	}
	rows, err := db.QueryContext(ctx, "PRAGMA foreign_key_check")
	if err != nil {
		return err
	}
	defer rows.Close()
	if rows.Next() {
		var table string
		var rowid sql.NullInt64
		var parent string
		var fkid int
		if err := rows.Scan(&table, &rowid, &parent, &fkid); err != nil {
			return err
		}
		return fmt.Errorf("foreign_key_check: %s row %d points at a missing %s row", table, rowid.Int64, parent)
	}
	return rows.Err()
}

// strings1 returns the first column of every row as text.
func strings1(ctx context.Context, db queryer, query string) ([]string, error) {
	rows, err := db.QueryContext(ctx, query)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []string
	for rows.Next() {
		var s string
		if err := rows.Scan(&s); err != nil {
			return nil, err
		}
		out = append(out, s)
	}
	return out, rows.Err()
}

// schemaOf is the database's schema, normalised for comparison: one "type name on tbl: sql" line
// per object, sorted, whitespace collapsed. SQLite's own objects and the tables a virtual table
// owns (ownedTables), with their indexes, are left out.
func schemaOf(ctx context.Context, db queryer) ([]string, error) {
	owned, err := ownedTables(ctx, db)
	if err != nil {
		return nil, err
	}
	rows, err := db.QueryContext(ctx, "SELECT type, name, tbl_name, coalesce(sql, '') FROM sqlite_schema WHERE name NOT LIKE 'sqlite_%'")
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []string
	for rows.Next() {
		var typ, name, tbl, text string
		if err := rows.Scan(&typ, &name, &tbl, &text); err != nil {
			return nil, err
		}
		if owned[tbl] {
			continue // the table, or an index on it
		}
		out = append(out, fmt.Sprintf("%s %s on %s: %s", typ, name, tbl, strings.Join(strings.Fields(text), " ")))
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	slices.Sort(out)
	return out, nil
}

// ownedTables names the tables a virtual table made for itself: those SQLite marks as shadow
// tables, plus "<vtab>_…" tables of a virtual table that does not mark them (sqlite-vec's
// chunks_vec_vector_chunks00). CREATE VIRTUAL TABLE recreates them, so schemas leave them out.
func ownedTables(ctx context.Context, db queryer) (map[string]bool, error) {
	rows, err := db.QueryContext(ctx, `
		SELECT t.name FROM pragma_table_list t WHERE t.schema = 'main' AND (t.type = 'shadow' OR EXISTS (
		  SELECT 1 FROM pragma_table_list v WHERE v.schema = 'main' AND v.type = 'virtual' AND substr(t.name, 1, length(v.name) + 1) = v.name || '_'))`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := map[string]bool{}
	for rows.Next() {
		var name string
		if err := rows.Scan(&name); err != nil {
			return nil, err
		}
		out[name] = true
	}
	return out, rows.Err()
}

// freshSchema is schemaOf a new in-memory database after the first v steps.
func freshSchema(ctx context.Context, v int) ([]string, error) {
	vecOnce.Do(sqlite_vec.Auto)
	db, err := sql.Open(driverName, ":memory:")
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
		return nil, fmt.Errorf("build a reference database at v%d: %w", v, err)
	}
	return schemaOf(ctx, conn)
}

// isVecObject reports a schema line for the vector table (or an object on it), which
// EnsureEmbedMeta creates.
func isVecObject(line string) bool {
	_, rest, _ := strings.Cut(line, " ")
	name, _, _ := strings.Cut(rest, " ")
	return name == "chunks_vec" || strings.HasPrefix(name, "chunks_vec_")
}

func hasVecTable(schema []string) bool { return slices.ContainsFunc(schema, isVecObject) }

// diffSchema describes the first differences between two normalised schemas, "" when equal.
func diffSchema(want, got []string) string {
	var missing, extra []string
	for _, w := range want {
		if !slices.Contains(got, w) {
			missing = append(missing, w)
		}
	}
	for _, g := range got {
		if !slices.Contains(want, g) {
			extra = append(extra, g)
		}
	}
	var parts []string
	if len(missing) > 0 {
		parts = append(parts, "missing or changed: "+strings.Join(missing, " | "))
	}
	if len(extra) > 0 {
		parts = append(parts, "unexpected: "+strings.Join(extra, " | "))
	}
	return strings.Join(parts, "; ")
}
