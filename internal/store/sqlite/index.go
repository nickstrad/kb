// The store.Index implementation. Everything here keeps seven tables consistent:
//
//	entries      one row per knowledge entry
//	chunks       the indexed pieces of that entry
//	chunks_fts   the FTS5 index over chunks (rowid = chunks.id)
//	chunks_exact the identifier FTS5 index over chunks (rowid = chunks.id; schema v6)
//	chunks_vec   the sqlite-vec index over chunks (chunk_id = chunks.id)
//	entry_tags   the entry's normalised tags, for the tag filter (schema v7; edges.go)
//	edges        the entry's outgoing links (schema v7; edges.go)
//
// chunks_fts and chunks_exact are CONTENTLESS (content='', contentless_delete=1): they keep only
// the inverted index, so their columns are write-only and readers join chunks on rowid. title is in
// chunks_fts only to carry the bm25 weight. chunks_exact differs only in its tokenizer, which keeps
// identifiers whole. chunks_vec exists only when embed_meta records dim > 0
// (vec0-width-is-lazy.md). Nothing maintains any index automatically, so every insert into chunks
// is paired with an insert into each, and every delete with a delete from each, in the same
// transaction.

package sqlite

import (
	"context"
	"database/sql"
	"encoding/binary"
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"slices"
	"strings"
	"time"

	sqlite_vec "github.com/asg017/sqlite-vec-go-bindings/cgo"
	sqlite3 "github.com/mattn/go-sqlite3"

	"github.com/nickstrad/kb/internal/assert"
	"github.com/nickstrad/kb/internal/errs"
	"github.com/nickstrad/kb/internal/store"
)

// MaxVecDim is the widest vector sqlite-vec stores (SQLITE_VEC_VEC0_MAX_DIMENSIONS).
const MaxVecDim = 8192

// vecTableSQL creates chunks_vec at the recorded width. %d is an int from EnsureEmbedMeta.
const vecTableSQL = `CREATE VIRTUAL TABLE IF NOT EXISTS chunks_vec USING vec0(
  chunk_id  INTEGER PRIMARY KEY,
  embedding FLOAT[%d] distance_metric=cosine
)`

// EnsureEmbedMeta records m on an index with no embed_meta row (fresh, migrated to schema 3 or
// reset), dropping any leftover chunks_vec and creating it at m.Dim; dim 0 is legal only for
// store.FTSOnlyModel. On an index with a row it returns a Conflict wrapping
// *store.EmbedMetaMismatchError when model or dim differ, and otherwise re-creates a missing
// chunks_vec. One transaction: a failure records nothing.
func (s *Store) EnsureEmbedMeta(ctx context.Context, m store.EmbedMeta) error {
	if err := s.ensureEmbedMeta(ctx, m); err != nil {
		return wrap(ctx, err, s.path, "sqlite.EnsureEmbedMeta", "could not record the embedding model %s", m.Model)
	}
	return nil
}

