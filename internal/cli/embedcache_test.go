package cli

import (
	"context"
	"database/sql"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/spf13/cobra"

	"github.com/nickstrad/kb/internal/config"
	"github.com/nickstrad/kb/internal/embed"
	"github.com/nickstrad/kb/internal/embed/cache"
	"github.com/nickstrad/kb/internal/embed/fake"
	"github.com/nickstrad/kb/internal/entry"
)

// countingEmbedder counts the texts and calls that reach the model.
type countingEmbedder struct {
	*fake.Embedder
	calls, texts *atomic.Int64
}

func (c countingEmbedder) Embed(ctx context.Context, texts []string) ([][]float32, error) {
	c.calls.Add(1)
	c.texts.Add(int64(len(texts)))
	return c.Embedder.Embed(ctx, texts)
}

// useEmbedCache turns the shared cache on at path for one test and counts model calls.
func useEmbedCache(t *testing.T, path string) (calls, texts *atomic.Int64) {
	t.Helper()
	origE, origP := depsFor(t).embedder, depsFor(t).embedCachePath
	t.Cleanup(func() { depsFor(t).embedder, depsFor(t).embedCachePath = origE, origP })
	calls, texts = new(atomic.Int64), new(atomic.Int64)
	depsFor(t).embedder = func(config.Embed) (embed.Embedder, error) {
		return countingEmbedder{fake.New("fake-cache-test", 16), calls, texts}, nil
	}
	depsFor(t).embedCachePath = func() (string, error) { return path, nil }
	return calls, texts
}

// seedDocs writes n valid entries under root's data directory.
func seedDocs(t *testing.T, root string, n int) {
	t.Helper()
	for i := range n {
		writeFile(t, filepath.Join(root, entry.DataDir, fmt.Sprintf("doc%d.md", i)),
			validFrontMatter(fmt.Sprintf("Doc %d", i), fmt.Sprintf("summary %d", i)))
	}
}

var embedCallsRe = regexp.MustCompile(`embed calls: (\d+)`)

func reportedEmbedCalls(t *testing.T, stdout string) string {
	t.Helper()
	m := embedCallsRe.FindStringSubmatch(stdout)
	if m == nil {
		t.Fatalf("no 'embed calls: N' in %q", stdout)
	}
	return m[1]
}

// TestEmbedCacheSharedAcrossStores is the 3.3 acceptance: a second store with the same documents
// reindexes with zero model calls, and says so.
func TestEmbedCacheSharedAcrossStores(t *testing.T) {
	t.Parallel()
	calls, texts := useEmbedCache(t, filepath.Join(t.TempDir(), "cache", "embed-cache.sqlite"))

	first := newTempRoot(t)
	seedDocs(t, first, 3)
	out, errOut, code := run(t, "reindex", "--all")
	if code != 0 || errOut != "" {
		t.Fatalf("first reindex: %d %q", code, errOut)
	}
	if got := reportedEmbedCalls(t, out); got == "0" || calls.Load() == 0 {
		t.Fatalf("first reindex reported %s calls, model saw %d: it should have embedded", got, calls.Load())
	}
	embeddedBefore, callsBefore := texts.Load(), calls.Load()

	second := newTempRoot(t)
	seedDocs(t, second, 3)
	out, errOut, code = run(t, "reindex", "--all")
	if code != 0 || errOut != "" {
		t.Fatalf("second reindex: %d %q", code, errOut)
	}
	if got := reportedEmbedCalls(t, out); got != "0" {
		t.Errorf("second store reported embed calls: %s, want 0 (%q)", got, out)
	}
	if calls.Load() != callsBefore || texts.Load() != embeddedBefore {
		t.Errorf("model calls %d -> %d, texts %d -> %d: the second store must not reach the model",
			callsBefore, calls.Load(), embeddedBefore, texts.Load())
	}
	// The second store's index is complete: searchable by vector.
	if _, errOut, code := run(t, "search", "Doc", "--mode", "vec"); code != 0 {
		t.Errorf("vec search in second store: %d %q", code, errOut)
	}
}

