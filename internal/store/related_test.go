package store

import (
	"slices"
	"testing"
)

func edgeRow(src, rel, dst, written string) EdgeRow {
	return EdgeRow{SrcPath: src, Rel: rel, Kind: EdgePath, DstPath: written, DstEntryPath: dst}
}

// TestRelatedModel: the reference walk the backends are compared with. Rows are edges within the
// depth, once each, read from the nearer end; unresolved edges lead nowhere; a self-link and a
// cycle end.
func TestRelatedModel(t *testing.T) {
	edges := []EdgeRow{
		edgeRow("a", "links", "b", "b"),
		edgeRow("b", "links", "c", "c"),
		edgeRow("c", "links", "d", "d"),
		edgeRow("d", "links", "a", "a"), // closes a cycle a-b-c-d
		edgeRow("a", "links", "a", "a"), // a self-link
		edgeRow("a", "related", "", "gone"),
		edgeRow("e", "supersedes", "a", "a"),
		edgeRow("f", "links", "", "nowhere"), // a dangling edge of an unreached entry
	}
	for _, tc := range []struct {
		name   string
		origin string
		depth  int
		want   []string
	}{
		{"depth 0 reaches nothing", "a", 0, nil},
		{"depth 1", "a", 1, []string{
			"1 out links a -> a", "1 out links a -> b", "1 out related a -> ", "1 in links a -> d", "1 in supersedes a -> e",
		}},
		{"depth 2 adds b-c and d-c from both sides", "a", 2, []string{
			"1 out links a -> a", "1 out links a -> b", "1 out related a -> ", "1 in links a -> d", "1 in supersedes a -> e",
			"2 out links b -> c", "2 in links d -> c",
		}},
		{"depth 3 adds nothing more: every edge was reached", "a", 3, []string{
			"1 out links a -> a", "1 out links a -> b", "1 out related a -> ", "1 in links a -> d", "1 in supersedes a -> e",
			"2 out links b -> c", "2 in links d -> c",
		}},
		{"an unreached entry has only its own edges", "f", 3, []string{"1 out links f -> "}},
		{"an unknown origin has none", "zz", 3, nil},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var got []string
			for _, r := range RelatedModel(edges, tc.origin, tc.depth) {
				got = append(got, itoa(r.Depth)+" "+r.Direction+" "+r.Rel+" "+r.Via+" -> "+r.Path)
			}
			if !slices.Equal(got, tc.want) {
				t.Errorf("got\n%q\nwant\n%q", got, tc.want)
			}
		})
	}
}

func itoa(n int) string { return string(rune('0' + n)) }

// TestSortRelated: depth, then out before in, then rel, kind, via, path, target.
func TestSortRelated(t *testing.T) {
	rows := []RelatedRow{
		{Depth: 2, Direction: DirOut, Rel: "a"},
		{Depth: 1, Direction: DirIn, Rel: "a"},
		{Depth: 1, Direction: DirOut, Rel: "b"},
		{Depth: 1, Direction: DirOut, Rel: "a", Kind: EdgeWikilink},
		{Depth: 1, Direction: DirOut, Rel: "a", Kind: EdgePath, Via: "z"},
		{Depth: 1, Direction: DirOut, Rel: "a", Kind: EdgePath, Via: "y", Path: "2"},
		{Depth: 1, Direction: DirOut, Rel: "a", Kind: EdgePath, Via: "y", Path: "1", Target: "t"},
		{Depth: 1, Direction: DirOut, Rel: "a", Kind: EdgePath, Via: "y", Path: "1", Target: "s"},
	}
	SortRelated(rows)
	want := []RelatedRow{
		{Depth: 1, Direction: DirOut, Rel: "a", Kind: EdgePath, Via: "y", Path: "1", Target: "s"},
		{Depth: 1, Direction: DirOut, Rel: "a", Kind: EdgePath, Via: "y", Path: "1", Target: "t"},
		{Depth: 1, Direction: DirOut, Rel: "a", Kind: EdgePath, Via: "y", Path: "2"},
		{Depth: 1, Direction: DirOut, Rel: "a", Kind: EdgePath, Via: "z"},
		{Depth: 1, Direction: DirOut, Rel: "a", Kind: EdgeWikilink},
		{Depth: 1, Direction: DirOut, Rel: "b"},
		{Depth: 1, Direction: DirIn, Rel: "a"},
		{Depth: 2, Direction: DirOut, Rel: "a"},
	}
	if !slices.Equal(rows, want) {
		t.Errorf("sorted =\n%+v\nwant\n%+v", rows, want)
	}
}
