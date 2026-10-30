//go:build !noassert

package search

import (
	"context"
	"errors"
	"testing"

	"github.com/nickstrad/kb/internal/assert"
)

func mustViolate(t *testing.T, kind string, f func()) {
	t.Helper()
	defer func() {
		v, ok := recover().(assert.Violation)
		if !ok || v.Kind != kind {
			t.Fatalf("recovered %#v, want a %s Violation", v, kind)
		}
	}()
	f()
}

// TestSearchAssertionsFire: each Fuse, applyCap and Search check rejects a broken value.
func TestSearchAssertionsFire(t *testing.T) {
	one, two := 1, 2
	a := Candidate{ChunkID: 1, FTSRank: &one, RRFScore: 0.5}
	b := Candidate{ChunkID: 2, FTSRank: &two, RRFScore: 0.4}
	mustViolate(t, "precondition", func() { applyCap(nil, nil, 0) })
	mustViolate(t, "precondition", func() { applyCap(nil, nil, MaxK+1) })
	mustViolate(t, "postcondition", func() { assertFused([]Candidate{b, a}) }) // out of order
	mustViolate(t, "postcondition", func() { assertFused([]Candidate{a, a}) }) // duplicate
	mustViolate(t, "postcondition", func() {                                   // ordered, but chunk 1 twice
		assertFused([]Candidate{a, b, {ChunkID: 1, FTSRank: &two, RRFScore: 0.3}})
	})
	mustViolate(t, "postcondition", func() { assertFused([]Candidate{{ChunkID: 3}}) })                                 // no rank
	mustViolate(t, "postcondition", func() { assertFused(make([]Candidate, CandidateLimit+1)) })                       // too long
	mustViolate(t, "postcondition", func() { assertCapped([]Candidate{a}, []Candidate{a, b}, map[int64]string{}, 1) }) // > k
	mustViolate(t, "postcondition", func() { assertCapped([]Candidate{a, b}, []Candidate{b, a}, map[int64]string{}, 2) })
	four := []Candidate{a, b, {ChunkID: 3, FTSRank: &one}, {ChunkID: 4, FTSRank: &one}}
	same := map[int64]string{1: "x", 2: "x", 3: "x", 4: "x"}
	mustViolate(t, "postcondition", func() { assertCapped(four, four, same, MaxK) }) // PerEntryCap
	mustViolate(t, "postcondition", func() { assertOutcome(&Outcome{Mode: "bogus", Result: NewResult("q", "bogus")}) })
	mustViolate(t, "postcondition", func() {
		assertOutcome(&Outcome{Mode: ModeFTSFallback, Result: NewResult("q", ModeFTSFallback)})
	})
	mustViolate(t, "postcondition", func() {
		assertOutcome(&Outcome{Mode: ModeFTS, FallbackErr: errors.New("x"), Result: NewResult("q", ModeFTS)})
	})
	mustViolate(t, "postcondition", func() {
		r := NewResult("q", ModeFTS)
		r.Hits = []Hit{{Rank: 2}}
		assertOutcome(&Outcome{Mode: ModeFTS, Result: r})
	})
	mustViolate(t, "postcondition", func() { assertOutcome(&Outcome{Mode: ModeFTS, Result: &Result{}}) })
	mustViolate(t, "precondition", func() { (&Searcher{}).Record(context.Background(), Request{}, nil) })
	mustViolate(t, "precondition", func() { (&Searcher{}).Record(context.Background(), Request{}, &Outcome{}) })
	// one candidate cannot be returned twice
	mustViolate(t, "postcondition", func() { assertCapped([]Candidate{a}, []Candidate{a, a}, map[int64]string{}, 2) })
	assertFused([]Candidate{a, b})
	assertCapped([]Candidate{a, b}, []Candidate{a, b}, map[int64]string{1: "x", 2: "y"}, 2)
}
