package reindex

import (
	"context"
	"os"
	"path/filepath"
	"testing"

	"github.com/nickstrad/kb/internal/embed/fake"
	"github.com/nickstrad/kb/internal/entry"
	"github.com/nickstrad/kb/internal/store"
)

const lateEntry = "---\ntitle: Late\nsummary: added while a reindex --all was running\n---\n\n## A\n\nlate body\n"

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
			sum, err := All(context.Background(), Options{Root: root, Sources: dataSources, Store: s, Embedder: base})
			if (err != nil) != tc.wantErr {
				t.Fatalf("All err = %v, wantErr %v", err, tc.wantErr)
			}
			if sum.Scanned != 0 || sum.Removed != tc.wantRemoved {
				t.Errorf("scanned %d, removed %d; want 0, %d", sum.Scanned, sum.Removed, tc.wantRemoved)
			}
		})
	}
}
