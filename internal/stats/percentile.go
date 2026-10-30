package stats

import (
	"math"
	"sort"

	"github.com/nickstrad/kb/internal/assert"
)

// quantileCont is the continuous quantile of xs at p in [0, 1]: linear interpolation between the
// two order statistics around position p*(n-1), as DuckDB's quantile_cont computes it. It sorts
// xs in place. ok is false when xs is empty, which a query reports as NULL.
func quantileCont(xs []float64, p float64) (v float64, ok bool) {
	assert.Pre(p >= 0 && p <= 1, "quantileCont: p=%v outside [0,1]", p)
	if len(xs) == 0 {
		return 0, false
	}
	sort.Float64s(xs)
	pos := p * float64(len(xs)-1)
	lo := math.Floor(pos)
	hi := math.Ceil(pos)
	if lo == hi {
		return xs[int(lo)], true
	}
	a, b := xs[int(lo)], xs[int(hi)]
	return a + (b-a)*(pos-lo), true
}

// roundTo rounds x half away from zero to digits decimal places, the way DuckDB's round(x, d)
// does for a DOUBLE.
func roundTo(x float64, digits int) float64 {
	m := math.Pow(10, float64(digits))
	return math.Round(x*m) / m
}
