package cache

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/binary"
	"errors"
	"fmt"
	"math"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"time"

	_ "github.com/mattn/go-sqlite3" // registers the sqlite3 driver

	"github.com/nickstrad/kb/internal/assert"
	"github.com/nickstrad/kb/internal/migrate"
)

// busyTimeout is how long a connection waits for another process's lock, in milliseconds.
const busyTimeout = 5000

// migrations[v] takes the cache from PRAGMA user_version v to v+1. Append-only: a schema change
// is a new step, never an edit. dim is part of the key so one model id at two widths never mixes.
var migrations = migrate.Steps[execer]{
	{
		Info: migrate.Info{From: 0, ID: "cache/0001-baseline", Desc: "create the emb table keyed by (model, dim, sha256(text))"},
		Up: func(ctx context.Context, db execer) error {
			_, err := db.ExecContext(ctx, `CREATE TABLE IF NOT EXISTS emb (
	model TEXT    NOT NULL,
	dim   INTEGER NOT NULL,
	hash  BLOB    NOT NULL,
	vec   BLOB    NOT NULL,
	PRIMARY KEY (model, dim, hash)
) WITHOUT ROWID`)
			return err
		},
	},
}

func init() {
	if assert.Enabled {
		err := migrations.Check(stepPrefix)
		assert.Invariant(err == nil, "embedding cache migrations: %v", err)
		assert.Invariant(migrations.Target() == SchemaVersion, "SchemaVersion %d != %d steps", SchemaVersion, migrations.Target())
	}
}

// stepPrefix is the artefact part of the cache's step IDs.
const stepPrefix = "cache"

// SchemaVersion is the PRAGMA user_version this build writes and reads: one per migration
// (asserted at init).
const SchemaVersion = 1

// execer is what a migration step needs; *sql.Tx and *sql.Conn satisfy it. Steps run inside the
// caller's transaction and never BEGIN or COMMIT.
type execer interface {
	ExecContext(ctx context.Context, query string, args ...any) (sql.Result, error)
}

// ErrNewerSchema is returned by Open for a file written by a newer kb. The file is left alone so
// that kb keeps working; this build runs without a cache.
var ErrNewerSchema = errors.New("embedding cache was written by a newer kb")

// errNegativeVersion marks a file whose user_version is negative: damaged, not merely old.
var errNegativeVersion = errors.New("embedding cache has a negative schema version")

// Cache is an open cache file. It is safe for concurrent use by several goroutines, and several
// processes may have the same file open.
type Cache struct {
	db   *sql.DB
	path string
}

// Open opens the cache at path, creating the file and its directory if needed. It puts the file
// in WAL mode and applies the schema. A file that is not a SQLite database, or has a newer schema
// (ErrNewerSchema), is an error.
func Open(ctx context.Context, path string) (*Cache, error) {
	return OpenWith(ctx, path, Options{})
}

// Options configure OpenWith.
type Options struct {
	// Driver is the database/sql driver name to open through; "" is the real SQLite driver.
	// Tests pass a sqlfault driver to fail a statement, so the open error paths and the
	// degrade-never-corrupt claim are testable per statement.
	Driver string
}

// OpenWith is Open with options.
func OpenWith(ctx context.Context, path string, opts Options) (*Cache, error) {
	drv := opts.Driver
	if drv == "" {
		drv = "sqlite3"
	}
	if dir := filepath.Dir(path); dir != "" && dir != "." {
		if err := os.MkdirAll(dir, 0o755); err != nil {
			return nil, fmt.Errorf("open embedding cache: create directory %s: %w", dir, err)
		}
	}
	db, err := sql.Open(drv, dsn(path, false))
	if err != nil {
		return nil, fmt.Errorf("open embedding cache %s: %w", path, err)
	}
	c := &Cache{db: db, path: path}
	if err := c.init(ctx); err != nil {
		return nil, errors.Join(fmt.Errorf("open embedding cache %s: %w", path, err), db.Close())
	}
	return c, nil
}

