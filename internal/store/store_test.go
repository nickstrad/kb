package store

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"errors"
	"fmt"
	"math"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	sqlite_vec "github.com/asg017/sqlite-vec-go-bindings/cgo"

	"knowledge/kb/internal/embed/fake"
)

// TestOpenAppliesSchema opens a fresh database in a temp directory and checks the three things a
// broken build would break: the migration stamp, sqlite-vec, and the FTS5 virtual table.
func TestOpenAppliesSchema(t *testing.T) {
	path := filepath.Join(t.TempDir(), "kb.sqlite")
	s, err := Open(path)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	defer s.Close()

	var version int
	if err := s.DB().QueryRow("PRAGMA user_version").Scan(&version); err != nil {
		t.Fatalf("read user_version: %v", err)
	}
	if version != schemaVersion {
		t.Errorf("user_version = %d, want %d", version, schemaVersion)
	}

	var vecVersion string
	if err := s.DB().QueryRow("SELECT vec_version()").Scan(&vecVersion); err != nil {
		t.Fatalf("vec_version(): %v", err)
	}
	if vecVersion == "" {
		t.Error("vec_version() returned an empty string")
	}

	for _, table := range []string{"entries", "chunks", "chunks_fts", "chunks_vec", "searches", "search_results", "search_candidates", "search_feedback", "embed_meta"} {
		var name string
		err := s.DB().QueryRow("SELECT name FROM sqlite_master WHERE name = ?", table).Scan(&name)
		if err != nil {
			t.Errorf("table %s missing: %v", table, err)
		}
	}

	// The FTS index must actually answer a MATCH, not merely exist.
	if _, err := s.DB().Exec("INSERT INTO chunks_fts(rowid, text, heading, title) VALUES (1, 'wal mode journal', 'Pragmas', 'Store')"); err != nil {
		t.Fatalf("insert into chunks_fts: %v", err)
	}
	var rowid int
	if err := s.DB().QueryRow("SELECT rowid FROM chunks_fts WHERE chunks_fts MATCH 'journal'").Scan(&rowid); err != nil {
		t.Fatalf("MATCH on chunks_fts: %v", err)
	}
	if rowid != 1 {
		t.Errorf("MATCH returned rowid %d, want 1", rowid)
	}
}

// TestOpenSetsPragmas checks the three connection pragmas the plan requires.
func TestOpenSetsPragmas(t *testing.T) {
	path := filepath.Join(t.TempDir(), "kb.sqlite")
	s, err := Open(path)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	defer s.Close()

	var journal string
	if err := s.DB().QueryRow("PRAGMA journal_mode").Scan(&journal); err != nil {
		t.Fatalf("journal_mode: %v", err)
	}
	if journal != "wal" {
		t.Errorf("journal_mode = %q, want wal", journal)
	}

	var foreignKeys int
	if err := s.DB().QueryRow("PRAGMA foreign_keys").Scan(&foreignKeys); err != nil {
		t.Fatalf("foreign_keys: %v", err)
	}
	if foreignKeys != 1 {
		t.Errorf("foreign_keys = %d, want 1", foreignKeys)
	}

	var busy int
	if err := s.DB().QueryRow("PRAGMA busy_timeout").Scan(&busy); err != nil {
		t.Fatalf("busy_timeout: %v", err)
	}
	if busy != 5000 {
		t.Errorf("busy_timeout = %d, want 5000", busy)
	}
}

// TestOpenIsIdempotent reopens an existing database: the migration must not run twice.
func TestOpenIsIdempotent(t *testing.T) {
	path := filepath.Join(t.TempDir(), "kb.sqlite")
	first, err := Open(path)
	if err != nil {
		t.Fatalf("first Open: %v", err)
	}
	first.Close()

	second, err := Open(path)
	if err != nil {
		t.Fatalf("second Open: %v", err)
	}
	defer second.Close()
	var version int
	if err := second.DB().QueryRow("PRAGMA user_version").Scan(&version); err != nil {
		t.Fatalf("read user_version: %v", err)
	}
	if version != schemaVersion {
		t.Errorf("user_version = %d, want %d", version, schemaVersion)
	}
}

// --- P2.2: entry and chunk writes -------------------------------------------------------------

// testDim is the only width the schema can store (see VecDim), so the fake embedder is
// configured with the real thing rather than a convenient 4.
const testDim = VecDim

