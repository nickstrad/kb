package sqlite

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
	"testing"
	"time"

	sqlite3 "github.com/mattn/go-sqlite3"

	"github.com/nickstrad/kb/internal/errs"
	"github.com/nickstrad/kb/internal/sqlfault"
	"github.com/nickstrad/kb/internal/store"
)

// faults is the fault driver the sqlite store tests open through (Options.driver). The driver
// itself is the shared internal/sqlfault package; these wrappers keep the call sites short.
// faultDriverName is what Options.driver, Artefact.driver and judgedLog take.
const faultDriverName = "sqlite3_fault"

var (
	faults   = sqlfault.Register(faultDriverName)
	errFault = sqlfault.ErrFault
)

const (
	rowsIter = sqlfault.RowsIter
	rowsScan = sqlfault.RowsScan
)

// fail arms the fault for the nth (1-based) statement containing match; it disarms at cleanup.
func fail(t *testing.T, match string, nth int) { t.Helper(); faults.Fail(t, match, nth) }

// arm arms (or with "" disarms) the fault without a cleanup.
func arm(match string, nth int) { faults.Arm(match, nth) }

// rewrite makes the first len(vals) statements containing match answer vals in order.
func rewrite(t *testing.T, match string, vals ...string) {
	t.Helper()
	faults.Rewrite(t, match, vals...)
}

// failRows fails the nth row of the first query containing match (see sqlfault.FailRows).
func failRows(t *testing.T, match string, nth int, mode sqlfault.RowFault) {
	t.Helper()
	faults.FailRows(t, match, nth, mode)
}

// openFaulty opens a store through the fault driver with embed_meta recorded at dim.
func openFaulty(t *testing.T, dim int) *Store {
	t.Helper()
	ctx := context.Background()
	st, err := Open(ctx, filepath.Join(t.TempDir(), "kb.sqlite"), Options{driver: faultDriverName})
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	t.Cleanup(func() { st.Close() })
	meta := store.EmbedMeta{Model: store.FTSOnlyModel}
	if dim > 0 {
		meta = store.EmbedMeta{Model: "m", Dim: dim}
	}
	if err := st.EnsureEmbedMeta(ctx, meta); err != nil {
		t.Fatalf("EnsureEmbedMeta: %v", err)
	}
	return st
}

// faultChunks is two chunks with vectors of width dim (none for 0).
func faultChunks(dim int, word string) []store.ChunkInput {
	var out []store.ChunkInput
	for i := 0; i < 2; i++ {
		c := store.ChunkInput{Ord: i, SourceFile: "a.md", Text: word + " text", TextHash: word + string(rune('0'+i))}
		if dim > 0 {
			c.Embedding = make([]float32, dim)
			c.Embedding[0] = 1
		}
		out = append(out, c)
	}
	return out
}

