package cache_test

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	"github.com/nickstrad/kb/internal/embed/cache"
)

const dim = 4

func openTemp(t *testing.T) *cache.Cache {
	t.Helper()
	c, err := cache.Open(context.Background(), filepath.Join(t.TempDir(), "sub", "embed-cache.sqlite"))
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	t.Cleanup(func() { c.Close() })
	return c
}

// rawDB opens the cache file directly, as a second process or a corrupting writer would.
func rawDB(t *testing.T, path string) *sql.DB {
	t.Helper()
	u := url.URL{Scheme: "file", Path: path}
	db, err := sql.Open("sqlite3", u.String()+"?_busy_timeout=5000")
	if err != nil {
		t.Fatalf("sql.Open: %v", err)
	}
	t.Cleanup(func() { db.Close() })
	return db
}

func vec(seed int) []float32 {
	v := make([]float32, dim)
	for i := range v {
		v[i] = float32(seed*10 + i)
	}
	return v
}

func equalVec(a, b []float32) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}

func TestPutGetBoundaries(t *testing.T) {
	ctx := context.Background()
	for _, n := range []int{0, 1, 100} {
		t.Run(fmt.Sprintf("%d texts", n), func(t *testing.T) {
			c := openTemp(t)
			texts := make([]string, n)
			vecs := make([][]float32, n)
			for i := range texts {
				texts[i] = fmt.Sprintf("text %d", i)
				vecs[i] = vec(i)
			}
			if err := c.Put(ctx, "m", dim, texts, vecs); err != nil {
				t.Fatalf("Put: %v", err)
			}
			got, err := c.Get(ctx, "m", dim, texts)
			if err != nil {
				t.Fatalf("Get: %v", err)
			}
			if len(got) != n {
				t.Fatalf("Get returned %d slots for %d texts", len(got), n)
			}
			for i := range got {
				if !equalVec(got[i], vecs[i]) {
					t.Fatalf("slot %d = %v, want %v", i, got[i], vecs[i])
				}
			}
		})
	}
}

func TestGetMixedHitsAndMissesKeepsOrder(t *testing.T) {
	ctx := context.Background()
	c := openTemp(t)
	if err := c.Put(ctx, "m", dim, []string{"b", "d"}, [][]float32{vec(2), vec(4)}); err != nil {
		t.Fatalf("Put: %v", err)
	}
	got, err := c.Get(ctx, "m", dim, []string{"a", "b", "c", "d", "b"})
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	want := [][]float32{nil, vec(2), nil, vec(4), vec(2)}
	for i := range want {
		if (got[i] == nil) != (want[i] == nil) || !equalVec(got[i], want[i]) {
			t.Fatalf("slot %d = %v, want %v", i, got[i], want[i])
		}
	}
}

func TestKeyIncludesModelAndDim(t *testing.T) {
	ctx := context.Background()
	c := openTemp(t)
	if err := c.Put(ctx, "model-a", dim, []string{"x"}, [][]float32{vec(1)}); err != nil {
		t.Fatalf("Put a: %v", err)
	}
	if err := c.Put(ctx, "model-b", dim, []string{"x"}, [][]float32{vec(2)}); err != nil {
		t.Fatalf("Put b: %v", err)
	}
	for model, want := range map[string][]float32{"model-a": vec(1), "model-b": vec(2), "model-c": nil} {
		got, err := c.Get(ctx, model, dim, []string{"x"})
		if err != nil {
			t.Fatalf("Get %s: %v", model, err)
		}
		if (got[0] == nil) != (want == nil) || !equalVec(got[0], want) {
			t.Fatalf("Get %s = %v, want %v", model, got[0], want)
		}
	}
	// Same model and text at another width is another key.
	got, err := c.Get(ctx, "model-a", 2*dim, []string{"x"})
	if err != nil {
		t.Fatalf("Get dim %d: %v", 2*dim, err)
	}
	if got[0] != nil {
		t.Fatalf("Get at dim %d hit a dim-%d entry", 2*dim, dim)
	}
	stats, err := c.Stats(ctx)
	if err != nil {
		t.Fatalf("Stats: %v", err)
	}
	want := []cache.ModelStats{{Model: "model-a", Dim: dim, Entries: 1}, {Model: "model-b", Dim: dim, Entries: 1}}
	if fmt.Sprint(stats) != fmt.Sprint(want) {
		t.Fatalf("Stats = %v, want %v", stats, want)
	}
}