// newTestStore opens a fresh database in a temp directory with embed_meta already recorded.
func newTestStore(t *testing.T) (*Store, *fake.Embedder) {
	t.Helper()
	s, err := Open(filepath.Join(t.TempDir(), "kb.sqlite"))
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	t.Cleanup(func() { s.Close() })
	emb := fake.New("fake-embed", testDim)
	if err := s.EnsureEmbedMeta(emb.Model(), emb.Dim()); err != nil {
		t.Fatalf("EnsureEmbedMeta: %v", err)
	}
	return s, emb
}

// vecFor returns the deterministic test vector for a text.
func vecFor(t *testing.T, emb *fake.Embedder, text string) []float32 {
	t.Helper()
	vs, err := emb.Embed(context.Background(), []string{text})
	if err != nil {
		t.Fatalf("fake embed: %v", err)
	}
	return vs[0]
}

// chunkInputs builds ChunkInput values with hashes and embeddings filled in from the texts.
func chunkInputs(t *testing.T, emb *fake.Embedder, texts ...string) []ChunkInput {
	t.Helper()
	out := make([]ChunkInput, len(texts))
	for i, text := range texts {
		sum := sha256.Sum256([]byte(text))
		out[i] = ChunkInput{
			Ord:        i,
			SourceFile: "sample.md",
			Heading:    fmt.Sprintf("Section %d", i),
			Text:       text,
			TextHash:   hex.EncodeToString(sum[:]),
			Embedding:  vecFor(t, emb, text),
		}
	}
	return out
}

// sampleEntry is the entry row the write tests reuse.
func sampleEntry() EntryInput {
	return EntryInput{
		Path:     "sample.md",
		Kind:     "file",
		Title:    "Sample entry",
		Summary:  "A sample entry used by the store tests.",
		Tags:     []string{"droplet", "sqlite"},
		Updated:  "2026-09-13",
		BodyHash: "hash-v1",
	}
}

// countRows answers a scalar count query.
func countRows(t *testing.T, s *Store, query string, args ...any) int {
	t.Helper()
	var n int
	if err := s.DB().QueryRow(query, args...).Scan(&n); err != nil {
		t.Fatalf("%s: %v", query, err)
	}
	return n
}

// countFTSDocs counts the rows actually present in the FTS index.
//
// This is trustworthy only because chunks_fts is contentless: with the previous external-content
// shape the same query would have scanned the chunks table and reported the right number even if
// the index were completely out of sync. TestFTSCountReadsTheIndex pins that property down.
func countFTSDocs(t *testing.T, s *Store) int {
	t.Helper()
	var n int
	if err := s.DB().QueryRow("SELECT count(*) FROM chunks_fts").Scan(&n); err != nil {
		t.Fatalf("count fts rows: %v", err)
	}
	return n
}

// TestFTSCountReadsTheIndex is the guard for countFTSDocs above: deleting the chunks rows behind
// the index's back must NOT change what chunks_fts reports, or every "the FTS table is in sync"
// assertion in this file would be vacuous.
func TestFTSCountReadsTheIndex(t *testing.T) {
	s, emb := newTestStore(t)
	ctx := context.Background()

	if _, err := s.ReplaceEntryChunks(ctx, sampleEntry(), chunkInputs(t, emb, "alpha", "bravo", "charlie")); err != nil {
		t.Fatalf("ReplaceEntryChunks: %v", err)
	}
	if n := countFTSDocs(t, s); n != 3 {
		t.Fatalf("indexed FTS rows = %d, want 3", n)
	}
	// Bypass the store helpers entirely; chunks_fts has no trigger tying it to this.
	if _, err := s.DB().Exec("DELETE FROM chunks"); err != nil {
		t.Fatalf("raw delete: %v", err)
	}
	if n := countFTSDocs(t, s); n != 3 {
		t.Errorf("after a raw DELETE FROM chunks the FTS count is %d; it must still be 3, "+
			"otherwise this helper is reading the chunks table and proves nothing", n)
	}
	if n := countRows(t, s, "SELECT count(*) FROM chunks_fts WHERE chunks_fts MATCH 'alpha'"); n != 1 {
		t.Errorf("MATCH 'alpha' = %d, want 1 (the index outlives its content table)", n)
	}
}

