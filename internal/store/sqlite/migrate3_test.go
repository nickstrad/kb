package sqlite

import (
	"context"
	"database/sql"
	"fmt"
	"github.com/nickstrad/kb/internal/store"
	"os"
	"path/filepath"
	"strings"
	"testing"

	sqlite_vec "github.com/asg017/sqlite-vec-go-bindings/cgo"
)

// logTables are the tables no migration may change: the search log is the only data in the file
// that cannot be rebuilt from the Markdown.
var logTables = []string{"searches", "search_results", "search_candidates", "search_feedback", "search_opens"}

// dumpTable renders every row of a table, in rowid order, as one string per row. It reads columns
// generically so it keeps working as later migrations add columns.
func dumpTable(t *testing.T, db *sql.DB, table string) []string {
	t.Helper()
	rows, err := db.Query("SELECT * FROM " + table + " ORDER BY rowid")
	if err != nil {
		t.Fatalf("dump %s: %v", table, err)
	}
	defer rows.Close()
	cols, err := rows.Columns()
	if err != nil {
		t.Fatalf("dump %s: %v", table, err)
	}
	var out []string
	for rows.Next() {
		vals := make([]any, len(cols))
		ptrs := make([]any, len(cols))
		for i := range vals {
			ptrs[i] = &vals[i]
		}
		if err := rows.Scan(ptrs...); err != nil {
			t.Fatalf("dump %s: %v", table, err)
		}
		var b strings.Builder
		for i, v := range vals {
			if bs, ok := v.([]byte); ok {
				v = string(bs)
			}
			fmt.Fprintf(&b, "%s=%v;", cols[i], v)
		}
		out = append(out, b.String())
	}
	if err := rows.Err(); err != nil {
		t.Fatalf("dump %s: %v", table, err)
	}
	return out
}

// openAtVersion opens path with only the first v migrations, i.e. as an older kb would. The
// older build's steps travel in Options.steps and Artefact.steps, never in package state.
func openAtVersion(t *testing.T, path string, v int) *Store {
	t.Helper()
	if _, err := os.Stat(path); err == nil { // an older file: Open would refuse it, Apply upgrades it
		if _, err := artefactAt(path, v).Apply(context.Background()); err != nil {
			t.Fatalf("upgrade to version %d: %v", v, err)
		}
	}
	s, err := Open(context.Background(), path, Options{steps: indexSteps[:v]})
	if err != nil {
		t.Fatalf("open at version %d: %v", v, err)
	}
	return s
}

