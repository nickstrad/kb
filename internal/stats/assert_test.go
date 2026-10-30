//go:build !noassert

package stats

import (
	"testing"

	"github.com/nickstrad/kb/internal/assert"
)

// TestQuantileContRejectsP: p comes from the registry, so outside [0,1] is a kb bug (it would
// otherwise index out of range).
func TestQuantileContRejectsP(t *testing.T) {
	for _, p := range []float64{-0.01, 1.01} {
		func() {
			defer func() {
				if v, ok := recover().(assert.Violation); !ok || v.Kind != "precondition" {
					t.Errorf("p=%v: recovered %#v, want a precondition Violation", p, v)
				}
			}()
			quantileCont([]float64{1, 2}, p)
		}()
	}
	for _, p := range []float64{0, 1} {
		if _, ok := quantileCont([]float64{1, 2}, p); !ok {
			t.Errorf("p=%v rejected", p)
		}
	}
}