// TestDriverFaultsAreInternalAndRollBack: every write path, failed at each of its statements, returns
// an Internal error that keeps the cause, and leaves the previous state intact (one transaction).
func TestDriverFaultsAreInternalAndRollBack(t *testing.T) {
	ctx := context.Background()
	entry := store.EntryInput{Path: "a.md", Kind: "file", Title: "T", Summary: "S", Tags: []string{"x"},
		Edges: []store.EdgeInput{{DstPath: "a.md", Rel: "links", Kind: store.EdgePath}}}
	resolve := func(st *Store) error { return st.ResolveEdges(ctx) }
	del := func(st *Store) error { return st.DeleteEntry(ctx, "a.md") }
	for _, tc := range []struct {
		name  string
		dim   int
		match string
		nth   int
		op    func(st *Store) error
	}{
		{"replace: begin", 4, "BEGIN", 1, nil},
		{"replace: read old entry", 4, "FROM entries WHERE path", 1, nil},
		{"replace: read old chunk ids", 4, "SELECT id FROM chunks WHERE entry_id", 1, nil},
		{"replace: vec table check in clear", 4, "sqlite_master", 2, nil},
		{"replace: un-index fts", 4, "DELETE FROM chunks_fts", 1, nil},
		{"replace: un-index exact", 4, "DELETE FROM chunks_exact", 1, nil},
		{"replace: un-index vec", 4, "DELETE FROM chunks_vec", 1, nil},
		{"replace: delete chunks", 4, "DELETE FROM chunks WHERE", 1, nil},
		{"replace: upsert entry", 4, "INSERT INTO entries", 1, nil},
		{"replace: prepare chunk insert", 4, "PREPARE \n\t\tINSERT INTO chunks", 1, nil},
		{"replace: prepare fts insert", 4, "PREPARE INSERT INTO chunks_fts", 1, nil},
		{"replace: prepare exact insert", 4, "PREPARE INSERT INTO chunks_exact", 1, nil},
		{"replace: prepare vec insert", 4, "PREPARE INSERT INTO chunks_vec", 1, nil},
		{"replace: insert chunk", 4, "INSERT INTO chunks (entry_id", 2, nil},
		{"replace: insert fts", 4, "INSERT INTO chunks_fts (rowid", 2, nil},
		{"replace: insert exact", 4, "INSERT INTO chunks_exact (rowid", 2, nil},
		{"replace: insert vec", 4, "INSERT INTO chunks_vec (chunk_id", 2, nil},
		{"replace: read old edges", 4, "SELECT dst_path, rel, kind, dst_entry_id FROM edges", 1, nil},
		{"replace: delete tags", 4, "DELETE FROM entry_tags", 1, nil},
		{"replace: delete edges", 4, "DELETE FROM edges WHERE src_entry_id", 1, nil},
		{"replace: insert tag", 4, "INSERT INTO entry_tags", 1, nil},
		{"replace: insert edge", 4, "INSERT INTO edges", 1, nil},
		{"replace: commit", 4, "COMMIT", 1, nil},
		{"replace fts-only: commit", 0, "COMMIT", 1, nil},
		{"delete: begin", 4, "BEGIN", 1, func(st *Store) error { return st.DeleteEntry(ctx, "a.md") }},
		{"delete: delete entry", 4, "DELETE FROM entries", 1, func(st *Store) error { return st.DeleteEntry(ctx, "a.md") }},
		{"delete: delete tags", 4, "DELETE FROM entry_tags", 1, del},
		{"delete: delete edges", 4, "DELETE FROM edges WHERE src_entry_id", 1, del},
		{"delete: detach inbound edges", 4, "UPDATE edges SET dst_entry_id = NULL", 1, del},
		{"reset: edges", 4, "DELETE FROM edges", 1, func(st *Store) error { return st.ResetIndex(ctx) }},
		{"reset: tags", 4, "DELETE FROM entry_tags", 1, func(st *Store) error { return st.ResetIndex(ctx) }},
		{"resolve: begin", 4, "BEGIN", 1, resolve},
		{"resolve: read entries", 4, "SELECT id, path, aliases FROM entries", 1, resolve},
		{"resolve: read files", 4, "SELECT DISTINCT entry_id, source_file", 1, resolve},
		{"resolve: read edges", 4, "SELECT rowid, dst_path, kind", 1, resolve},
		{"resolve: update", 4, "UPDATE edges SET dst_entry_id = ? WHERE rowid", 1, resolve},
		{"resolve: commit", 4, "COMMIT", 1, resolve},
		{"delete: commit", 4, "COMMIT", 1, func(st *Store) error { return st.DeleteEntry(ctx, "a.md") }},
		{"reset: fts delete-all", 4, "delete-all", 1, func(st *Store) error { return st.ResetIndex(ctx) }},
		{"reset: exact delete-all", 4, "delete-all", 2, func(st *Store) error { return st.ResetIndex(ctx) }},
		{"reset: begin", 4, "BEGIN", 1, func(st *Store) error { return st.ResetIndex(ctx) }},
		{"reset: commit", 4, "COMMIT", 1, func(st *Store) error { return st.ResetIndex(ctx) }},
	} {
		t.Run(tc.name, func(t *testing.T) {
			st := openFaulty(t, tc.dim)
			if _, err := st.ReplaceEntry(ctx, entry, faultChunks(tc.dim, "before")); err != nil {
				t.Fatal(err)
			}
			op := tc.op
			if op == nil {
				op = func(st *Store) error {
					_, err := st.ReplaceEntry(ctx, entry, faultChunks(tc.dim, "after"))
					return err
				}
			}
			fail(t, tc.match, tc.nth)
			err := op(st)
			if !errors.Is(err, errFault) || errs.KindOf(err) != errs.Internal {
				t.Fatalf("err = %v (kind %v), want the injected fault as Internal", err, errs.KindOf(err))
			}
			arm("", 0)
			e, err := st.Entry(ctx, "a.md")
			if err != nil || e == nil {
				t.Fatalf("entry lost after a failed op: %v, %v", e, err)
			}
			cs, _ := st.Chunks(ctx, e.ID)
			if len(cs) != 2 || cs[0].TextHash != "before0" {
				t.Errorf("chunks after a failed op = %+v, want the previous version", cs)
			}
			for _, exact := range []bool{false, true} {
				if res, err := st.FTS(ctx, store.FTSQuery{Terms: []string{"before"}, Limit: 5, Exact: exact}); err != nil || len(res.Rows) != 2 {
					t.Errorf("FTS(before, exact=%v) after a failed op = %+v, %v; want the previous version's 2 rows", exact, res.Rows, err)
				}
			}
			if _, ok, _ := st.EmbedMeta(ctx); !ok {
				t.Error("embed_meta lost after a failed op")
			}
			if es, err := st.Edges(ctx); err != nil || len(es) != 1 || es[0].DstEntryID != 0 {
				t.Errorf("edges after a failed op = %+v, %v; want the previous version's one unresolved edge", es, err)
			}
			if res, err := st.FTS(ctx, store.FTSQuery{Terms: []string{"before"}, Tag: "X", Limit: 5}); err != nil || len(res.Rows) != 2 {
				t.Errorf("tag filter after a failed op = %+v, %v; want the previous version's tag", res.Rows, err)
			}
		})
	}
}

