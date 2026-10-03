// Entry and chunk writes. Everything in this file keeps four tables consistent with each other:
//
//	entries      the metadata row, one per knowledge entry
//	chunks       the indexed pieces of that entry
//	chunks_fts   the FTS5 index over chunks
//	chunks_vec   the sqlite-vec index over chunks
//
// chunks_fts is a CONTENTLESS FTS5 table (an empty content= option, contentless_delete=1) whose rowid is
// chunks.id. Contentless means SQLite keeps only the inverted index, no copy of the values, so
// its three columns — text, heading, title — are WRITE-ONLY: MATCH and bm25() work, but
// `SELECT text FROM chunks_fts` does not. Readers get text and heading by joining chunks on
// rowid, and filter by tag by joining entries; title is in the table purely to carry the bm25
// weight (text 1.0, heading 2.0, title 3.0).
//
// Nothing maintains chunks_fts or chunks_vec automatically — content-table triggers need a
// content table, and the ON DELETE CASCADE on chunks reaches ordinary tables only — so every
// insert into chunks is paired here with an insert into both indexes, and every delete with
// `DELETE FROM chunks_fts WHERE rowid = ?` and `DELETE FROM chunks_vec WHERE chunk_id = ?`,
// inside the same transaction.
package store

import (
	"context"
	"database/sql"
	"encoding/binary"
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"time"

	sqlite_vec "github.com/asg017/sqlite-vec-go-bindings/cgo"
)

// VecDim is the width of the embedding column in schema.sql (`FLOAT[768]`), which is fixed at
// CREATE VIRTUAL TABLE time and therefore fixed for this build. nomic-embed-text produces exactly
// this many dimensions. A model of any other width cannot be stored at all, so EnsureEmbedMeta
// refuses it up front rather than letting every insert fail deep inside a transaction.
const VecDim = 768

// FTSOnlyModel is the embed_meta model of an index built with no embedder (KB_EMBEDDER=none). Its
// dim is 0, its chunks have no vectors, and only FTS can search it. It matches embed.NoneModel.
const FTSOnlyModel = "none"

// ErrEmbedMetaMismatch is returned by EnsureEmbedMeta when the database was built with a
// different embedding model or dimension than the one configured now. Match it with errors.Is;
// the concrete value is an *EmbedMetaMismatchError, which carries both sides.
var ErrEmbedMetaMismatch = errors.New("embedding model mismatch")

// EmbedMetaMismatchError reports the stored embed_meta row against the configured embedder.
type EmbedMetaMismatchError struct {
	StoredModel string
	StoredDim   int
	WantModel   string
	WantDim     int
}

func (e *EmbedMetaMismatchError) Error() string {
	return fmt.Sprintf("%s: this database was indexed with %s (%d dims) but this run uses %s (%d dims); rebuild it with `kb reindex --all`",
		ErrEmbedMetaMismatch, e.StoredModel, e.StoredDim, e.WantModel, e.WantDim)
}

// Unwrap makes errors.Is(err, ErrEmbedMetaMismatch) true.
func (e *EmbedMetaMismatchError) Unwrap() error { return ErrEmbedMetaMismatch }

// EntryRow is a row of the entries table. Updated and Verified are empty strings when the column
// is NULL; Tags is the decoded JSON array.
type EntryRow struct {
	ID        int64
	Path      string
	Kind      string
	Title     string
	Summary   string
	Tags      []string
	Updated   string
	Verified  string
	BodyHash  string
	IndexedAt string
}

// ChunkRow is a row of the chunks table.
type ChunkRow struct {
	ID         int64
	EntryID    int64
	Ord        int
	SourceFile string
	Heading    string
	Text       string
	TextHash   string
}

// EntryInput is what the indexer knows about an entry before it is written. Path is the
// repo-relative front door ("foo.md" or "droplet/README.md") and is the unique key; Kind is
// entry.KindFile or entry.KindDir.
type EntryInput struct {
	Path     string
	Kind     string
	Title    string
	Summary  string
	Tags     []string
	Updated  string
	Verified string
	BodyHash string
}

// ChunkInput is one chunk plus the vector to index it by. Embedding must be non-nil and exactly
// embed_meta.dim long: ReplaceEntryChunks refuses the whole batch otherwise, because a chunk
// without a vector would be findable by FTS and invisible to semantic search.
type ChunkInput struct {
	Ord        int
	SourceFile string
	Heading    string
	Text       string
	TextHash   string
	Embedding  []float32
}

