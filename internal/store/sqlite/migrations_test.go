package sqlite

import (
	"context"
	"database/sql"
	"errors"
	"flag"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	sqlite_vec "github.com/asg017/sqlite-vec-go-bindings/cgo"

	"github.com/nickstrad/kb/internal/migrate"
	"github.com/nickstrad/kb/internal/store"
)

// writeFixtures generates the per-version fixtures that do not exist yet:
//
//	go test -tags fts5 ./internal/store/sqlite -run TestWriteFixtures -write-fixtures
//
// It never overwrites one: a fixture freezes what an older kb wrote, and regenerating it with
// today's steps would hide an edit to an old step.
var writeFixtures = flag.Bool("write-fixtures", false, "write missing testdata/migrations/index-v*.sql fixtures")

// fixtureDir holds one SQL text fixture per schema version, from the oldest supported to the
// current one.
const fixtureDir = "testdata/migrations"

func fixturePath(v int) string { return filepath.Join(fixtureDir, fmt.Sprintf("index-v%d.sql", v)) }

// TestIndexSteps: the index steps are well-formed, and each declares the effect the plan's
// migration table gives it. A step that drops or clears derived data must say so here.
func TestIndexSteps(t *testing.T) {
	t.Parallel()
	if err := indexSteps.Check(artefactName); err != nil {
		t.Fatal(err)
	}
	want := []struct {
		id     string
		effect migrate.Effect
	}{
		{"index/0001-baseline", migrate.EffectNone},
		{"index/0002-log-provenance", migrate.EffectNone},
		{"index/0003-vec-width", migrate.EffectReembed},
		{"index/0004-search-settings", migrate.EffectNone},
		{"index/0005-hit-store", migrate.EffectNone},
		{"index/0006-exact-list", migrate.EffectNone},
	}
	if len(indexSteps) != len(want) {
		t.Fatalf("%d steps, the effect table lists %d: add the new step's (ID, Effect) row", len(indexSteps), len(want))
	}
	for i, w := range want {
		if got := indexSteps[i]; got.ID != w.id || got.Effect != w.effect {
			t.Errorf("step %d = (%s, %v), want (%s, %v)", i, got.ID, got.Effect, w.id, w.effect)
		}
	}
	if SchemaVersion() != indexSteps.Target() {
		t.Errorf("SchemaVersion %d != Target %d", SchemaVersion(), indexSteps.Target())
	}
}

// TestReplay: building a database one release at a time (one Open per version, as successive kb
// builds would) gives the same schema and user_version as creating it fresh at the target, and as
// the reference Verify uses.
func TestReplay(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	stepwise := filepath.Join(t.TempDir(), "stepwise.sqlite")
	for v := 1; v <= SchemaVersion(); v++ {
		openAtVersion(t, stepwise, v).Close()
	}
	fresh := filepath.Join(t.TempDir(), "fresh.sqlite")
	st, err := Open(ctx, fresh, Options{})
	if err != nil {
		t.Fatal(err)
	}
	st.Close()

	a, b := schemaAt(t, stepwise), schemaAt(t, fresh)
	if !slices.Equal(a, b) {
		t.Errorf("stepwise schema differs from fresh:\n%s", diffSchema(b, a))
	}
	ref, err := freshSchema(ctx, indexSteps, SchemaVersion())
	if err != nil {
		t.Fatal(err)
	}
	if !slices.Equal(ref, b[:len(b)-1]) { // b ends with its user_version line
		t.Errorf("Verify's reference differs from a fresh Open:\n%s", diffSchema(ref, b))
	}
	for _, p := range []string{stepwise, fresh} {
		if err := NewArtefact(p).Verify(ctx); err != nil {
			t.Errorf("Verify(%s): %v", filepath.Base(p), err)
		}
	}
}