// TestDriverFaultsOnReadsAndLog: read paths and the search log return Internal errors that keep the
// cause, instead of empty results.
func TestDriverFaultsOnReadsAndLog(t *testing.T) {
	ctx := context.Background()
	rec := store.SearchRecord{Query: "q", Mode: "fts", K: 5, Caller: "c",
		Results: []store.LoggedHit{{ChunkID: 1, EntryPath: "a.md"}}, Candidates: []store.LoggedHit{{ChunkID: 1, EntryPath: "a.md"}}}
	for _, tc := range []struct {
		name  string
		match string
		nth   int
		op    func(st *Store, id int64) error
	}{
		{"EnsureEmbedMeta begin", "BEGIN", 1, func(st *Store, _ int64) error {
			return st.EnsureEmbedMeta(ctx, store.EmbedMeta{Model: "m", Dim: 4})
		}},
		{"EnsureEmbedMeta read", "FROM embed_meta", 1, func(st *Store, _ int64) error {
			return st.EnsureEmbedMeta(ctx, store.EmbedMeta{Model: "m", Dim: 4})
		}},
		{"EnsureEmbedMeta create vec", "CREATE VIRTUAL TABLE", 1, func(st *Store, _ int64) error {
			return st.EnsureEmbedMeta(ctx, store.EmbedMeta{Model: "m", Dim: 4})
		}},
		{"EnsureEmbedMeta commit", "COMMIT", 1, func(st *Store, _ int64) error {
			return st.EnsureEmbedMeta(ctx, store.EmbedMeta{Model: "m", Dim: 4})
		}},
		{"EnsureEmbedMeta drop stale vec", "DROP TABLE IF EXISTS chunks_vec", 2, func(st *Store, _ int64) error {
			if err := st.ResetIndex(ctx); err != nil {
				return err
			}
			return st.EnsureEmbedMeta(ctx, store.EmbedMeta{Model: "m", Dim: 4})
		}},
		{"EnsureEmbedMeta record", "INSERT INTO embed_meta", 1, func(st *Store, _ int64) error {
			if err := st.ResetIndex(ctx); err != nil {
				return err
			}
			return st.EnsureEmbedMeta(ctx, store.EmbedMeta{Model: "m", Dim: 4})
		}},
		{"ReplaceEntry embed_meta read", "FROM embed_meta", 1, func(st *Store, _ int64) error {
			_, err := st.ReplaceEntry(ctx, store.EntryInput{Path: "b.md"}, nil)
			return err
		}},
		{"DeleteEntry read", "FROM entries WHERE path", 1, func(st *Store, _ int64) error { return st.DeleteEntry(ctx, "a.md") }},
		{"DeleteEntry chunk ids", "SELECT id FROM chunks WHERE entry_id", 1, func(st *Store, _ int64) error { return st.DeleteEntry(ctx, "a.md") }},
		{"FeedbackNone useful-rank read", "SELECT min(rank)", 1, func(st *Store, id int64) error {
			if err := st.Feedback(ctx, id, 1, true, "", time.Time{}); err != nil {
				return err
			}
			return st.FeedbackNone(ctx, id, "", time.Time{})
		}},
		{"EmbedMeta", "FROM embed_meta", 1, func(st *Store, _ int64) error { _, _, err := st.EmbedMeta(ctx); return err }},
		{"HasVectors", "sqlite_master", 1, func(st *Store, _ int64) error { _, err := st.HasVectors(ctx); return err }},
		{"Entries", "FROM entries ORDER BY", 1, func(st *Store, _ int64) error { _, err := st.Entries(ctx); return err }},
		{"EntryCount", "count(*) FROM entries", 1, func(st *Store, _ int64) error { _, err := st.EntryCount(ctx); return err }},
		{"Chunks", "FROM chunks WHERE entry_id", 1, func(st *Store, _ int64) error { _, err := st.Chunks(ctx, 1); return err }},
		{"Embeddings vec check", "sqlite_master", 1, func(st *Store, _ int64) error { _, err := st.Embeddings(ctx, 1); return err }},
		{"Embeddings read", "JOIN chunks_vec", 1, func(st *Store, _ int64) error { _, err := st.Embeddings(ctx, 1); return err }},
		{"FTS", "chunks_fts MATCH", 1, func(st *Store, _ int64) error {
			_, err := st.FTS(ctx, store.FTSQuery{Terms: []string{"before"}, Limit: 5})
			return err
		}},
		{"KNN", "embedding MATCH", 1, func(st *Store, _ int64) error {
			_, err := st.KNN(ctx, store.KNNQuery{Vec: []float32{1, 0, 0, 0}, K: 2})
			return err
		}},
		{"KNN tag filter", "t.tag = ? AND c.id IN", 1, func(st *Store, _ int64) error {
			_, err := st.KNN(ctx, store.KNNQuery{Vec: []float32{1, 0, 0, 0}, K: 2, Tag: "x", OverFetch: 4})
			return err
		}},
		{"EntriesByPath", "FROM entries WHERE path IN", 1, func(st *Store, _ int64) error {
			_, err := st.EntriesByPath(ctx, []string{"a.md", "b.md"})
			return err
		}},
		{"ChunkMeta", "SELECT c.id, e.path", 1, func(st *Store, _ int64) error { _, err := st.ChunkMeta(ctx, []int64{1}); return err }},
		{"Edges", "LEFT JOIN entries AS dst", 1, func(st *Store, _ int64) error { _, err := st.Edges(ctx); return err }},
		{"LogSearch begin", "BEGIN", 1, func(st *Store, _ int64) error { _, err := st.LogSearch(ctx, rec); return err }},
		{"LogSearch searches", "INSERT INTO searches", 1, func(st *Store, _ int64) error { _, err := st.LogSearch(ctx, rec); return err }},
		{"LogSearch prepare results", "PREPARE \n\t\tINSERT INTO search_results", 1, func(st *Store, _ int64) error { _, err := st.LogSearch(ctx, rec); return err }},
		{"LogSearch results", "INSERT INTO search_results", 2, func(st *Store, _ int64) error { _, err := st.LogSearch(ctx, rec); return err }},
		{"LogSearch prepare candidates", "PREPARE \n\t\tINSERT INTO search_candidates", 1, func(st *Store, _ int64) error { _, err := st.LogSearch(ctx, rec); return err }},
		{"LogSearch candidates", "INSERT INTO search_candidates", 2, func(st *Store, _ int64) error { _, err := st.LogSearch(ctx, rec); return err }},
		{"LogSearch commit", "COMMIT", 1, func(st *Store, _ int64) error { _, err := st.LogSearch(ctx, rec); return err }},
		{"Feedback read", "SELECT n_returned", 1, func(st *Store, id int64) error { return st.Feedback(ctx, id, 1, true, "", time.Time{}) }},
		{"Feedback begin", "BEGIN", 1, func(st *Store, id int64) error { return st.Feedback(ctx, id, 1, true, "", time.Time{}) }},
		{"Feedback write", "INSERT OR REPLACE INTO search_feedback", 1, func(st *Store, id int64) error { return st.Feedback(ctx, id, 1, true, "", time.Time{}) }},
		{"Feedback withdraw none", "DELETE FROM search_feedback", 1, func(st *Store, id int64) error { return st.Feedback(ctx, id, 1, true, "", time.Time{}) }},
		{"Feedback commit", "COMMIT", 1, func(st *Store, id int64) error { return st.Feedback(ctx, id, 1, true, "", time.Time{}) }},
		{"FeedbackNone write", "INSERT OR REPLACE INTO search_feedback", 1, func(st *Store, id int64) error { return st.FeedbackNone(ctx, id, "", time.Time{}) }},
		{"LastSearch", "FROM searches", 1, func(st *Store, _ int64) error { _, err := st.LastSearch(ctx, "c"); return err }},
		{"RecordOpen", "INSERT OR IGNORE INTO search_opens", 1, func(st *Store, _ int64) error {
			_, err := st.RecordOpen(ctx, store.OpenRecord{EntryPath: "a.md", At: time.Now(), Window: openWindow})
			return err
		}},
		{"QueryLog", "SELECT count(*) FROM searches", 1, func(st *Store, _ int64) error {
			_, err := st.QueryLog(ctx, "SELECT count(*) FROM searches")
			return err
		}},
		{"DriverInfo", "compile_options", 1, func(st *Store, _ int64) error { _, err := st.DriverInfo(ctx); return err }},
	} {
		t.Run(tc.name, func(t *testing.T) {
			st := openFaulty(t, 4)
			if _, err := st.ReplaceEntry(ctx, store.EntryInput{Path: "a.md", Kind: "file", Title: "T", Summary: "S", Tags: []string{"x"}},
				faultChunks(4, "before")); err != nil {
				t.Fatal(err)
			}
			id, err := st.LogSearch(ctx, rec)
			if err != nil {
				t.Fatal(err)
			}
			fail(t, tc.match, tc.nth)
			err = tc.op(st, id)
			if !errors.Is(err, errFault) || errs.KindOf(err) != errs.Internal {
				t.Fatalf("err = %v (kind %v), want the injected fault as Internal", err, errs.KindOf(err))
			}
		})
	}
}