func (s *Store) ensureEmbedMeta(ctx context.Context, m store.EmbedMeta) error {
	const op = "sqlite.EnsureEmbedMeta"
	switch {
	case m.Model == "":
		return errs.New(errs.Internal, op, "embed_meta: model must not be empty")
	case m.Model == store.FTSOnlyModel && m.Dim != 0:
		return errs.New(errs.Usage, op, "embed_meta: model %s is FTS only and must have 0 dimensions, got %d", m.Model, m.Dim)
	case m.Model != store.FTSOnlyModel && m.Dim <= 0:
		return errs.New(errs.Usage, op, "embed_meta: model %s reports %d dimensions; only the FTS-only model %q may have none", m.Model, m.Dim, store.FTSOnlyModel)
	case m.Dim > MaxVecDim:
		return errs.New(errs.Usage, op, "embed_meta: model %s reports %d dimensions; sqlite-vec stores at most %d", m.Model, m.Dim, MaxVecDim)
	}

	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("begin embed_meta check: %w", err)
	}
	defer tx.Rollback()

	var stored store.EmbedMeta
	err = tx.QueryRowContext(ctx, "SELECT model, dim FROM embed_meta WHERE id = 1").Scan(&stored.Model, &stored.Dim)
	switch {
	case errors.Is(err, sql.ErrNoRows):
		if _, err := tx.ExecContext(ctx, "DROP TABLE IF EXISTS chunks_vec"); err != nil {
			return fmt.Errorf("drop stale chunks_vec: %w", err)
		}
		if _, err := tx.ExecContext(ctx,
			"INSERT INTO embed_meta (id, model, dim, created_at) VALUES (1, ?, ?, ?)",
			m.Model, m.Dim, nowUTC()); err != nil {
			return fmt.Errorf("record embed_meta: %w", err)
		}
	case err != nil:
		return fmt.Errorf("read embed_meta: %w", err)
	case stored != m:
		mismatch := &store.EmbedMetaMismatchError{StoredModel: stored.Model, StoredDim: stored.Dim, WantModel: m.Model, WantDim: m.Dim}
		return errs.Wrap(mismatch, errs.Conflict, op, "%s", mismatch.Error())
	}
	if m.Dim > 0 {
		if _, err := tx.ExecContext(ctx, fmt.Sprintf(vecTableSQL, m.Dim)); err != nil {
			return fmt.Errorf("create chunks_vec FLOAT[%d]: %w", m.Dim, err)
		}
	}
	if err := tx.Commit(); err != nil {
		return fmt.Errorf("commit embed_meta: %w", err)
	}
	return nil
}

// EmbedMeta returns the recorded model and dimension; ok is false on a database never indexed.
func (s *Store) EmbedMeta(ctx context.Context) (store.EmbedMeta, bool, error) {
	m, ok, err := embedMeta(ctx, s.db)
	if err != nil {
		return store.EmbedMeta{}, false, wrap(ctx, err, s.path, "sqlite.EmbedMeta", "could not read the embedding model record")
	}
	return m, ok, nil
}

func embedMeta(ctx context.Context, q queryRower) (store.EmbedMeta, bool, error) {
	var m store.EmbedMeta
	err := q.QueryRowContext(ctx, "SELECT model, dim FROM embed_meta WHERE id = 1").Scan(&m.Model, &m.Dim)
	if errors.Is(err, sql.ErrNoRows) {
		return store.EmbedMeta{}, false, nil
	}
	if err != nil {
		return store.EmbedMeta{}, false, fmt.Errorf("read embed_meta: %w", err)
	}
	return m, true, nil
}

// HasVectors reports whether chunks_vec exists: absent on an FTS-only index and on a database
// migrated to schema 3 that has not been reindexed yet.
func (s *Store) HasVectors(ctx context.Context) (bool, error) {
	ok, err := vecTableExists(ctx, s.db)
	if err != nil {
		return false, wrap(ctx, err, s.path, "sqlite.HasVectors", "could not check for the vector index")
	}
	return ok, nil
}

// queryRower is satisfied by *sql.DB, *sql.Tx and *sql.Conn.
type queryRower interface {
	QueryRowContext(ctx context.Context, query string, args ...any) *sql.Row
}

// vecTableExists is the one check for chunks_vec, used by every reader and writer of it.
func vecTableExists(ctx context.Context, q queryRower) (bool, error) {
	var n int
	err := q.QueryRowContext(ctx,
		"SELECT count(*) FROM sqlite_master WHERE type = 'table' AND name = 'chunks_vec'").Scan(&n)
	if err != nil {
		return false, fmt.Errorf("look for chunks_vec: %w", err)
	}
	return n > 0, nil
}

