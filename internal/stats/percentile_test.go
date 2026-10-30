package stats

import "testing"

func TestQuantileCont(t *testing.T) {
	tests := []struct {
		name   string
		xs     []float64
		p      float64
		want   float64
		wantOK bool
	}{
		{"empty", nil, 0.5, 0, false},
		{"one", []float64{42}, 0.95, 42, true},
		{"two median", []float64{30, 20}, 0.5, 25, true},
		{"two p95", []float64{20, 30}, 0.95, 29.5, true},
		{"even median", []float64{110, 20, 95, 90}, 0.5, 92.5, true},
		{"odd median", []float64{120, 60, 100, 95, 110}, 0.5, 100, true},
		{"odd p95", []float64{120, 60, 100, 95, 110}, 0.95, 118, true},
		{"p0 is min", []float64{3, 1, 2}, 0, 1, true},
		{"p1 is max", []float64{3, 1, 2}, 1, 3, true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, ok := quantileCont(tt.xs, tt.p)
			if ok != tt.wantOK || got != tt.want {
				t.Errorf("quantileCont(%v, %v) = %v, %v; want %v, %v", tt.xs, tt.p, got, ok, tt.want, tt.wantOK)
			}
		})
	}
}

// TestRoundToScalesFirst pins half-away-from-zero after scaling: 137/40 is stored as
// 3.42499…, and must still show as 3.43.
func TestRoundToScalesFirst(t *testing.T) {
	for _, tt := range []struct {
		x      float64
		digits int
		want   float64
	}{
		{137.0 / 40, 2, 3.43},
		{118.04, 1, 118},
		{-2.5, 0, -3},
	} {
		if got := roundTo(tt.x, tt.digits); got != tt.want {
			t.Errorf("roundTo(%v, %d) = %v, want %v", tt.x, tt.digits, got, tt.want)
		}
	}
}
