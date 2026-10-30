package sqlite

import (
	"context"
	"fmt"
	"testing"
)

// BenchmarkRelated measures the Related walk (kb related, and search --neighbors once per hit at
// depth 1) on benchStore's graph: 5000 entries, 25000 edges, about 80% of them resolved. The
// "reach" step reads every edge whatever the depth. Superseded is the extra read every search
// makes (up to CandidateLimit paths). Leverage entry search-latency cites the numbers.
func BenchmarkRelated(b *testing.B) {
	ctx := context.Background()
	st := benchStore(b, 5000)
	if err := st.ResolveEdges(ctx); err != nil {
		b.Fatal(err)
	}
	const origin = "data/d0/e0.md"
	for depth := 1; depth <= 3; depth++ {
		b.Run(fmt.Sprintf("depth%d", depth), func(b *testing.B) {
			rows := 0
			for b.Loop() {
				got, err := st.Related(ctx, origin, depth)
				if err != nil {
					b.Fatal(err)
				}
				rows = len(got)
			}
			b.ReportMetric(float64(rows), "rows")
		})
	}
	b.Run("superseded40", func(b *testing.B) {
		paths := make([]string, 40)
		for i := range paths {
			paths[i] = fmt.Sprintf("data/d%d/e%d.md", i%50, i)
		}
		for b.Loop() {
			if _, err := st.Superseded(ctx, paths); err != nil {
				b.Fatal(err)
			}
		}
	})
}
