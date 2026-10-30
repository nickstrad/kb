package prior

import (
	"fmt"
	"testing"
	"time"
)

// BenchmarkBuild measures Build on the evidence of a 10k-search judged log (30k verdicts, 1k
// opens), the work every search with [prior] enabled does after reading the log. Leverage entry
// search-latency-and-scale cites the numbers.
func BenchmarkBuild(b *testing.B) {
	asOf := time.Date(2026, 6, 1, 0, 0, 0, 0, time.UTC)
	var events []Event
	for i := range 10000 {
		at := asOf.Add(-time.Duration(i) * time.Minute)
		for k := 1; k <= 3; k++ {
			kind := Useful
			if k%2 == 0 {
				kind = NotUseful
			}
			events = append(events, Event{SearchID: int64(i), EntryPath: fmt.Sprintf("e%d.md", (i*7+k)%5000), Kind: kind, Rank: k, At: at})
		}
		if i%10 == 0 {
			events = append(events, Event{SearchID: int64(i), EntryPath: fmt.Sprintf("e%d.md", i%5000), Kind: Open, At: at})
		}
	}
	p := Params{Enabled: true, MinJudged: 50, HalfLifeDays: 90, MaxBoost: 0.2}
	for b.Loop() {
		if _, err := Build(events, p, asOf); err != nil {
			b.Fatal(err)
		}
	}
	b.ReportMetric(float64(len(events)), "events")
}