// EnsureEmbedMeta records which embedder built this index, or checks the record. The first call
// on a fresh database inserts the row; later calls compare and return an *EmbedMetaMismatchError
// (matching ErrEmbedMetaMismatch) when the model or the dimension has changed, since stored
// vectors from another model are meaningless and vec0's FLOAT[768] column would reject another
// width outright.
func (s *Store) EnsureEmbedMeta(model string, dim int) error {
	if model == "" {
		return errors.New("embed_meta: model must not be empty")
	}
	if model == FTSOnlyModel {
		if dim != 0 {
			return fmt.Errorf("embed_meta: model %s is FTS only and must have 0 dimensions, got %d", model, dim)
		}
	} else if dim != VecDim {
		return fmt.Errorf("embed_meta: this build's chunks_vec column is FLOAT[%d]; model %s reports %d dimensions, so its vectors cannot be stored",
			VecDim, model, dim)
	}

	var storedModel string
	var storedDim int
	err := s.db.QueryRow("SELECT model, dim FROM embed_meta WHERE id = 1").Scan(&storedModel, &storedDim)
	switch {
	case errors.Is(err, sql.ErrNoRows):
		_, err := s.db.Exec(
			"INSERT INTO embed_meta (id, model, dim, created_at) VALUES (1, ?, ?, ?)",
			model, dim, nowUTC())
		if err != nil {
			return fmt.Errorf("record embed_meta: %w", err)
		}
		return nil
	case err != nil:
		return fmt.Errorf("read embed_meta: %w", err)
	case storedModel != model || storedDim != dim:
		return &EmbedMetaMismatchError{StoredModel: storedModel, StoredDim: storedDim, WantModel: model, WantDim: dim}
	default:
		return nil
	}
}

// EmbedMeta returns the recorded model and dimension. ok is false on a database that has never
// been indexed.
func (s *Store) EmbedMeta() (model string, dim int, ok bool, err error) {
	err = s.db.QueryRow("SELECT model, dim FROM embed_meta WHERE id = 1").Scan(&model, &dim)
	if errors.Is(err, sql.ErrNoRows) {
		return "", 0, false, nil
	}
	if err != nil {
		return "", 0, false, fmt.Errorf("read embed_meta: %w", err)
	}
	return model, dim, true, nil
}

// ResetForReindex empties the derived index for `kb reindex --all`: every entry (chunks cascade),
// every FTS row, every vector, and the embed_meta row, so the caller can record a new model. The
// search log tables (searches, search_results, search_candidates, search_feedback) are deliberately
// left alone — they are the only data in this file that is not rebuildable from the Markdown.
func (s *Store) ResetForReindex(ctx context.Context) error {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("begin reset: %w", err)
	}
	defer tx.Rollback()

	// 'delete-all' empties a contentless index in one statement, without a row-by-row delete.
	stmts := []string{
		"INSERT INTO chunks_fts(chunks_fts) VALUES ('delete-all')",
		"DELETE FROM chunks_vec",
		"DELETE FROM chunks",
		"DELETE FROM entries",
		"DELETE FROM embed_meta",
	}
	for _, stmt := range stmts {
		if _, err := tx.ExecContext(ctx, stmt); err != nil {
			return fmt.Errorf("reset (%s): %w", stmt, err)
		}
	}
	if err := tx.Commit(); err != nil {
		return fmt.Errorf("commit reset: %w", err)
	}
	return nil
}

// GetEntryByPath returns the entry with this repo-relative path, or (nil, nil) when there is none.
func (s *Store) GetEntryByPath(path string) (*EntryRow, error) {
	row := s.db.QueryRow(`
		SELECT id, path, kind, title, summary, tags, updated, verified, body_hash, indexed_at
		FROM entries WHERE path = ?`, path)
	e, err := scanEntry(row)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("read entry %s: %w", path, err)
	}
	return e, nil
}

