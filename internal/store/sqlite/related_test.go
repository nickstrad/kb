package sqlite

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"testing"

	"github.com/nickstrad/kb/internal/errs"
	"github.com/nickstrad/kb/internal/store"
)

// relatedGraph rebuilds the index from graph bytes: six entries n0..n5, and one edge per byte
// triple (src, dst, rel) with dst 6 meaning a missing entry.
func relatedGraph(t testing.TB, st store.Store, graph []byte) {
	ctx := context.Background()
	if err := st.ResetIndex(ctx); err != nil {
		t.Fatal(err)
	}
	if err := st.EnsureEmbedMeta(ctx, store.EmbedMeta{Model: store.FTSOnlyModel}); err != nil {
		t.Fatal(err)
	}
	rels := []string{"links", "supersedes", "related"}
	edges := make([][]store.EdgeInput, 6)
	for i := 0; i+2 < len(graph); i += 3 {
		src, dst := int(graph[i])%6, int(graph[i+1])%7
		target := fmt.Sprintf("n%d.md", dst)
		if dst == 6 {
			target = "gone.md"
		}
		edges[src] = append(edges[src], store.EdgeInput{DstPath: target, Rel: rels[int(graph[i+2])%3], Kind: store.EdgePath})
	}
	for i := range edges {
		in := store.EntryInput{Path: fmt.Sprintf("n%d.md", i), Kind: "file", Title: "t", Edges: edges[i]}
		if _, err := st.ReplaceEntry(ctx, in, []store.ChunkInput{{Ord: 0, SourceFile: in.Path, Text: "x", TextHash: "h"}}); err != nil {
			t.Fatal(err)
		}
	}
	if err := st.ResolveEdges(ctx); err != nil {
		t.Fatal(err)
	}
}

// checkRelated compares Related with the reference walk for every entry and depth, and checks the
// properties that hold for any graph: rows are distinct edges of the graph, a row's depth is
// within the bound, and a deeper walk contains the shallower one.
func checkRelated(t *testing.T, st store.Store, graph []byte) {
	ctx := context.Background()
	relatedGraph(t, st, graph)
	edges, err := st.Edges(ctx)
	if err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 6; i++ {
		p := fmt.Sprintf("n%d.md", i)
		var prev []store.RelatedRow
		for depth := 1; depth <= store.MaxRelatedDepth; depth++ {
			got, err := st.Related(ctx, p, depth)
			if err != nil {
				t.Fatalf("Related(%s, %d): %v", p, depth, err)
			}
			want := store.RelatedModel(edges, p, depth)
			if !slices.Equal(got, want) {
				t.Fatalf("graph %v: Related(%s, %d) =\n%+v\nmodel\n%+v", graph, p, depth, got, want)
			}
			if len(got) > len(edges) {
				t.Fatalf("Related(%s, %d) has %d rows for %d edges", p, depth, len(got), len(edges))
			}
			seen := map[store.RelatedRow]bool{}
			for _, r := range got {
				if r.Depth < 1 || r.Depth > depth || seen[r] {
					t.Fatalf("Related(%s, %d): bad or repeated row %+v", p, depth, r)
				}
				seen[r] = true
			}
			if len(got) < len(prev) {
				t.Fatalf("Related(%s, %d) shrank from %d to %d rows", p, depth, len(prev), len(got))
			}
			prev = got
		}
	}
}

func TestRelatedMatchesModel(t *testing.T) {
	st := openContract(t, 0)
	for _, graph := range [][]byte{
		nil,
		{0, 0, 0},                   // a self-loop
		{0, 1, 0, 1, 0, 0},          // A -> B -> A
		{0, 1, 0, 1, 2, 2, 2, 0, 1}, // a three-cycle
		{0, 6, 0, 6, 6, 1, 3, 0, 2}, // dangling edges and an inbound edge
		{0, 1, 0, 1, 2, 0, 2, 3, 0, 3, 4, 0, 4, 5, 0}, // a chain longer than the depth bound
		{0, 1, 0, 0, 1, 2, 1, 0, 1, 1, 0, 2},          // parallel edges with other rels
	} {
		checkRelated(t, st, graph)
	}
}

// FuzzRelated checks the recursive query against the reference walk on arbitrary graphs.
//
// Run: go test -tags fts5 ./internal/store/sqlite -run '^$' -fuzz '^FuzzRelated$' -fuzztime 60s
func FuzzRelated(f *testing.F) {
	f.Add([]byte{0, 1, 0, 1, 0, 0})
	f.Add([]byte{0, 0, 0, 0, 6, 1})
	f.Add([]byte{0, 1, 0, 1, 2, 2, 2, 0, 1, 3, 2, 0})
	st := openContract(f, 0)
	f.Fuzz(func(t *testing.T, graph []byte) {
		if len(graph) > 60 {
			graph = graph[:60]
		}
		checkRelated(t, st, graph)
	})
}

func TestRelatedErrors(t *testing.T) {
	st := openContract(t, 0)
	relatedGraph(t, st, []byte{0, 1, 0})
	ctx := context.Background()
	for _, tc := range []struct {
		name  string
		path  string
		depth int
		kind  errs.Kind
	}{
		{"depth 0", "n0.md", 0, errs.Usage},
		{"depth 4", "n0.md", 4, errs.Usage},
		{"unknown entry", "zz.md", 1, errs.NotFound},
	} {
		if _, err := st.Related(ctx, tc.path, tc.depth); errs.KindOf(err) != tc.kind {
			t.Errorf("%s: err = %v (kind %v), want %v", tc.name, err, errs.KindOf(err), tc.kind)
		}
	}
	closed := openContract(t, 0)
	closed.Close()
	if _, err := closed.Related(ctx, "n0.md", 1); err == nil {
		t.Error("Related on a closed store succeeded")
	}
	if _, err := closed.Superseded(ctx, []string{"n0.md"}); err == nil {
		t.Error("Superseded on a closed store succeeded")
	}
}

// TestRelatedFaults: a failing statement surfaces as an Internal error from each step of
// Related (the entry lookup, the walk) and Superseded.
func TestRelatedFaults(t *testing.T) {
	ctx := context.Background()
	for _, tc := range []struct {
		name, match string
		op          func(st *Store) error
	}{
		{"entry lookup", "FROM entries WHERE path", func(st *Store) error { _, err := st.Related(ctx, "n0.md", 1); return err }},
		{"walk", "WITH RECURSIVE walk", func(st *Store) error { _, err := st.Related(ctx, "n0.md", 1); return err }},
		{"superseded", "rel = 'supersedes'", func(st *Store) error { _, err := st.Superseded(ctx, []string{"n0.md"}); return err }},
	} {
		t.Run(tc.name, func(t *testing.T) {
			st := openFaulty(t, 0)
			relatedGraph(t, st, []byte{0, 1, 0})
			fail(t, tc.match, 1)
			if err := tc.op(st); !errors.Is(err, errFault) || errs.KindOf(err) != errs.Internal {
				t.Errorf("err = %v (kind %v), want the injected fault as Internal", err, errs.KindOf(err))
			}
		})
	}
}