// seedV2 writes what a schema-2 kb leaves behind, with raw SQL as that binary would: entries,
// chunks and FTS rows, embed_meta and (for vectors=true) FLOAT[768] vectors in the schema's
// chunks_vec, plus at least one row in every search log table.
func seedV2(t *testing.T, s *Store, index string) {
	t.Helper()
	exec := func(q string, args ...any) {
		t.Helper()
		if _, err := s.DB().Exec(q, args...); err != nil {
			t.Fatalf("seed: %s: %v", q, err)
		}
	}
	exec(`
		INSERT INTO searches (id, ts, query, mode, k, n_returned, caller, corpus_rev, kb_version)
		VALUES (1, '2026-09-07T09:00:00Z', 'wal', 'hybrid', 8, 1, 'claude', 'abc123', 'v0.1.0'),
		       (2, '2026-09-08T09:00:00Z', 'vacuum', 'fts', 8, 0, 'human', NULL, NULL);
		INSERT INTO search_results (search_id, rank, chunk_id, entry_path, heading, fts_rank, fts_score, vec_rank, vec_distance, rrf_score)
		VALUES (1, 1, 1, 'alpha.md', '', 1, -3.5, 2, 0.25, 0.032);
		INSERT INTO search_candidates (search_id, fused_rank, chunk_id, entry_path, heading, fts_rank, fts_score, vec_rank, vec_distance, rrf_score, returned)
		VALUES (1, 1, 1, 'alpha.md', '', 1, -3.5, 2, 0.25, 0.032, 1),
		       (1, 2, 2, 'alpha.md', 'Body', NULL, NULL, 1, 0.125, 0.016, 0);
		INSERT INTO search_feedback (search_id, rank, useful, note, ts) VALUES (1, 1, 1, 'spot on', '2026-09-07T09:01:00Z');
		INSERT INTO search_opens (search_id, entry_path, ts) VALUES (1, 'alpha.md', '2026-09-07T09:02:00Z');`)
	if index == "empty" {
		return
	}

	exec(`
		INSERT INTO entries (id, path, kind, title, summary, tags, body_hash, indexed_at)
		VALUES (1, 'alpha.md', 'file', 'Alpha', 'WAL notes', '[]', 'hash-a', '2026-09-01T00:00:00Z'),
		       (2, 'beta/README.md', 'dir', 'Beta', 'Vacuum notes', '["sqlite"]', 'hash-b', '2026-09-01T00:00:00Z');
		INSERT INTO chunks (id, entry_id, ord, source_file, heading, text, text_hash)
		VALUES (1, 1, 0, 'alpha.md', '', 'journal mode wal', 'h1'),
		       (2, 1, 1, 'alpha.md', 'Body', 'checkpoint starvation', 'h2'),
		       (3, 2, 0, 'beta/README.md', '', 'vacuum into', 'h3');
		INSERT INTO chunks_fts (rowid, text, heading, title)
		VALUES (1, 'journal mode wal', '', 'Alpha'), (2, 'checkpoint starvation', 'Body', 'Alpha'),
		       (3, 'vacuum into', '', 'Beta');`)
	if index == "fts only" {
		exec(`INSERT INTO embed_meta (id, model, dim, created_at) VALUES (1, 'none', 0, '2026-09-01T00:00:00Z')`)
		return
	}
	exec(`INSERT INTO embed_meta (id, model, dim, created_at) VALUES (1, 'nomic-embed-text', 768, '2026-09-01T00:00:00Z')`)
	for id := 1; id <= 3; id++ {
		v := make([]float32, 768)
		v[0], v[id] = 1, 1
		blob, err := sqlite_vec.SerializeFloat32(v)
		if err != nil {
			t.Fatalf("serialize: %v", err)
		}
		exec("INSERT INTO chunks_vec (chunk_id, embedding) VALUES (?, ?)", id, blob)
	}
}