// TestFixtures: every fixture is at its version, matches the steps' schema at that version (an
// edit to an old step fails here), passes Verify, round-trips through the dumper, and upgrades to
// the target with a result that passes Verify and keeps every search-log row.
func TestFixtures(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	for v := 1; v <= SchemaVersion(); v++ {
		t.Run(fmt.Sprintf("v%d", v), func(t *testing.T) {
			path := loadFixture(t, v)
			a := NewArtefact(path)
			st, err := a.Status(ctx)
			if err != nil || st.Current != v || len(st.Pending) != SchemaVersion()-v {
				t.Fatalf("Status = %+v, %v; want v%d with %d pending", st, err, v, SchemaVersion()-v)
			}
			if err := a.Verify(ctx); err != nil {
				t.Fatalf("fixture v%d does not match the steps at v%d (was an old step edited?): %v", v, v, err)
			}
			text, _ := os.ReadFile(fixturePath(v))
			if got := dumpFixture(t, path, v); got != string(text) {
				t.Errorf("dump(load(fixture)) differs from the fixture: the dumper loses something:\n%s", diffSchema(strings.Split(string(text), "\n"), strings.Split(got, "\n")))
			}

			before := logRows(t, path)
			applied, err := a.Apply(ctx)
			if err != nil {
				t.Fatalf("Apply: %v", err)
			}
			if len(applied) != SchemaVersion()-v {
				t.Errorf("applied %d steps, want %d", len(applied), SchemaVersion()-v)
			}
			if err := a.Verify(ctx); err != nil {
				t.Errorf("Verify after upgrading v%d: %v", v, err)
			}
			after := logRows(t, path)
			for table, rows := range before {
				if !sameLogRows(rows, after[table], renamesSince(v)) {
					t.Errorf("%s changed:\nbefore %v\nafter  %v", table, rows, after[table])
				}
			}
		})
	}
}

// renamesSince lists the log-column renames the steps after version v make (old name -> new).
func renamesSince(v int) map[string]string {
	if v < 2 {
		return map[string]string{"kb_version": "corpus_rev"} // index/0002-log-provenance
	}
	return nil
}

