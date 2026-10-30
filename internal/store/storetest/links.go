package storetest

import (
	"fmt"
	"slices"
	"testing"

	"github.com/nickstrad/kb/internal/errs"
	"github.com/nickstrad/kb/internal/store"
)

// tags: the entry row keeps its tags as written; the filter (FTS and KNN) matches the normalised
// form, so case and surrounding space do not matter, a LIKE metacharacter is a plain character and
// a replace or delete leaves no stale tag behind.
func tags(t *testing.T, st store.Store, dim int) {
	a := mustReplace(t, st, entryIn("a.md", "Docker", " docker", "Go"), chunks(dim, "tagword one"))
	mustReplace(t, st, entryIn("b.md", "docker-compose", "docke_"), chunks(dim, "tagword two"))
	e, err := st.Entry(ctx, "a.md")
	if err != nil || e == nil || !slices.Equal(e.Tags, []string{"Docker", " docker", "Go"}) {
		t.Fatalf("Entry(a.md).Tags = %+v, %v; want them as written", e, err)
	}
	aIDs := chunkIDs(t, st, a)
	for _, tc := range []struct {
		tag  string
		want []int64
	}{
		{"docker", aIDs},
		{"Docker", aIDs},
		{" DOCKER ", aIDs},
		{"go", aIDs},
		{"doc", nil},
		{"%", nil},
		{"docker%", nil},
		{"_ocker", nil},
		{"   ", nil},
	} {
		if got := ftsIDs(t, st, store.OpAnd, tc.tag, "tagword"); !slices.Equal(got, tc.want) {
			t.Errorf("FTS tag %q = %v, want %v", tc.tag, got, tc.want)
		}
		if got := queryIDs(t, st, store.FTSQuery{Terms: []string{"tagword"}, Tag: tc.tag, Limit: 20, Exact: true}); !slices.Equal(got, tc.want) {
			t.Errorf("exact FTS tag %q = %v, want %v", tc.tag, got, tc.want)
		}
	}
	if dim > 0 {
		rows, err := st.KNN(ctx, store.KNNQuery{Vec: vec(dim, 0), K: 5, Tag: "DOCKER", OverFetch: 10})
		if err != nil || len(rows) != 1 || rows[0].ChunkID != aIDs[0] {
			t.Errorf("KNN tag DOCKER = %v, %v; want only a.md's chunk", rows, err)
		}
	}
	mustReplace(t, st, entryIn("a.md", "Rust"), chunks(dim, "tagword one"))
	if got := ftsIDs(t, st, store.OpAnd, "docker", "tagword"); len(got) != 0 {
		t.Errorf("tag docker after a.md dropped it = %v, want nothing", got)
	}
	if got := ftsIDs(t, st, store.OpAnd, "rust", "tagword"); len(got) != 1 {
		t.Errorf("tag rust = %v, want a.md's chunk", got)
	}
	if err := st.DeleteEntry(ctx, "b.md"); err != nil {
		t.Fatal(err)
	}
	mustReplace(t, st, entryIn("c.md"), chunks(dim, "tagword three"))
	if got := ftsIDs(t, st, store.OpAnd, "docke_", "tagword"); len(got) != 0 {
		t.Errorf("tag of a deleted entry still filters to %v", got)
	}
}