// ListEntries returns every entry, sorted by path.
func (s *Store) ListEntries() ([]EntryRow, error) {
	rows, err := s.db.Query(`
		SELECT id, path, kind, title, summary, tags, updated, verified, body_hash, indexed_at
		FROM entries ORDER BY path`)
	if err != nil {
		return nil, fmt.Errorf("list entries: %w", err)
	}
	defer rows.Close()

	var out []EntryRow
	for rows.Next() {
		e, err := scanEntry(rows)
		if err != nil {
			return nil, fmt.Errorf("list entries: %w", err)
		}
		out = append(out, *e)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("list entries: %w", err)
	}
	return out, nil
}

// ListChunks returns one entry's chunks in ord order.
func (s *Store) ListChunks(entryID int64) ([]ChunkRow, error) {
	rows, err := s.db.Query(`
		SELECT id, entry_id, ord, source_file, heading, text, text_hash
		FROM chunks WHERE entry_id = ? ORDER BY ord`, entryID)
	if err != nil {
		return nil, fmt.Errorf("list chunks of entry %d: %w", entryID, err)
	}
	defer rows.Close()

	var out []ChunkRow
	for rows.Next() {
		var c ChunkRow
		if err := rows.Scan(&c.ID, &c.EntryID, &c.Ord, &c.SourceFile, &c.Heading, &c.Text, &c.TextHash); err != nil {
			return nil, fmt.Errorf("list chunks of entry %d: %w", entryID, err)
		}
		out = append(out, c)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("list chunks of entry %d: %w", entryID, err)
	}
	return out, nil
}

// ExistingEmbeddings returns the vectors already stored for an entry, keyed by the text_hash of
// the chunk they belong to. Chunking rule 6: on reindex a chunk whose text_hash is unchanged
// reuses its vector instead of costing another call to Ollama (~0.4 s each), so the caller looks
// each new chunk's hash up here before batching the rest for the embedder.
func (s *Store) ExistingEmbeddings(ctx context.Context, entryID int64) (map[string][]float32, error) {
	rows, err := s.db.QueryContext(ctx, `
		SELECT c.text_hash, v.embedding
		FROM chunks c JOIN chunks_vec v ON v.chunk_id = c.id
		WHERE c.entry_id = ?`, entryID)
	if err != nil {
		return nil, fmt.Errorf("read embeddings of entry %d: %w", entryID, err)
	}
	defer rows.Close()

	out := make(map[string][]float32)
	for rows.Next() {
		var hash string
		var blob []byte
		if err := rows.Scan(&hash, &blob); err != nil {
			return nil, fmt.Errorf("read embeddings of entry %d: %w", entryID, err)
		}
		vec, err := deserializeFloat32(blob)
		if err != nil {
			return nil, fmt.Errorf("read embeddings of entry %d: %w", entryID, err)
		}
		out[hash] = vec
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("read embeddings of entry %d: %w", entryID, err)
	}
	return out, nil
}