// dsn is the connection string for path. busy_timeout is per connection, so it goes in the DSN.
// _txlock=immediate makes every transaction take the write lock at BEGIN: two processes that both
// read and then try to write would otherwise get SQLITE_BUSY without the busy handler being
// consulted. readOnly opens with mode=ro, which never creates the file.
func dsn(path string, readOnly bool) string {
	uri := url.URL{Scheme: "file", Path: path}
	if readOnly {
		return fmt.Sprintf("%s?mode=ro&_busy_timeout=%d", uri.String(), busyTimeout)
	}
	return fmt.Sprintf("%s?_busy_timeout=%d&_txlock=immediate", uri.String(), busyTimeout)
}

// init sets WAL mode and creates or checks the schema.
func (c *Cache) init(ctx context.Context) error {
	if err := c.enableWAL(ctx); err != nil {
		return err
	}
	_, err := c.migrate(ctx)
	return err
}

// migrate applies the pending steps and returns them. The version read, every step and the stamp
// share one transaction (BEGIN IMMEDIATE via the DSN), so concurrent openers apply them once and a
// failure leaves the starting version. A newer file is ErrNewerSchema.
func (c *Cache) migrate(ctx context.Context) ([]migrate.Info, error) {
	tx, err := c.db.BeginTx(ctx, nil)
	if err != nil {
		return nil, fmt.Errorf("begin schema transaction: %w", err)
	}
	defer tx.Rollback() // no-op after Commit
	var version int
	if err := tx.QueryRowContext(ctx, "PRAGMA user_version").Scan(&version); err != nil {
		return nil, fmt.Errorf("read schema version: %w", err)
	}
	switch {
	case version < 0:
		return nil, fmt.Errorf("%w (schema version %d)", errNegativeVersion, version)
	case version > SchemaVersion:
		return nil, fmt.Errorf("%w (schema version %d, this kb knows %d)", ErrNewerSchema, version, SchemaVersion)
	case version == SchemaVersion:
		return nil, nil
	}
	applied, err := migrations.Up(ctx, execer(tx), version, stampVersion)
	if err != nil {
		return nil, fmt.Errorf("create schema: %w", err)
	}
	if err := tx.Commit(); err != nil {
		return nil, fmt.Errorf("commit schema: %w", err)
	}
	return applied, nil
}

// stampVersion sets PRAGMA user_version inside the schema transaction. PRAGMA takes no
// parameters; v is an int.
func stampVersion(ctx context.Context, db execer, v int) error {
	if _, err := db.ExecContext(ctx, fmt.Sprintf("PRAGMA user_version = %d", v)); err != nil {
		return fmt.Errorf("set schema version: %w", err)
	}
	return nil
}

// enableWAL switches the file to write-ahead logging. The journal mode is a property of the file,
// and SQLite returns SQLITE_BUSY for the switch without consulting the busy handler, so several
// processes opening a new file at once retry here rather than fail.
func (c *Cache) enableWAL(ctx context.Context) error {
	const attempts = 50
	var lastErr error
	for range attempts {
		var mode string
		err := c.db.QueryRowContext(ctx, "PRAGMA journal_mode = WAL").Scan(&mode)
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
			return fmt.Errorf("enable WAL: %w", ctx.Err())
		case <-time.After(10 * time.Millisecond):
		}
	}
	return fmt.Errorf("enable WAL: %w", lastErr)
}

// Path is the file the cache was opened from.
func (c *Cache) Path() string { return c.path }

// Close closes the cache.
func (c *Cache) Close() error {
	if err := c.db.Close(); err != nil {
		return fmt.Errorf("close embedding cache %s: %w", c.path, err)
	}
	return nil
}

// Key is the cache key of a text: sha256 of its exact bytes.
func Key(text string) [sha256.Size]byte { return sha256.Sum256([]byte(text)) }