// aliases: aliases round-trip on the entry row and are found by full-text search with the
// title's weight, in every chunk of the entry, but not by the identifier index; a replace without
// them takes them out of the index.
func aliases(t *testing.T, st store.Store, dim int) {
	in := entryIn("a.md")
	in.Aliases = []string{"Wombatwiki", "k8s handbook"}
	id := mustReplace(t, st, in, chunks(dim, "body one", "body two"))
	e, err := st.Entry(ctx, "a.md")
	if err != nil || e == nil || !slices.Equal(e.Aliases, in.Aliases) {
		t.Fatalf("Entry(a.md).Aliases = %+v, %v; want %q", e, err, in.Aliases)
	}
	if es, err := st.EntriesByPath(ctx, []string{"a.md"}); err != nil || !slices.Equal(es["a.md"].Aliases, in.Aliases) {
		t.Errorf("EntriesByPath aliases = %+v, %v", es, err)
	}
	all := chunkIDs(t, st, id)
	for _, term := range []string{"wombatwiki", "handbook", "k8s"} {
		if got := ftsIDs(t, st, store.OpAnd, "", term); !slices.Equal(got, all) {
			t.Errorf("FTS %q = %v, want every chunk of a.md %v", term, got, all)
		}
		if got := exactIDs(t, st, term); len(got) != 0 {
			t.Errorf("exact %q = %v, want nothing: aliases are not in the identifier index", term, got)
		}
	}
	for _, tc := range []struct {
		w        store.BM25Weights
		wantZero bool
	}{
		{store.BM25Weights{Title: 1}, false},
		{store.BM25Weights{Text: 1, Heading: 1}, true},
	} {
		res, err := st.FTS(ctx, store.FTSQuery{Terms: []string{"wombatwiki"}, Limit: 1, Weights: &tc.w})
		if err != nil || len(res.Rows) != 1 || (res.Rows[0].Score == 0) != tc.wantZero {
			t.Errorf("alias score with %+v = %+v, %v; want zero=%v (aliases weigh as the title)", tc.w, res.Rows, err, tc.wantZero)
		}
	}
	one := entryIn("a.md")
	one.Aliases = []string{"Okapiwiki"}
	mustReplace(t, st, one, chunks(dim, "body one"))
	if got := ftsIDs(t, st, store.OpAnd, "", "okapiwiki"); len(got) != 1 {
		t.Errorf("a single alias is not found: %v", got)
	}
	mustReplace(t, st, entryIn("a.md"), chunks(dim, "body one"))
	if got := ftsIDs(t, st, store.OpAnd, "", "wombatwiki"); len(got) != 0 {
		t.Errorf("alias still found after it was removed: %v", got)
	}
	if e, _ := st.Entry(ctx, "a.md"); e == nil || e.Aliases != nil {
		t.Errorf("Aliases after removal = %+v, want nil", e)
	}
}

// edgeStrings renders Edges() as "src -rel/kind-> dst => resolved" lines, in the order returned.
func edgeStrings(t testing.TB, st store.Store) []string {
	t.Helper()
	es, err := st.Edges(ctx)
	if err != nil {
		t.Fatalf("Edges: %v", err)
	}
	out := make([]string, len(es))
	for i, e := range es {
		if (e.DstEntryID == 0) != (e.DstEntryPath == "") {
			t.Errorf("edge %+v: DstEntryID and DstEntryPath disagree on dangling", e)
		}
		out[i] = fmt.Sprintf("%s -%s/%s-> %s => %s", e.SrcPath, e.Rel, e.Kind, e.DstPath, e.DstEntryPath)
	}
	return out
}

func edge(dst, rel, kind string) store.EdgeInput {
	return store.EdgeInput{DstPath: dst, Rel: rel, Kind: kind}
}

