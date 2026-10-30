package cli

import (
	"fmt"
	"testing"
	"time"
)

// TestParseSince is the --since grammar's boundary table: empty, zero, the day-suffix range edges
// (maxSinceDays and one above it, which would overflow time.Duration), negative durations, and
// date forms. The CLI tests only check that --since reaches the filter.
func TestParseSince(t *testing.T) {
	const slack = time.Minute // the bound is computed from time.Now inside parseSince
	for _, tc := range []struct {
		name    string
		in      string
		wantAgo time.Duration // used when wantErr is false and wantDate is ""
		zero    bool          // want the zero time (no bound)
		date    string        // want exactly this UTC date
		wantErr bool
	}{
		{name: "empty: no bound", in: "", zero: true},
		{name: "0d: now", in: "0d", wantAgo: 0},
		{name: "1d", in: "1d", wantAgo: 24 * time.Hour},
		{name: "7d", in: "7d", wantAgo: 7 * 24 * time.Hour},
		{name: "0s: now", in: "0s", wantAgo: 0},
		{name: "go duration 90m", in: "90m", wantAgo: 90 * time.Minute},
		{name: "at maxSinceDays", in: fmt.Sprintf("%dd", maxSinceDays), wantAgo: time.Duration(maxSinceDays) * 24 * time.Hour},
		{name: "maxSinceDays+1 overflows: error", in: fmt.Sprintf("%dd", maxSinceDays+1), wantErr: true},
		{name: "beyond int: error", in: "99999999999999999999d", wantErr: true},
		{name: "negative duration: error", in: "-1h", wantErr: true},
		{name: "negative zero is zero", in: "-0s", wantAgo: 0},
		{name: "fractional days: error", in: "1.5d", wantErr: true},
		{name: "bare d: error", in: "d", wantErr: true},
		{name: "date", in: "2026-01-02", date: "2026-01-02"},
		{name: "impossible date: error", in: "2026-02-30", wantErr: true},
		{name: "garbage: error", in: "bogus", wantErr: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got, err := parseSince(tc.in)
			if tc.wantErr {
				if err == nil {
					t.Fatalf("parseSince(%q) = %v, want an error", tc.in, got)
				}
				return
			}
			if err != nil {
				t.Fatalf("parseSince(%q): %v", tc.in, err)
			}
			switch {
			case tc.zero:
				if !got.IsZero() {
					t.Fatalf("parseSince(%q) = %v, want the zero time", tc.in, got)
				}
			case tc.date != "":
				if want, _ := time.Parse("2006-01-02", tc.date); !got.Equal(want) {
					t.Fatalf("parseSince(%q) = %v, want %v", tc.in, got, want)
				}
			default:
				want := time.Now().UTC().Add(-tc.wantAgo)
				if d := got.Sub(want); d > slack || d < -slack {
					t.Fatalf("parseSince(%q) = %v, want about %v", tc.in, got, want)
				}
			}
		})
	}
}
