package search

import (
	"fmt"
	"slices"
	"testing"

	"github.com/nickstrad/kb/internal/config"
)

// rankedIDs decodes bytes into a duplicate-free ranked list of chunk ids 1..64.
func rankedIDs(b []byte) []int64 {
	seen := map[int64]bool{}
	var out []int64
	for _, x := range b {
		id := int64(x%64) + 1
		if !seen[id] {
			seen[id] = true
			out = append(out, id)
		}
	}
	return out
}

func toFTS(ids []int64) []FTSRow {
	out := make([]FTSRow, len(ids))
	for i, id := range ids {
		out[i] = FTSRow{ChunkID: id, Score: -float64(len(ids) - i)}
	}
	return out
}

func toVec(ids []int64) []VecRow {
	out := make([]VecRow, len(ids))
	for i, id := range ids {
		out[i] = VecRow{ChunkID: id, Distance: float64(i) / 64}
	}
	return out
}

// fuseParams decodes the knobs: rrf_k in [1, MaxRRFK], candidate_limit in [1, 129] (above the
// 128 a fusion can hold, so both the cut and no cut occur) and weights that are exact binary
// fractions in [0, 31.875], so doubling them is exact.
func fuseParams(rrfK uint32, limit, wf, wv uint8) Params {
	p := DefaultParams()
	p.RRFK = 1 + int(rrfK%config.MaxRRFK)
	p.CandidateLimit = 1 + int(limit)%129
	p.FTSWeight, p.VecWeight = float64(wf)/8, float64(wv)/8
	return p
}

func scores(cands []Candidate) map[int64]float64 {
	m := make(map[int64]float64, len(cands))
	for _, c := range cands {
		m[c.ChunkID] = c.RRFScore
	}
	return m
}

func bestRank(c Candidate) int { return min(rankOrMax(c.FTSRank), rankOrMax(c.VecRank)) }

// FuzzFuse checks the metamorphic properties of weighted RRF. Order, uniqueness and the length cap
// are Fuse's own assertions and are not repeated here.
func FuzzFuse(f *testing.F) {
	f.Add([]byte{1, 2, 3}, []byte{3, 2, 1}, uint32(59), uint8(39), uint8(8), uint8(8))               // defaults
	f.Add([]byte{1, 2}, []byte{}, uint32(0), uint8(0), uint8(0), uint8(8))                           // rrf_k 1, limit 1, fts weight 0
	f.Add([]byte{5, 6, 7}, []byte{7, 8}, uint32(config.MaxRRFK-1), uint8(128), uint8(255), uint8(0)) // max rrf_k, vec weight 0
	f.Add([]byte{9, 10, 11, 12}, []byte{12, 11, 13}, uint32(0), uint8(2), uint8(1), uint8(1))
	f.Fuzz(func(t *testing.T, a, b []byte, rrfK uint32, limit, wf, wv uint8) {
		ftsIDs, vecIDs := rankedIDs(a), rankedIDs(b)
		fts, vec := toFTS(ftsIDs), toVec(vecIDs)
		p := fuseParams(rrfK, limit, wf, wv)
		got := Fuse(fts, vec, nil, p)

		// One list alone: the fused order is that list's order, cut at the limit, whatever the
		// weight (with weight 0 every score ties and the rank decides).
		if one := chunkIDs(Fuse(fts, nil, nil, p)); !slices.Equal(one, ftsIDs[:min(len(ftsIDs), p.CandidateLimit)]) {
			t.Fatalf("Fuse(fts, nil) = %v, want fts order %v", one, ftsIDs)
		}
		if one := chunkIDs(Fuse(nil, vec, nil, p)); !slices.Equal(one, vecIDs[:min(len(vecIDs), p.CandidateLimit)]) {
			t.Fatalf("Fuse(nil, vec) = %v, want vec order %v", one, vecIDs)
		}

		// A list of weight 0 adds no score: every chunk scores what the other list alone gives it.
		for _, tc := range []struct {
			name  string
			zero  func(*Params)
			alone []Candidate
		}{
			{"vec weight 0", func(q *Params) { q.VecWeight = 0 }, Fuse(fts, nil, nil, withLimit(p, 128))},
			{"fts weight 0", func(q *Params) { q.FTSWeight = 0 }, Fuse(nil, vec, nil, withLimit(p, 128))},
		} {
			q := p
			tc.zero(&q)
			want := scores(tc.alone)
			for _, c := range Fuse(fts, vec, nil, q) {
				if c.RRFScore != want[c.ChunkID] {
					t.Fatalf("%s: chunk %d scores %v, the other list alone gives %v", tc.name, c.ChunkID, c.RRFScore, want[c.ChunkID])
				}
			}
		}

		// The exact list (here b reversed, with its own weight) is a third list like the others:
		// alone it fuses in its own order; at weight 0 it adds chunks but no score; otherwise each
		// chunk's score is its two-list score plus its exact term, summed in Fuse's order.
		exactIDs := slices.Clone(vecIDs)
		slices.Reverse(exactIDs)
		exact := toFTS(exactIDs)
		pe := withLimit(p, 192)
		pe.ExactWeight = float64(wf^wv) / 8
		if one := chunkIDs(Fuse(nil, nil, exact, p)); !slices.Equal(one, exactIDs[:min(len(exactIDs), p.CandidateLimit)]) {
			t.Fatalf("Fuse(nil, nil, exact) = %v, want exact order %v", one, exactIDs)
		}
		two, third := scores(Fuse(fts, vec, nil, pe)), scores(Fuse(nil, nil, exact, pe))
		for _, c := range Fuse(fts, vec, exact, pe) {
			if want := two[c.ChunkID] + third[c.ChunkID]; c.RRFScore != want {
				t.Fatalf("exact weight %v: chunk %d scores %v, want two-list %v + exact %v", pe.ExactWeight, c.ChunkID, c.RRFScore, two[c.ChunkID], third[c.ChunkID])
			}
		}
		pz := pe
		pz.ExactWeight = 0
		for _, c := range Fuse(fts, vec, exact, pz) {
			if c.RRFScore != two[c.ChunkID] {
				t.Fatalf("exact weight 0: chunk %d scores %v, two lists give %v", c.ChunkID, c.RRFScore, two[c.ChunkID])
			}
		}

		// Doubling both weights doubles every score and keeps the order.
		q := p
		q.FTSWeight, q.VecWeight = 2*p.FTSWeight, 2*p.VecWeight
		doubled := Fuse(fts, vec, nil, q)
		if !slices.Equal(chunkIDs(doubled), chunkIDs(got)) {
			t.Fatalf("doubling the weights reordered %v to %v", chunkIDs(got), chunkIDs(doubled))
		}
		for i := range got {
			if doubled[i].RRFScore != 2*got[i].RRFScore {
				t.Fatalf("chunk %d: doubled score %v, want %v", got[i].ChunkID, doubled[i].RRFScore, 2*got[i].RRFScore)
			}
		}

		// A larger rrf_k never raises a score, and lowers it whenever the chunk's lists weigh
		// anything.
		if p.RRFK < config.MaxRRFK {
			q := withLimit(p, 128)
			before := scores(Fuse(fts, vec, nil, q))
			q.RRFK++
			for _, c := range Fuse(fts, vec, nil, q) {
				if c.RRFScore > before[c.ChunkID] || (before[c.ChunkID] > 0 && c.RRFScore == before[c.ChunkID]) {
					t.Fatalf("rrf_k %d→%d: chunk %d %v → %v", p.RRFK, q.RRFK, c.ChunkID, before[c.ChunkID], c.RRFScore)
				}
			}
		}

		// Dominance (equal positive weights): a chunk in both lists outranks a chunk in one list
		// whose rank is no better than the first chunk's best rank.
		if p.FTSWeight == p.VecWeight && p.FTSWeight > 0 {
			for i, x := range got {
				if x.FTSRank == nil || x.VecRank == nil {
					continue
				}
				for j, y := range got {
					if (y.FTSRank == nil) != (y.VecRank == nil) && bestRank(y) >= bestRank(x) && j < i {
						t.Fatalf("chunk %d (in one list, rank %d) precedes chunk %d (in both, best rank %d)",
							y.ChunkID, bestRank(y), x.ChunkID, bestRank(x))
					}
				}
			}
		}
	})
}