// TestReplaceEntryChunksWritesAllThreeTables is the core P2.2 check: one call must populate
// chunks, chunks_fts and chunks_vec together, and all three must answer queries.
func TestReplaceEntryChunksWritesAllThreeTables(t *testing.T) {
	s, emb := newTestStore(t)
	ctx := context.Background()

	texts := []string{
		"The tmux daemon dies when the login session ends.",
		"Postgres logs startup failures under /var/log/postgresql.",
		"VACUUM rewrites the SQLite file after a bulk delete.",
	}
	entryID, err := s.ReplaceEntryChunks(ctx, sampleEntry(), chunkInputs(t, emb, texts...))
	if err != nil {
		t.Fatalf("ReplaceEntryChunks: %v", err)
	}
	if entryID == 0 {
		t.Fatal("ReplaceEntryChunks returned entry id 0")
	}

	if n := countRows(t, s, "SELECT count(*) FROM chunks"); n != 3 {
		t.Errorf("chunks = %d, want 3", n)
	}
	if n := countFTSDocs(t, s); n != 3 {
		t.Errorf("indexed FTS rows = %d, want 3", n)
	}
	if n := countRows(t, s, "SELECT count(*) FROM chunks_vec"); n != 3 {
		t.Errorf("chunks_vec = %d, want 3", n)
	}

	// FTS: a word from the third chunk must find exactly that chunk's rowid.
	chunks, err := s.ListChunks(entryID)
	if err != nil {
		t.Fatalf("ListChunks: %v", err)
	}
	if len(chunks) != 3 {
		t.Fatalf("ListChunks returned %d chunks, want 3", len(chunks))
	}
	var ftsRowid int64
	if err := s.DB().QueryRow("SELECT rowid FROM chunks_fts WHERE chunks_fts MATCH 'vacuum'").Scan(&ftsRowid); err != nil {
		t.Fatalf("MATCH 'vacuum': %v", err)
	}
	if ftsRowid != chunks[2].ID {
		t.Errorf("MATCH 'vacuum' returned rowid %d, want %d", ftsRowid, chunks[2].ID)
	}
	// The denormalised title is indexed too, so a title word matches every chunk of the entry.
	if n := countRows(t, s, "SELECT count(*) FROM chunks_fts WHERE chunks_fts MATCH 'title:sample'"); n != 3 {
		t.Errorf("MATCH 'title:sample' returned %d rows, want 3", n)
	}

	// vec0: a KNN search with the second chunk's own vector must return the second chunk.
	query, err := sqlite_vec.SerializeFloat32(vecFor(t, emb, texts[1]))
	if err != nil {
		t.Fatalf("serialize query vector: %v", err)
	}
	var gotID int64
	var distance float64
	err = s.DB().QueryRow(
		"SELECT chunk_id, distance FROM chunks_vec WHERE embedding MATCH ? AND k = 1", query,
	).Scan(&gotID, &distance)
	if err != nil {
		t.Fatalf("KNN query: %v", err)
	}
	if gotID != chunks[1].ID {
		t.Errorf("KNN returned chunk %d, want %d", gotID, chunks[1].ID)
	}
	if distance > 1e-5 {
		t.Errorf("KNN distance to the identical vector = %g, want ~0", distance)
	}

	// The entry row round-trips, tags included.
	got, err := s.GetEntryByPath("sample.md")
	if err != nil {
		t.Fatalf("GetEntryByPath: %v", err)
	}
	if got == nil {
		t.Fatal("GetEntryByPath returned nil for an entry that was just written")
	}
	if got.ID != entryID || got.Title != "Sample entry" || got.BodyHash != "hash-v1" {
		t.Errorf("entry round-trip = %+v", got)
	}
	if len(got.Tags) != 2 || got.Tags[0] != "droplet" || got.Tags[1] != "sqlite" {
		t.Errorf("tags = %v, want [droplet sqlite]", got.Tags)
	}
	if got.Verified != "" {
		t.Errorf("verified = %q, want empty (NULL in the database)", got.Verified)
	}
	if list, err := s.ListEntries(); err != nil || len(list) != 1 {
		t.Errorf("ListEntries = %v, %v; want one entry", list, err)
	}
}

