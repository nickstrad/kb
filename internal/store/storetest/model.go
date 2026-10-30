package storetest

import (
	"fmt"
	"maps"
	"slices"
	"strings"
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
	// a links to b (unresolved), b arrives (resolved), b is deleted (dangling), a is replaced
	// keeping its edge without a resolve, b comes back and a resolve picks it up again.
	{0, 0x85, 4, 0x01, 0, 0x05, 7, 0, 0, 0x85, 4, 0x81, 0, 0x05},
	// edges to every slot, with tags in three cases, then deletes without a resolve.
	{0, 0x31, 4, 0x26, 8, 0x1b, 12, 0x0c, 3, 0x80, 11, 0x80, 1, 0x10},
}

// RunModel applies ops to st and to a model (path → chunk text hashes, tag, edges). Each op is two
// bytes: op%4 == 3 deletes entry e<op/4%4>, anything else replaces it with n%4 chunks whose text
// depends on n, the tag "Tag<n/4%3>" (in upper case when n&64), a path edge to e<n/4%4>.md and a
// wikilink to e<n/16%4>. After every op but those with n&128, ResolveEdges runs.
//
// After every op the entries, their chunk hashes and (for dim > 0) their stored vectors read back
// from st must equal the model, and each entry's word (main FTS index) and identifier (exact
// index) find exactly that entry's chunks: no index keeps a stale row or misses a live one. Each
// tag, in lower case, filters to exactly the chunks of the entries carrying it, and Edges equals
// the model's edges, whose targets follow the contract: a replace keeps the target of an unchanged
// edge, a delete makes edges to the entry dangle, ResolveEdges resolves every edge whose target is
// live. The backend asserts the row-set invariant itself.
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
	links := map[string]modelLinks{}
	for i := 0; i+1 < len(ops); i += 2 {
		op, n := ops[i], ops[i+1]
		path := fmt.Sprintf("e%d.md", op/4%4)
		if op%4 == 3 {
			if err := st.DeleteEntry(ctx, path); err != nil {
				t.Fatalf("op %d: DeleteEntry %s: %v", i/2, path, err)
			}
			delete(model, path)
			delete(links, path)
			for _, l := range links {
				for k, target := range l.edges {
					if target == path {
						l.edges[k] = ""
					}
				}
			}
		} else {
			texts := make([]string, int(n%4))
			for j := range texts {
				texts[j] = fmt.Sprintf("modelword%d v%d c%d %s", op/4%4, n, j, modelIdent(int(op/4%4)))
			}
			cs := chunks(dim, texts...)
			tag := fmt.Sprintf("Tag%d", n/4%3)
			if n&64 != 0 {
				tag = strings.ToUpper(tag)
			}
			in := entryIn(path, tag)
			in.Edges = []store.EdgeInput{
				{DstPath: fmt.Sprintf("e%d.md", n/4%4), Rel: "links", Kind: store.EdgePath},
				{DstPath: fmt.Sprintf("e%d", n/16%4), Rel: "related", Kind: store.EdgeWikilink},
			}
			if _, err := st.ReplaceEntry(ctx, in, cs); err != nil {
				t.Fatalf("op %d: ReplaceEntry %s: %v", i/2, path, err)
			}
			next := modelLinks{tag: strings.ToLower(tag), edges: map[store.EdgeInput]string{}}
			for _, g := range in.Edges {
				next.edges[g] = links[path].edges[g] // an unchanged edge keeps its target
			}
			links[path] = next
			hashes := make([]string, len(cs))
			for j, c := range cs {
				hashes[j] = c.TextHash
			}
			model[path] = hashes
		}
		if n&128 == 0 {
			if err := st.ResolveEdges(ctx); err != nil {
				t.Fatalf("op %d: ResolveEdges: %v", i/2, err)
			}
			for _, l := range links {
				for g := range l.edges {
					target := g.DstPath
					if g.Kind == store.EdgeWikilink {
						target += ".md" // e<k> names e<k>.md by path (+".md") and by basename
					}
					if _, live := model[target]; !live {
						target = ""
					}
					l.edges[g] = target
				}
			}
		}
		checkModel(t, st, dim, model, i/2)
		checkLinks(t, st, model, links, i/2)
	}
}

// modelLinks is the model of one entry's tag (lower case) and edges (edge → resolved target path,
// "" while dangling).
type modelLinks struct {
	tag   string
	edges map[store.EdgeInput]string
}

// checkLinks compares the tag filter and Edges with the model.
func checkLinks(t testing.TB, st store.Store, model map[string][]string, links map[string]modelLinks, step int) {
	t.Helper()
	es, err := st.Entries(ctx)
	if err != nil {
		t.Fatalf("step %d: Entries: %v", step, err)
	}
	words := []string{"modelword0", "modelword1", "modelword2", "modelword3"}
	for k := 0; k < 3; k++ {
		tag := fmt.Sprintf("tag%d", k)
		var want []int64
		for _, e := range es {
			if links[e.Path].tag == tag {
				want = append(want, chunkIDs(t, st, e.ID)...)
			}
		}
		slices.Sort(want)
		if got := ftsIDs(t, st, store.OpOr, tag, words...); !slices.Equal(got, want) {
			t.Fatalf("step %d: FTS tag %s = %v, want %v", step, tag, got, want)
		}
	}
	var want []string
	for _, src := range slices.Sorted(maps.Keys(links)) {
		var lines []store.EdgeRow
		for g, target := range links[src].edges {
			lines = append(lines, store.EdgeRow{SrcPath: src, DstPath: g.DstPath, Rel: g.Rel, Kind: g.Kind, DstEntryPath: target})
		}
		slices.SortFunc(lines, func(a, b store.EdgeRow) int {
			return strings.Compare(a.Rel+"\x00"+a.Kind+"\x00"+a.DstPath, b.Rel+"\x00"+b.Kind+"\x00"+b.DstPath)
		})
		for _, l := range lines {
			want = append(want, fmt.Sprintf("%s -%s/%s-> %s => %s", l.SrcPath, l.Rel, l.Kind, l.DstPath, l.DstEntryPath))
		}
	}
	if got := edgeStrings(t, st); !slices.Equal(got, want) {
		t.Fatalf("step %d: Edges =\n%q\nmodel\n%q", step, got, want)
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
