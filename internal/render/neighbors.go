package render

import (
	"fmt"
	"strings"

	"github.com/nickstrad/kb/internal/assert"
	"github.com/nickstrad/kb/internal/store"
)

// Link is one neighbour of a hit's entry: the entry on the other end of an edge, the edge's rel
// and which way it points (store.DirOut: the hit's entry links to it; store.DirIn: it links to
// the hit's entry). The values are those of store.RelatedRow at depth 1.
type Link struct {
	Rel       string
	Direction string
	Path      string
}

// NeighborsPerHit caps the neighbours listed on one hit's line; the rest are counted.
const NeighborsPerHit = 5

// NeighborLine is the neighbours of one hit: its rank and the links (all of them; FormatNeighbors
// shows the first NeighborsPerHit).
type NeighborLine struct {
	Rank  int
	Links []Link
}

// Neighbors lays out the neighbours of hits for the "Related:" section. links maps a hit's entry
// path to its depth-1 links, already sorted. An entry already returned as a hit is never a
// neighbour, and an entry shows up under the best-ranked hit that has it only. A hit with no
// neighbour left has no line. Hits of one entry (several chunks) share the entry's line, under
// the first rank (the later chunks find its neighbours already claimed). hits must be in rank order; those from another store (Store != "") are skipped.
func Neighbors(hits []Hit, links map[string][]Link) []NeighborLine {
	isHit := map[string]bool{}
	for _, h := range hits {
		if h.Store == "" {
			isHit[h.Path] = true
		}
	}
	claimed := map[string]bool{}
	var out []NeighborLine
	for _, h := range hits {
		if h.Store != "" {
			continue
		}
		var keep []Link
		for _, l := range links[h.Path] {
			if isHit[l.Path] || claimed[l.Path] {
				continue
			}
			claimed[l.Path] = true
			keep = append(keep, l)
		}
		if len(keep) == 0 {
			continue
		}
		out = append(out, NeighborLine{Rank: h.Rank, Links: keep})
	}
	if assert.Enabled {
		seen := map[string]bool{}
		for _, l := range out {
			for _, k := range l.Links {
				assert.Post(!isHit[k.Path] && !seen[k.Path], "Neighbors: %q is a hit or listed twice", k.Path)
				seen[k.Path] = true
			}
		}
	}
	return out
}

// FormatNeighbors is the human "Related:" section, one line per hit: `  [2] → path (rel), ←
// path (rel) (+1 more)`; "" for no lines.
func FormatNeighbors(lines []NeighborLine) string {
	if len(lines) == 0 {
		return ""
	}
	var b strings.Builder
	b.WriteString("Related:")
	for _, l := range lines {
		shown := l.Links[:min(len(l.Links), NeighborsPerHit)]
		parts := make([]string, len(shown))
		for i, k := range shown {
			arrow := "→"
			if k.Direction == store.DirIn {
				arrow = "←"
			}
			parts[i] = fmt.Sprintf("%s %s (%s)", arrow, k.Path, k.Rel)
		}
		fmt.Fprintf(&b, "\n  [%d] %s", l.Rank, strings.Join(parts, ", "))
		if len(l.Links) > len(shown) {
			fmt.Fprintf(&b, " (+%d more)", len(l.Links)-len(shown))
		}
	}
	return b.String()
}
