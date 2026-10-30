package sqlite

import (
	"context"
	"database/sql"
	"database/sql/driver"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	sqlite3 "github.com/mattn/go-sqlite3"

	"github.com/nickstrad/kb/internal/errs"
	"github.com/nickstrad/kb/internal/store"
)

// faultDriver wraps the real driver and fails the nth statement whose SQL contains match
// ("COMMIT" matches Tx.Commit). It reaches error paths only a broken disk or driver would (H20).
// Tests set it with fail() before the operation; it is reset per test.
const faultDriverName = "sqlite3_fault"

var (
	faultMu    sync.Mutex
	faultMatch string
	faultNth   int
	faultSeen  int
	faultHook  func() error // when set, runs at the matched statement and its error is returned
	errFault   = errors.New("injected fault")

	// faultRewrite answers the statements containing faultRewriteMatch with a one-row, one-column
	// result of the next value here instead of running them, so a PRAGMA can "succeed" with a
	// surprising value. Once the values run out, statements run for real.
	faultRewriteMatch string
	faultRewriteVals  []string
)

func init() {
	sql.Register(faultDriverName, faultDriver{&sqlite3.SQLiteDriver{}})
}

// fail arms the fault for the nth (1-based) statement containing match; "" disarms it.
func fail(t *testing.T, match string, nth int) {
	arm(match, nth)
	t.Cleanup(func() { arm("", 0) })
}

// rewrite makes the first len(vals) statements containing match answer vals in order.
func rewrite(t *testing.T, match string, vals ...string) {
	faultMu.Lock()
	faultRewriteMatch, faultRewriteVals = match, vals
	faultMu.Unlock()
	t.Cleanup(func() { rewrite0() })
}

func rewrite0() {
	faultMu.Lock()
	faultRewriteMatch, faultRewriteVals = "", nil
	faultMu.Unlock()
}

// rewritten reports the canned answer for query, if one is due.
func rewritten(query string) (driver.Rows, bool) {
	faultMu.Lock()
	defer faultMu.Unlock()
	if faultRewriteMatch == "" || !strings.Contains(query, faultRewriteMatch) || len(faultRewriteVals) == 0 {
		return nil, false
	}
	v := faultRewriteVals[0]
	faultRewriteVals = faultRewriteVals[1:]
	return &oneRow{col: "value", val: v}, true
}

type oneRow struct {
	col  string
	val  string
	done bool
}

func (r *oneRow) Columns() []string { return []string{r.col} }
func (r *oneRow) Close() error      { return nil }
func (r *oneRow) Next(dest []driver.Value) error {
	if r.done {
		return io.EOF
	}
	r.done, dest[0] = true, r.val
	return nil
}

func arm(match string, nth int) {
	faultMu.Lock()
	faultMatch, faultNth, faultSeen, faultHook = match, nth, 0, nil
	faultMu.Unlock()
}

func faulty(query string) error {
	faultMu.Lock()
	defer faultMu.Unlock()
	if faultMatch == "" || !strings.Contains(query, faultMatch) {
		return nil
	}
	faultSeen++
	if faultSeen == faultNth {
		if faultHook != nil {
			return faultHook()
		}
		return errFault
	}
	return nil
}

type faultDriver struct{ inner driver.Driver }

func (d faultDriver) Open(name string) (driver.Conn, error) {
	c, err := d.inner.Open(name)
	if err != nil {
		return nil, err
	}
	return faultConn{c.(*sqlite3.SQLiteConn)}, nil
}

type faultConn struct{ c *sqlite3.SQLiteConn }