// TestMigration3 upgrades a schema-2 database (with 768-wide vectors, FTS only, and never
// indexed) and checks the 2→3 contract from PLAN §2.5: chunks_vec and embed_meta are gone, every
// body_hash is cleared so `kb reindex --all` re-embeds everything, entries/chunks/FTS still answer,
// and every search log row is unchanged. It then indexes at a new width (384) to show the lazy
// table works on the upgraded file.
func TestMigration3(t *testing.T) {
	for _, index := range []string{"with vectors", "fts only", "empty"} {
		t.Run(index, func(t *testing.T) {
			path := filepath.Join(t.TempDir(), "kb.sqlite")
			v2 := openAtVersion(t, path, 2)
			if got := vecWidth(t, v2); got != 768 {
				t.Fatalf("a schema-2 database has chunks_vec FLOAT[%d], want the schema's 768", got)
			}
			seedV2(t, v2, index)
			before := map[string][]string{}
			for _, table := range logTables {
				before[table] = dumpTable(t, v2.DB(), table)
				if len(before[table]) == 0 {
					t.Fatalf("seed left %s empty; the preservation check would be vacuous", table)
				}
			}
			nEntries := countRows(t, v2, "SELECT count(*) FROM entries")
			nChunks := countRows(t, v2, "SELECT count(*) FROM chunks")
			v2.Close()

			s := openAtVersion(t, path, 3) // the upgrade under test is v2 to v3
			defer s.Close()
			ctx := context.Background()

			if n := countRows(t, s, "PRAGMA user_version"); n != 3 {
				t.Errorf("user_version = %d, want 3", n)
			}
			// Row-set invariant (TESTING.md §3.5 / §6 store row): when internal/assert lands, the
			// per-table comparisons below become one assert over the log tables' row sets.
			for _, table := range logTables {
				after := dumpTable(t, s.DB(), table)
				if strings.Join(after, "\n") != strings.Join(before[table], "\n") {
					t.Errorf("%s changed by the migration:\nbefore %q\nafter  %q", table, before[table], after)
				}
			}
			if got := vecWidth(t, s); got != 0 {
				t.Errorf("chunks_vec FLOAT[%d] survived the migration", got)
			}
			if _, ok, err := s.EmbedMeta(context.Background()); ok || err != nil {
				t.Errorf("EmbedMeta ok, err = %v, %v; want the row deleted", ok, err)
			}
			if n := countRows(t, s, "SELECT count(*) FROM entries WHERE body_hash != ''"); n != 0 {
				t.Errorf("%d entries kept a body_hash; reindex would skip them", n)
			}
			if n := countRows(t, s, "SELECT count(*) FROM entries"); n != nEntries {
				t.Errorf("entries = %d, want %d", n, nEntries)
			}
			if n := countRows(t, s, "SELECT count(*) FROM chunks"); n != nChunks {
				t.Errorf("chunks = %d, want %d", n, nChunks)
			}
			if nEntries > 0 {
				if n := countRows(t, s, "SELECT count(*) FROM chunks_fts WHERE chunks_fts MATCH 'checkpoint'"); n != 1 {
					t.Errorf("FTS MATCH after the upgrade = %d, want 1: FTS must keep working before the reindex", n)
				}
				if m, err := s.Embeddings(ctx, 1); err != nil || len(m) != 0 {
					t.Errorf("ExistingEmbeddings before reindex = %v, %v; want empty, nil", m, err)
				}
			}

			// The reindex path: record the new model, which creates chunks_vec at its width.
			if err := s.EnsureEmbedMeta(context.Background(), store.EmbedMeta{Model: "BAAI/bge-small-en-v1.5", Dim: 384}); err != nil {
				t.Fatalf("EnsureEmbedMeta(384) after the upgrade: %v", err)
			}
			if got := vecWidth(t, s); got != 384 {
				t.Fatalf("chunks_vec width = %d, want 384", got)
			}
			in := store.ChunkInput{Ord: 0, SourceFile: "alpha.md", Text: "journal mode wal", TextHash: "h1", Embedding: make([]float32, 384)}
			in.Embedding[0] = 1
			id, err := s.ReplaceEntry(ctx, store.EntryInput{Path: "alpha.md", Kind: "file", Title: "Alpha", Summary: "s", BodyHash: "new"}, []store.ChunkInput{in})
			if err != nil {
				t.Fatalf("ReplaceEntryChunks at 384: %v", err)
			}
			m, err := s.Embeddings(ctx, id)
			if err != nil || len(m["h1"]) != 384 {
				t.Errorf("ExistingEmbeddings = %d-wide, %v; want 384", len(m["h1"]), err)
			}
			blob, _ := sqlite_vec.SerializeFloat32(in.Embedding)
			if n := countRows(t, s, "SELECT count(*) FROM chunks_vec WHERE embedding MATCH ? AND k = 5", blob); n != 1 {
				t.Errorf("KNN rows = %d, want 1", n)
			}
			// The search log is still intact after the reindex writes.
			if n := countRows(t, s, "SELECT count(*) FROM searches"); n != len(before["searches"]) {
				t.Errorf("searches = %d after reindexing, want %d", n, len(before["searches"]))
			}
		})
	}
}

// TestMigration3DeleteBeforeReindex: `kb rm` on an upgraded, not yet reindexed database must work
// even though chunks_vec does not exist.
func TestMigration3DeleteBeforeReindex(t *testing.T) {
	path := filepath.Join(t.TempDir(), "kb.sqlite")
	v2 := openAtVersion(t, path, 2)
	seedV2(t, v2, "with vectors")
	v2.Close()

	s := openAtVersion(t, path, 3) // the upgrade under test is v2 to v3
	defer s.Close()
	if err := s.DeleteEntry(context.Background(), "alpha.md"); err != nil {
		t.Fatalf("DeleteEntry before reindex: %v", err)
	}
	if n := countRows(t, s, "SELECT count(*) FROM chunks"); n != 1 {
		t.Errorf("chunks = %d, want 1 (beta's)", n)
	}
	if n := countFTSDocs(t, s); n != 1 {
		t.Errorf("FTS rows = %d, want 1", n)
	}
}