// TestEmbedCacheDegrades: a cache that cannot be used is a warning, never a failure; the command
// embeds without it and exits as it would have.
func TestEmbedCacheDegrades(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name string
		make func(t *testing.T) string // returns the cache path
	}{
		{"garbage file", func(t *testing.T) string {
			p := filepath.Join(t.TempDir(), "c.sqlite")
			writeFile(t, p, strings.Repeat("not a database ", 512))
			return p
		}},
		{"newer schema", func(t *testing.T) string {
			p := filepath.Join(t.TempDir(), "c.sqlite")
			db, err := sql.Open("sqlite3", p)
			if err != nil {
				t.Fatal(err)
			}
			defer db.Close()
			if _, err := db.Exec(fmt.Sprintf("PRAGMA user_version = %d", cache.SchemaVersion+1)); err != nil {
				t.Fatal(err)
			}
			return p
		}},
		{"directory is a file", func(t *testing.T) string {
			blocker := filepath.Join(t.TempDir(), "file")
			writeFile(t, blocker, "x")
			return filepath.Join(blocker, "c.sqlite")
		}},
		{"no user cache directory", func(t *testing.T) string { return "" }},
	} {
		t.Run(tc.name, func(t *testing.T) {
			path := tc.make(t)
			calls, _ := useEmbedCache(t, path)
			if path == "" {
				depsFor(t).embedCachePath = func() (string, error) { return embedCacheFile("") }
			}
			before, _ := os.ReadFile(path)
			root := newTempRoot(t)
			seedDocs(t, root, 2)
			out, errOut, code := run(t, "reindex", "--all")
			if code != 0 {
				t.Fatalf("exit %d, want 0; stderr %q", code, errOut)
			}
			if !strings.Contains(errOut, "warning: embedding cache not used, embedding without it") ||
				!strings.Contains(errOut, "kb model cache --clear") {
				t.Errorf("stderr lacks the cache warning with the way out: %q", errOut)
			}
			if calls.Load() == 0 || reportedEmbedCalls(t, out) == "0" {
				t.Errorf("nothing was embedded without the cache: calls %d, %q", calls.Load(), out)
			}
			switch tc.name {
			case "garbage file":
				if after, _ := os.ReadFile(path); string(after) != string(before) {
					t.Errorf("a garbage cache file was modified")
				}
			case "newer schema":
				// Open may switch the journal mode (a header byte) before it reads the version;
				// the schema and the version stay.
				db, err := sql.Open("sqlite3", path)
				if err != nil {
					t.Fatal(err)
				}
				defer db.Close()
				var v int
				if err := db.QueryRow("PRAGMA user_version").Scan(&v); err != nil || v != cache.SchemaVersion+1 {
					t.Errorf("newer cache user_version = %d, %v; want it untouched", v, err)
				}
			}
		})
	}
}

// TestEmbedCacheGoesOffMidRun: a cache that fails after it opened (its table is gone) is
// reported once after the command, which still succeeds.
func TestEmbedCacheGoesOffMidRun(t *testing.T) {
	t.Parallel()
	path := filepath.Join(t.TempDir(), "c.sqlite")
	c, err := cache.Open(context.Background(), path)
	if err != nil {
		t.Fatal(err)
	}
	if err := c.Close(); err != nil {
		t.Fatal(err)
	}
	db, err := sql.Open("sqlite3", path)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec("DROP TABLE emb"); err != nil {
		t.Fatal(err)
	}
	db.Close()
	useEmbedCache(t, path)
	root := newTempRoot(t)
	seedDocs(t, root, 2)
	_, errOut, code := run(t, "reindex", "--all")
	if code != 0 {
		t.Fatalf("exit %d, stderr %q", code, errOut)
	}
	if n := strings.Count(errOut, "embedding cache disabled"); n != 1 {
		t.Errorf("want one 'embedding cache disabled' warning, got %d in %q", n, errOut)
	}
}

func TestModelCacheCommand(t *testing.T) {
	t.Parallel()
	path := filepath.Join(t.TempDir(), "kb", "embed-cache.sqlite")
	useEmbedCache(t, path)
	chdir(t, realTempDir(t))

	t.Run("missing cache", func(t *testing.T) {
		for _, args := range [][]string{{"model", "cache"}, {"model", "cache", "--stats"}, {"model", "cache", "--clear"}} {
			out, errOut, code := run(t, args...)
			if code != 0 || errOut != "" || !strings.Contains(out, "not created yet") {
				t.Errorf("%v: %d %q %q", args, code, out, errOut)
			}
		}
		if _, err := os.Stat(path); err == nil {
			t.Error("a stats or clear created the cache file")
		}
	})

	root := newTempRoot(t)
	seedDocs(t, root, 2)
	if _, errOut, code := run(t, "reindex", "--all"); code != 0 {
		t.Fatalf("reindex: %d %q", code, errOut)
	}

	t.Run("stats lists models", func(t *testing.T) {
		for _, args := range [][]string{{"model", "cache"}, {"model", "cache", "--stats"}} {
			out, errOut, code := run(t, args...)
			if code != 0 || errOut != "" || !strings.Contains(out, path) || !strings.Contains(out, "fake-cache-test") ||
				!strings.Contains(out, "16 dims") || strings.Contains(out, ": 0 entries") {
				t.Errorf("%v: %d %q %q", args, code, out, errOut)
			}
		}
	})
	t.Run("both flags", func(t *testing.T) {
		if _, errOut, code := run(t, "model", "cache", "--stats", "--clear"); code != ExitUsage || !strings.Contains(errOut, "cannot be used together") {
			t.Errorf("%d %q", code, errOut)
		}
	})
	t.Run("clear empties", func(t *testing.T) {
		out, errOut, code := run(t, "model", "cache", "--clear")
		if code != 0 || errOut != "" || !strings.Contains(out, "cleared") {
			t.Fatalf("%d %q %q", code, out, errOut)
		}
		if out, _, _ := run(t, "model", "cache", "--stats"); !strings.Contains(out, ": 0 entries") {
			t.Errorf("stats after clear: %q", out)
		}
	})
	t.Run("clear removes an unreadable file, stats refuses it", func(t *testing.T) {
		writeFile(t, path, strings.Repeat("not a database ", 512))
		if _, errOut, code := run(t, "model", "cache", "--stats"); code != ExitUsage && code != 1 || !strings.Contains(errOut, "kb model cache --clear") {
			t.Errorf("stats of a garbage cache: %d %q", code, errOut)
		}
		out, errOut, code := run(t, "model", "cache", "--clear")
		if code != 0 || errOut != "" || !strings.Contains(out, "removed the unreadable") {
			t.Fatalf("%d %q %q", code, out, errOut)
		}
		if _, err := os.Stat(path); err == nil {
			t.Error("the garbage file is still there")
		}
	})
	t.Run("no cache configured", func(t *testing.T) {
		depsFor(t).embedCachePath = nil
		if _, errOut, code := run(t, "model", "cache"); code != ExitUsage || !strings.Contains(errOut, "not available") {
			t.Errorf("%d %q", code, errOut)
		}
	})
}