// ReplaceEntryChunks writes an entry and its complete set of chunks in one transaction: the entry
// row is inserted or updated by path, the entry's previous chunks and their FTS and vector rows
// are removed, and the new chunks are inserted into chunks, chunks_fts and chunks_vec together.
// Either all four tables move or none do, so a crash or a failing embedding can never leave the
// index half-rewritten.
//
// Every chunk must carry an embedding of exactly the dimension recorded in embed_meta, which must
// already exist (call EnsureEmbedMeta first). An FTS-only index (embed_meta dim 0) is the
// exception: its chunks carry no embedding and nothing is written to chunks_vec.
func (s *Store) ReplaceEntryChunks(ctx context.Context, e EntryInput, chunks []ChunkInput) (entryID int64, err error) {
	if e.Path == "" {
		return 0, errors.New("replace entry: path must not be empty")
	}

	_, dim, ok, err := s.EmbedMeta()
	if err != nil {
		return 0, err
	}
	if !ok {
		return 0, errors.New("replace entry: embed_meta is not set; call EnsureEmbedMeta before indexing")
	}
	for i, c := range chunks {
		if dim == 0 {
			if c.Embedding != nil {
				return 0, fmt.Errorf("replace entry %s: chunk %d (ord %d) has an embedding but this index is FTS only", e.Path, i, c.Ord)
			}
			continue
		}
		if c.Embedding == nil {
			return 0, fmt.Errorf("replace entry %s: chunk %d (ord %d) has no embedding", e.Path, i, c.Ord)
		}
		if len(c.Embedding) != dim {
			return 0, fmt.Errorf("replace entry %s: chunk %d (ord %d) has %d dimensions, embed_meta says %d",
				e.Path, i, c.Ord, len(c.Embedding), dim)
		}
	}

	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return 0, fmt.Errorf("begin replace of %s: %w", e.Path, err)
	}
	defer tx.Rollback()

	// Look the entry up before the upsert: its id is what identifies the chunks (and therefore
	// the FTS and vector rows) that have to go.
	old, err := getEntryTx(ctx, tx, e.Path)
	if err != nil {
		return 0, err
	}
	if old != nil {
		if err := clearChunksTx(ctx, tx, old); err != nil {
			return 0, err
		}
	}

	tagsJSON, err := encodeTags(e.Tags)
	if err != nil {
		return 0, fmt.Errorf("replace entry %s: %w", e.Path, err)
	}
	err = tx.QueryRowContext(ctx, `
		INSERT INTO entries (path, kind, title, summary, tags, updated, verified, body_hash, indexed_at)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?)
		ON CONFLICT(path) DO UPDATE SET
			kind = excluded.kind, title = excluded.title, summary = excluded.summary,
			tags = excluded.tags, updated = excluded.updated, verified = excluded.verified,
			body_hash = excluded.body_hash, indexed_at = excluded.indexed_at
		RETURNING id`,
		e.Path, e.Kind, e.Title, e.Summary, tagsJSON,
		nullString(e.Updated), nullString(e.Verified), e.BodyHash, nowUTC(),
	).Scan(&entryID)
	if err != nil {
		return 0, fmt.Errorf("write entry %s: %w", e.Path, err)
	}

	insertChunk, err := tx.PrepareContext(ctx, `
		INSERT INTO chunks (entry_id, ord, source_file, heading, text, text_hash)
		VALUES (?, ?, ?, ?, ?, ?) RETURNING id`)
	if err != nil {
		return 0, fmt.Errorf("prepare chunk insert: %w", err)
	}
	defer insertChunk.Close()
	insertFTS, err := tx.PrepareContext(ctx, `
		INSERT INTO chunks_fts (rowid, text, heading, title) VALUES (?, ?, ?, ?)`)
	if err != nil {
		return 0, fmt.Errorf("prepare fts insert: %w", err)
	}
	defer insertFTS.Close()
	insertVec, err := tx.PrepareContext(ctx, `INSERT INTO chunks_vec (chunk_id, embedding) VALUES (?, ?)`)
	if err != nil {
		return 0, fmt.Errorf("prepare vec insert: %w", err)
	}
	defer insertVec.Close()

	for _, c := range chunks {
		var chunkID int64
		if err := insertChunk.QueryRowContext(ctx, entryID, c.Ord, c.SourceFile, c.Heading, c.Text, c.TextHash).Scan(&chunkID); err != nil {
			return 0, fmt.Errorf("insert chunk %d of %s: %w", c.Ord, e.Path, err)
		}
		if _, err := insertFTS.ExecContext(ctx, chunkID, c.Text, c.Heading, e.Title); err != nil {
			return 0, fmt.Errorf("index chunk %d of %s for FTS: %w", c.Ord, e.Path, err)
		}
		if dim == 0 {
			continue
		}
		blob, err := sqlite_vec.SerializeFloat32(c.Embedding)
		if err != nil {
			return 0, fmt.Errorf("serialize embedding of chunk %d of %s: %w", c.Ord, e.Path, err)
		}
		if _, err := insertVec.ExecContext(ctx, chunkID, blob); err != nil {
			return 0, fmt.Errorf("index chunk %d of %s for vector search: %w", c.Ord, e.Path, err)
		}
	}

	if err := tx.Commit(); err != nil {
		return 0, fmt.Errorf("commit replace of %s: %w", e.Path, err)
	}
	return entryID, nil
}

// DeleteEntry removes an entry, its chunks and both indexes. Deleting an entry that is not in the
// database is not an error: `kb rm` deletes the file first and must still succeed on a repo whose
// index is stale.
func (s *Store) DeleteEntry(ctx context.Context, path string) error {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("begin delete of %s: %w", path, err)
	}
	defer tx.Rollback()

	old, err := getEntryTx(ctx, tx, path)
	if err != nil {
		return err
	}
	if old == nil {
		return nil
	}
	if err := clearChunksTx(ctx, tx, old); err != nil {
		return err
	}
	if _, err := tx.ExecContext(ctx, "DELETE FROM entries WHERE id = ?", old.ID); err != nil {
		return fmt.Errorf("delete entry %s: %w", path, err)
	}
	if err := tx.Commit(); err != nil {
		return fmt.Errorf("commit delete of %s: %w", path, err)
	}
	return nil
}

