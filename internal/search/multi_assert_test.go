//go:build !noassert

package search

import (
	"context"
	"testing"
)

// TestMergeAssertions: Merge's pre- and postconditions fire on bad input (assert builds).
func TestMergeAssertions(t *testing.T) {
	o := outcomeOf(8, cand(1, "a.md", 0))
	mustViolate(t, "precondition", func() { Merge([]Part{{Ref: "x@/q"}}, []*Outcome{o}) })
	mustViolate(t, "precondition", func() { Merge([]Part{{}}, []*Outcome{nil}) })
	mustViolate(t, "precondition", func() { Merge([]Part{{}, {Ref: "x@/q"}}, []*Outcome{o}) })
	mustViolate(t, "precondition", func() { Merge([]Part{{}, {Ref: "x@/q"}, {Ref: "x@/q"}}, []*Outcome{o, o, o}) })
	mustViolate(t, "precondition", func() { Merge([]Part{{}, {}}, []*Outcome{o, o}) })
	mustViolate(t, "precondition", func() { SearchStores(context.Background(), []Part{{Ref: "x@/q"}}, Request{}) })
	mustViolate(t, "precondition", func() { SearchStores(context.Background(), nil, Request{}) })

	k := func(path string, score float64, rank, part, n int) *merged {
		return &merged{key: mergeKey{"/r", path, 0}, score: score, bestRank: rank, bestPart: part, nParts: n}
	}
	mustViolate(t, "postcondition", func() { assertMerged([]*merged{k("a", 0.2, 1, 0, 1), k("b", 0.1, 2, 0, 1)}, 1) }) // > limit
	mustViolate(t, "postcondition", func() { assertMerged([]*merged{k("a", 0.1, 1, 0, 1), k("b", 0.2, 2, 0, 1)}, 9) }) // order
	mustViolate(t, "postcondition", func() { assertMerged([]*merged{k("a", 0.2, 1, 0, 2), k("a", 0.1, 2, 1, 2)}, 9) }) // duplicate
	mustViolate(t, "postcondition", func() { assertMerged([]*merged{k("a", 0.2, 2, 0, 1), k("b", 0.1, 1, 0, 1)}, 9) }) // store order
	mustViolate(t, "postcondition", func() { assertMerged([]*merged{k("a", 0.2, 1, 0, 1), k("b", 0.1, 1, 0, 1)}, 9) }) // same rank twice
	assertMerged([]*merged{k("a", 0.2, 2, 0, 2), k("b", 0.1, 1, 0, 2)}, 2)                                             // ensembles are exempt
	assertMerged([]*merged{k("a", 0.2, 1, 0, 1), k("b", 0.1, 1, 1, 1), k("c", 0.05, 2, 0, 1)}, 3)                      // per store
}
