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
)

// SchemaVersion is the PRAGMA user_version this build writes and reads.
const SchemaVersion = 1

// busyTimeout is how long a connection waits for another process's lock, in milliseconds.
const busyTimeout = 5000

// schema is version 1. dim is part of the key so one model id at two widths never mixes.
const schema = `CREATE TABLE IF NOT EXISTS emb (
	model TEXT    NOT NULL,
	dim   INTEGER NOT NULL,
	hash  BLOB    NOT NULL,
	vec   BLOB    NOT NULL,
	PRIMARY KEY (model, dim, hash)
) WITHOUT ROWID`

// ErrNewerSchema is returned by Open for a file written by a newer kb. The file is left alone so
// that kb keeps working; this build runs without a cache.
var ErrNewerSchema = errors.New("embedding cache was written by a newer kb")

// Cache is an open cache file. It is safe for concurrent use by several goroutines, and several
// processes may have the same file open.
type Cache struct {
	db   *sql.DB
	path string
}

// DefaultPath is the cache file used when the caller does not choose one:
// os.UserCacheDir()/kb/embed-cache.sqlite (on macOS ~/Library/Caches/kb/embed-cache.sqlite).
func DefaultPath() (string, error) {
	base, err := os.UserCacheDir()
	if err != nil {
		return "", fmt.Errorf("find user cache directory: %w", err)
	}
	return filepath.Join(base, "kb", "embed-cache.sqlite"), nil
}

// Open opens the cache at path, creating the file and its directory if needed. It puts the file
// in WAL mode and applies the schema. A file that is not a SQLite database, or has a newer schema
// (ErrNewerSchema), is an error.
func Open(ctx context.Context, path string) (*Cache, error) {
	if dir := filepath.Dir(path); dir != "" && dir != "." {
		if err := os.MkdirAll(dir, 0o755); err != nil {
			return nil, fmt.Errorf("open embedding cache: create directory %s: %w", dir, err)
		}
	}
	// busy_timeout is per connection, so it goes in the DSN. _txlock=immediate makes every
	// transaction take the write lock at BEGIN: two processes that both read and then try to
	// write would otherwise get SQLITE_BUSY without the busy handler being consulted.
	uri := url.URL{Scheme: "file", Path: path}
	dsn := fmt.Sprintf("%s?_busy_timeout=%d&_txlock=immediate", uri.String(), busyTimeout)
	db, err := sql.Open("sqlite3", dsn)
	if err != nil {
		return nil, fmt.Errorf("open embedding cache %s: %w", path, err)
	}
	c := &Cache{db: db, path: path}
	if err := c.init(ctx); err != nil {
		return nil, errors.Join(fmt.Errorf("open embedding cache %s: %w", path, err), db.Close())
	}
	return c, nil
}

// init sets WAL mode and creates or checks the schema.
func (c *Cache) init(ctx context.Context) error {
	if err := c.enableWAL(ctx); err != nil {
		return err
	}
	tx, err := c.db.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("begin schema transaction: %w", err)
	}
	defer tx.Rollback() // no-op after Commit
	var version int
	if err := tx.QueryRowContext(ctx, "PRAGMA user_version").Scan(&version); err != nil {
		return fmt.Errorf("read schema version: %w", err)
	}
	switch {
	case version > SchemaVersion:
		return fmt.Errorf("%w (schema version %d, this kb knows %d)", ErrNewerSchema, version, SchemaVersion)
	case version == SchemaVersion:
		return nil
	}
	if _, err := tx.ExecContext(ctx, schema); err != nil {
		return fmt.Errorf("create schema: %w", err)
	}
	// PRAGMA takes no parameters; SchemaVersion is a constant.
	if _, err := tx.ExecContext(ctx, fmt.Sprintf("PRAGMA user_version = %d", SchemaVersion)); err != nil {
		return fmt.Errorf("set schema version: %w", err)
	}
	if err := tx.Commit(); err != nil {
		return fmt.Errorf("commit schema: %w", err)
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
