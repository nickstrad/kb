package render

import (
	"fmt"
	"strings"
	"testing"
)

func lk(dir, rel, path string) Link { return Link{Rel: rel, Direction: dir, Path: path} }

// TestNeighbors: neighbours dedupe against the hits and against each other, in rank order;
// another store's hit gets none; a hit with nothing left has no line; chunks of one entry share
// the entry's line.
func TestNeighbors(t *testing.T) {
	for _, tc := range []struct {
		name  string
		hits  []Hit
		links map[string][]Link
		want  string
	}{
		{"no hits", nil, nil, ""},
		{"no links", []Hit{{Rank: 1, Path: "a.md"}}, nil, ""},
		{"one neighbour", []Hit{{Rank: 1, Path: "a.md"}},
			map[string][]Link{"a.md": {lk("out", "links", "b.md")}},
			"Related:\n  [1] → b.md (links)"},
		{"in and out", []Hit{{Rank: 1, Path: "a.md"}},
			map[string][]Link{"a.md": {lk("out", "links", "b.md"), lk("in", "supersedes", "c.md")}},
			"Related:\n  [1] → b.md (links), ← c.md (supersedes)"},
		{"a hit is never a neighbour", []Hit{{Rank: 1, Path: "a.md"}, {Rank: 2, Path: "b.md"}},
			map[string][]Link{"a.md": {lk("out", "links", "b.md"), lk("out", "links", "c.md")}, "b.md": {lk("in", "links", "a.md")}},
			"Related:\n  [1] → c.md (links)"},
		{"a shared neighbour is listed under the best hit only", []Hit{{Rank: 1, Path: "a.md"}, {Rank: 2, Path: "b.md"}},
			map[string][]Link{"a.md": {lk("out", "links", "c.md")}, "b.md": {lk("out", "related", "c.md"), lk("out", "related", "d.md")}},
			"Related:\n  [1] → c.md (links)\n  [2] → d.md (related)"},
		{"chunks of one entry share the line", []Hit{{Rank: 1, Path: "a.md"}, {Rank: 2, Path: "a.md"}},
			map[string][]Link{"a.md": {lk("out", "links", "b.md")}},
			"Related:\n  [1] → b.md (links)"},
		{"another store's hit has none, and its path is not a local hit", []Hit{{Rank: 1, Store: "x", Path: "a.md"}, {Rank: 2, Path: "b.md"}},
			map[string][]Link{"a.md": {lk("out", "links", "z.md")}, "b.md": {lk("out", "links", "a.md")}},
			"Related:\n  [2] → a.md (links)"},
		{"capped at five with a count", []Hit{{Rank: 1, Path: "a.md"}},
			map[string][]Link{"a.md": {lk("out", "l", "1"), lk("out", "l", "2"), lk("out", "l", "3"), lk("out", "l", "4"), lk("out", "l", "5"), lk("out", "l", "6"), lk("out", "l", "7")}},
			"Related:\n  [1] → 1 (l), → 2 (l), → 3 (l), → 4 (l), → 5 (l) (+2 more)"},
		{"exactly five is not capped", []Hit{{Rank: 1, Path: "a.md"}},
			map[string][]Link{"a.md": {lk("out", "l", "1"), lk("out", "l", "2"), lk("out", "l", "3"), lk("out", "l", "4"), lk("out", "l", "5")}},
			"Related:\n  [1] → 1 (l), → 2 (l), → 3 (l), → 4 (l), → 5 (l)"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := FormatNeighbors(Neighbors(tc.hits, tc.links)); got != tc.want {
				t.Errorf("got\n%q\nwant\n%q", got, tc.want)
			}
		})
	}
}

// TestNeighborsProperties: on generated graphs no neighbour is a local hit or appears twice, and
// each line's rank is a hit's.
func TestNeighborsProperties(t *testing.T) {
	for seed := 0; seed < 200; seed++ {
		var hits []Hit
		links := map[string][]Link{}
		for i := 0; i < 1+seed%6; i++ {
			hits = append(hits, Hit{Rank: i + 1, Path: fmt.Sprintf("p%d", (seed*7+i*3)%5)})
		}
		for i := 0; i < 5; i++ {
			for j := 0; j < (seed+i)%4; j++ {
				p := fmt.Sprintf("p%d", i)
				links[p] = append(links[p], lk("out", "links", fmt.Sprintf("p%d", (seed+i+j*2)%8)))
			}
		}
		isHit := map[string]bool{}
		for _, h := range hits {
			isHit[h.Path] = true
		}
		seen := map[string]bool{}
		for _, line := range Neighbors(hits, links) {
			if line.Rank < 1 || line.Rank > len(hits) || len(line.Links) == 0 {
				t.Fatalf("seed %d: bad line %+v", seed, line)
			}
			for _, k := range line.Links {
				if isHit[k.Path] || seen[k.Path] {
					t.Fatalf("seed %d: neighbour %s is a hit or repeated (hits %v, line %+v)", seed, k.Path, hits, line)
				}
				seen[k.Path] = true
			}
		}
	}
}

func TestSupersededHeader(t *testing.T) {
	got := header(Hit{Rank: 2, Path: "a.md", Heading: "H", Updated: "2026-01-01", Superseded: true, Also: []int{3}})
	if want := "[2] a.md › H · updated 2026-01-01 · superseded · also [3]"; got != want {
		t.Errorf("header = %q, want %q", got, want)
	}
	if strings.Contains(header(Hit{Rank: 1, Path: "a.md"}), "superseded") {
		t.Error("a plain hit is marked superseded")
	}
}
