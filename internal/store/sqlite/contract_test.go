package sqlite

import (
	"context"
	"path/filepath"
	"testing"

	"github.com/nickstrad/kb/internal/store"
	"github.com/nickstrad/kb/internal/store/storetest"
)

var _ store.Store = (*Store)(nil)

// openContract is the storetest.Factory for this backend.
func openContract(t testing.TB, dim int) store.Store {
	t.Helper()
	ctx := context.Background()
	st, err := Open(ctx, filepath.Join(t.TempDir(), "kb.sqlite"), Options{})
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	t.Cleanup(func() { st.Close() })
	if err := st.EnsureEmbedMeta(ctx, storetest.Meta(dim)); err != nil {
		t.Fatalf("EnsureEmbedMeta: %v", err)
	}
	return st
}

func TestContract(t *testing.T) { storetest.Run(t, openContract) }

// FuzzStoreModel is the stateful model test under the fuzzer: one store per worker, reset by
// RunModel on each input (TESTING.md §5.3).
//
// Run: go test -tags fts5 ./internal/store/sqlite -run '^$' -fuzz '^FuzzStoreModel$' -fuzztime 60s
func FuzzStoreModel(f *testing.F) {
	for _, seed := range storetest.ModelSeeds {
		f.Add(seed, false)
		f.Add(seed, true)
	}
	stores := map[bool]store.Store{false: openContract(f, 0), true: openContract(f, 8)}
	f.Fuzz(func(t *testing.T, ops []byte, vectors bool) {
		if len(ops) > 64 {
			ops = ops[:64]
		}
		dim := 0
		if vectors {
			dim = 8
		}
		storetest.RunModel(t, stores[vectors], dim, ops)
	})
}