// TestReplaceEntryChunksReplaces rewrites an entry with fewer chunks and a new title: the old
// chunks must disappear from all three tables, and the stale title must no longer match.
func TestReplaceEntryChunksReplaces(t *testing.T) {
	s, emb := newTestStore(t)
	ctx := context.Background()

	first := sampleEntry()
	if _, err := s.ReplaceEntryChunks(ctx, first, chunkInputs(t, emb,
		"alpha content about pragmas",
		"bravo content about journals",
		"charlie content about vacuuming",
	)); err != nil {
		t.Fatalf("first ReplaceEntryChunks: %v", err)
	}

	second := first
	second.Title = "Renamed entry"
	second.Tags = []string{"droplet"}
	second.BodyHash = "hash-v2"
	entryID, err := s.ReplaceEntryChunks(ctx, second, chunkInputs(t, emb,
		"delta content about pragmas",
		"echo content about journals",
	))
	if err != nil {
		t.Fatalf("second ReplaceEntryChunks: %v", err)
	}

	if n := countRows(t, s, "SELECT count(*) FROM chunks"); n != 2 {
		t.Errorf("chunks = %d, want 2", n)
	}
	if n := countFTSDocs(t, s); n != 2 {
		t.Errorf("indexed FTS rows = %d, want 2", n)
	}
	if n := countRows(t, s, "SELECT count(*) FROM chunks_vec"); n != 2 {
		t.Errorf("chunks_vec = %d, want 2", n)
	}
	if n := countRows(t, s, "SELECT count(*) FROM entries"); n != 1 {
		t.Errorf("entries = %d, want 1 (the upsert must not insert a second row)", n)
	}

	// Terms only the removed chunks carried must be gone from the index...
	for _, term := range []string{"alpha", "charlie"} {
		if n := countRows(t, s, "SELECT count(*) FROM chunks_fts WHERE chunks_fts MATCH ?", term); n != 0 {
			t.Errorf("MATCH %q returned %d rows after replacement, want 0", term, n)
		}
	}
	// ...and so must the old title, which is why the pre-upsert entry row is used to un-index.
	if n := countRows(t, s, "SELECT count(*) FROM chunks_fts WHERE chunks_fts MATCH 'title:sample'"); n != 0 {
		t.Errorf("the old title is still indexed (%d rows)", n)
	}
	if n := countRows(t, s, "SELECT count(*) FROM chunks_fts WHERE chunks_fts MATCH 'title:renamed'"); n != 2 {
		t.Errorf("MATCH on the new title returned %d rows, want 2", n)
	}

	got, err := s.GetEntryByPath("sample.md")
	if err != nil || got == nil {
		t.Fatalf("GetEntryByPath: %v, %v", got, err)
	}
	if got.ID != entryID {
		t.Errorf("entry id changed across replacement: %d then %d", entryID, got.ID)
	}
	if got.BodyHash != "hash-v2" || got.Title != "Renamed entry" {
		t.Errorf("entry was not updated: %+v", got)
	}
}