func (f faultConn) Prepare(q string) (driver.Stmt, error) {
	return f.PrepareContext(context.Background(), q)
}
func (f faultConn) PrepareContext(ctx context.Context, q string) (driver.Stmt, error) {
	if err := faulty("PREPARE " + q); err != nil {
		return nil, err
	}
	s, err := f.c.PrepareContext(ctx, q)
	if err != nil {
		return nil, err
	}
	return faultStmt{s, q}, nil
}
func (f faultConn) Close() error { return f.c.Close() }
func (f faultConn) Begin() (driver.Tx, error) {
	return f.BeginTx(context.Background(), driver.TxOptions{})
}
func (f faultConn) BeginTx(ctx context.Context, o driver.TxOptions) (driver.Tx, error) {
	if err := faulty("BEGIN"); err != nil {
		return nil, err
	}
	tx, err := f.c.BeginTx(ctx, o)
	if err != nil {
		return nil, err
	}
	return faultTx{tx}, nil
}
func (f faultConn) ExecContext(ctx context.Context, q string, args []driver.NamedValue) (driver.Result, error) {
	if err := faulty(q); err != nil {
		return nil, err
	}
	return f.c.ExecContext(ctx, q, args)
}
func (f faultConn) QueryContext(ctx context.Context, q string, args []driver.NamedValue) (driver.Rows, error) {
	if rows, ok := rewritten(q); ok {
		return rows, nil
	}
	if err := faulty(q); err != nil {
		return nil, err
	}
	return f.c.QueryContext(ctx, q, args)
}
func (f faultConn) Ping(ctx context.Context) error { return f.c.Ping(ctx) }

type faultTx struct{ tx driver.Tx }

func (t faultTx) Commit() error {
	if err := faulty("COMMIT"); err != nil {
		_ = t.tx.Rollback() // the injected commit failure leaves nothing applied
		return err
	}
	return t.tx.Commit()
}
func (t faultTx) Rollback() error { return t.tx.Rollback() }

type faultStmt struct {
	s driver.Stmt
	q string
}