// TestDriverFaultsOnRows: a row that fails to scan, or an iteration that fails part-way, is an
// Internal error that keeps the cause for every multi-row reader, never a short result.
func TestDriverFaultsOnRows(t *testing.T) {
	ctx := context.Background()
	for _, tc := range []struct {
		name, match string
		op          func(st *Store) error
	}{
		{"Related", "WITH RECURSIVE walk", func(st *Store) error { _, err := st.Related(ctx, "n0.md", 1); return err }},
		{"Superseded", "rel = 'supersedes'", func(st *Store) error { _, err := st.Superseded(ctx, []string{"n1.md"}); return err }},
		{"Edges", "LEFT JOIN entries AS dst", func(st *Store) error { _, err := st.Edges(ctx); return err }},
		{"Entries", "FROM entries ORDER BY", func(st *Store) error { _, err := st.Entries(ctx); return err }},
		{"Chunks", "FROM chunks WHERE entry_id", func(st *Store) error { _, err := st.Chunks(ctx, 1); return err }},
	} {
		for _, mode := range []struct {
			name string
			m    sqlfault.RowFault
		}{{"next", rowsIter}, {"scan", rowsScan}} {
			t.Run(tc.name+" "+mode.name, func(t *testing.T) {
				st := openFaulty(t, 0)
				relatedGraph(t, st, []byte{0, 1, 1})
				failRows(t, tc.match, 1, mode.m)
				err := tc.op(st)
				if err == nil || errs.KindOf(err) != errs.Internal {
					t.Fatalf("err = %v (kind %v), want an Internal error", err, errs.KindOf(err))
				}
				if mode.m == rowsIter && !errors.Is(err, errFault) {
					t.Errorf("err = %v, want the injected fault kept as the cause", err)
				}
			})
		}
	}
}