// TestReplaceEntryChunksRollsBackMidInsert is the atomicity test. The old chunks are cleared and
// the entry row is upserted before the new chunks go in, so a failure partway through the insert
// loop would — without one enclosing transaction — leave the entry renamed and its index empty.
// Two chunks sharing an ord violate UNIQUE(entry_id, ord) on the second insert, after the first
// has already been written to all three tables.
func TestReplaceEntryChunksRollsBackMidInsert(t *testing.T) {
	s, emb := newTestStore(t)
	ctx := context.Background()

	original := sampleEntry()
	if _, err := s.ReplaceEntryChunks(ctx, original, chunkInputs(t, emb,
		"alpha content", "bravo content", "charlie content")); err != nil {
		t.Fatalf("first ReplaceEntryChunks: %v", err)
	}

	doomed := original
	doomed.Title = "Renamed entry"
	doomed.BodyHash = "hash-v2"
	bad := chunkInputs(t, emb, "delta content", "echo content")
	bad[1].Ord = bad[0].Ord // UNIQUE (entry_id, ord)

	if _, err := s.ReplaceEntryChunks(ctx, doomed, bad); err == nil {
		t.Fatal("ReplaceEntryChunks accepted two chunks with the same ord")
	}

	if n := countRows(t, s, "SELECT count(*) FROM chunks"); n != 3 {
		t.Errorf("chunks = %d after the failed call, want the original 3", n)
	}
	if n := countFTSDocs(t, s); n != 3 {
		t.Errorf("indexed FTS rows = %d after the failed call, want the original 3", n)
	}
	if n := countRows(t, s, "SELECT count(*) FROM chunks_vec"); n != 3 {
		t.Errorf("chunks_vec = %d after the failed call, want the original 3", n)
	}
	if n := countRows(t, s, "SELECT count(*) FROM chunks_fts WHERE chunks_fts MATCH 'title:sample'"); n != 3 {
		t.Errorf("MATCH on the original title = %d rows, want 3: the rollback did not restore the index", n)
	}
	if n := countRows(t, s, "SELECT count(*) FROM chunks_fts WHERE chunks_fts MATCH 'title:renamed'"); n != 0 {
		t.Errorf("MATCH on the abandoned title = %d rows, want 0", n)
	}
	if n := countRows(t, s, "SELECT count(*) FROM chunks_fts WHERE chunks_fts MATCH 'delta'"); n != 0 {
		t.Errorf("a chunk from the failed call is still indexed (%d rows)", n)
	}

	got, err := s.GetEntryByPath(original.Path)
	if err != nil || got == nil {
		t.Fatalf("GetEntryByPath: %v, %v", got, err)
	}
	if got.Title != original.Title {
		t.Errorf("title = %q after the failed call, want %q", got.Title, original.Title)
	}
	if got.BodyHash != original.BodyHash {
		t.Errorf("body_hash = %q after the failed call, want %q: a reindex would now skip this "+
			"entry as unchanged and the stale chunks would never be repaired", got.BodyHash, original.BodyHash)
	}
}

// TestDeleteEntry checks that removing an entry empties all three tables and is a no-op for a
// path that was never indexed.
func TestDeleteEntry(t *testing.T) {
	s, emb := newTestStore(t)
	ctx := context.Background()

	if _, err := s.ReplaceEntryChunks(ctx, sampleEntry(), chunkInputs(t, emb,
		"first chunk", "second chunk", "third chunk")); err != nil {
		t.Fatalf("ReplaceEntryChunks: %v", err)
	}
	if err := s.DeleteEntry(ctx, "sample.md"); err != nil {
		t.Fatalf("DeleteEntry: %v", err)
	}

	for _, q := range []string{
		"SELECT count(*) FROM entries",
		"SELECT count(*) FROM chunks",
		"SELECT count(*) FROM chunks_vec",
	} {
		if n := countRows(t, s, q); n != 0 {
			t.Errorf("%s = %d, want 0", q, n)
		}
	}
	if n := countFTSDocs(t, s); n != 0 {
		t.Errorf("indexed FTS rows = %d, want 0", n)
	}
	if n := countRows(t, s, "SELECT count(*) FROM chunks_fts WHERE chunks_fts MATCH 'second'"); n != 0 {
		t.Errorf("MATCH 'second' returned %d rows after delete, want 0", n)
	}

	got, err := s.GetEntryByPath("sample.md")
	if err != nil {
		t.Fatalf("GetEntryByPath after delete: %v", err)
	}
	if got != nil {
		t.Errorf("GetEntryByPath returned %+v after delete, want nil", got)
	}
	if err := s.DeleteEntry(ctx, "never-indexed.md"); err != nil {
		t.Errorf("DeleteEntry on an unknown path = %v, want nil", err)
	}
}

// TestExistingEmbeddings round-trips a stored vector: chunking rule 6 reuses it instead of paying
// for another Ollama call, so the bytes that come back must be the bytes that went in.
func TestExistingEmbeddings(t *testing.T) {
	s, emb := newTestStore(t)
	ctx := context.Background()

	inputs := chunkInputs(t, emb, "reuse me unchanged", "rewrite me")
	entryID, err := s.ReplaceEntryChunks(ctx, sampleEntry(), inputs)
	if err != nil {
		t.Fatalf("ReplaceEntryChunks: %v", err)
	}

	stored, err := s.ExistingEmbeddings(ctx, entryID)
	if err != nil {
		t.Fatalf("ExistingEmbeddings: %v", err)
	}
	if len(stored) != 2 {
		t.Fatalf("ExistingEmbeddings returned %d vectors, want 2", len(stored))
	}
	want := inputs[0].Embedding
	got, ok := stored[inputs[0].TextHash]
	if !ok {
		t.Fatalf("no vector stored for text_hash %s", inputs[0].TextHash)
	}
	if len(got) != len(want) {
		t.Fatalf("vector length %d, want %d", len(got), len(want))
	}
	for i := range want {
		if diff := math.Abs(float64(got[i] - want[i])); diff > 1e-6 {
			t.Fatalf("vector[%d] = %v, want %v (diff %g)", i, got[i], want[i], diff)
		}
	}

	if m, err := s.ExistingEmbeddings(ctx, 99999); err != nil || len(m) != 0 {
		t.Errorf("ExistingEmbeddings for an unknown entry = %v, %v; want empty, nil", m, err)
	}
}