// TestStepEffects applies each step alone to the fixture at its From and checks what the step
// promises, plus that the search log survives it.
func TestStepEffects(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	checks := map[string]func(t *testing.T, db *sql.DB){
		"index/0002-log-provenance": func(t *testing.T, db *sql.DB) {
			var corpusRev string
			var kbVersion sql.NullString
			if err := db.QueryRow("SELECT corpus_rev, kb_version FROM searches WHERE id = 1").Scan(&corpusRev, &kbVersion); err != nil {
				t.Fatal(err)
			}
			if corpusRev != "a1b2c3d" || kbVersion.Valid {
				t.Errorf("corpus_rev, kb_version = %q, %v; want the old git hash and NULL", corpusRev, kbVersion)
			}
			if n := count(t, db, "SELECT count(*) FROM search_opens"); n != 0 {
				t.Errorf("search_opens has %d rows, want a new empty table", n)
			}
		},
		"index/0003-vec-width": func(t *testing.T, db *sql.DB) {
			if n := count(t, db, "SELECT count(*) FROM sqlite_schema WHERE name = 'chunks_vec'"); n != 0 {
				t.Error("chunks_vec survived")
			}
			if n := count(t, db, "SELECT count(*) FROM embed_meta"); n != 0 {
				t.Error("embed_meta kept its row")
			}
			if n := count(t, db, "SELECT count(*) FROM entries WHERE body_hash != ''"); n != 0 {
				t.Errorf("%d entries kept a body_hash: reindex would not re-embed them (Effect reembed)", n)
			}
			if n := count(t, db, "SELECT count(*) FROM chunks_fts WHERE chunks_fts MATCH 'checkpoint'"); n != 1 {
				t.Errorf("FTS MATCH = %d after the step, want 1", n)
			}
		},
		"index/0004-search-settings": func(t *testing.T, db *sql.DB) {
			for _, col := range []string{"settings", "settings_hash", "fts_query"} {
				if n := count(t, db, "SELECT count(*) FROM pragma_table_info('searches') WHERE name = '"+col+"' AND type = 'TEXT' AND \"notnull\" = 0"); n != 1 {
					t.Errorf("searches.%s: want one nullable TEXT column, found %d", col, n)
				}
				// Old searches have no recorded settings: NULL, never "" (not known, not empty).
				if n := count(t, db, "SELECT count(*) FROM searches WHERE "+col+" IS NOT NULL"); n != 0 {
					t.Errorf("%d old searches have a %s; want NULL", n, col)
				}
			}
			if n := count(t, db, "SELECT count(*) FROM searches"); n == 0 {
				t.Error("the fixture has no searches: the preservation check would be vacuous")
			}
		},
		"index/0006-exact-list": func(t *testing.T, db *sql.DB) {
			// Backfilled: every chunk is in chunks_exact, and an identifier the main tokenizer
			// splits is one token there (the v5 fixture's chunks are alpha.md's).
			if got, want := count(t, db, "SELECT count(*) FROM chunks_exact"), count(t, db, "SELECT count(*) FROM chunks"); got != want || want == 0 {
				t.Errorf("chunks_exact has %d rows, chunks %d: want equal and non-zero", got, want)
			}
			if n := count(t, db, "SELECT count(*) FROM chunks_exact WHERE chunks_exact MATCH 'checkpoint'"); n != 1 {
				t.Errorf("exact MATCH checkpoint = %d after the backfill, want 1", n)
			}
			if n := count(t, db, `SELECT count(*) FROM chunks c JOIN chunks_exact x ON x.rowid = c.id WHERE chunks_exact MATCH 'starvation'`); n != 1 {
				t.Errorf("exact rowids do not join chunks: %d", n)
			}
			if n := count(t, db, "SELECT count(*) FROM chunks_exact_vocab WHERE term = 'checkpoint'"); n != 1 {
				t.Errorf("chunks_exact_vocab has %d rows for checkpoint, want 1", n)
			}
			for _, table := range []string{"search_results", "search_candidates"} {
				for col, typ := range map[string]string{"exact_rank": "INTEGER", "exact_score": "REAL"} {
					if n := count(t, db, "SELECT count(*) FROM pragma_table_info('"+table+"') WHERE name = '"+col+"' AND type = '"+typ+"' AND \"notnull\" = 0"); n != 1 {
						t.Errorf("%s.%s: want one nullable %s column, found %d", table, col, typ, n)
					}
					// No search before v6 ran an exact list: NULL, never 0.
					if n := count(t, db, "SELECT count(*) FROM "+table+" WHERE "+col+" IS NOT NULL"); n != 0 {
						t.Errorf("%d old %s rows have a %s; want NULL", n, table, col)
					}
				}
			}
			// The open of search 4 was of the other store's hit (only it returned notes/wal.md):
			// the inferred store is now on the row.
			var st sql.NullString
			if err := db.QueryRow("SELECT store FROM search_opens WHERE search_id = 4").Scan(&st); err != nil {
				t.Fatal(err)
			}
			if st.String != "notes@/Users/someone/knowledge" {
				t.Errorf("search 4's open store = %v, want the ref that returned the path", st)
			}
			var primary sql.NullString
			if err := db.QueryRow("SELECT store FROM search_opens WHERE search_id = 1").Scan(&primary); err != nil {
				t.Fatal(err)
			}
			if primary.Valid {
				t.Errorf("search 1's open store = %q, want NULL (this store returned it)", primary.String)
			}
			// The key is (search_id, store, entry_path): the same path from another store is a
			// second open, the same open again is ignored.
			if _, err := db.Exec("INSERT OR IGNORE INTO search_opens (search_id, store, entry_path, ts) VALUES (1, 'x@/r', 'alpha.md', 't'), (1, NULL, 'alpha.md', 't')"); err != nil {
				t.Fatal(err)
			}
			if n := count(t, db, "SELECT count(*) FROM search_opens WHERE search_id = 1"); n != 2 {
				t.Errorf("search 1 has %d opens after adding another store's and a duplicate, want 2", n)
			}
			if _, err := db.Exec("DELETE FROM search_opens WHERE store = 'x@/r'"); err != nil {
				t.Fatal(err)
			}
		},
		"index/0005-hit-store": func(t *testing.T, db *sql.DB) {
			for _, table := range []string{"search_results", "search_candidates"} {
				if n := count(t, db, "SELECT count(*) FROM pragma_table_info('"+table+"') WHERE name = 'store' AND type = 'TEXT' AND \"notnull\" = 0"); n != 1 {
					t.Errorf("%s.store: want one nullable TEXT column, found %d", table, n)
				}
				// Every hit logged before v5 came from the primary store: NULL, never "".
				if n := count(t, db, "SELECT count(*) FROM "+table+" WHERE store IS NOT NULL"); n != 0 {
					t.Errorf("%d old %s rows have a store; want NULL (primary)", n, table)
				}
				if n := count(t, db, "SELECT count(*) FROM "+table); n == 0 {
					t.Errorf("the fixture has no %s rows: the NULL check would be vacuous", table)
				}
			}
		},
	}
	for _, step := range indexSteps[1:] { // the baseline has no From fixture: v0 is "no file"
		t.Run(step.ID, func(t *testing.T) {
			check, ok := checks[step.ID]
			if !ok {
				t.Fatalf("no effect check for %s: add one", step.ID)
			}
			path := loadFixture(t, step.From)
			before := logRows(t, path)
			if _, err := artefactAt(path, step.From+1).Apply(ctx); err != nil {
				t.Fatal(err)
			}
			db := rawDB(t, path)
			if v := count(t, db, "PRAGMA user_version"); v != step.From+1 {
				t.Fatalf("user_version = %d, want %d", v, step.From+1)
			}
			check(t, db)
			after := logRows(t, path)
			for table, rows := range before {
				if !sameLogRows(rows, after[table], renamesSince(step.From)) {
					t.Errorf("%s changed:\nbefore %v\nafter  %v", table, rows, after[table])
				}
			}
		})
	}
}