// ResetIndex empties the derived index for `kb reindex --all`: entries, chunks, tags, edges, both
// FTS indexes, embed_meta and the chunks_vec table, dropped because the next model may have another
// width. The search log tables are left alone: they are the only data here not rebuildable from
// Markdown.
func (s *Store) ResetIndex(ctx context.Context) error {
	if err := s.resetIndex(ctx); err != nil {
		return wrap(ctx, err, s.path, "sqlite.ResetIndex", "could not reset the index")
	}
	return nil
}

func (s *Store) resetIndex(ctx context.Context) error {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("begin reset: %w", err)
	}
	defer tx.Rollback()
	// 'delete-all' empties a contentless index in one statement.
	for _, stmt := range []string{
		"INSERT INTO chunks_fts(chunks_fts) VALUES ('delete-all')",
		"INSERT INTO chunks_exact(chunks_exact) VALUES ('delete-all')",
		"DROP TABLE IF EXISTS chunks_vec",
		"DELETE FROM edges",
		"DELETE FROM entry_tags",
		"DELETE FROM chunks",
		"DELETE FROM entries",
		"DELETE FROM embed_meta",
	} {
		if _, err := tx.ExecContext(ctx, stmt); err != nil {
			return fmt.Errorf("reset (%s): %w", stmt, err)
		}
	}
	if err := tx.Commit(); err != nil {
		return fmt.Errorf("commit reset: %w", err)
	}
	return nil
}

const entryColumns = `id, path, kind, title, summary, tags, aliases, updated, verified, body_hash, indexed_at`

// Entry returns the entry at path, or (nil, nil) when there is none.
func (s *Store) Entry(ctx context.Context, path string) (*store.EntryRow, error) {
	e, err := getEntry(ctx, s.db, path)
	if err != nil {
		return nil, wrap(ctx, err, s.path, "sqlite.Entry", "could not read entry %s", path)
	}
	return e, nil
}

// EntriesByPath reads the entries at paths in one query (render looks up every hit's entry this
// way); a path with no entry is absent from the map. Duplicate paths are fine.
func (s *Store) EntriesByPath(ctx context.Context, paths []string) (map[string]store.EntryRow, error) {
	out, err := s.entriesByPath(ctx, paths)
	if err != nil {
		return nil, wrap(ctx, err, s.path, "sqlite.EntriesByPath", "could not read %d entries", len(paths))
	}
	return out, nil
}