func (s faultStmt) Close() error  { return s.s.Close() }
func (s faultStmt) NumInput() int { return s.s.NumInput() }
func (s faultStmt) Exec(args []driver.Value) (driver.Result, error) {
	return nil, errors.New("faultStmt: Exec without context is not used")
}
func (s faultStmt) Query(args []driver.Value) (driver.Rows, error) {
	return nil, errors.New("faultStmt: Query without context is not used")
}
func (s faultStmt) ExecContext(ctx context.Context, args []driver.NamedValue) (driver.Result, error) {
	if err := faulty(s.q); err != nil {
		return nil, err
	}
	return s.s.(driver.StmtExecContext).ExecContext(ctx, args)
}
func (s faultStmt) QueryContext(ctx context.Context, args []driver.NamedValue) (driver.Rows, error) {
	if err := faulty(s.q); err != nil {
		return nil, err
	}
	return s.s.(driver.StmtQueryContext).QueryContext(ctx, args)
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
	entry := store.EntryInput{Path: "a.md", Kind: "file", Title: "T", Summary: "S"}
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
		{"replace: un-index vec", 4, "DELETE FROM chunks_vec", 1, nil},
		{"replace: delete chunks", 4, "DELETE FROM chunks WHERE", 1, nil},
		{"replace: upsert entry", 4, "INSERT INTO entries", 1, nil},
		{"replace: prepare chunk insert", 4, "PREPARE \n\t\tINSERT INTO chunks", 1, nil},
		{"replace: prepare fts insert", 4, "PREPARE INSERT INTO chunks_fts", 1, nil},
		{"replace: prepare vec insert", 4, "PREPARE INSERT INTO chunks_vec", 1, nil},
		{"replace: insert chunk", 4, "INSERT INTO chunks (entry_id", 2, nil},
		{"replace: insert fts", 4, "INSERT INTO chunks_fts (rowid", 2, nil},
		{"replace: insert vec", 4, "INSERT INTO chunks_vec (chunk_id", 2, nil},
		{"replace: commit", 4, "COMMIT", 1, nil},
		{"replace fts-only: commit", 0, "COMMIT", 1, nil},
		{"delete: begin", 4, "BEGIN", 1, func(st *Store) error { return st.DeleteEntry(ctx, "a.md") }},
		{"delete: delete entry", 4, "DELETE FROM entries", 1, func(st *Store) error { return st.DeleteEntry(ctx, "a.md") }},
		{"delete: commit", 4, "COMMIT", 1, func(st *Store) error { return st.DeleteEntry(ctx, "a.md") }},
		{"reset: fts delete-all", 4, "delete-all", 1, func(st *Store) error { return st.ResetIndex(ctx) }},
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
			if _, ok, _ := st.EmbedMeta(ctx); !ok {
				t.Error("embed_meta lost after a failed op")
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
			if err := st.Feedback(ctx, id, 1, true, ""); err != nil {
				return err
			}
			return st.FeedbackNone(ctx, id, "")
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
		{"KNN tag filter", "e.tags LIKE", 1, func(st *Store, _ int64) error {
			_, err := st.KNN(ctx, store.KNNQuery{Vec: []float32{1, 0, 0, 0}, K: 2, Tag: "x", OverFetch: 4})
			return err
		}},
		{"ChunkMeta", "SELECT c.id, e.path", 1, func(st *Store, _ int64) error { _, err := st.ChunkMeta(ctx, []int64{1}); return err }},
		{"LogSearch begin", "BEGIN", 1, func(st *Store, _ int64) error { _, err := st.LogSearch(ctx, rec); return err }},
		{"LogSearch searches", "INSERT INTO searches", 1, func(st *Store, _ int64) error { _, err := st.LogSearch(ctx, rec); return err }},
		{"LogSearch prepare results", "PREPARE \n\t\tINSERT INTO search_results", 1, func(st *Store, _ int64) error { _, err := st.LogSearch(ctx, rec); return err }},
		{"LogSearch results", "INSERT INTO search_results", 2, func(st *Store, _ int64) error { _, err := st.LogSearch(ctx, rec); return err }},
		{"LogSearch prepare candidates", "PREPARE \n\t\tINSERT INTO search_candidates", 1, func(st *Store, _ int64) error { _, err := st.LogSearch(ctx, rec); return err }},
		{"LogSearch candidates", "INSERT INTO search_candidates", 2, func(st *Store, _ int64) error { _, err := st.LogSearch(ctx, rec); return err }},
		{"LogSearch commit", "COMMIT", 1, func(st *Store, _ int64) error { _, err := st.LogSearch(ctx, rec); return err }},
		{"Feedback read", "SELECT n_returned", 1, func(st *Store, id int64) error { return st.Feedback(ctx, id, 1, true, "") }},
		{"Feedback begin", "BEGIN", 1, func(st *Store, id int64) error { return st.Feedback(ctx, id, 1, true, "") }},
		{"Feedback write", "INSERT OR REPLACE INTO search_feedback", 1, func(st *Store, id int64) error { return st.Feedback(ctx, id, 1, true, "") }},
		{"Feedback withdraw none", "DELETE FROM search_feedback", 1, func(st *Store, id int64) error { return st.Feedback(ctx, id, 1, true, "") }},
		{"Feedback commit", "COMMIT", 1, func(st *Store, id int64) error { return st.Feedback(ctx, id, 1, true, "") }},
		{"FeedbackNone write", "INSERT OR REPLACE INTO search_feedback", 1, func(st *Store, id int64) error { return st.FeedbackNone(ctx, id, "") }},
		{"LastSearch", "FROM searches", 1, func(st *Store, _ int64) error { _, err := st.LastSearch(ctx, "c"); return err }},
		{"RecordOpen", "INSERT OR IGNORE INTO search_opens", 1, func(st *Store, _ int64) error {
			_, err := st.RecordOpen(ctx, "a.md", time.Now(), openWindow)
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
			faultMu.Lock()
			faultHook = func() error { cancel(); return tc.err }
			faultMu.Unlock()
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
		faultMu.Lock()
		faultMatch, faultNth, faultSeen = "journal_mode", 1, 0
		faultHook = func() error { faultSeen = 0; return errFault } // re-arm: fail every time
		faultMu.Unlock()
		t.Cleanup(func() { arm("", 0) })
		_, err := Open(ctx, filepath.Join(t.TempDir(), "kb.sqlite"), Options{driver: faultDriverName})
		if !errors.Is(err, errFault) || !strings.Contains(fmt.Sprint(errs.Chain(err)), "enable WAL") {
			t.Fatalf("err = %v (chain %v), want the injected fault after the retries", err, errs.Chain(err))
		}
	})
	t.Run("cancelled while retrying", func(t *testing.T) {
		cctx, cancel := context.WithCancel(ctx)
		faultMu.Lock()
		faultMatch, faultNth, faultSeen = "journal_mode", 1, 0
		faultHook = func() error { cancel(); return errFault }
		faultMu.Unlock()
		t.Cleanup(func() { arm("", 0) })
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
				faultMu.Lock()
				faultHook = func() error { return cause }
				faultMu.Unlock()
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