// A blob whose length does not match its dim (a corrupt or foreign row) reads as a miss, and the
// next Put of that key repairs it.
func TestWrongLengthBlobIsMissAndPutRepairs(t *testing.T) {
	ctx := context.Background()
	c := openTemp(t)
	key := cache.Key("x")
	for name, blob := range map[string][]byte{"short": make([]byte, 4*(dim-1)), "long": make([]byte, 4*(dim+1)), "empty": {}} {
		t.Run(name, func(t *testing.T) {
			if _, err := rawDB(t, c.Path()).Exec("INSERT OR REPLACE INTO emb VALUES (?, ?, ?, ?)", "m", dim, key[:], blob); err != nil {
				t.Fatalf("insert bad row: %v", err)
			}
			got, err := c.Get(ctx, "m", dim, []string{"x"})
			if err != nil {
				t.Fatalf("Get: %v", err)
			}
			if got[0] != nil {
				t.Fatalf("a %d-byte blob for dim %d was a hit: %v", len(blob), dim, got[0])
			}
			if err := c.Put(ctx, "m", dim, []string{"x"}, [][]float32{vec(7)}); err != nil {
				t.Fatalf("Put: %v", err)
			}
			got, err = c.Get(ctx, "m", dim, []string{"x"})
			if err != nil || !equalVec(got[0], vec(7)) {
				t.Fatalf("after Put, Get = %v, %v; want %v", got, err, vec(7))
			}
		})
	}
}

func TestPutAndGetRejectBadArguments(t *testing.T) {
	ctx := context.Background()
	c := openTemp(t)
	cases := []struct {
		name  string
		dim   int
		texts []string
		vecs  [][]float32
		want  string
	}{
		{"dim 0", 0, []string{"x"}, [][]float32{{}}, "dim 0 must be positive"},
		{"dim -1", -1, nil, nil, "dim -1 must be positive"},
		{"more texts than vectors", dim, []string{"x", "y"}, [][]float32{vec(1)}, "2 texts but 1 vectors"},
		{"vector one short", dim, []string{"x", "y"}, [][]float32{vec(1), vec(2)[:dim-1]}, "vector 1 has 3 dims, want 4"},
		{"vector one long", dim, []string{"x"}, [][]float32{append(vec(1), 0)}, "vector 0 has 5 dims, want 4"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			err := c.Put(ctx, "m", tc.dim, tc.texts, tc.vecs)
			if err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("Put error = %v, want it to contain %q", err, tc.want)
			}
		})
	}
	// Nothing was written by the rejected batches (the first text of one was valid).
	if stats, err := c.Stats(ctx); err != nil || len(stats) != 0 {
		t.Fatalf("Stats after rejected puts = %v, %v; want empty", stats, err)
	}
	if _, err := c.Get(ctx, "m", 0, []string{"x"}); err == nil {
		t.Fatal("Get with dim 0 succeeded")
	}
}

func TestReopenKeepsEntries(t *testing.T) {
	ctx := context.Background()
	path := filepath.Join(t.TempDir(), "c.sqlite")
	c, err := cache.Open(ctx, path)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	if err := c.Put(ctx, "m", dim, []string{"x"}, [][]float32{vec(1)}); err != nil {
		t.Fatalf("Put: %v", err)
	}
	if err := c.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
	c, err = cache.Open(ctx, path)
	if err != nil {
		t.Fatalf("reopen: %v", err)
	}
	defer c.Close()
	got, err := c.Get(ctx, "m", dim, []string{"x"})
	if err != nil || !equalVec(got[0], vec(1)) {
		t.Fatalf("Get after reopen = %v, %v", got, err)
	}
	var mode string
	if err := rawDB(t, path).QueryRow("PRAGMA journal_mode").Scan(&mode); err != nil || mode != "wal" {
		t.Fatalf("journal_mode = %q, %v; want wal", mode, err)
	}
}