// TestReplaceEntryChunksRejectsBadEmbeddings: a chunk with no vector or the wrong width must fail
// the whole call, leaving the database untouched.
func TestReplaceEntryChunksRejectsBadEmbeddings(t *testing.T) {
	s, emb := newTestStore(t)
	ctx := context.Background()

	missing := chunkInputs(t, emb, "one", "two")
	missing[1].Embedding = nil
	if _, err := s.ReplaceEntryChunks(ctx, sampleEntry(), missing); err == nil {
		t.Error("ReplaceEntryChunks accepted a chunk with a nil embedding")
	}

	wrongWidth := chunkInputs(t, emb, "one", "two")
	wrongWidth[0].Embedding = make([]float32, 4)
	if _, err := s.ReplaceEntryChunks(ctx, sampleEntry(), wrongWidth); err == nil {
		t.Error("ReplaceEntryChunks accepted a 4-dimensional embedding")
	}

	if n := countRows(t, s, "SELECT count(*) FROM entries"); n != 0 {
		t.Errorf("entries = %d after two rejected calls, want 0", n)
	}
	if n := countRows(t, s, "SELECT count(*) FROM chunks"); n != 0 {
		t.Errorf("chunks = %d after two rejected calls, want 0", n)
	}
}

// TestEnsureEmbedMeta covers the three outcomes: first write, agreeing re-check, and the mismatch
// that must tell the user to rebuild.
func TestEnsureEmbedMeta(t *testing.T) {
	s, err := Open(filepath.Join(t.TempDir(), "kb.sqlite"))
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	defer s.Close()

	if err := s.EnsureEmbedMeta("nomic-embed-text", 768); err != nil {
		t.Fatalf("first EnsureEmbedMeta: %v", err)
	}
	if err := s.EnsureEmbedMeta("nomic-embed-text", 768); err != nil {
		t.Fatalf("repeat EnsureEmbedMeta: %v", err)
	}
	if n := countRows(t, s, "SELECT count(*) FROM embed_meta"); n != 1 {
		t.Errorf("embed_meta rows = %d, want 1", n)
	}

	err = s.EnsureEmbedMeta("some-other-model", 768)
	if !errors.Is(err, ErrEmbedMetaMismatch) {
		t.Fatalf("model change: err = %v, want ErrEmbedMetaMismatch", err)
	}
	if !strings.Contains(err.Error(), "kb reindex --all") {
		t.Errorf("mismatch error does not tell the user how to fix it: %v", err)
	}
	var mismatch *EmbedMetaMismatchError
	if !errors.As(err, &mismatch) || mismatch.StoredModel != "nomic-embed-text" || mismatch.WantModel != "some-other-model" {
		t.Errorf("errors.As gave %+v", mismatch)
	}
	// A different width is not a mismatch to report later but a model that cannot be stored at
	// all: chunks_vec is declared FLOAT[768], so it is refused before anything is written.
	err = s.EnsureEmbedMeta("some-384-model", 384)
	if err == nil {
		t.Fatal("EnsureEmbedMeta accepted a 384-dimensional model")
	}
	if !strings.Contains(err.Error(), "FLOAT[768]") || !strings.Contains(err.Error(), "some-384-model") {
		t.Errorf("the wrong-width error should name the column width and the model: %v", err)
	}
	if err := s.EnsureEmbedMeta("nomic-embed-text", VecDim); err != nil {
		t.Errorf("EnsureEmbedMeta with the right width still fails: %v", err)
	}

	model, dim, ok, err := s.EmbedMeta()
	if err != nil || !ok || model != "nomic-embed-text" || dim != 768 {
		t.Errorf("EmbedMeta = %q, %d, %v, %v", model, dim, ok, err)
	}
}

