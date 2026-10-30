package store

import (
	"path"
	"slices"
	"strings"
)

// NormTag is the form a tag is stored in for the tag filter and compared in (schema v7):
// trimmed and lower case. The entry row keeps the tags as written, for display.
func NormTag(tag string) string { return strings.ToLower(strings.TrimSpace(tag)) }

// NormTags is the set of normalised tags (NormTag) of tags: sorted, without repeats or empty
// ones.
func NormTags(tags []string) []string {
	var out []string
	for _, t := range tags {
		if n := NormTag(t); n != "" {
			out = append(out, n)
		}
	}
	slices.Sort(out)
	return slices.Compact(out)
}

// UniqueEdges drops repeated edges with the same (DstPath, Rel, Kind), keeping the first, so a
// backend keyed on them never collides.
func UniqueEdges(edges []EdgeInput) []EdgeInput {
	seen := make(map[EdgeInput]bool, len(edges))
	var out []EdgeInput
	for _, e := range edges {
		if !seen[e] {
			seen[e] = true
			out = append(out, e)
		}
	}
	return out
}

// EdgeTarget is an entry as edge resolution sees it: its id, path, aliases and the files indexed
// for it (a directory entry's docs/*.md; the entry's own path may be among them).
type EdgeTarget struct {
	ID      int64
	Path    string
	Aliases []string
	Files   []string
}

// EdgeResolver decides which entry an edge's target names. It is the one rule every backend's
// ResolveEdges applies. Within a tier, several matching entries resolve to the one with the
// smallest path (byte order), so the result never depends on read order.
//
// An EdgePath target (a cleaned root-relative path) matches, in order:
//  1. the entry at that path;
//  2. the directory entry the path names (`data/droplet` → `data/droplet/README.md`);
//  3. the entry that indexes that file (`data/droplet/docs/setup.md` → `data/droplet/README.md`).
//
// An EdgeWikilink target T (trimmed, as written) matches, in order:
//  1. by path: T, then T+".md", each as an EdgePath target;
//  2. by basename, when T has no "/": an entry whose name equals T without a trailing ".md",
//     case-insensitively. A file entry's name is its file name without ".md"; a README.md
//     entry's name is its directory's name;
//  3. by alias: an entry with an alias equal to T, case-insensitively.
//
// Anything else is dangling: the edge is kept with no target entry.
type EdgeResolver struct {
	byPath  map[string]resolved
	byDir   map[string]resolved
	byFile  map[string]resolved
	byName  map[string]resolved
	byAlias map[string]resolved
}

type resolved struct {
	id   int64
	path string
}

// NewEdgeResolver indexes targets for Resolve.
func NewEdgeResolver(targets []EdgeTarget) *EdgeResolver {
	r := &EdgeResolver{
		byPath:  map[string]resolved{},
		byDir:   map[string]resolved{},
		byFile:  map[string]resolved{},
		byName:  map[string]resolved{},
		byAlias: map[string]resolved{},
	}
	for _, t := range targets {
		v := resolved{t.ID, t.Path}
		keep(r.byPath, t.Path, v)
		dir, base := path.Split(t.Path)
		dir = strings.TrimSuffix(dir, "/")
		if base == "README.md" && dir != "" {
			keep(r.byDir, dir, v)
			keep(r.byName, strings.ToLower(path.Base(dir)), v)
		} else {
			keep(r.byName, strings.ToLower(strings.TrimSuffix(base, ".md")), v)
		}
		for _, f := range t.Files {
			keep(r.byFile, f, v)
		}
		for _, a := range t.Aliases {
			if a = strings.ToLower(strings.TrimSpace(a)); a != "" {
				keep(r.byAlias, a, v)
			}
		}
	}
	return r
}

// keep records v under key unless an entry with a smaller path is there already.
func keep(m map[string]resolved, key string, v resolved) {
	if old, ok := m[key]; !ok || v.path < old.path {
		m[key] = v
	}
}

// Resolve returns the id of the entry the target names, or ok false when it is dangling. kind is
// EdgePath or EdgeWikilink; any other kind never resolves.
func (r *EdgeResolver) Resolve(dst, kind string) (id int64, ok bool) {
	switch kind {
	case EdgePath:
		return r.path(dst)
	case EdgeWikilink:
		t := strings.TrimSpace(dst)
		if t == "" {
			return 0, false
		}
		if id, ok := r.path(t); ok {
			return id, true
		}
		if !strings.HasSuffix(t, ".md") {
			if id, ok := r.path(t + ".md"); ok {
				return id, true
			}
		}
		if !strings.Contains(t, "/") {
			if v, ok := r.byName[strings.ToLower(strings.TrimSuffix(t, ".md"))]; ok {
				return v.id, true
			}
		}
		if v, ok := r.byAlias[strings.ToLower(t)]; ok {
			return v.id, true
		}
	}
	return 0, false
}

// path applies the EdgePath tiers.
func (r *EdgeResolver) path(p string) (int64, bool) {
	for _, m := range []map[string]resolved{r.byPath, r.byDir, r.byFile} {
		if v, ok := m[p]; ok {
			return v.id, true
		}
	}
	return 0, false
}