func TestOpenErrors(t *testing.T) {
	ctx := context.Background()
	dir := t.TempDir()

	garbage := filepath.Join(dir, "garbage.sqlite")
	if err := os.WriteFile(garbage, []byte(strings.Repeat("not a database ", 512)), 0o644); err != nil {
		t.Fatal(err)
	}
	newer := filepath.Join(dir, "newer.sqlite")
	if _, err := rawDB(t, newer).Exec(fmt.Sprintf("PRAGMA user_version = %d", cache.SchemaVersion+1)); err != nil {
		t.Fatal(err)
	}
	blocker := filepath.Join(dir, "file")
	if err := os.WriteFile(blocker, nil, 0o644); err != nil {
		t.Fatal(err)
	}
	// Version 0 with the name emb already taken by an index: creating the schema fails.
	clash := filepath.Join(dir, "clash.sqlite")
	if _, err := rawDB(t, clash).Exec("CREATE TABLE x (a); CREATE INDEX emb ON x (a)"); err != nil {
		t.Fatal(err)
	}
	cancelled, cancel := context.WithCancel(ctx)
	cancel()

	cases := []struct {
		name  string
		ctx   context.Context
		path  string
		check func(error) bool
	}{
		{"not a database", ctx, garbage, func(err error) bool { return err != nil }},
		{"newer schema", ctx, newer, func(err error) bool { return errors.Is(err, cache.ErrNewerSchema) }},
		{"directory is a file", ctx, filepath.Join(blocker, "c.sqlite"), func(err error) bool { return err != nil }},
		{"schema name clash", ctx, clash, func(err error) bool { return err != nil && strings.Contains(err.Error(), "create schema") }},
		{"cancelled ctx", cancelled, filepath.Join(dir, "new.sqlite"), func(err error) bool { return errors.Is(err, context.Canceled) }},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			c, err := cache.Open(tc.ctx, tc.path)
			if err == nil {
				c.Close()
			}
			if !tc.check(err) {
				t.Fatalf("Open error = %v", err)
			}
		})
	}
	// A newer file is left as it was.
	var v int
	if err := rawDB(t, newer).QueryRow("PRAGMA user_version").Scan(&v); err != nil || v != cache.SchemaVersion+1 {
		t.Fatalf("user_version after failed Open = %d, %v", v, err)
	}
}

func TestClosedCacheErrors(t *testing.T) {
	ctx := context.Background()
	c, err := cache.Open(ctx, filepath.Join(t.TempDir(), "c.sqlite"))
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	if err := c.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
	if _, err := c.Get(ctx, "m", dim, []string{"x"}); err == nil {
		t.Error("Get on a closed cache succeeded")
	}
	if err := c.Put(ctx, "m", dim, []string{"x"}, [][]float32{vec(1)}); err == nil {
		t.Error("Put on a closed cache succeeded")
	}
	if _, err := c.Stats(ctx); err == nil {
		t.Error("Stats on a closed cache succeeded")
	}
	if err := c.Clear(ctx); err == nil {
		t.Error("Clear on a closed cache succeeded")
	}
}

func TestMissingTableErrors(t *testing.T) {
	ctx := context.Background()
	c := openTemp(t)
	if _, err := rawDB(t, c.Path()).Exec("DROP TABLE emb"); err != nil {
		t.Fatal(err)
	}
	if _, err := c.Get(ctx, "m", dim, []string{"x"}); err == nil {
		t.Error("Get without the table succeeded")
	}
	if err := c.Put(ctx, "m", dim, []string{"x"}, [][]float32{vec(1)}); err == nil {
		t.Error("Put without the table succeeded")
	}
}