// TestOpenFaults: a failure while opening or migrating is an error that keeps the cause, and a
// failed migration leaves the file at version 0 so the next open retries it.
func TestOpenFaults(t *testing.T) {
	for _, match := range []string{"PRAGMA user_version", "compile_options", "vec_version", "SELECT vec_version()", "BEGIN IMMEDIATE", "CREATE TABLE", "PRAGMA user_version = 1", "COMMIT"} {
		t.Run(match, func(t *testing.T) {
			path := filepath.Join(t.TempDir(), "kb.sqlite")
			fail(t, match, 1)
			_, err := Open(context.Background(), path, Options{driver: faultDriverName})
			if !errors.Is(err, errFault) {
				t.Fatalf("err = %v, want the injected fault", err)
			}
			arm("", 0)
			st, err := Open(context.Background(), path, Options{})
			if err != nil {
				t.Fatalf("reopen after a failed open: %v", err)
			}
			defer st.Close()
			var v int
			if err := st.DB().QueryRow("PRAGMA user_version").Scan(&v); err != nil || v != SchemaVersion() {
				t.Errorf("user_version = %d, %v after reopen", v, err)
			}
		})
	}
}

// TestOpenReadOnlyAndDirFaults: the read-only version read failing, and a database directory that
// cannot be created, are errors that keep their cause.
func TestOpenReadOnlyAndDirFaults(t *testing.T) {
	ctx := context.Background()
	path := filepath.Join(t.TempDir(), "kb.sqlite")
	st, err := Open(ctx, path, Options{})
	if err != nil {
		t.Fatal(err)
	}
	st.Close()
	fail(t, "PRAGMA user_version", 1)
	if _, err := Open(ctx, path, Options{ReadOnly: true, driver: faultDriverName}); !errors.Is(err, errFault) {
		t.Errorf("read-only version read: err = %v, want the injected fault", err)
	}
	arm("", 0)

	// A stat failure other than not-exist (EACCES on the directory) is Usage naming the path.
	t.Run("unreadable directory", func(t *testing.T) {
		if os.Geteuid() == 0 {
			t.Skip("root ignores directory permissions")
		}
		dir := filepath.Join(t.TempDir(), "locked")
		if err := os.Mkdir(dir, 0o755); err != nil {
			t.Fatal(err)
		}
		locked := filepath.Join(dir, "kb.sqlite")
		if err := os.WriteFile(locked, nil, 0o644); err != nil {
			t.Fatal(err)
		}
		if err := os.Chmod(dir, 0o000); err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { os.Chmod(dir, 0o755) })
		_, err := Open(ctx, locked, Options{ReadOnly: true})
		if errs.KindOf(err) != errs.Usage || !strings.Contains(err.Error(), "cannot read the database "+locked) || !strings.Contains(err.Error(), "permissions") {
			t.Errorf("err = %v (kind %v), want Usage naming the path and permissions", err, errs.KindOf(err))
		}
		if errors.Is(err, fs.ErrNotExist) || !errors.Is(err, fs.ErrPermission) {
			t.Errorf("err = %v, want the permission cause kept (and not NotFound)", err)
		}
	})

	blocker := filepath.Join(t.TempDir(), "file")
	if err := os.WriteFile(blocker, nil, 0o644); err != nil {
		t.Fatal(err)
	}
	_, err = Open(ctx, filepath.Join(blocker, "sub", "kb.sqlite"), Options{})
	if errs.KindOf(err) != errs.Usage || !strings.Contains(err.Error(), "could not create the database directory") {
		t.Errorf("dir under a file: err = %v (kind %v), want Usage", err, errs.KindOf(err))
	}
}

