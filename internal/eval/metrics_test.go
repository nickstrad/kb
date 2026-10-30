package eval

import (
	"math"
	"testing"
)

const eps = 1e-12

// Hand-computed DCG discounts: 1/log2(r+1).
const (
	d1 = 1.0
	d2 = 0.6309297535714575 // 1/log2(3)
	d3 = 0.5                // 1/log2(4)
)

// TestScoreRanking is the metric table: each row is computed by hand from the formulas in
// ScoreRanking's doc comment. -1 is a hit that matches no gold.
func TestScoreRanking(t *testing.T) {
	for _, tc := range []struct {
		name    string
		matches []int
		nGold   int
		k       int
		want    Score
	}{
		{"k=1, gold first", []int{0, -1}, 1, 1, Score{FirstRank: 1, Hit: true, RR: 1, NDCG: 1}},
		{"k=1, gold second: beyond k", []int{-1, 0}, 1, 1, Score{}},
		{"no hits at all", nil, 1, 8, Score{}},
		{"no hit matches gold", []int{-1, -1, -1}, 2, 8, Score{}},
		{"gold at rank 2", []int{-1, 0, -1}, 1, 8, Score{FirstRank: 2, Hit: true, RR: 0.5, NDCG: d2}},
		{"gold exactly at k", []int{-1, -1, 0}, 1, 3, Score{FirstRank: 3, Hit: true, RR: 1.0 / 3, NDCG: d3}},
		{"gold one past k", []int{-1, -1, -1, 0}, 1, 3, Score{}},
		{"two gold at 1 and 3", []int{0, -1, 1}, 2, 8, Score{FirstRank: 1, Hit: true, RR: 1, NDCG: (d1 + d3) / (d1 + d2)}},
		{"two gold, one found", []int{-1, 1}, 2, 8, Score{FirstRank: 2, Hit: true, RR: 0.5, NDCG: d2 / (d1 + d2)}},
		{"tie: two hits on one gold item earn once", []int{0, 0}, 2, 8, Score{FirstRank: 1, Hit: true, RR: 1, NDCG: d1 / (d1 + d2)}},
		{"tie on the only gold item: still perfect", []int{0, 0, 0}, 1, 8, Score{FirstRank: 1, Hit: true, RR: 1, NDCG: 1}},
		{"more gold than k: ideal uses k", []int{2, 0}, 3, 2, Score{FirstRank: 1, Hit: true, RR: 1, NDCG: 1}},
		{"more gold than k, one found", []int{-1, 1}, 3, 2, Score{FirstRank: 2, Hit: true, RR: 0.5, NDCG: d2 / (d1 + d2)}},
		{"reversed perfect order is still perfect", []int{1, 0}, 2, 8, Score{FirstRank: 1, Hit: true, RR: 1, NDCG: 1}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got := ScoreRanking(tc.matches, tc.nGold, tc.k)
			if !sameScore(got, tc.want) {
				t.Errorf("ScoreRanking(%v, %d, %d) = %+v, want %+v", tc.matches, tc.nGold, tc.k, got, tc.want)
			}
		})
	}
}

// TestPercentileIndex covers the percentile edges, including the float product 0.975*1000.
func TestPercentileIndex(t *testing.T) {
	for _, tc := range []struct {
		p    float64
		n    int
		want int
	}{
		{0.025, 1000, 24},
		{1 - 0.05/2, 1000, 974},
		{0, 10, 0},
		{1, 10, 9},
		{0.5, 1, 0},
		{0.5, 4, 1},
		{0.51, 4, 2},
	} {
		if got := percentileIndex(tc.p, tc.n); got != tc.want {
			t.Errorf("percentileIndex(%g, %d) = %d, want %d", tc.p, tc.n, got, tc.want)
		}
	}
}

// TestBootstrap: a constant sample has a zero-width interval; one value is its own interval; the
// interval of a mixed sample brackets the mean and is the same for the same seed.
func TestBootstrap(t *testing.T) {
	if got := Bootstrap([]float64{0.5, 0.5, 0.5}, 200, Alpha, 1); got != (Estimate{0.5, 0.5, 0.5}) {
		t.Errorf("constant sample: %+v", got)
	}
	if got := Bootstrap([]float64{1}, 50, Alpha, 1); got != (Estimate{1, 1, 1}) {
		t.Errorf("one value: %+v", got)
	}
	xs := []float64{0, 1, 0, 1, 1, 0.5, 0.25, 0, 1, 1}
	a := Bootstrap(xs, DefaultResamples, Alpha, DefaultSeed)
	if a.Mean != 0.575 || !(a.Lo < a.Mean && a.Mean < a.Hi) || a.Lo < 0 || a.Hi > 1 {
		t.Errorf("mixed sample: %+v", a)
	}
	if b := Bootstrap(xs, DefaultResamples, Alpha, DefaultSeed); b != a {
		t.Errorf("same seed, different interval: %+v vs %+v", a, b)
	}
	if c := Bootstrap(xs, DefaultResamples, Alpha, DefaultSeed+1); c == a {
		t.Errorf("another seed gave the identical interval %+v: is the seed used?", c)
	}
	narrow := Bootstrap(xs, DefaultResamples, 0.5, DefaultSeed)
	if !(narrow.Hi-narrow.Lo < a.Hi-a.Lo) {
		t.Errorf("a 50%% interval %+v is not narrower than the 95%% one %+v", narrow, a)
	}
}

// TestBootstrapPinned pins the interval for a fixed sample and seed: a change in the generator,
// the resampling or the percentile rule shows up here.
func TestBootstrapPinned(t *testing.T) {
	got := Bootstrap([]float64{0, 1, 0, 1, 1, 0.5, 0.25, 0, 1, 1}, DefaultResamples, Alpha, DefaultSeed)
	want := Estimate{Mean: 0.575, Lo: 0.3, Hi: 0.825}
	if math.Abs(got.Lo-want.Lo) > eps || math.Abs(got.Hi-want.Hi) > eps {
		t.Errorf("Bootstrap = %+v, want %+v", got, want)
	}
}