func (s *Store) entriesByPath(ctx context.Context, paths []string) (map[string]store.EntryRow, error) {
	out := make(map[string]store.EntryRow, len(paths))
	if len(paths) == 0 {
		return out, nil
	}
	args := make([]any, len(paths))
	for i, p := range paths {
		args[i] = p
	}
	rows, err := s.db.QueryContext(ctx, `SELECT `+entryColumns+` FROM entries WHERE path IN (`+placeholders(len(args))+`)`, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	for rows.Next() {
		e, err := scanEntry(rows)
		if err != nil {
			return nil, err
		}
		out[e.Path] = *e
	}
	return out, rows.Err()
}

// Entries returns every entry, sorted by path.
func (s *Store) Entries(ctx context.Context) ([]store.EntryRow, error) {
	out, err := s.entries(ctx)
	if err != nil {
		return nil, wrap(ctx, err, s.path, "sqlite.Entries", "could not list the indexed entries")
	}
	return out, nil
}

func (s *Store) entries(ctx context.Context) ([]store.EntryRow, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT `+entryColumns+` FROM entries ORDER BY path`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []store.EntryRow
	for rows.Next() {
		e, err := scanEntry(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, *e)
	}
	return out, rows.Err()
}

// Chunks returns one entry's chunks in ord order.
func (s *Store) Chunks(ctx context.Context, entryID int64) ([]store.ChunkRow, error) {
	out, err := s.chunks(ctx, entryID)
	if err != nil {
		return nil, wrap(ctx, err, s.path, "sqlite.Chunks", "could not read the chunks of entry %d", entryID)
	}
	return out, nil
}

func (s *Store) chunks(ctx context.Context, entryID int64) ([]store.ChunkRow, error) {
	rows, err := s.db.QueryContext(ctx, `
		SELECT id, entry_id, ord, source_file, heading, text, text_hash
		FROM chunks WHERE entry_id = ? ORDER BY ord`, entryID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []store.ChunkRow
	for rows.Next() {
		var c store.ChunkRow
		if err := rows.Scan(&c.ID, &c.EntryID, &c.Ord, &c.SourceFile, &c.Heading, &c.Text, &c.TextHash); err != nil {
			return nil, err
		}
		out = append(out, c)
	}
	return out, rows.Err()
}

// Embeddings returns the vectors stored for an entry keyed by their chunk's text_hash, so a
// reindex reuses a vector whose chunk text is unchanged (~33 ms saved per chunk). Empty when there
// is no chunks_vec.
func (s *Store) Embeddings(ctx context.Context, entryID int64) (map[string][]float32, error) {
	out, err := s.embeddings(ctx, entryID)
	if err != nil {
		return nil, wrap(ctx, err, s.path, "sqlite.Embeddings", "could not read the embeddings of entry %d", entryID)
	}
	return out, nil
}

func (s *Store) embeddings(ctx context.Context, entryID int64) (map[string][]float32, error) {
	out := make(map[string][]float32)
	hasVec, err := vecTableExists(ctx, s.db)
	if err != nil || !hasVec {
		return out, err
	}
	rows, err := s.db.QueryContext(ctx, `
		SELECT c.text_hash, v.embedding
		FROM chunks c JOIN chunks_vec v ON v.chunk_id = c.id
		WHERE c.entry_id = ?`, entryID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	for rows.Next() {
		var hash string
		var blob []byte
		if err := rows.Scan(&hash, &blob); err != nil {
			return nil, err
		}
		vec, err := deserializeFloat32(blob)
		if err != nil {
			return nil, err
		}
		out[hash] = vec
	}
	return out, rows.Err()
}

// ReplaceEntry writes an entry and its complete chunk set in one transaction: the entry row is
// upserted by path, its old chunks and their FTS and vector rows are removed, and the new chunks go
// into chunks, chunks_fts (title plus aliases in the title column), chunks_exact and chunks_vec
// together, with the entry's tags (entry_tags) and edges (writeLinks). Every chunk must carry an
// embedding of exactly the recorded dim (none on an FTS-only index); a wrong one is a Conflict and
// nothing is written.
// embed_meta must be recorded first (EnsureEmbedMeta).
func (s *Store) ReplaceEntry(ctx context.Context, e store.EntryInput, chunks []store.ChunkInput) (int64, error) {
	id, err := s.replaceEntry(ctx, e, chunks)
	if err != nil {
		return 0, wrap(ctx, err, s.path, "sqlite.ReplaceEntry", "could not index %s", e.Path)
	}
	return id, nil
}

func (s *Store) replaceEntry(ctx context.Context, e store.EntryInput, chunks []store.ChunkInput) (entryID int64, err error) {
	const op = "sqlite.ReplaceEntry"
	if e.Path == "" {
		return 0, errs.New(errs.Internal, op, "replace entry: path must not be empty")
	}

	meta, ok, err := embedMeta(ctx, s.db)
	if err != nil {
		return 0, err
	}
	if !ok {
		return 0, errs.New(errs.Conflict, op, "replace entry %s: no embedding model is recorded for this index; run kb reindex --all", e.Path)
	}
	dim := meta.Dim
	if err := checkEdges(op, e.Path, e.Edges); err != nil {
		return 0, err
	}
	for i, c := range chunks {
		var bad string
		switch {
		case dim == 0 && c.Embedding != nil:
			bad = "has an embedding but this index is FTS only"
		case dim > 0 && c.Embedding == nil:
			bad = "has no embedding"
		case dim > 0 && len(c.Embedding) != dim:
			bad = fmt.Sprintf("has %d dimensions, embed_meta says %d", len(c.Embedding), dim)
		}
		if bad != "" {
			return 0, errs.New(errs.Conflict, op, "replace entry %s: chunk %d (ord %d) %s", e.Path, i, c.Ord, bad).With("path", e.Path)
		}
	}

	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return 0, fmt.Errorf("begin replace of %s: %w", e.Path, err)
	}
	defer tx.Rollback()

	old, err := getEntry(ctx, tx, e.Path)
	if err != nil {
		return 0, err
	}
	if old != nil {
		if err := clearChunks(ctx, tx, old); err != nil {
			return 0, err
		}
	}

	tagsJSON, err := encodeList(e.Tags)
	if err != nil {
		return 0, fmt.Errorf("replace entry %s: tags: %w", e.Path, err)
	}
	aliasesJSON, err := encodeList(e.Aliases)
	if err != nil {
		return 0, fmt.Errorf("replace entry %s: aliases: %w", e.Path, err)
	}
	err = tx.QueryRowContext(ctx, `
		INSERT INTO entries (path, kind, title, summary, tags, aliases, updated, verified, body_hash, indexed_at)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?)
		ON CONFLICT(path) DO UPDATE SET
			kind = excluded.kind, title = excluded.title, summary = excluded.summary,
			tags = excluded.tags, aliases = excluded.aliases, updated = excluded.updated,
			verified = excluded.verified, body_hash = excluded.body_hash, indexed_at = excluded.indexed_at
		RETURNING id`,
		e.Path, e.Kind, e.Title, e.Summary, tagsJSON, aliasesJSON,
		nullString(e.Updated), nullString(e.Verified), e.BodyHash, nowUTC(),
	).Scan(&entryID)
	if err != nil {
		return 0, fmt.Errorf("write entry %s: %w", e.Path, err)
	}
	if err := writeLinks(ctx, tx, entryID, e); err != nil {
		return 0, err
	}

	insertChunk, err := tx.PrepareContext(ctx, `
		INSERT INTO chunks (entry_id, ord, source_file, heading, text, text_hash)
		VALUES (?, ?, ?, ?, ?, ?) RETURNING id`)
	if err != nil {
		return 0, fmt.Errorf("prepare chunk insert: %w", err)
	}
	defer insertChunk.Close()
	insertFTS, err := tx.PrepareContext(ctx, `INSERT INTO chunks_fts (rowid, text, heading, title) VALUES (?, ?, ?, ?)`)
	if err != nil {
		return 0, fmt.Errorf("prepare fts insert: %w", err)
	}
	defer insertFTS.Close()
	insertExact, err := tx.PrepareContext(ctx, `INSERT INTO chunks_exact (rowid, text, heading) VALUES (?, ?, ?)`)
	if err != nil {
		return 0, fmt.Errorf("prepare exact insert: %w", err)
	}
	defer insertExact.Close()
	var insertVec *sql.Stmt
	if dim > 0 {
		hasVec, err := vecTableExists(ctx, tx)
		if err != nil {
			return 0, err
		}
		if !hasVec {
			return 0, errs.New(errs.Conflict, op, "replace entry %s: embed_meta records %d dimensions but the vector index is missing; run kb reindex --all", e.Path, dim)
		}
		insertVec, err = tx.PrepareContext(ctx, `INSERT INTO chunks_vec (chunk_id, embedding) VALUES (?, ?)`)
		if err != nil {
			return 0, fmt.Errorf("prepare vec insert: %w", err)
		}
		defer insertVec.Close()
	}

	title := ftsTitle(e.Title, e.Aliases)
	for _, c := range chunks {
		var chunkID int64
		if err := insertChunk.QueryRowContext(ctx, entryID, c.Ord, c.SourceFile, c.Heading, c.Text, c.TextHash).Scan(&chunkID); err != nil {
			return 0, fmt.Errorf("insert chunk %d of %s: %w", c.Ord, e.Path, err)
		}
		if _, err := insertFTS.ExecContext(ctx, chunkID, c.Text, c.Heading, title); err != nil {
			return 0, fmt.Errorf("index chunk %d of %s for FTS: %w", c.Ord, e.Path, err)
		}
		if _, err := insertExact.ExecContext(ctx, chunkID, c.Text, c.Heading); err != nil {
			return 0, fmt.Errorf("index chunk %d of %s for identifier search: %w", c.Ord, e.Path, err)
		}
		if dim == 0 {
			continue
		}
		blob, err := sqlite_vec.SerializeFloat32(c.Embedding)
		if err != nil {
			return 0, fmt.Errorf("serialize embedding of chunk %d of %s: %w", c.Ord, e.Path, err)
		}
		if _, err := insertVec.ExecContext(ctx, chunkID, blob); err != nil {
			return 0, fmt.Errorf("index chunk %d of %s for vector search: %w", c.Ord, e.Path, vecInsertCause(ctx, tx, err))
		}
	}

	if assert.Enabled {
		assertRowSets(ctx, tx, entryID, len(chunks), dim)
		assertLinkSets(ctx, tx, entryID, e)
	}
	if err := tx.Commit(); err != nil {
		return 0, fmt.Errorf("commit replace of %s: %w", e.Path, err)
	}
	return entryID, nil
}

// assertRowSets checks, inside the transaction that just wrote them, that the entry's chunk ids
// are the same set in chunks, chunks_fts, chunks_exact and (for dim > 0) chunks_vec. The rows were written by
// this transaction, so a mismatch is a kb bug, not bad data.
func assertRowSets(ctx context.Context, tx *sql.Tx, entryID int64, n, dim int) {
	failed := false
	ids := func(query string) []int64 {
		rows, err := tx.QueryContext(ctx, query, entryID)
		if err != nil {
			failed = true // a driver failure is reported by Commit, not by an assertion
			return nil
		}
		defer rows.Close()
		var out []int64
		for rows.Next() {
			var id int64
			if rows.Scan(&id) != nil {
				failed = true
			}
			out = append(out, id)
		}
		if rows.Err() != nil {
			failed = true
		}
		return out
	}
	const sub = "(SELECT id FROM chunks WHERE entry_id = ?)"
	chunkIDs := ids("SELECT id FROM chunks WHERE entry_id = ? ORDER BY id")
	ftsIDs := ids("SELECT rowid FROM chunks_fts WHERE rowid IN " + sub + " ORDER BY rowid")
	exactIDs := ids("SELECT rowid FROM chunks_exact WHERE rowid IN " + sub + " ORDER BY rowid")
	var vecIDs []int64
	if dim > 0 {
		vecIDs = ids("SELECT chunk_id FROM chunks_vec WHERE chunk_id IN " + sub + " ORDER BY chunk_id")
	}
	if failed {
		return
	}
	assert.Invariant(len(chunkIDs) == n, "ReplaceEntry: entry %d has %d chunk rows, wrote %d", entryID, len(chunkIDs), n)
	assert.Invariant(slices.Equal(ftsIDs, chunkIDs), "ReplaceEntry: entry %d chunks_fts ids %v != chunks ids %v", entryID, ftsIDs, chunkIDs)
	assert.Invariant(slices.Equal(exactIDs, chunkIDs), "ReplaceEntry: entry %d chunks_exact ids %v != chunks ids %v", entryID, exactIDs, chunkIDs)
	if dim > 0 {
		assert.Invariant(slices.Equal(vecIDs, chunkIDs), "ReplaceEntry: entry %d chunks_vec ids %v != chunks ids %v", entryID, vecIDs, chunkIDs)
	}
}

// DeleteEntry removes an entry, its chunks and every index of them, its tags and outgoing edges,
// and leaves the edges that pointed at it dangling. A missing entry is not an error:
// `kb rm` deletes the file first and must still succeed on a stale index.
func (s *Store) DeleteEntry(ctx context.Context, path string) error {
	if err := s.deleteEntry(ctx, path); err != nil {
		return wrap(ctx, err, s.path, "sqlite.DeleteEntry", "could not remove %s from the index", path)
	}
	return nil
}

func (s *Store) deleteEntry(ctx context.Context, path string) error {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("begin delete of %s: %w", path, err)
	}
	defer tx.Rollback()

	old, err := getEntry(ctx, tx, path)
	if err != nil || old == nil {
		return err
	}
	if err := clearChunks(ctx, tx, old); err != nil {
		return err
	}
	if err := clearLinks(ctx, tx, old.ID, path); err != nil {
		return err
	}
	if _, err := tx.ExecContext(ctx, "UPDATE edges SET dst_entry_id = NULL WHERE dst_entry_id = ?", old.ID); err != nil {
		return fmt.Errorf("detach links to %s: %w", path, err)
	}
	if _, err := tx.ExecContext(ctx, "DELETE FROM entries WHERE id = ?", old.ID); err != nil {
		return fmt.Errorf("delete entry %s: %w", path, err)
	}
	if err := tx.Commit(); err != nil {
		return fmt.Errorf("commit delete of %s: %w", path, err)
	}
	return nil
}

// clearChunks removes every chunk of an entry from chunks, chunks_fts, chunks_exact and chunks_vec
// (when it exists). contentless_delete=1 lets an FTS row go with a plain DELETE by rowid.
func clearChunks(ctx context.Context, tx *sql.Tx, e *store.EntryRow) error {
	ids, err := chunkIDsOf(ctx, tx, e.ID)
	if err != nil {
		return fmt.Errorf("read old chunks of %s: %w", e.Path, err)
	}
	if len(ids) == 0 {
		return nil
	}
	hasVec, err := vecTableExists(ctx, tx)
	if err != nil {
		return fmt.Errorf("clear chunks of %s: %w", e.Path, err)
	}
	for _, id := range ids {
		if _, err := tx.ExecContext(ctx, "DELETE FROM chunks_fts WHERE rowid = ?", id); err != nil {
			return fmt.Errorf("un-index chunk %d of %s: %w", id, e.Path, err)
		}
		if _, err := tx.ExecContext(ctx, "DELETE FROM chunks_exact WHERE rowid = ?", id); err != nil {
			return fmt.Errorf("un-index chunk %d of %s for identifier search: %w", id, e.Path, err)
		}
		if !hasVec {
			continue
		}
		if _, err := tx.ExecContext(ctx, "DELETE FROM chunks_vec WHERE chunk_id = ?", id); err != nil {
			return fmt.Errorf("un-index chunk %d of %s for vector search: %w", id, e.Path, err)
		}
	}
	if _, err := tx.ExecContext(ctx, "DELETE FROM chunks WHERE entry_id = ?", e.ID); err != nil {
		return fmt.Errorf("delete chunks of %s: %w", e.Path, err)
	}
	return nil
}

// chunkIDsOf lists an entry's chunk ids.
func chunkIDsOf(ctx context.Context, tx *sql.Tx, entryID int64) ([]int64, error) {
	rows, err := tx.QueryContext(ctx, "SELECT id FROM chunks WHERE entry_id = ?", entryID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var ids []int64
	for rows.Next() {
		var id int64
		if err := rows.Scan(&id); err != nil {
			return nil, err
		}
		ids = append(ids, id)
	}
	return ids, rows.Err()
}

// getEntry reads one entry by path through q (the db or an open transaction); (nil, nil) when
// there is none.
func getEntry(ctx context.Context, q queryRower, path string) (*store.EntryRow, error) {
	row := q.QueryRowContext(ctx, `SELECT `+entryColumns+` FROM entries WHERE path = ?`, path)
	e, err := scanEntry(row)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("read entry %s: %w", path, err)
	}
	return e, nil
}

// scanner is satisfied by both *sql.Row and *sql.Rows.
type scanner interface{ Scan(dest ...any) error }

// scanEntry reads one entries row: NULL columns become "" and the tags and aliases JSON slices.
func scanEntry(row scanner) (*store.EntryRow, error) {
	var e store.EntryRow
	var tagsJSON, aliasesJSON string
	var updated, verified sql.NullString
	if err := row.Scan(&e.ID, &e.Path, &e.Kind, &e.Title, &e.Summary, &tagsJSON, &aliasesJSON, &updated, &verified, &e.BodyHash, &e.IndexedAt); err != nil {
		return nil, err
	}
	e.Updated = updated.String
	e.Verified = verified.String
	var err error
	if e.Tags, err = decodeList("tags", tagsJSON); err != nil {
		return nil, fmt.Errorf("entry %s: %w", e.Path, err)
	}
	if e.Aliases, err = decodeList("aliases", aliasesJSON); err != nil {
		return nil, fmt.Errorf("entry %s: %w", e.Path, err)
	}
	return &e, nil
}

// encodeList renders a list as the JSON array in entries.tags or entries.aliases, as written (the
// tag filter reads entry_tags, not this). An empty slice is "[]", never "null".
func encodeList(items []string) (string, error) {
	if len(items) == 0 {
		return "[]", nil
	}
	b, err := json.Marshal(items)
	if err != nil {
		return "", fmt.Errorf("encode list: %w", err)
	}
	return string(b), nil
}

// decodeList reads entries.tags or entries.aliases (named by column) back. "" and "[]" mean an
// empty list; anything else that is not a JSON array of strings is a corrupt row.
func decodeList(column, s string) ([]string, error) {
	if s == "" || s == "[]" {
		return nil, nil
	}
	var items []string
	if err := json.Unmarshal([]byte(s), &items); err != nil {
		return nil, fmt.Errorf("decode %s %q: %w", column, s, err)
	}
	return items, nil
}

// deserializeFloat32 decodes a sqlite-vec blob (little-endian float32s).
func deserializeFloat32(blob []byte) ([]float32, error) {
	if len(blob)%4 != 0 {
		return nil, fmt.Errorf("vector blob is %d bytes, not a whole number of float32s", len(blob))
	}
	out := make([]float32, len(blob)/4)
	for i := range out {
		out[i] = math.Float32frombits(binary.LittleEndian.Uint32(blob[i*4:]))
	}
	return out, nil
}

// nullString writes "" as SQL NULL.
func nullString(s string) any {
	if s == "" {
		return nil
	}
	return s
}

// nowUTC is the ISO-8601 UTC stamp used for indexed_at, created_at and ts columns.
func nowUTC() string { return time.Now().UTC().Format(time.RFC3339) }

// vecChunkFailure is the generic message sqlite-vec returns, as SQLITE_ERROR, when the write of a
// vector chunk fails for any reason, a full disk included (queue Q47).
const vecChunkFailure = "Could not insert a new vector chunk"

// vecInsertCause returns err, or, when err is sqlite-vec's generic chunk failure, the SQLITE_FULL
// that a probe write in the same transaction hits when the database cannot grow: the probe creates
// a table (it needs a new page, as the vec chunk did) and the transaction rolls back with the
// error. The probe error comes first so classify sees SQLITE_FULL and the cause chain keeps both.
func vecInsertCause(ctx context.Context, tx *sql.Tx, err error) error {
	var se sqlite3.Error
	if !errors.As(err, &se) || se.Code != sqlite3.ErrError || !strings.Contains(err.Error(), vecChunkFailure) {
		return err
	}
	_, perr := tx.ExecContext(ctx, "CREATE TABLE kb_full_probe (x)")
	var pe sqlite3.Error
	if errors.As(perr, &pe) && pe.Code == sqlite3.ErrFull {
		return fmt.Errorf("%w (%w)", perr, err)
	}
	return err
}