// TestWriteFixtures writes the missing fixtures under -write-fixtures; otherwise it checks that
// every version has one.
func TestWriteFixtures(t *testing.T) {
	for v := 1; v <= SchemaVersion(); v++ {
		_, err := os.Stat(fixturePath(v))
		switch {
		case err == nil:
			continue
		case !errors.Is(err, fs.ErrNotExist):
			t.Fatal(err)
		case !*writeFixtures:
			t.Errorf("no fixture for v%d: run with -write-fixtures and check it in", v)
			continue
		}
		seed, ok := fixtureSeeds[v]
		if !ok {
			t.Fatalf("no seed for v%d in fixtureSeeds", v)
		}
		path := filepath.Join(t.TempDir(), "kb.sqlite")
		s := openAtVersion(t, path, v)
		seed(t, s)
		s.Close()
		if err := os.MkdirAll(fixtureDir, 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(fixturePath(v), []byte(dumpFixture(t, path, v)), 0o644); err != nil {
			t.Fatal(err)
		}
		t.Logf("wrote %s", fixturePath(v))
	}
}

// fixtureSeeds write realistic rows as the kb of each version did: entries, chunks, FTS, the
// embedding model and vectors, and at least one row in every search-log table of that version.
var fixtureSeeds = map[int]func(t *testing.T, s *Store){
	1: seedV1,
	2: func(t *testing.T, s *Store) { seedV2(t, s, "with vectors") },
	3: seedV3,
	4: seedV4,
	5: seedV5,
	6: seedV6,
}

func seedV1(t *testing.T, s *Store) {
	t.Helper()
	execAll(t, s.DB(), `
		INSERT INTO searches (id, ts, query, mode, k, n_returned, caller, kb_version)
		VALUES (1, '2026-09-01T09:00:00Z', 'checkpoint', 'hybrid', 8, 1, 'claude', 'a1b2c3d'),
		       (2, '2026-09-02T09:00:00Z', 'vacuum', 'fts', 8, 0, 'human', NULL);
		INSERT INTO search_results (search_id, rank, chunk_id, entry_path, heading, fts_rank, fts_score, vec_rank, vec_distance, rrf_score)
		VALUES (1, 1, 2, 'alpha.md', 'Body', 1, -2.75, 1, 0.125, 0.0328);
		INSERT INTO search_candidates (search_id, fused_rank, chunk_id, entry_path, heading, fts_rank, fts_score, vec_rank, vec_distance, rrf_score, returned)
		VALUES (1, 1, 2, 'alpha.md', 'Body', 1, -2.75, 1, 0.125, 0.0328, 1),
		       (1, 2, 1, 'alpha.md', '', NULL, NULL, 2, 0.5, 0.0161, 0);
		INSERT INTO search_feedback (search_id, rank, useful, note, ts) VALUES (1, 1, 1, 'right entry', '2026-09-01T09:01:00Z');
		INSERT INTO entries (id, path, kind, title, summary, tags, body_hash, indexed_at)
		VALUES (1, 'alpha.md', 'file', 'Alpha', 'WAL notes', '["sqlite"]', 'hash-a', '2026-09-01T00:00:00Z');
		INSERT INTO chunks (id, entry_id, ord, source_file, heading, text, text_hash)
		VALUES (1, 1, 0, 'alpha.md', '', 'Alpha: WAL notes', 'h1'), (2, 1, 1, 'alpha.md', 'Body', 'checkpoint starvation', 'h2');
		INSERT INTO chunks_fts (rowid, text, heading, title) VALUES (1, 'Alpha: WAL notes', '', 'Alpha'), (2, 'checkpoint starvation', 'Body', 'Alpha');
		INSERT INTO embed_meta (id, model, dim, created_at) VALUES (1, 'nomic-embed-text', 768, '2026-09-01T00:00:00Z');`)
	for id := 1; id <= 2; id++ {
		v := make([]float32, 768)
		v[0], v[id] = 1, 0.5
		blob, err := sqlite_vec.SerializeFloat32(v)
		if err != nil {
			t.Fatal(err)
		}
		if _, err := s.DB().Exec("INSERT INTO chunks_vec (chunk_id, embedding) VALUES (?, ?)", id, blob); err != nil {
			t.Fatal(err)
		}
	}
}

// seedV3 is a v3 store after a reindex: vectors at the model's width (4) in the table
// EnsureEmbedMeta creates, plus a full search log with opens.
func seedV3(t *testing.T, s *Store) {
	t.Helper()
	ctx := context.Background()
	if err := s.EnsureEmbedMeta(ctx, store.EmbedMeta{Model: "BAAI/bge-small-en-v1.5", Dim: 4}); err != nil {
		t.Fatal(err)
	}
	chunks := []store.ChunkInput{
		{Ord: 0, SourceFile: "alpha.md", Text: "Alpha: WAL notes", TextHash: "h1", Embedding: []float32{1, 0, 0, 0}},
		{Ord: 1, SourceFile: "alpha.md", Heading: "Body", Text: "checkpoint starvation", TextHash: "h2", Embedding: []float32{0, 1, 0, 0}},
	}
	if _, err := s.ReplaceEntry(ctx, store.EntryInput{Path: "alpha.md", Kind: "file", Title: "Alpha", Summary: "WAL notes", Tags: []string{"sqlite"}, BodyHash: "hash-a"}, chunks); err != nil {
		t.Fatal(err)
	}
	execAll(t, s.DB(), `
		INSERT INTO searches (id, ts, query, mode, k, n_returned, caller, corpus_rev, kb_version)
		VALUES (1, '2026-10-01T09:00:00Z', 'checkpoint', 'hybrid', 8, 1, 'claude', 'a1b2c3d', 'v0.2.0');
		INSERT INTO search_results (search_id, rank, chunk_id, entry_path, heading, fts_rank, fts_score, vec_rank, vec_distance, rrf_score)
		VALUES (1, 1, 2, 'alpha.md', 'Body', 1, -2.75, 1, 0.125, 0.0328);
		INSERT INTO search_candidates (search_id, fused_rank, chunk_id, entry_path, heading, fts_rank, fts_score, vec_rank, vec_distance, rrf_score, returned)
		VALUES (1, 1, 2, 'alpha.md', 'Body', 1, -2.75, 1, 0.125, 0.0328, 1);
		INSERT INTO search_feedback (search_id, rank, useful, note, ts) VALUES (1, 1, 1, NULL, '2026-10-01T09:01:00Z');
		INSERT INTO search_opens (search_id, entry_path, ts) VALUES (1, 'alpha.md', '2026-10-01T09:02:00Z');`)
}

// seedV4 is a v4 store: the v3 data, plus searches that recorded their settings as a v4 kb does
// (one with an FTS expression, one fts-only with none), next to the v3 search that predates them.
func seedV4(t *testing.T, s *Store) {
	t.Helper()
	seedV3(t, s)
	execAll(t, s.DB(), `
		INSERT INTO searches (id, ts, query, mode, k, n_returned, caller, corpus_rev, kb_version, settings, settings_hash, fts_query)
		VALUES (2, '2026-10-02T09:00:00Z', 'wal checkpoint', 'hybrid', 8, 1, 'claude', 'a1b2c3d', 'v0.3.0',
		        '{"embed":{"batch":16,"dim":0,"int8":false,"model":"BAAI/bge-small-en-v1.5","workers":0},"search":{"k":8,"rrf_k":60}}',
		        '46f97a2e73fad76c672908714f5b5b8d84d59364f53d925f8f7fcec05bb4092f', '"wal" AND "checkpoint"'),
		       (3, '2026-10-03T09:00:00Z', 'vec only', 'vec', 8, 0, 'human', NULL, 'v0.3.0',
		        '{"embed":{"batch":16,"dim":0,"int8":false,"model":"BAAI/bge-small-en-v1.5","workers":0},"search":{"k":8,"rrf_k":20}}',
		        '9b1d0f4c2a6e8d3b7f5a1c9e0d2b4f6a8c0e1d3b5f7a9c1e2d4b6f8a0c2e4d6b', NULL);
		INSERT INTO search_results (search_id, rank, chunk_id, entry_path, heading, fts_rank, fts_score, vec_rank, vec_distance, rrf_score)
		VALUES (2, 1, 2, 'alpha.md', 'Body', 1, -2.5, 2, 0.25, 0.0325);
		INSERT INTO search_feedback (search_id, rank, useful, note, ts) VALUES (2, 1, 1, NULL, '2026-10-02T09:01:00Z');`)
}

// seedV5 is a v5 store: the v4 data, plus a cross-store search as a v5 kb logs it, with one
// primary hit (store NULL) and one hit of another store (its canonical ref, its own chunk id).
func seedV5(t *testing.T, s *Store) {
	t.Helper()
	seedV4(t, s)
	execAll(t, s.DB(), `
		INSERT INTO searches (id, ts, query, mode, k, n_returned, caller, corpus_rev, kb_version, settings, settings_hash, fts_query)
		VALUES (4, '2026-10-04T09:00:00Z', 'checkpoint', 'hybrid', 8, 2, 'claude', NULL, 'v0.4.0',
		        '{"embed":{"batch":16,"dim":0,"int8":false,"model":"BAAI/bge-small-en-v1.5","workers":0},"search":{"k":8,"rrf_k":60}}',
		        '46f97a2e73fad76c672908714f5b5b8d84d59364f53d925f8f7fcec05bb4092f', '"checkpoint"');
		INSERT INTO search_results (search_id, rank, chunk_id, entry_path, heading, fts_rank, fts_score, vec_rank, vec_distance, rrf_score, store)
		VALUES (4, 1, 2, 'alpha.md', 'Body', 1, -2.5, 1, 0.25, 0.0328, NULL),
		       (4, 2, 7, 'notes/wal.md', 'WAL', 1, -1.5, NULL, NULL, 0.0164, 'notes@/Users/someone/knowledge');
		INSERT INTO search_candidates (search_id, fused_rank, chunk_id, entry_path, heading, fts_rank, fts_score, vec_rank, vec_distance, rrf_score, returned, store)
		VALUES (4, 1, 2, 'alpha.md', 'Body', 1, -2.5, 1, 0.25, 0.0328, 1, NULL),
		       (4, 2, 7, 'notes/wal.md', 'WAL', 1, -1.5, NULL, NULL, 0.0164, 1, 'notes@/Users/someone/knowledge');
		INSERT INTO search_opens (search_id, entry_path, ts) VALUES (4, 'notes/wal.md', '2026-10-04T09:02:00Z');`)
}

// seedV6 is a v6 store: the v5 data with the open of search 4 carrying its store, plus a search
// whose hits carry exact-list ranks, an entry whose chunks hold identifiers (written to
// chunks_exact by ReplaceEntry) and one path opened from two stores of one search (the widened
// search_opens key).
func seedV6(t *testing.T, s *Store) {
	t.Helper()
	seedV5(t, s)
	ctx := context.Background()
	chunks := []store.ChunkInput{
		{Ord: 0, SourceFile: "beta.md", Text: "Beta: config notes", TextHash: "h3", Embedding: []float32{0, 0, 1, 0}},
		{Ord: 1, SourceFile: "beta.md", Heading: "Flags", Text: "set --embed-model in kb.toml; see foo_bar and a/b:c", TextHash: "h4", Embedding: []float32{0, 0, 0, 1}},
	}
	if _, err := s.ReplaceEntry(ctx, store.EntryInput{Path: "beta.md", Kind: "file", Title: "Beta", Summary: "config notes", BodyHash: "hash-b"}, chunks); err != nil {
		t.Fatal(err)
	}
	execAll(t, s.DB(), `
		UPDATE search_opens SET store = 'notes@/Users/someone/knowledge' WHERE search_id = 4;
		INSERT INTO searches (id, ts, query, mode, k, n_returned, caller, corpus_rev, kb_version, settings, settings_hash, fts_query)
		VALUES (5, '2026-10-05T09:00:00Z', 'foo_bar', 'hybrid', 8, 2, 'claude', NULL, 'v0.5.0',
		        '{"embed":{"batch":16,"dim":0,"int8":false,"model":"BAAI/bge-small-en-v1.5","workers":0},"search":{"exact_list":true,"k":8,"rrf_k":60}}',
		        '5c3e0b7a9d1f2e4c6a8b0d2f4e6c8a0b1d3f5e7c9a1b3d5f7e9c1a3b5d7f9e1c', '"foo_bar"');
		INSERT INTO search_results (search_id, rank, chunk_id, entry_path, heading, fts_rank, fts_score, vec_rank, vec_distance, exact_rank, exact_score, rrf_score, store)
		VALUES (5, 1, 4, 'beta.md', 'Flags', 1, -2.25, 3, 0.5, 1, -3.5, 0.0484, NULL),
		       (5, 2, 7, 'beta.md', 'Flags', NULL, NULL, NULL, NULL, 1, -1.25, 0.0164, 'notes@/Users/someone/knowledge');
		INSERT INTO search_candidates (search_id, fused_rank, chunk_id, entry_path, heading, fts_rank, fts_score, vec_rank, vec_distance, exact_rank, exact_score, rrf_score, returned, store)
		VALUES (5, 1, 4, 'beta.md', 'Flags', 1, -2.25, 3, 0.5, 1, -3.5, 0.0484, 1, NULL),
		       (5, 2, 7, 'beta.md', 'Flags', NULL, NULL, NULL, NULL, 1, -1.25, 0.0164, 1, 'notes@/Users/someone/knowledge'),
		       (5, 3, 3, 'beta.md', '', NULL, NULL, 1, 0.25, NULL, NULL, 0.0164, 0, NULL);
		INSERT INTO search_feedback (search_id, rank, useful, note, ts) VALUES (5, 1, 1, NULL, '2026-10-05T09:01:00Z');
		INSERT INTO search_opens (search_id, store, entry_path, ts)
		VALUES (5, NULL, 'beta.md', '2026-10-05T09:02:00Z'),
		       (5, 'notes@/Users/someone/knowledge', 'beta.md', '2026-10-05T09:03:00Z');`)
}

// dumpFixture renders the database at path as the SQL text that rebuilds it: the version, every
// CREATE in creation order (tables a virtual table owns are left to its CREATE VIRTUAL TABLE),
// then every row of every real table, those owned tables and sqlite_sequence included (their
// automatic rows are deleted first). Values are SQLite's own quote() literals.
func dumpFixture(t *testing.T, path string, v int) string {
	t.Helper()
	ctx := context.Background()
	db := rawDB(t, path)
	owned, err := ownedTables(ctx, db)
	if err != nil {
		t.Fatal(err)
	}
	var b strings.Builder
	fmt.Fprintf(&b, "-- kb index DB fixture at schema v%d: what a kb of that version wrote, with realistic rows.\n", v)
	b.WriteString("-- Generated by TestWriteFixtures (-write-fixtures). NEVER EDIT OR REGENERATE: it freezes an old\n")
	b.WriteString("-- kb's output, so an edit to an old migration step fails TestFixtures instead of passing silently.\n")
	fmt.Fprintf(&b, "PRAGMA user_version = %d;\nBEGIN;\n", v)

	type obj struct{ typ, name, tbl, sql string }
	var objs []obj
	rows, err := db.Query("SELECT type, name, tbl_name, coalesce(sql, '') FROM sqlite_schema ORDER BY rowid")
	if err != nil {
		t.Fatal(err)
	}
	for rows.Next() {
		var o obj
		if err := rows.Scan(&o.typ, &o.name, &o.tbl, &o.sql); err != nil {
			t.Fatal(err)
		}
		objs = append(objs, o)
	}
	rows.Close()
	for _, o := range objs {
		if o.sql == "" || owned[o.tbl] || strings.HasPrefix(o.name, "sqlite_") {
			continue
		}
		b.WriteString(o.sql + ";\n")
	}
	// sqlite-vec preallocates large vector blobs in its shadow tables, so a vec0 table is dumped
	// through its own columns; other virtual tables (contentless FTS5 cannot be read back) through
	// the tables they own.
	vec0 := map[string]bool{}
	for _, o := range objs {
		if strings.Contains(o.sql, "USING vec0") {
			vec0[o.name] = true
		}
	}
	// Data in name order: creation order differs between a file and its reload (sqlite_sequence).
	slices.SortFunc(objs, func(a, b obj) int { return strings.Compare(a.name, b.name) })
	for _, o := range objs {
		switch {
		case o.typ != "table":
			continue
		case strings.HasPrefix(o.sql, "CREATE VIRTUAL") && !vec0[o.name]:
			continue
		case owned[o.name] && vec0[ownerOf(o.name, vec0)]:
			continue
		}
		cols := tableColumns(t, db, o.name)
		quoted := make([]string, len(cols))
		for i, c := range cols {
			quoted[i] = fmt.Sprintf("quote(%q)", c)
		}
		where := ""
		if o.name == "sqlite_sequence" { // vec0 rebuilds its own counters as its rows are inserted
			var skip []string
			for vt := range vec0 {
				skip = append(skip, fmt.Sprintf("name LIKE '%s\\_%%' ESCAPE '\\'", vt))
			}
			if len(skip) > 0 {
				slices.Sort(skip)
				where = " WHERE NOT (" + strings.Join(skip, " OR ") + ")"
			}
		}
		rows, err := db.Query(fmt.Sprintf("SELECT %s FROM %q%s", strings.Join(quoted, " || ',' || "), o.name, where)) // names from sqlite_schema
		if err != nil {
			t.Fatal(err)
		}
		var inserts []string
		for rows.Next() {
			var vals string
			if err := rows.Scan(&vals); err != nil {
				t.Fatal(err)
			}
			inserts = append(inserts, fmt.Sprintf("INSERT INTO %s (%s) VALUES (%s);\n", o.name, strings.Join(cols, ", "), vals))
		}
		if err := rows.Err(); err != nil {
			t.Fatal(err)
		}
		rows.Close()
		// Tables a CREATE makes with rows in them are emptied first. sqlite_sequence only appears
		// with rows: on reload it exists only if some CREATE made it.
		if o.name == "sqlite_sequence" && len(inserts) == 0 {
			continue
		}
		if owned[o.name] || o.name == "sqlite_sequence" {
			fmt.Fprintf(&b, "DELETE FROM %s;\n", o.name)
		}
		b.WriteString(strings.Join(inserts, ""))
	}
	b.WriteString("COMMIT;\n")
	return b.String()
}

// ownerOf is the virtual table among vts whose "<name>_" prefixes table, or "".
func ownerOf(table string, vts map[string]bool) string {
	for vt := range vts {
		if strings.HasPrefix(table, vt+"_") {
			return vt
		}
	}
	return ""
}

func tableColumns(t *testing.T, db *sql.DB, table string) []string {
	t.Helper()
	cols, err := strings1(context.Background(), db, fmt.Sprintf("SELECT name FROM pragma_table_info(%q)", table))
	if err != nil {
		t.Fatal(err)
	}
	return cols
}

// loadFixture builds a database file from the fixture for version v and returns its path.
func loadFixture(t *testing.T, v int) string {
	t.Helper()
	text, err := os.ReadFile(fixturePath(v))
	if err != nil {
		t.Fatalf("read fixture (generate it with -write-fixtures): %v", err)
	}
	path := filepath.Join(t.TempDir(), "kb.sqlite")
	db := rawDB(t, path)
	if _, err := db.Exec(string(text)); err != nil {
		t.Fatalf("load fixture v%d: %v", v, err)
	}
	db.Close()
	return path
}

// rawDB opens path with the plain driver: no WAL switch, no version check, no migration.
func rawDB(t *testing.T, path string) *sql.DB {
	t.Helper()
	vecOnce.Do(sqlite_vec.Auto)
	db, err := sql.Open(driverName, path)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { db.Close() })
	return db
}

