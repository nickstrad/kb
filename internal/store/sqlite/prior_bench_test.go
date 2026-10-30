package sqlite

import (
	"context"
	"fmt"
	"testing"

	"github.com/nickstrad/kb/internal/store"
)

// BenchmarkPriorReads measures the two store reads a search makes with [prior] enabled: Judged
// (the whole log, every search; Q133) on a log of 10k searches with 3 results and 3 verdicts each
// and one open per 10 searches, and LocateChunks for a full memory lookup (memoryMaxGold = 100
// refs) at 5k entries. Leverage entry search-latency-and-scale cites the numbers.
func BenchmarkPriorReads(b *testing.B) {
	ctx := context.Background()
	st := benchStore(b, 5000)
	if _, err := st.db.ExecContext(ctx, `
		WITH RECURSIVE seq(i) AS (SELECT 1 UNION ALL SELECT i + 1 FROM seq WHERE i < 10000)
		INSERT INTO searches (id, ts, query, mode, k, n_returned, caller, kb_version, settings, settings_hash)
		SELECT i, '2026-01-01T00:00:00Z', 'query ' || (i % 500), 'fts', 8, 3, 'bench', 'x', '{}', 'h' FROM seq;
		WITH RECURSIVE seq(i) AS (SELECT 1 UNION ALL SELECT i + 1 FROM seq WHERE i < 10000), r(k) AS (VALUES (1), (2), (3))
		INSERT INTO search_results (search_id, rank, chunk_id, entry_path, heading, rrf_score)
		SELECT i, k, 1, 'data/d' || ((i + k) % 50) || '/e' || ((i * 7 + k) % 5000) || '.md', '', 0.01 FROM seq, r;
		INSERT INTO search_feedback (search_id, rank, useful, ts)
		SELECT search_id, rank, rank % 2, '2026-01-01T00:01:00Z' FROM search_results;
		INSERT INTO search_opens (search_id, entry_path, ts)
		SELECT search_id, entry_path, '2026-01-01T00:02:00Z' FROM search_results WHERE rank = 1 AND search_id % 10 = 0;`); err != nil {
		b.Fatal(err)
	}
	b.Run("judged10k", func(b *testing.B) {
		n := 0
		for b.Loop() {
			got, err := st.Judged(ctx, store.JudgedFilter{})
			if err != nil {
				b.Fatal(err)
			}
			n = len(got)
		}
		b.ReportMetric(float64(n), "searches")
	})
	b.Run("locate100", func(b *testing.B) {
		refs := make([]store.ChunkRef, 100)
		for i := range refs {
			refs[i] = store.ChunkRef{Path: fmt.Sprintf("data/d%d/e%d.md", (i*37)%50, i*37), AnyHeading: i%2 == 0}
		}
		found := 0
		for b.Loop() {
			got, err := st.LocateChunks(ctx, refs, "")
			if err != nil {
				b.Fatal(err)
			}
			found = len(got)
		}
		b.ReportMetric(float64(found), "found")
	})
}
