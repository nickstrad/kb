package sqlite

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"errors"
	"fmt"
	"github.com/nickstrad/kb/internal/errs"
	"github.com/nickstrad/kb/internal/migrate"
	"github.com/nickstrad/kb/internal/store"
	"math"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"testing"

	sqlite_vec "github.com/asg017/sqlite-vec-go-bindings/cgo"

	"github.com/nickstrad/kb/internal/embed/fake"
)

// TestOpenAppliesSchema opens a fresh database in a temp directory and checks the three things a
// broken build would break: the migration stamp, sqlite-vec, and the FTS5 virtual table.
func TestOpenAppliesSchema(t *testing.T) {
	path := filepath.Join(t.TempDir(), "kb.sqlite")
	s, err := Open(context.Background(), path, Options{})
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

	for _, table := range []string{"entries", "chunks", "chunks_fts", "searches", "search_results", "search_candidates", "search_feedback", "search_opens", "embed_meta"} {
		var name string
		err := s.DB().QueryRow("SELECT name FROM sqlite_master WHERE name = ?", table).Scan(&name)
		if err != nil {
			t.Errorf("table %s missing: %v", table, err)
		}
	}
	// chunks_vec is created by EnsureEmbedMeta at the model's width, never by the schema: schema.sql
	// still declares FLOAT[768] and migration 2→3 drops it again.
	if n := countRows(t, s, "SELECT count(*) FROM sqlite_master WHERE name = 'chunks_vec'"); n != 0 {
		t.Errorf("a fresh database has chunks_vec (%d); EnsureEmbedMeta should create it", n)
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
	s, err := Open(context.Background(), path, Options{})
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
	first, err := Open(context.Background(), path, Options{})
	if err != nil {
		t.Fatalf("first Open: %v", err)
	}
	first.Close()

	second, err := Open(context.Background(), path, Options{})
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

// --- entry and chunk writes ------------------------------------------------------------------

// testDim is the fake embedder's width. chunks_vec takes whatever width EnsureEmbedMeta records,
// so the write tests use a small one.
const testDim = 16

// newTestStore opens a fresh database in a temp directory with embed_meta already recorded.
func newTestStore(t *testing.T) (*Store, *fake.Embedder) {
	t.Helper()
	s, err := Open(context.Background(), filepath.Join(t.TempDir(), "kb.sqlite"), Options{})
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	t.Cleanup(func() { s.Close() })
	emb := fake.New("fake-embed", testDim)
	if err := s.EnsureEmbedMeta(context.Background(), store.EmbedMeta{Model: emb.Model(), Dim: emb.Dim()}); err != nil {
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

// chunkInputs builds store.ChunkInput values with hashes and embeddings filled in from the texts.
func chunkInputs(t *testing.T, emb *fake.Embedder, texts ...string) []store.ChunkInput {
	t.Helper()
	out := make([]store.ChunkInput, len(texts))
	for i, text := range texts {
		sum := sha256.Sum256([]byte(text))
		out[i] = store.ChunkInput{
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
func sampleEntry() store.EntryInput {
	return store.EntryInput{
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

	if _, err := s.ReplaceEntry(ctx, sampleEntry(), chunkInputs(t, emb, "alpha", "bravo", "charlie")); err != nil {
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

// TestReplaceEntryChunksWritesAllThreeTables: one call must populate
// chunks, chunks_fts and chunks_vec together, and all three must answer queries.
func TestReplaceEntryChunksWritesAllThreeTables(t *testing.T) {
	s, emb := newTestStore(t)
	ctx := context.Background()

	texts := []string{
		"The tmux daemon dies when the login session ends.",
		"Postgres logs startup failures under /var/log/postgresql.",
		"VACUUM rewrites the SQLite file after a bulk delete.",
	}
	entryID, err := s.ReplaceEntry(ctx, sampleEntry(), chunkInputs(t, emb, texts...))
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
	chunks, err := s.Chunks(context.Background(), entryID)
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
	got, err := s.Entry(context.Background(), "sample.md")
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
	if list, err := s.Entries(context.Background()); err != nil || len(list) != 1 {
		t.Errorf("ListEntries = %v, %v; want one entry", list, err)
	}
}

// TestReplaceEntryChunksReplaces rewrites an entry with fewer chunks and a new title: the old
// chunks must disappear from all three tables, and the stale title must no longer match.
func TestReplaceEntryChunksReplaces(t *testing.T) {
	s, emb := newTestStore(t)
	ctx := context.Background()

	first := sampleEntry()
	if _, err := s.ReplaceEntry(ctx, first, chunkInputs(t, emb,
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
	entryID, err := s.ReplaceEntry(ctx, second, chunkInputs(t, emb,
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

	got, err := s.Entry(context.Background(), "sample.md")
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
	if _, err := s.ReplaceEntry(ctx, original, chunkInputs(t, emb,
		"alpha content", "bravo content", "charlie content")); err != nil {
		t.Fatalf("first ReplaceEntryChunks: %v", err)
	}

	doomed := original
	doomed.Title = "Renamed entry"
	doomed.BodyHash = "hash-v2"
	bad := chunkInputs(t, emb, "delta content", "echo content")
	bad[1].Ord = bad[0].Ord // UNIQUE (entry_id, ord)

	if _, err := s.ReplaceEntry(ctx, doomed, bad); err == nil {
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

	got, err := s.Entry(context.Background(), original.Path)
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

	if _, err := s.ReplaceEntry(ctx, sampleEntry(), chunkInputs(t, emb,
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

	got, err := s.Entry(context.Background(), "sample.md")
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
// for another embedding, so the bytes that come back must be the bytes that went in.
func TestExistingEmbeddings(t *testing.T) {
	s, emb := newTestStore(t)
	ctx := context.Background()

	inputs := chunkInputs(t, emb, "reuse me unchanged", "rewrite me")
	entryID, err := s.ReplaceEntry(ctx, sampleEntry(), inputs)
	if err != nil {
		t.Fatalf("ReplaceEntryChunks: %v", err)
	}

	stored, err := s.Embeddings(ctx, entryID)
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

	if m, err := s.Embeddings(ctx, 99999); err != nil || len(m) != 0 {
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
	if _, err := s.ReplaceEntry(ctx, sampleEntry(), missing); err == nil {
		t.Error("ReplaceEntryChunks accepted a chunk with a nil embedding")
	}

	wrongWidth := chunkInputs(t, emb, "one", "two")
	wrongWidth[0].Embedding = make([]float32, 4)
	if _, err := s.ReplaceEntry(ctx, sampleEntry(), wrongWidth); err == nil {
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
	s := openBare(t)

	if err := s.EnsureEmbedMeta(context.Background(), store.EmbedMeta{Model: "nomic-embed-text", Dim: 768}); err != nil {
		t.Fatalf("first EnsureEmbedMeta: %v", err)
	}
	if err := s.EnsureEmbedMeta(context.Background(), store.EmbedMeta{Model: "nomic-embed-text", Dim: 768}); err != nil {
		t.Fatalf("repeat EnsureEmbedMeta: %v", err)
	}
	if n := countRows(t, s, "SELECT count(*) FROM embed_meta"); n != 1 {
		t.Errorf("embed_meta rows = %d, want 1", n)
	}

	err := s.EnsureEmbedMeta(context.Background(), store.EmbedMeta{Model: "some-other-model", Dim: 768})
	if !errors.Is(err, store.ErrEmbedMetaMismatch) {
		t.Fatalf("model change: err = %v, want store.ErrEmbedMetaMismatch", err)
	}
	if !strings.Contains(err.Error(), "kb reindex --all") {
		t.Errorf("mismatch error does not tell the user how to fix it: %v", err)
	}
	var mismatch *store.EmbedMetaMismatchError
	if !errors.As(err, &mismatch) || mismatch.StoredModel != "nomic-embed-text" || mismatch.WantModel != "some-other-model" {
		t.Errorf("errors.As gave %+v", mismatch)
	}

	m, ok, err := s.EmbedMeta(context.Background())
	if err != nil || !ok || m.Model != "nomic-embed-text" || m.Dim != 768 {
		t.Errorf("EmbedMeta = %+v, %v, %v", m, ok, err)
	}
}

// openBare opens a fresh database with no embed_meta row and no chunks_vec.
func openBare(t *testing.T) *Store {
	t.Helper()
	s, err := Open(context.Background(), filepath.Join(t.TempDir(), "kb.sqlite"), Options{})
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	t.Cleanup(func() { s.Close() })
	return s
}

// vecWidth reads the FLOAT[n] width chunks_vec was created with, or 0 when there is no table.
func vecWidth(t *testing.T, s *Store) int {
	t.Helper()
	var ddl string
	err := s.DB().QueryRow("SELECT sql FROM sqlite_master WHERE name = 'chunks_vec'").Scan(&ddl)
	if errors.Is(err, sql.ErrNoRows) {
		return 0
	}
	if err != nil {
		t.Fatalf("read chunks_vec DDL: %v", err)
	}
	var width int
	i := strings.Index(ddl, "FLOAT[")
	if i < 0 {
		t.Fatalf("chunks_vec DDL has no FLOAT[n]: %s", ddl)
	}
	if _, err := fmt.Sscanf(ddl[i:], "FLOAT[%d]", &width); err != nil {
		t.Fatalf("parse chunks_vec width from %q: %v", ddl, err)
	}
	return width
}

// TestEnsureEmbedMetaWidthBoundaries is the TESTING.md §2.1 boundary table for the dimension a
// fresh database accepts: below, at and above each limit (0 is FTS only, 1 the narrowest vector,
// MaxVecDim the widest sqlite-vec stores), plus the two real widths kb ships with. A refused call
// must record nothing: no embed_meta row and no chunks_vec.
func TestEnsureEmbedMetaWidthBoundaries(t *testing.T) {
	for _, tc := range []struct {
		name      string
		model     string
		dim       int
		wantErr   string // substring; "" = accepted
		wantWidth int    // chunks_vec width afterwards; 0 = no table
	}{
		{"empty model", "", 384, "must not be empty", 0},
		{"none at 0: FTS only, no table", store.FTSOnlyModel, 0, "", 0},
		{"none at 1", store.FTSOnlyModel, 1, "FTS only", 0},
		{"model at -1", "m", -1, "only the FTS-only model", 0},
		{"model at 0", "m", 0, "only the FTS-only model", 0},
		{"model at 1", "m", 1, "", 1},
		{"bge-small 384", "BAAI/bge-small-en-v1.5", 384, "", 384},
		{"768", "google/embeddinggemma-300m", 768, "", 768},
		{"MaxVecDim", "m", MaxVecDim, "", MaxVecDim},
		{"MaxVecDim+1", "m", MaxVecDim + 1, "at most 8192", 0},
	} {
		t.Run(tc.name, func(t *testing.T) {
			s := openBare(t)
			err := s.EnsureEmbedMeta(context.Background(), store.EmbedMeta{Model: tc.model, Dim: tc.dim})
			if tc.wantErr != "" {
				if err == nil || !strings.Contains(err.Error(), tc.wantErr) {
					t.Fatalf("err = %v, want one containing %q", err, tc.wantErr)
				}
				if n := countRows(t, s, "SELECT count(*) FROM embed_meta"); n != 0 {
					t.Errorf("embed_meta rows = %d after a refused call, want 0", n)
				}
			} else if err != nil {
				t.Fatalf("EnsureEmbedMeta(%q, %d): %v", tc.model, tc.dim, err)
			}
			if got := vecWidth(t, s); got != tc.wantWidth {
				t.Errorf("chunks_vec width = %d, want %d", got, tc.wantWidth)
			}
			hasVec, err := s.HasVectors(context.Background())
			if err != nil || hasVec != (tc.wantWidth > 0) {
				t.Errorf("HasVectors = %v, %v; want %v", hasVec, err, tc.wantWidth > 0)
			}
		})
	}
}

// TestEnsureEmbedMetaRecordedDimBoundaries: once a width is recorded, only exactly that width is
// accepted; one either side is a mismatch carrying both dims, and the table keeps its width.
func TestEnsureEmbedMetaRecordedDimBoundaries(t *testing.T) {
	const model, recorded = "BAAI/bge-small-en-v1.5", 384
	for _, tc := range []struct {
		name     string
		dim      int
		mismatch bool
	}{
		{"recorded-1", recorded - 1, true},
		{"recorded", recorded, false},
		{"recorded+1", recorded + 1, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			s := openBare(t)
			if err := s.EnsureEmbedMeta(context.Background(), store.EmbedMeta{Model: model, Dim: recorded}); err != nil {
				t.Fatalf("record: %v", err)
			}
			err := s.EnsureEmbedMeta(context.Background(), store.EmbedMeta{Model: model, Dim: tc.dim})
			if !tc.mismatch {
				if err != nil {
					t.Fatalf("same model and dim: %v", err)
				}
			} else {
				var mm *store.EmbedMetaMismatchError
				if !errors.As(err, &mm) || mm.StoredDim != recorded || mm.WantDim != tc.dim || mm.StoredModel != model {
					t.Fatalf("err = %v (%+v), want a mismatch %d vs %d", err, mm, recorded, tc.dim)
				}
			}
			if got := vecWidth(t, s); got != recorded {
				t.Errorf("chunks_vec width = %d, want %d", got, recorded)
			}
		})
	}
}

// TestEnsureEmbedMetaRepairsTable: the vector table always follows embed_meta. A missing table
// under a matching row is re-created; a leftover table with no row (whatever its width) is
// replaced at the new model's width.
func TestEnsureEmbedMetaRepairsTable(t *testing.T) {
	s := openBare(t)
	if err := s.EnsureEmbedMeta(context.Background(), store.EmbedMeta{Model: "m", Dim: 16}); err != nil {
		t.Fatalf("record: %v", err)
	}
	if _, err := s.DB().Exec("DROP TABLE chunks_vec"); err != nil {
		t.Fatalf("drop: %v", err)
	}
	if err := s.EnsureEmbedMeta(context.Background(), store.EmbedMeta{Model: "m", Dim: 16}); err != nil {
		t.Fatalf("re-check: %v", err)
	}
	if got := vecWidth(t, s); got != 16 {
		t.Errorf("width after re-check = %d, want 16 (re-created)", got)
	}

	if _, err := s.DB().Exec("DELETE FROM embed_meta"); err != nil {
		t.Fatalf("clear embed_meta: %v", err)
	}
	if err := s.EnsureEmbedMeta(context.Background(), store.EmbedMeta{Model: "other", Dim: 384}); err != nil {
		t.Fatalf("record other: %v", err)
	}
	if got := vecWidth(t, s); got != 384 {
		t.Errorf("width = %d, want 384: a stale table with no embed_meta must be replaced", got)
	}
}

// TestFTSOnlyIndexHasNoVectorTable walks every chunks_vec reader and writer over an index built
// with the none embedder, which has no chunks_vec: replace (insert, then clear and re-insert),
// ExistingEmbeddings, DeleteEntry and ResetForReindex must all work without the table.
func TestFTSOnlyIndexHasNoVectorTable(t *testing.T) {
	s := openBare(t)
	ctx := context.Background()
	if err := s.EnsureEmbedMeta(context.Background(), store.EmbedMeta{Model: store.FTSOnlyModel, Dim: 0}); err != nil {
		t.Fatalf("EnsureEmbedMeta(none, 0): %v", err)
	}
	ftsChunks := func(texts ...string) []store.ChunkInput {
		out := make([]store.ChunkInput, len(texts))
		for i, text := range texts {
			out[i] = store.ChunkInput{Ord: i, SourceFile: "sample.md", Text: text, TextHash: text}
		}
		return out
	}

	id, err := s.ReplaceEntry(ctx, sampleEntry(), ftsChunks("alpha", "bravo"))
	if err != nil {
		t.Fatalf("first replace: %v", err)
	}
	if _, err := s.ReplaceEntry(ctx, sampleEntry(), ftsChunks("charlie")); err != nil {
		t.Fatalf("second replace (clears the old chunks): %v", err)
	}
	if n := countRows(t, s, "SELECT count(*) FROM chunks_fts WHERE chunks_fts MATCH 'charlie'"); n != 1 {
		t.Errorf("MATCH charlie = %d, want 1", n)
	}
	if n := countFTSDocs(t, s); n != 1 {
		t.Errorf("FTS rows = %d, want 1 (the old two cleared)", n)
	}
	if m, err := s.Embeddings(ctx, id); err != nil || len(m) != 0 {
		t.Errorf("ExistingEmbeddings = %v, %v; want empty, nil", m, err)
	}

	withVec := ftsChunks("delta")
	withVec[0].Embedding = []float32{1}
	if _, err := s.ReplaceEntry(ctx, sampleEntry(), withVec); err == nil || !strings.Contains(err.Error(), "FTS only") {
		t.Errorf("replace with an embedding on an FTS-only index = %v, want an FTS-only error", err)
	}

	if err := s.DeleteEntry(ctx, "sample.md"); err != nil {
		t.Fatalf("DeleteEntry: %v", err)
	}
	if n := countRows(t, s, "SELECT count(*) FROM chunks"); n != 0 {
		t.Errorf("chunks = %d after delete, want 0", n)
	}
	if err := s.ResetIndex(ctx); err != nil {
		t.Fatalf("ResetForReindex without chunks_vec: %v", err)
	}
	if got := vecWidth(t, s); got != 0 {
		t.Errorf("chunks_vec width = %d, want no table", got)
	}
}

// TestReplaceEntryChunksNeedsVectorTable: embed_meta says 16 dims but chunks_vec is gone (the
// caller skipped EnsureEmbedMeta). The write is refused as a Conflict pointing at kb reindex
// --all, and nothing is written.
func TestReplaceEntryChunksNeedsVectorTable(t *testing.T) {
	s, emb := newTestStore(t)
	if _, err := s.DB().Exec("DROP TABLE chunks_vec"); err != nil {
		t.Fatalf("drop: %v", err)
	}
	_, err := s.ReplaceEntry(context.Background(), sampleEntry(), chunkInputs(t, emb, "one"))
	if errs.KindOf(err) != errs.Conflict || !strings.Contains(err.Error(), "kb reindex --all") {
		t.Fatalf("err = %v, want a Conflict naming kb reindex --all", err)
	}
	if n := countRows(t, s, "SELECT count(*) FROM entries"); n != 0 {
		t.Errorf("entries = %d, want 0", n)
	}
}

// TestReplaceEntryChunksWithoutEmbedMeta: indexing before EnsureEmbedMeta is refused.
func TestReplaceEntryChunksWithoutEmbedMeta(t *testing.T) {
	s := openBare(t)
	_, err := s.ReplaceEntry(context.Background(), sampleEntry(), nil)
	if errs.KindOf(err) != errs.Conflict || !strings.Contains(err.Error(), "no embedding model is recorded") {
		t.Fatalf("err = %v, want a Conflict: no embedding model is recorded", err)
	}
	if _, err := s.ReplaceEntry(context.Background(), store.EntryInput{}, nil); err == nil {
		t.Error("ReplaceEntryChunks accepted an empty path")
	}
}

// TestResetForReindex empties the derived tables for `kb reindex --all` but keeps the search log,
// which is the only data in the file that cannot be rebuilt from the Markdown.
func TestResetForReindex(t *testing.T) {
	s, emb := newTestStore(t)
	ctx := context.Background()

	if _, err := s.ReplaceEntry(ctx, sampleEntry(), chunkInputs(t, emb, "one", "two")); err != nil {
		t.Fatalf("ReplaceEntryChunks: %v", err)
	}
	if _, err := s.DB().Exec(`INSERT INTO searches (ts, query, mode, k, n_returned) VALUES ('2026-09-13T00:00:00Z', 'vacuum', 'hybrid', 8, 2)`); err != nil {
		t.Fatalf("insert search log row: %v", err)
	}

	if err := s.ResetIndex(ctx); err != nil {
		t.Fatalf("ResetForReindex: %v", err)
	}
	for _, q := range []string{
		"SELECT count(*) FROM entries",
		"SELECT count(*) FROM chunks",
		"SELECT count(*) FROM embed_meta",
	} {
		if n := countRows(t, s, q); n != 0 {
			t.Errorf("%s = %d after reset, want 0", q, n)
		}
	}
	if got := vecWidth(t, s); got != 0 {
		t.Errorf("chunks_vec still exists (FLOAT[%d]) after reset; it must be dropped so the next model can pick its width", got)
	}
	if n := countFTSDocs(t, s); n != 0 {
		t.Errorf("indexed FTS rows = %d after reset, want 0", n)
	}
	if n := countRows(t, s, "SELECT count(*) FROM searches"); n != 1 {
		t.Errorf("searches = %d after reset, want 1 (the search log is not rebuildable)", n)
	}

	// A reset database accepts a different model, and a different width, without complaint.
	if err := s.EnsureEmbedMeta(context.Background(), store.EmbedMeta{Model: "another-model", Dim: testDim + 1}); err != nil {
		t.Errorf("EnsureEmbedMeta after reset: %v", err)
	}
	if got := vecWidth(t, s); got != testDim+1 {
		t.Errorf("chunks_vec width after reset = %d, want %d", got, testDim+1)
	}
}

// TestPragmasOnEveryPooledConnection guards the mistake this package is most likely to regress
// into: setting foreign_keys on the first connection only. database/sql hands out any connection
// in the pool, so the pragmas are in the DSN. Holding several connections open at once forces the
// pool to create new ones and each is checked.
func TestPragmasOnEveryPooledConnection(t *testing.T) {
	s, err := Open(context.Background(), filepath.Join(t.TempDir(), "kb.sqlite"), Options{})
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

	entryID, err := s.ReplaceEntry(ctx, sampleEntry(), chunkInputs(t, emb, "one", "two"))
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
// After a second migration is added, reopening a version-1 database must apply that one step
// only, not schema.sql again (which would fail with "table entries already exists").
func TestMigrateRunsOnlyOutstandingMigrations(t *testing.T) {
	path := filepath.Join(t.TempDir(), "kb.sqlite")
	first, err := Open(context.Background(), path, Options{})
	if err != nil {
		t.Fatalf("first Open: %v", err)
	}
	first.Close()

	origMigrations, origVersion := migrations, schemaVersion
	defer func() { migrations, schemaVersion = origMigrations, origVersion }()
	migrations = append(slices.Clone(origMigrations), migrate.Step[execer]{
		Info: migrate.Info{From: len(origMigrations), ID: "index/9999-probe", Desc: "probe"},
		Up: func(ctx context.Context, db execer) error {
			_, err := db.ExecContext(ctx, "CREATE TABLE probe (id INTEGER PRIMARY KEY)")
			return err
		},
	})
	schemaVersion = migrations.Target()

	second, err := Open(context.Background(), path, Options{})
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
	s, err := Open(context.Background(), path, Options{})
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	if _, err := s.DB().Exec(fmt.Sprintf("PRAGMA user_version = %d", schemaVersion+1)); err != nil {
		t.Fatalf("bump user_version: %v", err)
	}
	s.Close()

	if _, err := Open(context.Background(), path, Options{}); err == nil {
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
			stores[i], errs[i] = Open(context.Background(), path, Options{})
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

// TestMigration2KeepsSearchLog: upgrading a version-1 database moves the old git hash into
// corpus_rev, adds an empty kb_version and search_opens, and keeps every logged row.
func TestMigration2KeepsSearchLog(t *testing.T) {
	path := filepath.Join(t.TempDir(), "kb.sqlite")

	origMigrations, origVersion := migrations, schemaVersion
	migrations, schemaVersion = origMigrations[:1], 1
	v1, err := Open(context.Background(), path, Options{})
	if err != nil {
		migrations, schemaVersion = origMigrations, origVersion
		t.Fatalf("open at version 1: %v", err)
	}
	_, err = v1.DB().Exec(`
		INSERT INTO searches (id, ts, query, mode, k, n_returned, caller, kb_version)
		VALUES (1, '2026-09-07T09:00:00Z', 'q', 'fts', 8, 0, 'claude', 'abc123');
		INSERT INTO search_feedback (search_id, rank, useful, ts) VALUES (1, 0, 0, '2026-09-07T09:01:00Z');`)
	v1.Close()
	migrations, schemaVersion = origMigrations, origVersion
	if err != nil {
		t.Fatalf("seed version-1 log: %v", err)
	}

	st, err := Open(context.Background(), path, Options{})
	if err != nil {
		t.Fatalf("upgrade: %v", err)
	}
	defer st.Close()

	var corpusRev string
	var kbVersion sql.NullString
	if err := st.DB().QueryRow("SELECT corpus_rev, kb_version FROM searches WHERE id = 1").Scan(&corpusRev, &kbVersion); err != nil {
		t.Fatalf("read upgraded search: %v", err)
	}
	if corpusRev != "abc123" || kbVersion.Valid {
		t.Errorf("corpus_rev, kb_version = %q, %v; want abc123, NULL", corpusRev, kbVersion)
	}
	if n := countRows(t, st, "SELECT count(*) FROM search_feedback"); n != 1 {
		t.Errorf("search_feedback rows = %d, want 1", n)
	}
	if n := countRows(t, st, "SELECT count(*) FROM search_opens"); n != 0 {
		t.Errorf("search_opens rows = %d, want an empty table", n)
	}
}