func schemaAt(t *testing.T, path string) []string {
	t.Helper()
	s, err := schemaOf(context.Background(), rawDB(t, path))
	if err != nil {
		t.Fatal(err)
	}
	return append(s, fmt.Sprintf("user_version %d", count(t, rawDB(t, path), "PRAGMA user_version")))
}

// artefactAt is the index artefact at path as a build whose last step is v sees it.
func artefactAt(path string, v int) *Artefact {
	return &Artefact{path: path, steps: indexSteps[:v]}
}

// logRows reads every search-log table that exists, each row as column -> value.
func logRows(t *testing.T, path string) map[string][]map[string]any {
	t.Helper()
	db := rawDB(t, path)
	out := map[string][]map[string]any{}
	for _, table := range logTables {
		if count(t, db, "SELECT count(*) FROM sqlite_schema WHERE name = ?", table) == 0 {
			continue
		}
		rows, err := db.Query("SELECT * FROM " + table + " ORDER BY rowid")
		if err != nil {
			t.Fatal(err)
		}
		cols, _ := rows.Columns()
		for rows.Next() {
			vals := make([]any, len(cols))
			ptrs := make([]any, len(cols))
			for i := range vals {
				ptrs[i] = &vals[i]
			}
			if err := rows.Scan(ptrs...); err != nil {
				t.Fatal(err)
			}
			row := map[string]any{}
			for i, c := range cols {
				if b, ok := vals[i].([]byte); ok {
					vals[i] = string(b)
				}
				row[c] = vals[i]
			}
			out[table] = append(out[table], row)
		}
		rows.Close()
	}
	return out
}

// sameLogRows: every old row is still there in order, each old column's value under its (possibly
// renamed) column. New columns may appear.
func sameLogRows(before, after []map[string]any, renames map[string]string) bool {
	if len(before) != len(after) {
		return false
	}
	for i, row := range before {
		for col, v := range row {
			if to, ok := renames[col]; ok {
				col = to
			}
			if fmt.Sprint(after[i][col]) != fmt.Sprint(v) {
				return false
			}
		}
	}
	return true
}

func execAll(t *testing.T, db *sql.DB, q string) {
	t.Helper()
	if _, err := db.Exec(q); err != nil {
		t.Fatalf("seed: %v", err)
	}
}
