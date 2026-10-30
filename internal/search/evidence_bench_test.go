package search

import (
	"context"
	"fmt"
	"path/filepath"
	"testing"
	"time"

	"github.com/nickstrad/kb/internal/store"
	"github.com/nickstrad/kb/internal/store/sqlite"
)

// benchPriorStore is an FTS-only index of four one-chunk entries that "sprocket" finds.
func benchPriorStore(b *testing.B) *sqlite.Store {
	ctx := context.Background()
	st, err := sqlite.Open(ctx, filepath.Join(b.TempDir(), "kb.sqlite"), sqlite.Options{})
	if err != nil {
		b.Fatal(err)
	}
	b.Cleanup(func() { st.Close() })
	if err := st.EnsureEmbedMeta(ctx, store.EmbedMeta{Model: store.FTSOnlyModel}); err != nil {
		b.Fatal(err)
	}
	for _, p := range []string{"old.md", "new.md", "other.md", "hidden.md"} {
		in := store.EntryInput{Path: p, Kind: "file", Title: p}
		if _, err := st.ReplaceEntry(ctx, in, []store.ChunkInput{{Ord: 0, SourceFile: p, Text: "sprocket calibration " + p, TextHash: p}}); err != nil {
			b.Fatal(err)
		}
	}
	return st
}

// BenchmarkReplayPrior is an eval replay with [prior] and the query memory on over a 2k-search
// judged log (each search one useful verdict a minute after it; queries over 100 keys): 200
// replayed searches per op, each as of its own search's time without itself (PLAN 6.4).
// "read-per-replay" is the pre-6.4 cost (Search reads the whole log and builds the prior each
// time, O(N) per replay, O(N²) per eval); "prepared" reads it once and slices per replay (Q133).
// Leverage entry search-latency-and-scale cites the numbers.
func BenchmarkReplayPrior(b *testing.B) {
	ctx := context.Background()
	st := benchPriorStore(b)
	const n = 2000
	start := time.Date(2026, 6, 1, 0, 0, 0, 0, time.UTC)
	tsOf := func(i int) time.Time { return start.Add(time.Duration(i) * time.Minute) }
	paths := []string{"old.md", "new.md", "other.md", "hidden.md"}
	for i := range n {
		id, err := st.LogSearch(ctx, store.SearchRecord{TS: tsOf(i), Query: fmt.Sprintf("sprocket q%d", i%100), Mode: "fts", K: 8, Caller: "c",
			Results: []store.LoggedHit{{ChunkID: 1, EntryPath: paths[i%len(paths)], RRFScore: 1}}})
		if err != nil {
			b.Fatal(err)
		}
		if err := st.Feedback(ctx, id, 1, i%3 != 0, "", tsOf(i).Add(30*time.Second)); err != nil {
			b.Fatal(err)
		}
	}
	replay := func(b *testing.B, s *Searcher) {
		for r := range 200 {
			i := n/2 + r*(n/2)/200 // replays from the middle of the log on: half of it as evidence and more
			req := Request{Query: fmt.Sprintf("sprocket q%d", i%100), Mode: ModeFTS, AsOf: tsOf(i), ExcludeEvidence: []int64{int64(i + 1)}}
			out, err := s.Search(ctx, req)
			if err != nil {
				b.Fatal(err)
			}
			if !out.Prior.Active {
				b.Fatalf("prior inactive: %+v", out.Prior)
			}
		}
	}
	b.Run("read-per-replay", func(b *testing.B) {
		s := evidenceSearcher(b, st, st)
		for b.Loop() {
			replay(b, s)
		}
		b.ReportMetric(float64(b.Elapsed().Microseconds())/float64(b.N*200)/1000, "ms/replay")
	})
	b.Run("prepared", func(b *testing.B) {
		for b.Loop() {
			judged, err := st.Judged(ctx, store.JudgedFilter{})
			if err != nil {
				b.Fatal(err)
			}
			replay(b, evidenceSearcher(b, st, NewEvidence(judged)))
		}
		b.ReportMetric(float64(b.Elapsed().Microseconds())/float64(b.N*200)/1000, "ms/replay")
	})
}