// edges: ReplaceEntry writes edges deduplicated and dangling; ResolveEdges resolves them by the
// EdgeResolver rule; a replace keeps the target of an unchanged edge; a delete drops the entry's
// edges and leaves edges to it dangling; a bad edge is refused with nothing written; Edges is
// sorted.
func edgesContract(t *testing.T, st store.Store, dim int) {
	const path, wiki = store.EdgePath, store.EdgeWikilink
	a := entryIn("a.md")
	a.Edges = []store.EdgeInput{
		edge("b.md", "links", path),
		edge("Bee", "related", wiki),
		edge("d", "links", path),
		edge("gone.md", "links", path),
		edge("b.md", "links", path), // a repeat is written once
		edge("b.md", "links", wiki), // same target, other kind: a second edge
		edge("d/docs/setup.md", "links", path),
	}
	mustReplace(t, st, a, chunks(dim, "edge source"))
	want := []string{
		"a.md -links/path-> b.md => ",
		"a.md -links/path-> d => ",
		"a.md -links/path-> d/docs/setup.md => ",
		"a.md -links/path-> gone.md => ",
		"a.md -links/wikilink-> b.md => ",
		"a.md -related/wikilink-> Bee => ",
	}
	if got := edgeStrings(t, st); !slices.Equal(got, want) {
		t.Fatalf("Edges before ResolveEdges =\n%q\nwant every edge dangling\n%q", got, want)
	}

	b := entryIn("b.md")
	b.Aliases = []string{"bee"}
	b.Edges = []store.EdgeInput{edge("a.md", "supersedes", path)}
	mustReplace(t, st, b, chunks(dim, "edge target"))
	d := entryIn("d/README.md")
	d.Kind = "dir"
	dcs := chunks(dim, "dir target", "setup page")
	dcs[0].SourceFile, dcs[1].SourceFile = "d/README.md", "d/docs/setup.md"
	mustReplace(t, st, d, dcs)
	if err := st.ResolveEdges(ctx); err != nil {
		t.Fatal(err)
	}
	want = []string{
		"a.md -links/path-> b.md => b.md",
		"a.md -links/path-> d => d/README.md",
		"a.md -links/path-> d/docs/setup.md => d/README.md",
		"a.md -links/path-> gone.md => ",
		"a.md -links/wikilink-> b.md => b.md",
		"a.md -related/wikilink-> Bee => b.md",
		"b.md -supersedes/path-> a.md => a.md",
	}
	if got := edgeStrings(t, st); !slices.Equal(got, want) {
		t.Fatalf("Edges after ResolveEdges =\n%q\nwant\n%q", got, want)
	}
	if err := st.ResolveEdges(ctx); err != nil {
		t.Fatal(err)
	}
	if got := edgeStrings(t, st); !slices.Equal(got, want) {
		t.Errorf("a second ResolveEdges changed the edges:\n%q", got)
	}

	// Replace a.md: unchanged edges keep their targets, a new one dangles until ResolveEdges,
	// a dropped one is gone.
	a.Edges = []store.EdgeInput{edge("b.md", "links", path), edge("d", "links", path), edge("b.md", "depends_on", path)}
	mustReplace(t, st, a, chunks(dim, "edge source again"))
	want = []string{
		"a.md -depends_on/path-> b.md => ",
		"a.md -links/path-> b.md => b.md",
		"a.md -links/path-> d => d/README.md",
		"b.md -supersedes/path-> a.md => a.md",
	}
	if got := edgeStrings(t, st); !slices.Equal(got, want) {
		t.Fatalf("Edges after replacing a.md =\n%q\nwant\n%q", got, want)
	}

	// Delete b.md: its edges go, edges to it dangle.
	if err := st.DeleteEntry(ctx, "b.md"); err != nil {
		t.Fatal(err)
	}
	want = []string{
		"a.md -depends_on/path-> b.md => ",
		"a.md -links/path-> b.md => ",
		"a.md -links/path-> d => d/README.md",
	}
	if got := edgeStrings(t, st); !slices.Equal(got, want) {
		t.Fatalf("Edges after deleting b.md =\n%q\nwant\n%q", got, want)
	}
	// A new b.md takes the dangling edges back on ResolveEdges.
	mustReplace(t, st, entryIn("b.md"), chunks(dim, "edge target back"))
	if err := st.ResolveEdges(ctx); err != nil {
		t.Fatal(err)
	}
	want[0], want[1] = "a.md -depends_on/path-> b.md => b.md", "a.md -links/path-> b.md => b.md"
	if got := edgeStrings(t, st); !slices.Equal(got, want) {
		t.Fatalf("Edges after b.md came back =\n%q\nwant\n%q", got, want)
	}

	// A bad edge is a caller bug: Internal, and nothing of the entry changes.
	for _, bad := range []store.EdgeInput{edge("", "links", path), edge("x.md", "", path), edge("x.md", "links", "other")} {
		in := entryIn("a.md", "newtag")
		in.Edges = []store.EdgeInput{bad}
		if _, err := st.ReplaceEntry(ctx, in, chunks(dim, "edge refused")); errs.KindOf(err) != errs.Internal {
			t.Errorf("edge %+v: err = %v (kind %v), want Internal", bad, err, errs.KindOf(err))
		}
	}
	if got := edgeStrings(t, st); !slices.Equal(got, want) {
		t.Errorf("a refused replace changed the edges:\n%q", got)
	}
	if got := ftsIDs(t, st, store.OpAnd, "", "refused"); len(got) != 0 {
		t.Errorf("a refused replace wrote chunks: %v", got)
	}

	if err := st.ResetIndex(ctx); err != nil {
		t.Fatal(err)
	}
	if got := edgeStrings(t, st); len(got) != 0 {
		t.Errorf("edges survived ResetIndex: %q", got)
	}
}
