//go:build !noassert

package eval

import (
	"errors"
	"testing"

	"github.com/nickstrad/kb/internal/assert"
)

// TestMetricPreconditions: calls only a kb programmer can get wrong fail a precondition.
func TestMetricPreconditions(t *testing.T) {
	for _, tc := range []struct {
		name string
		call func()
	}{
		{"no gold", func() { ScoreRanking([]int{-1}, 0, 8) }},
		{"k 0", func() { ScoreRanking([]int{0}, 1, 0) }},
		{"match beyond the gold", func() { ScoreRanking([]int{-1, 1}, 1, 8) }},
		{"bootstrap of nothing", func() { Bootstrap(nil, 10, Alpha, 1) }},
		{"zero resamples", func() { Bootstrap([]float64{1}, 0, Alpha, 1) }},
		{"alpha 0", func() { Bootstrap([]float64{1}, 10, 0, 1) }},
		{"alpha 1", func() { Bootstrap([]float64{1}, 10, 1, 1) }},
	} {
		t.Run(tc.name, func(t *testing.T) {
			defer func() {
				var v assert.Violation
				// The kind matters: a later postcondition would also catch most of these.
				if err, _ := recover().(error); !errors.As(err, &v) || v.Kind != "precondition" {
					t.Fatalf("recovered %v, want a precondition Violation", err)
				}
			}()
			tc.call()
		})
	}
	// The edges that are allowed: a match on the last gold item, alpha just inside (0, 1).
	ScoreRanking([]int{0}, 1, 1)
	Bootstrap([]float64{1}, 1, 0.999, 1)
}