// TestCancelMidWriteIsInterrupted: SIGINT cancels ctx while a write transaction is running. However
// the cancel surfaces (ctx.Err(), SQLITE_INTERRUPT, or sql.ErrTxDone after database/sql rolled the
// transaction back), the store reports Interrupted, never Internal, and the previous version stays.
func TestCancelMidWriteIsInterrupted(t *testing.T) {
	entry := store.EntryInput{Path: "a.md", Kind: "file", Title: "T", Summary: "S"}
	for _, tc := range []struct {
		match string
		err   error // what the matched statement returns after the cancel
	}{
		{"DELETE FROM chunks_fts", nil},
		{"INSERT INTO entries", nil},
		{"INSERT INTO chunks (entry_id", nil},
		{"INSERT INTO chunks_vec (chunk_id", nil},
		{"COMMIT", sql.ErrTxDone}, // database/sql rolled back in the background before Commit ran
	} {
		match := tc.match
		t.Run(match, func(t *testing.T) {
			st := openFaulty(t, 4)
			if _, err := st.ReplaceEntry(context.Background(), entry, faultChunks(4, "before")); err != nil {
				t.Fatal(err)
			}
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			fail(t, match, 1)
			faults.SetHook(func() error { cancel(); return tc.err })
			_, err := st.ReplaceEntry(ctx, entry, faultChunks(4, "after"))
			if errs.KindOf(err) != errs.Interrupted {
				t.Fatalf("err = %v (kind %v), want Interrupted", err, errs.KindOf(err))
			}
			arm("", 0)
			e, _ := st.Entry(context.Background(), "a.md")
			cs, _ := st.Chunks(context.Background(), e.ID)
			if len(cs) != 2 || cs[0].TextHash != "before0" {
				t.Errorf("chunks after an interrupted write = %+v, want the previous version", cs)
			}
		})
	}
}

