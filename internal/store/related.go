package store

import (
	"cmp"
	"context"
	"slices"
	"strings"
)

// MaxRelatedDepth is the deepest Related walk: links of links of links.
const MaxRelatedDepth = 3

// Directions of a RelatedRow, relative to the entry the walk reached it from.
const (
	DirOut = "out" // the near entry links to Path
	DirIn  = "in"  // Path links to the near entry
)

// Relater is the graph read behind `kb related`. (Superseded, the read behind the search
// demotion, is part of Retriever: an entry that is deleted has no edges, so its supersession goes
// with it.)
type Relater interface {
	// Related walks the edges around the entry at path, in both directions, up to depth hops
	// (1..MaxRelatedDepth; anything else is a Usage error), and returns each edge reached once.
	// An unknown path is NotFound. Only resolved edges are followed; an edge to no entry is
	// returned (Path "") for the entry it leaves but leads nowhere. Rows are sorted by depth,
	// direction (out first), rel, kind, near path and target.
	Related(ctx context.Context, path string, depth int) ([]RelatedRow, error)
}

// RelatedRow is one edge a Related walk reached. Via is the entry nearer the walk's origin
// (the origin itself at depth 1); Path is the entry on the far side, "" when the edge is
// dangling (Target then holds the link as written). Direction says which way the edge points:
// DirOut when Via links to Path, DirIn when Path links to Via. Depth is the number of hops from
// the origin to Via, plus one. When both ends are the same distance away the edge reads as
// DirOut. A link from an entry to itself is a DirOut row with Via == Path.
type RelatedRow struct {
	Rel       string
	Kind      string
	Direction string
	Depth     int
	Via       string
	Path      string
	Target    string
}

// RelatedModel is the reference Related: the same walk done on the full edge list (Edges), for
// tests to compare a backend against. depth is not validated.
func RelatedModel(edges []EdgeRow, origin string, depth int) []RelatedRow {
	dist := map[string]int{origin: 0}
	for d := 0; d < depth; d++ {
		for _, e := range edges {
			if e.DstEntryPath == "" {
				continue
			}
			a, aok := dist[e.SrcPath]
			b, bok := dist[e.DstEntryPath]
			if aok && a == d && !bok {
				dist[e.DstEntryPath] = d + 1
			}
			if bok && b == d && !aok {
				dist[e.SrcPath] = d + 1
			}
		}
	}
	const far = 1 << 20
	get := func(p string) int {
		if d, ok := dist[p]; ok {
			return d
		}
		return far
	}
	var out []RelatedRow
	for _, e := range edges {
		ds, dd := get(e.SrcPath), get(e.DstEntryPath) // "" (dangling) is never in dist: far
		near := min(ds, dd)
		if near >= depth {
			continue
		}
		r := RelatedRow{Rel: e.Rel, Kind: e.Kind, Depth: near + 1}
		if ds <= dd {
			r.Direction, r.Via, r.Path, r.Target = DirOut, e.SrcPath, e.DstEntryPath, e.DstPath
		} else {
			r.Direction, r.Via, r.Path, r.Target = DirIn, e.DstEntryPath, e.SrcPath, e.SrcPath
		}
		out = append(out, r)
	}
	SortRelated(out)
	return out
}

// SortRelated orders rows as Related returns them.
func SortRelated(rows []RelatedRow) {
	slices.SortFunc(rows, func(a, b RelatedRow) int {
		return cmp.Or(
			cmp.Compare(a.Depth, b.Depth),
			cmp.Compare(dirRank(a.Direction), dirRank(b.Direction)),
			strings.Compare(a.Rel, b.Rel),
			strings.Compare(a.Kind, b.Kind),
			strings.Compare(a.Via, b.Via),
			strings.Compare(a.Path, b.Path),
			strings.Compare(a.Target, b.Target),
		)
	})
}

// dirRank sorts DirOut before DirIn.
func dirRank(dir string) int {
	if dir == DirOut {
		return 0
	}
	return 1
}