func withLimit(p Params, limit int) Params {
	p.CandidateLimit = limit
	return p
}

// FuzzApplyCap checks the diversity cap's metamorphic and maximality properties; its local
// postconditions are assertions in applyCap.
func FuzzApplyCap(f *testing.F) {
	f.Add([]byte{1, 2, 3, 4, 5, 6}, []byte{0, 0, 0, 0, 1, 7}, uint8(8), uint8(3))
	f.Add([]byte{1, 2, 3}, []byte{1, 1, 1}, uint8(1), uint8(1))                // k MinK, cap 1
	f.Add([]byte{9, 8, 7, 6, 5}, []byte{2, 2, 2, 2, 2}, uint8(20), uint8(255)) // k MaxK, cap far above k
	f.Fuzz(func(t *testing.T, ids, entries []byte, kb, capb uint8) {
		order := rankedIDs(ids)
		entryOf := map[int64]string{}
		for i, id := range order {
			if i < len(entries) && entries[i]%7 == 0 {
				continue // unmapped: a dangling chunk
			}
			e := byte(0)
			if i < len(entries) {
				e = entries[i] % 5
			}
			entryOf[id] = fmt.Sprintf("e%d.md", e)
		}
		k := MinK + int(kb)%MaxK
		perEntryCap := 1 + int(capb)
		fresh := func() []Candidate {
			out := make([]Candidate, len(order))
			for i, id := range order {
				out[i] = Candidate{ChunkID: id}
			}
			return out
		}

		cands := fresh()
		got := applyCap(cands, entryOf, k, perEntryCap)

		// Prefix: one more place never changes the places already given.
		if k < MaxK {
			next := applyCap(fresh(), entryOf, k+1, perEntryCap)
			if len(next) < len(got) || !slices.Equal(chunkIDs(next[:len(got)]), chunkIDs(got)) {
				t.Fatalf("k=%d gave %v, k+1 gave %v: not a prefix", k, chunkIDs(got), chunkIDs(next))
			}
		}

		// Maximality and the drop marks: below k, every mapped chunk left out is marked
		// CapDropped and its entry holds exactly perEntryCap places; an unmapped one is never
		// marked.
		returned := map[int64]bool{}
		perEntry := map[string]int{}
		for _, c := range got {
			returned[c.ChunkID] = true
			perEntry[entryOf[c.ChunkID]]++
		}
		for _, c := range cands {
			path, mapped := entryOf[c.ChunkID]
			if !mapped && c.CapDropped {
				t.Fatalf("unmapped chunk %d marked CapDropped", c.ChunkID)
			}
			if len(got) < k && mapped && !returned[c.ChunkID] && (!c.CapDropped || perEntry[path] != perEntryCap) {
				t.Fatalf("chunk %d of %s left out with %d < k places; dropped=%v, entry holds %d",
					c.ChunkID, path, len(got), c.CapDropped, perEntry[path])
			}
		}
	})
}