func TestClear(t *testing.T) {
	ctx := context.Background()
	c := openTemp(t)
	if err := c.Put(ctx, "m", dim, []string{"x", "y"}, [][]float32{vec(1), vec(2)}); err != nil {
		t.Fatalf("Put: %v", err)
	}
	if err := c.Clear(ctx); err != nil {
		t.Fatalf("Clear: %v", err)
	}
	got, err := c.Get(ctx, "m", dim, []string{"x", "y"})
	if err != nil || got[0] != nil || got[1] != nil {
		t.Fatalf("Get after Clear = %v, %v; want two misses", got, err)
	}
}

// Several processes share one cache file. Each "process" here is its own Open (its own *sql.DB
// and connection pool) on a brand-new file, so the first opens race on WAL and the schema; then
// all of them, and several goroutines per handle, write and read overlapping keys at once.
func TestConcurrentHandlesAndGoroutines(t *testing.T) {
	ctx := context.Background()
	path := filepath.Join(t.TempDir(), "shared.sqlite")
	const handles, goroutines, keys = 4, 4, 20

	caches := make([]*cache.Cache, handles)
	errc := make(chan error, handles*goroutines)
	var wg sync.WaitGroup
	for h := range handles {
		wg.Add(1)
		go func() {
			defer wg.Done()
			c, err := cache.Open(ctx, path)
			if err != nil {
				errc <- fmt.Errorf("open handle %d: %w", h, err)
				return
			}
			caches[h] = c
		}()
	}
	wg.Wait()
	for _, c := range caches {
		if c != nil {
			t.Cleanup(func() { c.Close() })
		}
	}
	select {
	case err := <-errc:
		t.Fatal(err)
	default:
	}

	texts := make([]string, keys)
	vecs := make([][]float32, keys)
	for i := range texts {
		texts[i] = fmt.Sprintf("shared %d", i)
		vecs[i] = vec(i)
	}
	for h, c := range caches {
		for g := range goroutines {
			wg.Add(1)
			go func() {
				defer wg.Done()
				// Rotate the batch so writers overlap on different keys first.
				off := (h*goroutines + g) % keys
				ts := append(append([]string{}, texts[off:]...), texts[:off]...)
				vs := append(append([][]float32{}, vecs[off:]...), vecs[:off]...)
				for i := 0; i < len(ts); i += 5 {
					if err := c.Put(ctx, "m", dim, ts[i:i+5], vs[i:i+5]); err != nil {
						errc <- fmt.Errorf("handle %d goroutine %d put: %w", h, g, err)
						return
					}
					got, err := c.Get(ctx, "m", dim, ts[:i+5])
					if err != nil {
						errc <- fmt.Errorf("handle %d goroutine %d get: %w", h, g, err)
						return
					}
					for j := range got {
						if !equalVec(got[j], vs[j]) {
							errc <- fmt.Errorf("handle %d goroutine %d: %q = %v, want %v", h, g, ts[j], got[j], vs[j])
							return
						}
					}
				}
			}()
		}
	}
	wg.Wait()
	close(errc)
	for err := range errc {
		t.Error(err)
	}
	stats, err := caches[0].Stats(ctx)
	if err != nil || len(stats) != 1 || stats[0].Entries != keys {
		t.Fatalf("Stats = %v, %v; want one model with %d entries", stats, err, keys)
	}
}

func TestPutFailsWhenInsertIsRefused(t *testing.T) {
	c := openTemp(t)
	if _, err := rawDB(t, c.Path()).Exec("CREATE TRIGGER no_insert BEFORE INSERT ON emb BEGIN SELECT RAISE(FAIL, 'refused'); END"); err != nil {
		t.Fatal(err)
	}
	err := c.Put(context.Background(), "m", dim, []string{"x"}, [][]float32{vec(1)})
	if err == nil || !strings.Contains(err.Error(), "refused") {
		t.Fatalf("Put error = %v, want the trigger's refusal", err)
	}
}

func TestStatsFailsOnBadRow(t *testing.T) {
	c := openTemp(t)
	if _, err := rawDB(t, c.Path()).Exec("INSERT INTO emb VALUES ('m', 'not a number', x'00', x'00')"); err != nil {
		t.Fatal(err)
	}
	if _, err := c.Stats(context.Background()); err == nil {
		t.Fatal("Stats scanned a text dim into an int")
	}
}
