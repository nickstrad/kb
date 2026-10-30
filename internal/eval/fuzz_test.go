package eval

import (
	"math"
	"testing"
)

// decodeRanking turns fuzz bytes into a ScoreRanking input: data[0] picks nGold (1..8), data[1]
// picks k (1..20), and each later byte is a hit matching gold item b%(nGold+1)-1 (-1: no gold).
func decodeRanking(data []byte) (matches []int, nGold, k int) {
	nGold, k = 1, 8
	if len(data) > 0 {
		nGold = int(data[0])%8 + 1
	}
	if len(data) > 1 {
		k = int(data[1])%20 + 1
	}
	if len(data) > 2 {
		for _, b := range data[2:] {
			matches = append(matches, int(b)%(nGold+1)-1)
		}
	}
	return matches, nGold, k
}

// FuzzScoreRanking checks the metric properties for every ranking:
//   - rr and nDCG are in [0, 1], and hit ⇔ rr > 0 ⇔ nDCG > 0;
//   - a perfect ranking (min(nGold, k) distinct gold items first) scores rr = nDCG = 1;
//   - appending a non-gold hit never changes any metric, and inserting one right after the first gold
//     hit never changes MRR (metamorphic);
//   - putting a non-gold hit in front never raises rr or nDCG.
func FuzzScoreRanking(f *testing.F) {
	for _, seed := range [][]byte{
		{}, {0}, {0, 0}, {0, 0, 1}, {0, 0, 0, 1}, {1, 2, 2, 1, 0, 2}, {7, 19, 1, 2, 3, 4, 5, 6, 7, 8},
		{2, 0, 1, 1, 3}, {3, 1, 4, 4, 4, 4},
	} {
		f.Add(seed)
	}
	f.Fuzz(func(t *testing.T, data []byte) {
		matches, nGold, k := decodeRanking(data)
		s := ScoreRanking(matches, nGold, k)
		if s.RR < 0 || s.RR > 1 || s.NDCG < 0 || s.NDCG > 1+eps || math.IsNaN(s.NDCG) {
			t.Fatalf("out of range: %+v for %v nGold=%d k=%d", s, matches, nGold, k)
		}
		if s.Hit != (s.RR > 0) || s.Hit != (s.NDCG > 0) {
			t.Fatalf("hit, rr and ndcg disagree: %+v", s)
		}

		appended := ScoreRanking(append(append([]int{}, matches...), -1), nGold, k)
		if appended != s {
			t.Fatalf("appending a miss changed %+v to %+v (%v)", s, appended, matches)
		}

		if s.FirstRank > 0 {
			after := append(append(append([]int{}, matches[:s.FirstRank]...), -1), matches[s.FirstRank:]...)
			if got := ScoreRanking(after, nGold, k); got.RR != s.RR || got.FirstRank != s.FirstRank {
				t.Fatalf("a miss after the first gold hit changed MRR: %+v to %+v", s, got)
			}
		}

		front := ScoreRanking(append([]int{-1}, matches...), nGold, k)
		if front.RR > s.RR || front.NDCG > s.NDCG+eps {
			t.Fatalf("a miss in front raised %+v to %+v (%v)", s, front, matches)
		}

		perfect := make([]int, 0, min(nGold, k)+len(matches))
		for g := range min(nGold, k) {
			perfect = append(perfect, g)
		}
		if p := ScoreRanking(append(perfect, matches...), nGold, k); p.RR != 1 || math.Abs(p.NDCG-1) > eps {
			t.Fatalf("perfect ranking %v scored %+v", perfect, p)
		}
	})
}