// TestCancelMidResolveIsInterrupted: SIGINT during ResolveEdges (after its last UPDATE ran) is
// Interrupted, and no edge target is committed.
func TestCancelMidResolveIsInterrupted(t *testing.T) {
	st := openFaulty(t, 4)
	entry := store.EntryInput{Path: "a.md", Kind: "file", Title: "T", Summary: "S",
		Edges: []store.EdgeInput{{DstPath: "a.md", Rel: "links", Kind: store.EdgePath}}}
	if _, err := st.ReplaceEntry(context.Background(), entry, faultChunks(4, "before")); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	fail(t, "UPDATE edges SET dst_entry_id = ? WHERE rowid", 1)
	faults.SetHook(func() error { cancel(); return nil })
	err := st.ResolveEdges(ctx)
	if errs.KindOf(err) != errs.Interrupted {
		t.Fatalf("err = %v (kind %v), want Interrupted", err, errs.KindOf(err))
	}
	arm("", 0)
	if es, err := st.Edges(context.Background()); err != nil || len(es) != 1 || es[0].DstEntryID != 0 {
		t.Errorf("edges after an interrupted resolve = %+v, %v; want the one edge still unresolved", es, err)
	}
}

// TestEnableWALRetries: entering WAL can fail transiently (SQLITE_BUSY without the busy handler
// when another process opens the same fresh file); enableWAL retries, and gives up with the last
// cause after its attempts.
func TestEnableWALRetries(t *testing.T) {
	ctx := context.Background()
	t.Run("one failure then success", func(t *testing.T) {
		fail(t, "journal_mode", 1)
		st, err := Open(ctx, filepath.Join(t.TempDir(), "kb.sqlite"), Options{driver: faultDriverName})
		if err != nil {
			t.Fatalf("Open after one transient WAL failure: %v", err)
		}
		defer st.Close()
		var mode string
		if err := st.DB().QueryRow("PRAGMA journal_mode").Scan(&mode); err != nil || mode != "wal" {
			t.Errorf("journal_mode = %q, %v; want wal", mode, err)
		}
	})
	t.Run("every attempt fails", func(t *testing.T) {
		faults.FailAlways(t, "journal_mode") // every attempt fails
		_, err := Open(ctx, filepath.Join(t.TempDir(), "kb.sqlite"), Options{driver: faultDriverName})
		if !errors.Is(err, errFault) || !strings.Contains(fmt.Sprint(errs.Chain(err)), "enable WAL") {
			t.Fatalf("err = %v (chain %v), want the injected fault after the retries", err, errs.Chain(err))
		}
	})
	t.Run("cancelled while retrying", func(t *testing.T) {
		cctx, cancel := context.WithCancel(ctx)
		fail(t, "journal_mode", 1)
		faults.SetHook(func() error { cancel(); return errFault })
		_, err := Open(cctx, filepath.Join(t.TempDir(), "kb.sqlite"), Options{driver: faultDriverName})
		if errs.KindOf(err) != errs.Interrupted {
			t.Fatalf("err = %v (kind %v), want Interrupted", err, errs.KindOf(err))
		}
	})
}

