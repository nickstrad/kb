package storetest

import (
	"fmt"
	"maps"
	"slices"
	"testing"

	"github.com/nickstrad/kb/internal/store"
)

// ModelSeeds are the fixed op sequences Run replays; a backend's fuzz target adds them as seeds.
var ModelSeeds = [][]byte{
	{0, 3, 4, 2, 3, 0},              // replace a with 3 chunks, b with 2, delete a
	{0, 2, 0, 1, 0, 0, 0, 3},        // replace a three times (2, 1, 0 chunks), then 3
	{1, 2, 5, 1, 9, 0, 13, 0, 3, 0}, // four entries, then delete a missing one
	{0, 2, 0, 2},                    // identical replace twice: same hashes, new rows
	{},
}

// RunModel applies ops to st and to a map model (path → chunk text hashes). Each op is two bytes:
// op%4 == 3 deletes entry op/4%4, anything else replaces that entry with n%4 chunks whose text
// depends on n. After every op the entries, their chunk hashes and (for dim > 0) their stored
// vectors read back from st must equal the model, and each entry's word (main FTS index) and
// identifier (exact index) find exactly that entry's chunks: no index keeps a stale row or misses
// a live one. The backend asserts the row-set invariant itself.
// st must be fresh or reset; RunModel resets it first and leaves embed_meta recorded for dim.
func RunModel(t testing.TB, st store.Store, dim int, ops []byte) {
	t.Helper()
	if err := st.ResetIndex(ctx); err != nil {
		t.Fatalf("ResetIndex: %v", err)
	}
	if err := st.EnsureEmbedMeta(ctx, Meta(dim)); err != nil {
		t.Fatalf("EnsureEmbedMeta: %v", err)
	}
	model := map[string][]string{}
	for i := 0; i+1 < len(ops); i += 2 {
		op, n := ops[i], ops[i+1]
		path := fmt.Sprintf("e%d.md", op/4%4)
		if op%4 == 3 {
			if err := st.DeleteEntry(ctx, path); err != nil {
				t.Fatalf("op %d: DeleteEntry %s: %v", i/2, path, err)
			}
			delete(model, path)
		} else {
			texts := make([]string, int(n%4))
			for j := range texts {
				texts[j] = fmt.Sprintf("modelword%d v%d c%d %s", op/4%4, n, j, modelIdent(int(op/4%4)))
			}
			cs := chunks(dim, texts...)
			if _, err := st.ReplaceEntry(ctx, entryIn(path), cs); err != nil {
				t.Fatalf("op %d: ReplaceEntry %s: %v", i/2, path, err)
			}
			hashes := make([]string, len(cs))
			for j, c := range cs {
				hashes[j] = c.TextHash
			}
			model[path] = hashes
		}
		checkModel(t, st, dim, model, i/2)
	}
}

// modelIdent is entry i's identifier: one token in the exact index, three words in the main one.
func modelIdent(i int) string { return fmt.Sprintf("model_ident.%d", i) }

func checkModel(t testing.TB, st store.Store, dim int, model map[string][]string, step int) {
	t.Helper()
	es, err := st.Entries(ctx)
	if err != nil {
		t.Fatalf("step %d: Entries: %v", step, err)
	}
	var paths []string
	for _, e := range es {
		paths = append(paths, e.Path)
	}
	want := slices.Sorted(maps.Keys(model))
	if !slices.Equal(paths, want) {
		t.Fatalf("step %d: entries %v, model %v", step, paths, want)
	}
	for _, e := range es {
		cs, err := st.Chunks(ctx, e.ID)
		if err != nil {
			t.Fatalf("step %d: Chunks: %v", step, err)
		}
		got := make([]string, len(cs))
		for i, c := range cs {
			got[i] = c.TextHash
		}
		if !slices.Equal(got, model[e.Path]) {
			t.Fatalf("step %d: %s chunks %v, model %v", step, e.Path, got, model[e.Path])
		}
		embs, err := st.Embeddings(ctx, e.ID)
		if err != nil {
			t.Fatalf("step %d: Embeddings: %v", step, err)
		}
		wantVecs := 0
		if dim > 0 {
			wantVecs = len(model[e.Path])
		}
		if len(embs) != wantVecs {
			t.Fatalf("step %d: %s has %d vectors, want %d", step, e.Path, len(embs), wantVecs)
		}
	}
	// Every entry slot, live or not: a deleted or emptied entry must leave no index rows.
	for i := 0; i < 4; i++ {
		path := fmt.Sprintf("e%d.md", i)
		var want []int64
		for _, e := range es {
			if e.Path == path {
				want = chunkIDs(t, st, e.ID)
			}
		}
		if got := ftsIDs(t, st, store.OpAnd, "", fmt.Sprintf("modelword%d", i)); !slices.Equal(got, want) {
			t.Fatalf("step %d: FTS modelword%d = %v, want %s's chunks %v", step, i, got, path, want)
		}
		if got := exactIDs(t, st, modelIdent(i)); !slices.Equal(got, want) {
			t.Fatalf("step %d: exact %s = %v, want %s's chunks %v", step, modelIdent(i), got, path, want)
		}
	}
}