// TestResetForReindex empties the derived tables for `kb reindex --all` but keeps the search log,
// which is the only data in the file that cannot be rebuilt from the Markdown.
func TestResetForReindex(t *testing.T) {
	s, emb := newTestStore(t)
	ctx := context.Background()

	if _, err := s.ReplaceEntryChunks(ctx, sampleEntry(), chunkInputs(t, emb, "one", "two")); err != nil {
		t.Fatalf("ReplaceEntryChunks: %v", err)
	}
	if _, err := s.DB().Exec(`INSERT INTO searches (ts, query, mode, k, n_returned) VALUES ('2026-09-13T00:00:00Z', 'vacuum', 'hybrid', 8, 2)`); err != nil {
		t.Fatalf("insert search log row: %v", err)
	}

	if err := s.ResetForReindex(ctx); err != nil {
		t.Fatalf("ResetForReindex: %v", err)
	}
	for _, q := range []string{
		"SELECT count(*) FROM entries",
		"SELECT count(*) FROM chunks",
		"SELECT count(*) FROM chunks_vec",
		"SELECT count(*) FROM embed_meta",
	} {
		if n := countRows(t, s, q); n != 0 {
			t.Errorf("%s = %d after reset, want 0", q, n)
		}
	}
	if n := countFTSDocs(t, s); n != 0 {
		t.Errorf("indexed FTS rows = %d after reset, want 0", n)
	}
	if n := countRows(t, s, "SELECT count(*) FROM searches"); n != 1 {
		t.Errorf("searches = %d after reset, want 1 (the search log is not rebuildable)", n)
	}

	// A reset database accepts a different model without complaint.
	if err := s.EnsureEmbedMeta("another-model", 768); err != nil {
		t.Errorf("EnsureEmbedMeta after reset: %v", err)
	}
}

// TestPragmasOnEveryPooledConnection guards the mistake this package is most likely to regress
// into: setting foreign_keys on the first connection only. database/sql hands out any connection
// in the pool, so the pragmas are in the DSN. Holding several connections open at once forces the
// pool to create new ones and each is checked.
func TestPragmasOnEveryPooledConnection(t *testing.T) {
	s, err := Open(filepath.Join(t.TempDir(), "kb.sqlite"))
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	defer s.Close()
	ctx := context.Background()

	var conns []*sql.Conn
	defer func() {
		for _, c := range conns {
			c.Close()
		}
	}()
	for i := 0; i < 4; i++ {
		conn, err := s.DB().Conn(ctx)
		if err != nil {
			t.Fatalf("open connection %d: %v", i, err)
		}
		conns = append(conns, conn)
		var fk, busy int
		var journal string
		if err := conn.QueryRowContext(ctx, "PRAGMA foreign_keys").Scan(&fk); err != nil {
			t.Fatalf("connection %d foreign_keys: %v", i, err)
		}
		if fk != 1 {
			t.Errorf("connection %d: foreign_keys = %d, want 1", i, fk)
		}
		if err := conn.QueryRowContext(ctx, "PRAGMA busy_timeout").Scan(&busy); err != nil {
			t.Fatalf("connection %d busy_timeout: %v", i, err)
		}
		if busy != 5000 {
			t.Errorf("connection %d: busy_timeout = %d, want 5000", i, busy)
		}
		if err := conn.QueryRowContext(ctx, "PRAGMA journal_mode").Scan(&journal); err != nil {
			t.Fatalf("connection %d journal_mode: %v", i, err)
		}
		if journal != "wal" {
			t.Errorf("connection %d: journal_mode = %q, want wal", i, journal)
		}
	}
}

// TestForeignKeyCascade proves the pragma is doing real work: deleting an entry row directly must
// take its chunks with it.
func TestForeignKeyCascade(t *testing.T) {
	s, emb := newTestStore(t)
	ctx := context.Background()

	entryID, err := s.ReplaceEntryChunks(ctx, sampleEntry(), chunkInputs(t, emb, "one", "two"))
	if err != nil {
		t.Fatalf("ReplaceEntryChunks: %v", err)
	}
	if _, err := s.DB().Exec("DELETE FROM entries WHERE id = ?", entryID); err != nil {
		t.Fatalf("delete entry: %v", err)
	}
	if n := countRows(t, s, "SELECT count(*) FROM chunks"); n != 0 {
		t.Errorf("chunks = %d after the entry was deleted, want 0 (ON DELETE CASCADE)", n)
	}
}