// TestWithEmbedCacheWrapping is the decision table of which embedders get the cache.
func TestWithEmbedCacheWrapping(t *testing.T) {
	t.Parallel()
	path := filepath.Join(t.TempDir(), "c.sqlite")
	d := productionDeps().forRun()
	d.embedCachePath = func() (string, error) { return path, nil }
	t.Cleanup(func() { d.finishEmbedCaches(os.Stderr) })
	cmdNamed := func(n string) *cobra.Command { return &cobra.Command{Use: n} }
	plain := fake.New("m", 4)
	pingable := pingEmbedder{plain, nil}

	for _, tc := range []struct {
		name       string
		cmd        *cobra.Command
		in         embed.Embedder
		wantCached bool
		wantPing   bool
	}{
		{"plain embedder", cmdNamed("reindex"), plain, true, false},
		{"pinger keeps Ping", cmdNamed("reindex"), pingable, true, true},
		{"nil command", nil, plain, true, false},
		{"none is never cached", cmdNamed("reindex"), embed.None{}, false, false},
		{"doctor measures the real embedder", cmdNamed("doctor"), pingable, false, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got := d.withEmbedCache(tc.cmd, tc.in)
			_, cached := got.(cacheStatser)
			_, ping := got.(pinger)
			if cached != tc.wantCached || ping != tc.wantPing {
				t.Errorf("cached=%v ping=%v, want cached=%v ping=%v (%T)", cached, ping, tc.wantCached, tc.wantPing, got)
			}
		})
	}
}

func TestModelCallsPrefersTheCachedCount(t *testing.T) {
	t.Parallel()
	if got := modelCalls(fake.New("m", 4), 7); got != 7 {
		t.Errorf("uncached: %d, want the caller's 7", got)
	}
	c, err := cache.Open(context.Background(), filepath.Join(t.TempDir(), "c.sqlite"))
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	ce := embed.Cached(fake.New("m", 4), c)
	for range 2 {
		if _, err := ce.Embed(context.Background(), []string{"a"}); err != nil {
			t.Fatal(err)
		}
	}
	if got := modelCalls(ce, 99); got != 1 {
		t.Errorf("cached: %d, want 1 model call (second was a hit)", got)
	}
}

func TestEmbedCacheFile(t *testing.T) {
	t.Parallel()
	if got, err := embedCacheFile("/c"); err != nil || got != "/c/kb/embed-cache.sqlite" {
		t.Errorf("embedCacheFile = %q, %v", got, err)
	}
	if _, err := embedCacheFile(""); err == nil {
		t.Error("no cache base must be an error")
	}
}

// TestEmbedCacheConcurrentReindex: several reindexes of the same documents share one cache file
// at once (each with its own deps, as each process has) and all succeed, quietly. Run it with
// -race.
func TestEmbedCacheConcurrentReindex(t *testing.T) {
	t.Parallel()
	_, texts := useEmbedCache(t, filepath.Join(t.TempDir(), "c.sqlite"))
	const n = 4
	roots := make([]string, n)
	for i := range roots {
		roots[i] = newTempRoot(t)
		seedDocs(t, roots[i], 6)
	}
	type result struct {
		code        int
		stdout, err string
	}
	results := make(chan result, n)
	for _, root := range roots {
		d := *depsFor(t)
		go func() {
			var out, errBuf strings.Builder
			code := execute(context.Background(), &d, []string{"--root", root, "reindex", "--all"}, &out, &errBuf)
			results <- result{code, out.String(), errBuf.String()}
		}()
	}
	for range n {
		if r := <-results; r.code != 0 || r.err != "" {
			t.Errorf("reindex: exit %d, stderr %q", r.code, r.err)
		}
	}
	if texts.Load() == 0 {
		t.Error("nothing reached the model")
	}
}
