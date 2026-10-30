package reindex

import (
	"bytes"
	"context"
	"github.com/nickstrad/kb/internal/store/sqlite"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	"github.com/nickstrad/kb/internal/embed"
	"github.com/nickstrad/kb/internal/embed/fake"
	"github.com/nickstrad/kb/internal/entry"
	"github.com/nickstrad/kb/internal/store"
)

const lateEntry = "---\ntitle: Late\nsummary: added while a reindex --all was running\n---\n\n## A\n\nlate body\n"

// concurrentAddEmbedder plays a second kb process: on its first Embed call, which All makes after
// its Discover, it does what `kb add` does (file on disk first, then the row, through its own
// Store handle) for data/late.md. That is the Q22 interleaving, made deterministic.
type concurrentAddEmbedder struct {
	embed.Embedder
	t      *testing.T
	root   string
	dbPath string
	once   sync.Once
}

func (c *concurrentAddEmbedder) Embed(ctx context.Context, texts []string) ([][]float32, error) {
	c.once.Do(func() {
		path := filepath.Join(c.root, "data", "late.md")
		if err := os.WriteFile(path, []byte(lateEntry), 0o644); err != nil {
			c.t.Errorf("write late entry: %v", err)
			return
		}
		other, err := sqlite.Open(context.Background(), c.dbPath, sqlite.Options{})
		if err != nil {
			c.t.Errorf("second store handle: %v", err)
			return
		}
		defer other.Close()
		e, err := entry.Resolve(c.root, "late.md")
		if err != nil {
			c.t.Errorf("resolve late entry: %v", err)
			return
		}
		if _, err := One(ctx, Options{Root: c.root, Store: other, Embedder: c.Embedder}, e); err != nil {
			c.t.Errorf("concurrent add: %v", err)
		}
	})
	return c.Embedder.Embed(ctx, texts)
}

// TestAllKeepsEntryAddedDuringRun is the Q22 regression: an entry a concurrent `kb add` writes
// after All's Discover is listed by ListEntries but was not discovered; All must not prune it.
// A row whose file really is gone is still pruned in the same run.
func TestAllKeepsEntryAddedDuringRun(t *testing.T) {
	root := newTestRepo(t)
	base := fake.New("fake-embed", testDim)
	dbPath := filepath.Join(t.TempDir(), "kb.sqlite")
	s, err := sqlite.Open(context.Background(), dbPath, sqlite.Options{})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { s.Close() })
	if err := s.EnsureEmbedMeta(context.Background(), store.EmbedMeta{Model: base.Model(), Dim: base.Dim()}); err != nil {
		t.Fatal(err)
	}
	// A stale row whose file is gone: the prune must still remove it.
	if _, err := s.ReplaceEntry(context.Background(),
		store.EntryInput{Path: "data/gone.md", Kind: entry.KindFile, Title: "Gone", Summary: "s"}, nil); err != nil {
		t.Fatal(err)
	}

	emb := &concurrentAddEmbedder{Embedder: base, t: t, root: root, dbPath: dbPath}
	var stdout bytes.Buffer
	sum, err := All(context.Background(), Options{Root: root, Store: s, Embedder: emb, Stdout: &stdout})
	if err != nil {
		t.Fatalf("All: %v", err)
	}
	if row, err := s.Entry(context.Background(), "data/late.md"); err != nil || row == nil {
		t.Fatalf("data/late.md after All = %v, %v; the concurrently added entry was pruned", row, err)
	}
	if row, err := s.Entry(context.Background(), "data/gone.md"); err != nil || row != nil {
		t.Errorf("data/gone.md after All = %v, %v; want it pruned", row, err)
	}
	if sum.Removed != 1 || !strings.Contains(stdout.String(), "removed data/gone.md (no longer on disk)") ||
		strings.Contains(stdout.String(), "removed data/late.md") {
		t.Errorf("Removed = %d, stdout:\n%s\nwant exactly data/gone.md removed", sum.Removed, stdout.String())
	}
}

// TestAllMissingDataDir is the Q23 decision: a root with no data/ is an empty corpus (exit 0
// upstream), and its stale rows are pruned; a data/ that exists but cannot be read is an error.
func TestAllMissingDataDir(t *testing.T) {
	base := fake.New("fake-embed", testDim)
	for _, tc := range []struct {
		name        string
		setup       func(root string) error
		wantErr     bool
		wantRemoved int
	}{
		{"no data dir: empty corpus", func(string) error { return nil }, false, 1},
		{"data is a file: error", func(root string) error {
			return os.WriteFile(filepath.Join(root, "data"), []byte("x"), 0o644)
		}, true, 0},
	} {
		t.Run(tc.name, func(t *testing.T) {
			root := t.TempDir()
			if err := tc.setup(root); err != nil {
				t.Fatal(err)
			}
			s := newTestStore(t, base)
			if _, err := s.ReplaceEntry(context.Background(),
				store.EntryInput{Path: "data/old.md", Kind: entry.KindFile, Title: "Old", Summary: "s"}, nil); err != nil {
				t.Fatal(err)
			}
			sum, err := All(context.Background(), Options{Root: root, Store: s, Embedder: base})
			if (err != nil) != tc.wantErr {
				t.Fatalf("All err = %v, wantErr %v", err, tc.wantErr)
			}
			if sum.Scanned != 0 || sum.Removed != tc.wantRemoved {
				t.Errorf("scanned %d, removed %d; want 0, %d", sum.Scanned, sum.Removed, tc.wantRemoved)
			}
		})
	}
}
