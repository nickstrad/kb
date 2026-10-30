package eval

import (
	"math"
	"math/rand/v2"
	"slices"

	"github.com/nickstrad/kb/internal/assert"
)

// Score is one query's metrics at k. FirstRank is the rank (1-based) of the first hit that
// matches gold, 0 when none does within k.
type Score struct {
	FirstRank int     `json:"first_gold_rank"`
	Hit       bool    `json:"hit"`
	RR        float64 `json:"rr"`
	NDCG      float64 `json:"ndcg"`
}

// ScoreRanking scores one ranked list. matches[i] is the index of the gold item the hit at rank
// i+1 matches, or -1; nGold is the number of gold items (> 0). Only the first k hits count, and a
// gold item earns its gain once: a second hit on the same item (another chunk of an entry-level
// gold path, say) scores like a miss, so nDCG never exceeds 1. Gains are binary and the ideal DCG
// puts min(nGold, k) gold items at the top.
func ScoreRanking(matches []int, nGold, k int) Score {
	assert.Pre(nGold > 0, "ScoreRanking: nGold=%d, want > 0", nGold)
	assert.Pre(k > 0, "ScoreRanking: k=%d, want > 0", k)
	var s Score
	seen := make(map[int]bool, nGold)
	dcg := 0.0
	for i, g := range matches {
		if i >= k {
			break
		}
		if g < 0 || seen[g] {
			continue
		}
		assert.Pre(g < nGold, "ScoreRanking: match %d at rank %d outside %d gold items", g, i+1, nGold)
		seen[g] = true
		if s.FirstRank == 0 {
			s.FirstRank = i + 1
		}
		dcg += discount(i + 1)
	}
	if s.FirstRank > 0 {
		s.Hit = true
		s.RR = 1 / float64(s.FirstRank)
	}
	s.NDCG = dcg / idealDCG(nGold, k)
	if assert.Enabled {
		assert.Post(s.RR >= 0 && s.RR <= 1, "ScoreRanking: rr=%g outside [0,1]", s.RR)
		assert.Post(s.NDCG >= 0 && s.NDCG <= 1+1e-12, "ScoreRanking: ndcg=%g outside [0,1]", s.NDCG)
		assert.Post(s.Hit == (s.NDCG > 0), "ScoreRanking: hit=%v but ndcg=%g", s.Hit, s.NDCG)
	}
	return s
}

// discount is the DCG weight of rank r: 1/log2(r+1).
func discount(r int) float64 { return 1 / math.Log2(float64(r)+1) }

// idealDCG is the DCG of a ranking with min(nGold, k) gold items first.
func idealDCG(nGold, k int) float64 {
	d := 0.0
	for r := 1; r <= min(nGold, k); r++ {
		d += discount(r)
	}
	return d
}

// Estimate is a mean over queries with a percentile bootstrap confidence interval.
type Estimate struct {
	Mean float64 `json:"mean"`
	Lo   float64 `json:"ci_low"`
	Hi   float64 `json:"ci_high"`
}

// Bootstrap estimates the mean of xs with a (1-alpha) percentile bootstrap interval from
// resamples resamples drawn with a PCG generator seeded with seed, so the same xs, seed and
// resamples always give the same interval. xs must be non-empty.
func Bootstrap(xs []float64, resamples int, alpha float64, seed uint64) Estimate {
	assert.Pre(len(xs) > 0, "Bootstrap: no values")
	assert.Pre(resamples > 0, "Bootstrap: resamples=%d", resamples)
	assert.Pre(alpha > 0 && alpha < 1, "Bootstrap: alpha=%g", alpha)
	est := Estimate{Mean: mean(xs)}
	r := rand.New(rand.NewPCG(seed, seed^0x9e3779b97f4a7c15))
	means := make([]float64, resamples)
	for b := range means {
		sum := 0.0
		for range xs {
			sum += xs[r.IntN(len(xs))]
		}
		means[b] = sum / float64(len(xs))
	}
	slices.Sort(means)
	est.Lo = means[percentileIndex(alpha/2, resamples)]
	est.Hi = means[percentileIndex(1-alpha/2, resamples)]
	assert.Post(est.Lo <= est.Hi, "Bootstrap: lo=%g > hi=%g", est.Lo, est.Hi)
	return est
}

// percentileIndex is the index of the p-th percentile (0 <= p <= 1) among n sorted values: the
// smallest index with at least p*n values at or below it.
func percentileIndex(p float64, n int) int {
	i := int(math.Ceil(p*float64(n)-1e-9)) - 1 // the epsilon keeps 0.975*1000 at 975
	return max(0, i)                           // p <= 1 keeps i <= n-1
}

func mean(xs []float64) float64 {
	sum := 0.0
	for _, x := range xs {
		sum += x
	}
	return sum / float64(len(xs))
}
