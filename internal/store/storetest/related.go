package storetest

import (
	"fmt"
	"slices"
	"testing"

	"github.com/nickstrad/kb/internal/errs"
	"github.com/nickstrad/kb/internal/store"
)

// relatedStrings renders rows as "d<depth> <dir> <rel>/<kind> <via> -> <path>" (a dangling row
// shows "?<target>").
func relatedStrings(rows []store.RelatedRow) []string {
	out := make([]string, len(rows))
	for i, r := range rows {
		far := r.Path
		if far == "" {
			far = "?" + r.Target
		}
		out[i] = fmt.Sprintf("d%d %s %s/%s %s -> %s", r.Depth, r.Direction, r.Rel, r.Kind, r.Via, far)
	}
	return out
}

// related: Related walks resolved edges both ways, each edge once, to a depth of 1..3; cycles and
// self-links end; a dangling edge is listed but leads nowhere; the depth bounds and an unknown
// path are refused; Superseded names the targets of supersedes edges from other entries, and a
// deleted superseder (or a self-supersede) does not count. The result equals RelatedModel.
func related(t *testing.T, st store.Store, dim int) {
	const path, wiki = store.EdgePath, store.EdgeWikilink
	put := func(p string, edges ...store.EdgeInput) {
		in := entryIn(p)
		in.Edges = edges
		mustReplace(t, st, in, chunks(dim, "related "+p))
	}
	// a -> b -> c -> a is a cycle; a -> a a self-link; d -> a; e -> d a second hop; a -> gone.
	put("a.md", edge("b.md", "links", path), edge("a.md", "links", path), edge("gone.md", "links", path))
	put("b.md", edge("c.md", "related", path))
	put("c.md", edge("a.md", "supersedes", path))
	put("d.md", edge("a.md", "depends_on", path), edge("A", "related", wiki)) // [[A]] resolves by basename
	put("e.md", edge("d.md", "links", path))
	put("lonely.md")
	if err := st.ResolveEdges(ctx); err != nil {
		t.Fatal(err)
	}

	for _, tc := range []struct {
		name  string
		path  string
		depth int
		want  []string // nil: only compare with the model and the count
		n     int
	}{
		{"depth 1", "a.md", 1, []string{
			"d1 out links/path a.md -> ?gone.md", // a dangling edge sorts first: its path is empty
			"d1 out links/path a.md -> a.md",
			"d1 out links/path a.md -> b.md",
			"d1 in depends_on/path a.md -> d.md",
			"d1 in related/wikilink a.md -> d.md",
			"d1 in supersedes/path a.md -> c.md",
		}, 6},
		{"depth 2 reaches the second hop once", "a.md", 2, nil, 6 + 2},
		{"depth 3 ends in the cycle", "a.md", 3, nil, 6 + 2},
		{"from the middle of the cycle", "b.md", 1, []string{
			"d1 out related/path b.md -> c.md",
			"d1 in links/path b.md -> a.md",
		}, 2},
		{"no links", "lonely.md", 3, []string{}, 0},
	} {
		rows, err := st.Related(ctx, tc.path, tc.depth)
		if err != nil {
			t.Errorf("%s: Related(%s, %d): %v", tc.name, tc.path, tc.depth, err)
			continue
		}
		got := relatedStrings(rows)
		if tc.want != nil && !slices.Equal(got, tc.want) {
			t.Errorf("%s: Related(%s, %d) =\n%q\nwant\n%q", tc.name, tc.path, tc.depth, got, tc.want)
		}
		if len(rows) != tc.n {
			t.Errorf("%s: Related(%s, %d) has %d rows, want %d: %q", tc.name, tc.path, tc.depth, len(rows), tc.n, got)
		}
		edges, err := st.Edges(ctx)
		if err != nil {
			t.Fatal(err)
		}
		if model := relatedStrings(store.RelatedModel(edges, tc.path, tc.depth)); !slices.Equal(got, model) {
			t.Errorf("%s: Related(%s, %d) =\n%q\ndiffers from the model\n%q", tc.name, tc.path, tc.depth, got, model)
		}
	}
	// e -> d is two hops from a, and reached once even though a cycle leads back.
	rows, _ := st.Related(ctx, "a.md", 2)
	if got := relatedStrings(rows); !slices.Contains(got, "d2 in links/path d.md -> e.md") {
		t.Errorf("depth 2 from a.md lacks e -> d: %q", got)
	}
	if rows, err := st.Related(ctx, "e.md", 1); err != nil || len(rows) != 1 || rows[0].Direction != store.DirOut || rows[0].Path != "d.md" {
		t.Errorf("Related(e.md, 1) = %+v, %v; want the one edge e -> d", rows, err)
	}

	for _, depth := range []int{-1, 0, 4} {
		if _, err := st.Related(ctx, "a.md", depth); errs.KindOf(err) != errs.Usage {
			t.Errorf("Related(a.md, %d) err = %v (kind %v), want Usage", depth, err, errs.KindOf(err))
		}
	}
	if _, err := st.Related(ctx, "nope.md", 1); errs.KindOf(err) != errs.NotFound {
		t.Errorf("Related(nope.md) err = %v (kind %v), want NotFound", err, errs.KindOf(err))
	}

	// Superseded: a.md is superseded by c.md; a self-supersede does not count.
	put("self.md", edge("self.md", "supersedes", path))
	if err := st.ResolveEdges(ctx); err != nil {
		t.Fatal(err)
	}
	sup, err := st.Superseded(ctx, []string{"a.md", "b.md", "self.md", "nope.md"})
	if err != nil || len(sup) != 1 || !sup["a.md"] {
		t.Errorf("Superseded = %v, %v; want only a.md", sup, err)
	}
	if sup, err := st.Superseded(ctx, nil); err != nil || len(sup) != 0 {
		t.Errorf("Superseded(nil) = %v, %v; want empty", sup, err)
	}
	if err := st.DeleteEntry(ctx, "c.md"); err != nil {
		t.Fatal(err)
	}
	if sup, err := st.Superseded(ctx, []string{"a.md"}); err != nil || len(sup) != 0 {
		t.Errorf("Superseded after the superseder was deleted = %v, %v; want empty", sup, err)
	}
}
