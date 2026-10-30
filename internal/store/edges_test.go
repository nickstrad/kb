package store

import (
	"slices"
	"testing"
)

// TestEdgeResolver is the resolution rule's decision table: each tier of each kind, the tier
// order, ties broken by the smallest path, case folding where the rule says so, and dangling.
func TestEdgeResolver(t *testing.T) {
	r := NewEdgeResolver([]EdgeTarget{
		{ID: 1, Path: "data/foo.md", Aliases: []string{"Foo Alias", " k8s "}, Files: []string{"data/foo.md"}},
		{ID: 2, Path: "data/droplet/README.md", Aliases: []string{"Ship"}, Files: []string{"data/droplet/README.md", "data/droplet/docs/setup.md"}},
		{ID: 3, Path: "notes/foo.md", Aliases: []string{"k8s"}}, // same basename and alias as 1, larger path
		{ID: 4, Path: "a/ship.md"},                                 // basename "ship" beats 2's alias "Ship"
		{ID: 5, Path: "data/bar.md", Aliases: []string{"droplet"}}, // alias loses to 2's basename
		{ID: 6, Path: "data/Mixed.md", Aliases: []string{""}},
		{ID: 7, Path: "zz/foo.md.md"},
	})
	for _, tc := range []struct {
		name, dst, kind string
		want            int64 // 0 = dangling
	}{
		{"path: entry path", "data/foo.md", EdgePath, 1},
		{"path: directory names its README entry", "data/droplet", EdgePath, 2},
		{"path: a secondary file names its entry", "data/droplet/docs/setup.md", EdgePath, 2},
		{"path: an entry with no indexed files", "notes/foo.md", EdgePath, 3},
		{"path: no .md added", "data/foo", EdgePath, 0},
		{"path: case-sensitive", "data/FOO.md", EdgePath, 0},
		{"path: no basename tier", "foo.md", EdgePath, 0},
		{"path: no alias tier", "Foo Alias", EdgePath, 0},
		{"path: dangling", "data/missing.md", EdgePath, 0},
		{"wikilink: exact path", "data/foo.md", EdgeWikilink, 1},
		{"wikilink: path without .md", "data/foo", EdgeWikilink, 1},
		{"wikilink: directory path", "data/droplet", EdgeWikilink, 2},
		{"wikilink: secondary file", "data/droplet/docs/setup.md", EdgeWikilink, 2},
		{"wikilink: trimmed", "  data/foo  ", EdgeWikilink, 1},
		{"wikilink: basename, smallest path wins", "foo", EdgeWikilink, 1},
		{"wikilink: basename with .md", "foo.md", EdgeWikilink, 1},
		{"wikilink: basename case-insensitive", "MIXED", EdgeWikilink, 6},
		{"wikilink: directory basename", "droplet", EdgeWikilink, 2},
		{"wikilink: basename beats alias", "ship", EdgeWikilink, 4},
		{"wikilink: alias case-insensitive", "foo alias", EdgeWikilink, 1},
		{"wikilink: alias trimmed, smallest path wins", "K8S", EdgeWikilink, 1},
		{"wikilink: a path with / skips the basename tier", "x/foo", EdgeWikilink, 0},
		{"wikilink: only one .md is dropped", "foo.md.md", EdgeWikilink, 7},
		{"wikilink: empty alias never matches", "", EdgeWikilink, 0},
		{"wikilink: blank", "   ", EdgeWikilink, 0},
		{"wikilink: dangling", "nothing", EdgeWikilink, 0},
		{"unknown kind", "data/foo.md", "other", 0},
	} {
		t.Run(tc.name, func(t *testing.T) {
			id, ok := r.Resolve(tc.dst, tc.kind)
			if ok != (tc.want != 0) || id != tc.want {
				t.Errorf("Resolve(%q, %s) = %d, %v; want %d", tc.dst, tc.kind, id, ok, tc.want)
			}
		})
	}
}

// TestEdgeResolverOrderIndependent: ties go to the smallest path whatever order the targets come
// in.
func TestEdgeResolverOrderIndependent(t *testing.T) {
	targets := []EdgeTarget{
		{ID: 1, Path: "b/x.md", Aliases: []string{"al"}},
		{ID: 2, Path: "a/x.md", Aliases: []string{"al"}},
		{ID: 3, Path: "c/x.md", Aliases: []string{"AL"}},
	}
	for i := 0; i < len(targets); i++ {
		r := NewEdgeResolver(append(slices.Clone(targets[i:]), targets[:i]...))
		for _, dst := range []string{"x", "al"} {
			if id, _ := r.Resolve(dst, EdgeWikilink); id != 2 {
				t.Errorf("rotation %d: Resolve(%q) = %d, want 2 (a/x.md)", i, dst, id)
			}
		}
	}
}

func TestNormTags(t *testing.T) {
	for _, tc := range []struct {
		in   []string
		want []string
	}{
		{nil, nil},
		{[]string{"Docker", " docker ", "DOCKER"}, []string{"docker"}},
		{[]string{"b", "", "  ", "A"}, []string{"a", "b"}},
		{[]string{"Ärger", "straße"}, []string{"straße", "ärger"}},
	} {
		if got := NormTags(tc.in); !slices.Equal(got, tc.want) {
			t.Errorf("NormTags(%q) = %q, want %q", tc.in, got, tc.want)
		}
	}
}

func TestUniqueEdges(t *testing.T) {
	a := EdgeInput{DstPath: "x.md", Rel: "links", Kind: EdgePath}
	b := EdgeInput{DstPath: "x.md", Rel: "related", Kind: EdgePath}
	c := EdgeInput{DstPath: "x.md", Rel: "links", Kind: EdgeWikilink}
	if got := UniqueEdges([]EdgeInput{a, b, a, c, b}); !slices.Equal(got, []EdgeInput{a, b, c}) {
		t.Errorf("UniqueEdges = %v, want a, b, c", got)
	}
}