// Get looks up texts for (model, dim). It returns one slot per text, in order: the vector for a
// hit, nil for a miss. A stored vector whose length is not dim is a miss. dim must be positive.
func (c *Cache) Get(ctx context.Context, model string, dim int, texts []string) ([][]float32, error) {
	if dim <= 0 {
		return nil, fmt.Errorf("embedding cache get: dim %d must be positive", dim)
	}
	out := make([][]float32, len(texts))
	if len(texts) == 0 {
		return out, nil
	}
	stmt, err := c.db.PrepareContext(ctx, "SELECT vec FROM emb WHERE model = ? AND dim = ? AND hash = ?")
	if err != nil {
		return nil, fmt.Errorf("embedding cache get (model %s): %w", model, err)
	}
	defer stmt.Close()
	for i, text := range texts {
		key := Key(text)
		var blob []byte
		err := stmt.QueryRowContext(ctx, model, dim, key[:]).Scan(&blob)
		switch {
		case errors.Is(err, sql.ErrNoRows):
			continue
		case err != nil:
			return nil, fmt.Errorf("embedding cache get (model %s): %w", model, err)
		}
		out[i] = decode(blob, dim) // nil when the length is wrong: a miss
	}
	return out, nil
}

// Put stores vecs[i] for texts[i] under (model, dim), replacing any existing entry, in one
// transaction. Every vector must have exactly dim floats; otherwise nothing is written.
func (c *Cache) Put(ctx context.Context, model string, dim int, texts []string, vecs [][]float32) error {
	if dim <= 0 {
		return fmt.Errorf("embedding cache put: dim %d must be positive", dim)
	}
	if len(texts) != len(vecs) {
		return fmt.Errorf("embedding cache put: %d texts but %d vectors", len(texts), len(vecs))
	}
	for i, v := range vecs {
		if len(v) != dim {
			return fmt.Errorf("embedding cache put: vector %d has %d dims, want %d", i, len(v), dim)
		}
	}
	if len(texts) == 0 {
		return nil
	}
	tx, err := c.db.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("embedding cache put (model %s): %w", model, err)
	}
	defer tx.Rollback() // no-op after Commit
	stmt, err := tx.PrepareContext(ctx, "INSERT OR REPLACE INTO emb (model, dim, hash, vec) VALUES (?, ?, ?, ?)")
	if err != nil {
		return fmt.Errorf("embedding cache put (model %s): %w", model, err)
	}
	defer stmt.Close()
	for i, text := range texts {
		key := Key(text)
		if _, err := stmt.ExecContext(ctx, model, dim, key[:], encode(vecs[i])); err != nil {
			return fmt.Errorf("embedding cache put (model %s): %w", model, err)
		}
	}
	if err := tx.Commit(); err != nil {
		return fmt.Errorf("embedding cache put (model %s): %w", model, err)
	}
	return nil
}

// ModelStats counts the entries for one (model, dim).
type ModelStats struct {
	Model   string
	Dim     int
	Entries int64
}

// Stats lists the entries per (model, dim), ordered by model then dim.
func (c *Cache) Stats(ctx context.Context) ([]ModelStats, error) {
	rows, err := c.db.QueryContext(ctx, "SELECT model, dim, count(*) FROM emb GROUP BY model, dim ORDER BY model, dim")
	if err != nil {
		return nil, fmt.Errorf("embedding cache stats: %w", err)
	}
	defer rows.Close()
	var out []ModelStats
	for rows.Next() {
		var s ModelStats
		if err := rows.Scan(&s.Model, &s.Dim, &s.Entries); err != nil {
			return nil, fmt.Errorf("embedding cache stats: %w", err)
		}
		out = append(out, s)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("embedding cache stats: %w", err)
	}
	return out, nil
}

// Clear deletes every entry. The file keeps its size until SQLite reuses the pages.
func (c *Cache) Clear(ctx context.Context) error {
	if _, err := c.db.ExecContext(ctx, "DELETE FROM emb"); err != nil {
		return fmt.Errorf("embedding cache clear: %w", err)
	}
	return nil
}

// encode packs v as little-endian float32s.
func encode(v []float32) []byte {
	b := make([]byte, 4*len(v))
	for i, f := range v {
		binary.LittleEndian.PutUint32(b[4*i:], math.Float32bits(f))
	}
	return b
}

// decode unpacks a blob written by encode, or returns nil when it does not hold exactly dim floats.
func decode(b []byte, dim int) []float32 {
	if len(b) != 4*dim {
		return nil
	}
	v := make([]float32, dim)
	for i := range v {
		v[i] = math.Float32frombits(binary.LittleEndian.Uint32(b[4*i:]))
	}
	return v
}