// TestEnableWAL: the journal-mode loop returns only on "wal" with no error. A PRAGMA that answers
// another mode without an error (the `err == nil && EqualFold` mutant to `||` accepts it) is
// retried (the real PRAGMA then runs and the file ends in WAL), and fails the open naming the mode
// when it never changes. The "error then WAL" case is in TestEnableWALRetries.
func TestEnableWAL(t *testing.T) {
	ctx := context.Background()
	mode := func(t *testing.T, path string) string {
		t.Helper()
		db, err := sql.Open(driverName, "file:"+path)
		if err != nil {
			t.Fatal(err)
		}
		defer db.Close()
		var m string
		if err := db.QueryRow("PRAGMA journal_mode").Scan(&m); err != nil {
			t.Fatal(err)
		}
		return m
	}
	for _, tc := range []struct {
		name    string
		arrange func(t *testing.T)
		wantErr string // "" = Open succeeds
	}{
		{"a non-WAL answer then WAL", func(t *testing.T) { rewrite(t, "journal_mode = WAL", "delete") }, ""},
		{"stays delete", func(t *testing.T) {
			rewrite(t, "journal_mode = WAL", slices.Repeat([]string{"delete"}, 60)...)
		}, `journal_mode is "delete"`},
	} {
		t.Run(tc.name, func(t *testing.T) {
			tc.arrange(t)
			path := filepath.Join(t.TempDir(), "kb.sqlite")
			st, err := Open(ctx, path, Options{driver: faultDriverName})
			if tc.wantErr != "" {
				if err == nil || !strings.Contains(fmt.Sprint(errs.Chain(err)), tc.wantErr) {
					t.Fatalf("Open = %v, want an error containing %q", err, tc.wantErr)
				}
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			st.Close()
			if got := mode(t, path); !strings.EqualFold(got, "wal") {
				t.Errorf("file journal_mode = %q, want wal (the real PRAGMA must have run)", got)
			}
		})
	}
}

// TestDiskFullAndIOErrorAreUsage: SQLITE_FULL and SQLITE_IOERR at each write statement (and at
// COMMIT) reach the caller as Usage with the "free space and retry" message, keep the cause and
// leave the previous version intact. Usage is exit 1 in cli.report: not an incident.
func TestDiskFullAndIOErrorAreUsage(t *testing.T) {
	entry := store.EntryInput{Path: "a.md", Kind: "file", Title: "T", Summary: "S"}
	for _, code := range []sqlite3.ErrNo{sqlite3.ErrFull, sqlite3.ErrIoErr} {
		for _, match := range []string{"INSERT INTO entries", "INSERT INTO chunks (entry_id", "INSERT INTO chunks_vec (chunk_id", "COMMIT"} {
			t.Run(fmt.Sprintf("%v/%s", code, match), func(t *testing.T) {
				st := openFaulty(t, 4)
				if _, err := st.ReplaceEntry(context.Background(), entry, faultChunks(4, "before")); err != nil {
					t.Fatal(err)
				}
				cause := sqlite3.Error{Code: code}
				fail(t, match, 1)
				faults.SetHook(func() error { return cause })
				_, err := st.ReplaceEntry(context.Background(), entry, faultChunks(4, "after"))
				if errs.KindOf(err) != errs.Usage || !errors.Is(err, cause) ||
					!strings.Contains(err.Error(), "disk full or unwritable: "+st.Path()+"; free space and retry") {
					t.Fatalf("err = %v (kind %v), want Usage 'disk full or unwritable' keeping the cause", err, errs.KindOf(err))
				}
				arm("", 0)
				e, _ := st.Entry(context.Background(), "a.md")
				cs, _ := st.Chunks(context.Background(), e.ID)
				if len(cs) != 2 || cs[0].TextHash != "before0" {
					t.Errorf("chunks after the failed write = %+v, want the previous version", cs)
				}
			})
		}
	}
}

// TestVecInsertDiskFullIsUsage: a real SQLITE_FULL (the page limit pinned at the current size)
// inside a sqlite-vec insert surfaces from the extension as a generic SQLITE_ERROR; ReplaceEntry
// probes and reports the disk-full Usage error, keeping the cause (Q47). A generic vec failure
// with room left stays Internal.
func TestVecInsertDiskFullIsUsage(t *testing.T) {
	ctx := context.Background()
	entry := store.EntryInput{Path: "a.md", Kind: "file", Title: "T", Summary: "S"}
	for _, tc := range []struct {
		name  string
		limit bool
		want  errs.Kind
	}{
		{"page limit reached", true, errs.Usage},
		{"vec failure with room left", false, errs.Internal},
	} {
		t.Run(tc.name, func(t *testing.T) {
			st := openFaulty(t, 4)
			st.DB().SetMaxOpenConns(1) // max_page_count is per connection
			if tc.limit {
				var pages int
				if err := st.DB().QueryRow("PRAGMA page_count").Scan(&pages); err != nil {
					t.Fatal(err)
				}
				if _, err := st.DB().Exec(fmt.Sprintf("PRAGMA max_page_count = %d", pages)); err != nil {
					t.Fatal(err)
				}
			} else {
				fail(t, "INSERT INTO chunks_vec (chunk_id", 1)
				faults.SetHook(func() error {
					return fmt.Errorf("%s: %w", vecChunkFailure, sqlite3.Error{Code: sqlite3.ErrError})
				})
			}
			_, err := st.ReplaceEntry(ctx, entry, faultChunks(4, "x"))
			if errs.KindOf(err) != tc.want {
				t.Fatalf("kind = %v (%v), want %v", errs.KindOf(err), err, tc.want)
			}
			if tc.want == errs.Usage && !strings.Contains(err.Error(), "disk full or unwritable") {
				t.Errorf("err = %v, want the disk-full message", err)
			}
			if tc.limit && !strings.Contains(errors.Unwrap(err).Error(), "for vector search") {
				t.Errorf("err = %v, want the failure to come from the vec insert (the probe path)", err)
			}
		})
	}
}