// --- migrations -------------------------------------------------------------------------------

// TestMigrateRunsOnlyOutstandingMigrations mutates the package-level migrations and schemaVersion
// variables and restores them afterwards. NO TEST IN THIS PACKAGE MAY CALL t.Parallel: a parallel
// test opening a database while this one is running would be migrated against the wrong schema.
//
// It is the regression test for the bug of re-running
// schema.sql on an existing database: after a second migration is added, reopening a version-1
// database must apply that one step only, not the whole schema again (which would fail with
// "table entries already exists").
func TestMigrateRunsOnlyOutstandingMigrations(t *testing.T) {
	path := filepath.Join(t.TempDir(), "kb.sqlite")
	first, err := Open(path)
	if err != nil {
		t.Fatalf("first Open: %v", err)
	}
	first.Close()

	origMigrations, origVersion := migrations, schemaVersion
	defer func() { migrations, schemaVersion = origMigrations, origVersion }()
	migrations = append(append([]func(execer) error{}, origMigrations...), func(db execer) error {
		_, err := db.Exec("CREATE TABLE probe (id INTEGER PRIMARY KEY)")
		return err
	})
	schemaVersion = len(migrations)

	second, err := Open(path)
	if err != nil {
		t.Fatalf("reopen after adding a migration: %v", err)
	}
	defer second.Close()

	var version int
	if err := second.DB().QueryRow("PRAGMA user_version").Scan(&version); err != nil {
		t.Fatalf("read user_version: %v", err)
	}
	if version != schemaVersion {
		t.Errorf("user_version = %d, want %d", version, schemaVersion)
	}
	var name string
	if err := second.DB().QueryRow("SELECT name FROM sqlite_master WHERE name = 'probe'").Scan(&name); err != nil {
		t.Errorf("the new migration did not run: %v", err)
	}
	// The old data survived, i.e. the schema was not recreated underneath it.
	if n := countRows(t, second, "SELECT count(*) FROM entries"); n != 0 {
		t.Errorf("entries = %d", n)
	}
}

// TestOpenRefusesNewerSchema: a database written by a future kb must be refused, not misread.
func TestOpenRefusesNewerSchema(t *testing.T) {
	path := filepath.Join(t.TempDir(), "kb.sqlite")
	s, err := Open(path)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	if _, err := s.DB().Exec(fmt.Sprintf("PRAGMA user_version = %d", schemaVersion+1)); err != nil {
		t.Fatalf("bump user_version: %v", err)
	}
	s.Close()

	if _, err := Open(path); err == nil {
		t.Error("Open accepted a database from a newer schema version")
	} else if !strings.Contains(err.Error(), "schema version") {
		t.Errorf("unhelpful error for a newer database: %v", err)
	}
}

// TestConcurrentOpen races several openers at one fresh database file. Without BEGIN IMMEDIATE
// around the read-decide-apply sequence, the losers either apply the schema a second time or fail
// with SQLITE_BUSY; all of them must succeed and see exactly one schema.
func TestConcurrentOpen(t *testing.T) {
	path := filepath.Join(t.TempDir(), "kb.sqlite")
	const openers = 8

	var wg sync.WaitGroup
	errs := make([]error, openers)
	stores := make([]*Store, openers)
	start := make(chan struct{})
	for i := 0; i < openers; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			<-start
			stores[i], errs[i] = Open(path)
		}(i)
	}
	close(start)
	wg.Wait()

	for i, err := range errs {
		if err != nil {
			t.Errorf("opener %d: %v", i, err)
			continue
		}
		defer stores[i].Close()
	}
	if t.Failed() {
		return
	}

	var version int
	if err := stores[0].DB().QueryRow("PRAGMA user_version").Scan(&version); err != nil {
		t.Fatalf("read user_version: %v", err)
	}
	if version != schemaVersion {
		t.Errorf("user_version = %d, want %d", version, schemaVersion)
	}
	if n := countRows(t, stores[0], "SELECT count(*) FROM sqlite_master WHERE name = 'entries'"); n != 1 {
		t.Errorf("entries table appears %d times in sqlite_master, want 1", n)
	}
}