// clearChunksTx removes every chunk of an entry from chunks, chunks_fts and chunks_vec. Both
// indexes are keyed by the chunk's id, so the ids are all it needs: contentless_delete=1 lets the
// FTS row be removed with a plain DELETE by rowid, with none of the old-value bookkeeping an
// external-content table's 'delete' command demands.
func clearChunksTx(ctx context.Context, tx *sql.Tx, e *EntryRow) error {
	rows, err := tx.QueryContext(ctx, "SELECT id FROM chunks WHERE entry_id = ?", e.ID)
	if err != nil {
		return fmt.Errorf("read old chunks of %s: %w", e.Path, err)
	}
	var ids []int64
	for rows.Next() {
		var id int64
		if err := rows.Scan(&id); err != nil {
			rows.Close()
			return fmt.Errorf("read old chunks of %s: %w", e.Path, err)
		}
		ids = append(ids, id)
	}
	if err := rows.Err(); err != nil {
		rows.Close()
		return fmt.Errorf("read old chunks of %s: %w", e.Path, err)
	}
	rows.Close()
	if len(ids) == 0 {
		return nil
	}

	for _, id := range ids {
		if _, err := tx.ExecContext(ctx, "DELETE FROM chunks_fts WHERE rowid = ?", id); err != nil {
			return fmt.Errorf("un-index chunk %d of %s: %w", id, e.Path, err)
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

// getEntryTx is GetEntryByPath inside an open transaction.
func getEntryTx(ctx context.Context, tx *sql.Tx, path string) (*EntryRow, error) {
	row := tx.QueryRowContext(ctx, `
		SELECT id, path, kind, title, summary, tags, updated, verified, body_hash, indexed_at
		FROM entries WHERE path = ?`, path)
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

// scanEntry reads one entries row, turning the nullable columns into empty strings and the tags
// JSON back into a slice.
func scanEntry(row scanner) (*EntryRow, error) {
	var e EntryRow
	var tagsJSON string
	var updated, verified sql.NullString
	if err := row.Scan(&e.ID, &e.Path, &e.Kind, &e.Title, &e.Summary, &tagsJSON, &updated, &verified, &e.BodyHash, &e.IndexedAt); err != nil {
		return nil, err
	}
	e.Updated = updated.String
	e.Verified = verified.String
	tags, err := decodeTags(tagsJSON)
	if err != nil {
		return nil, fmt.Errorf("entry %s: %w", e.Path, err)
	}
	e.Tags = tags
	return &e, nil
}

// encodeTags renders tags as the JSON array stored in entries.tags, which is what search's tag
// filter joins against. A nil or empty slice is "[]", never "null", so a LIKE over that column
// never has to special-case it.
func encodeTags(tags []string) (string, error) {
	if len(tags) == 0 {
		return "[]", nil
	}
	b, err := json.Marshal(tags)
	if err != nil {
		return "", fmt.Errorf("encode tags: %w", err)
	}
	return string(b), nil
}

// decodeTags reads entries.tags back. "" and "[]" both mean no tags; anything else that is not a
// JSON array is a corrupt row and is reported.
func decodeTags(s string) ([]string, error) {
	if s == "" || s == "[]" {
		return nil, nil
	}
	var tags []string
	if err := json.Unmarshal([]byte(s), &tags); err != nil {
		return nil, fmt.Errorf("decode tags %q: %w", s, err)
	}
	return tags, nil
}

// deserializeFloat32 decodes a sqlite-vec vector blob: little-endian float32s, the inverse of
// sqlite_vec.SerializeFloat32.
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

// nullString writes an empty optional front-matter field (updated, verified) as SQL NULL.
func nullString(s string) any {
	if s == "" {
		return nil
	}
	return s
}

// nowUTC is the ISO-8601 UTC stamp used for indexed_at and created_at.
func nowUTC() string { return time.Now().UTC().Format(time.RFC3339) }
