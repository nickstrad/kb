package eval

import (
	"context"
	"testing"

	"github.com/nickstrad/kb/internal/store"
)

// TestOtherStoreGoldDecisions is Q77's decision table: gold a cross-store search recorded on
// another store's hit (Verdict.Store, OtherOpens) is counted in Report.OtherStore and never
// scored, never checked against this store's index (where a same-named path is another entry),
// and never a negative of this store's replay. kb eval replays this store only.
func TestOtherStoreGoldDecisions(t *testing.T) {
	const notes = "default@/notes"
	other := func(v store.Verdict) store.Verdict { v.Store = notes; return v }
	for _, tc := range []struct {
		name      string
		searches  []store.JudgedSearch
		hits      []string // the replay's hits for "q"
		want      OtherStoreGold
		chunkQ    int
		opensQ    int
		negatives int
		replayed  bool
		reads     bool // the corpus was read
		perQuery  int  // per_query[0].other_store_gold
	}{
		{name: "other store's useful hit, same path here: counted, not scored, index not read",
			searches: []store.JudgedSearch{{Verdicts: []store.Verdict{other(useful(1, "a.md", "H1"))}}},
			want:     OtherStoreGold{Chunks: 1, Queries: 1}, perQuery: 1},
		{name: "other store's open only: counted, not replayed",
			searches: []store.JudgedSearch{{OtherOpens: []store.StorePath{{Store: notes, Path: "a.md"}}}},
			want:     OtherStoreGold{Opens: 1, Queries: 1}, perQuery: 1},
		{name: "own and other gold: own scored, other counted",
			searches: []store.JudgedSearch{{Verdicts: []store.Verdict{useful(1, "a.md", "H1"), other(useful(2, "bb.md", "H2"))}}},
			hits:     []string{"a.md#H1"},
			want:     OtherStoreGold{Chunks: 1}, chunkQ: 1, replayed: true, reads: true, perQuery: 1},
		{name: "the same chunk useful here and there: both kept apart",
			searches: []store.JudgedSearch{{Verdicts: []store.Verdict{useful(1, "a.md", "H1"), other(useful(2, "a.md", "H1"))}}},
			hits:     []string{"a.md#H1"},
			want:     OtherStoreGold{Chunks: 1}, chunkQ: 1, replayed: true, reads: true, perQuery: 1},
		{name: "other store's not-useful hit is no negative here",
			searches: []store.JudgedSearch{{Verdicts: []store.Verdict{useful(1, "a.md", "H1"), other(notUseful(2, "bb.md", "H2"))}}},
			hits:     []string{"a.md#H1", "bb.md#H2"},
			chunkQ:   1, replayed: true, reads: true},
		{name: "own not-useful hit still a negative",
			searches: []store.JudgedSearch{{Verdicts: []store.Verdict{useful(1, "a.md", "H1"), notUseful(2, "bb.md", "H2")}}},
			hits:     []string{"a.md#H1", "bb.md#H2"},
			chunkQ:   1, negatives: 1, replayed: true, reads: true},
		{name: "one other chunk judged in two searches of the query counts once",
			searches: []store.JudgedSearch{
				{Verdicts: []store.Verdict{other(useful(1, "x.md", "H"))}},
				{ID: 2, Verdicts: []store.Verdict{other(useful(3, "x.md", "H"))}, OtherOpens: []store.StorePath{{Store: notes, Path: "x.md"}}},
			},
			want: OtherStoreGold{Chunks: 1, Opens: 1, Queries: 1}, perQuery: 2},
		{name: "two other stores, one path: two gold items",
			searches: []store.JudgedSearch{{Verdicts: []store.Verdict{other(useful(1, "x.md", "H")),
				{Rank: 2, Useful: true, Store: "adr@/loc", EntryPath: "x.md", Heading: "H"}}}},
			want: OtherStoreGold{Chunks: 2, Queries: 1}, perQuery: 2},
		{name: "own open with other useful gold: opens scored",
			searches: []store.JudgedSearch{{Verdicts: []store.Verdict{other(useful(1, "x.md", "H"))}, Opens: []string{"ccc.md"}}},
			hits:     []string{"ccc.md#"},
			want:     OtherStoreGold{Chunks: 1}, opensQ: 1, replayed: true, reads: true, perQuery: 1},
	} {
		t.Run(tc.name, func(t *testing.T) {
			for i := range tc.searches {
				if tc.searches[i].ID == 0 {
					tc.searches[i].ID = 1
				}
				tc.searches[i].Query = "q"
			}
			fs := &fakeSearcher{hits: map[string][]string{"q": tc.hits}}
			c := corpus()
			rep, err := Evaluate(context.Background(), tc.searches, c, fs, Options{Mode: "fts", K: 8})
			if err != nil {
				t.Fatal(err)
			}
			if rep.OtherStore != tc.want {
				t.Errorf("other_store %+v, want %+v", rep.OtherStore, tc.want)
			}
			if rep.Chunk.Queries != tc.chunkQ || rep.Opens.Queries != tc.opensQ || rep.NegativesInTopK != tc.negatives {
				t.Errorf("chunk %d opens %d negatives %d, want %d %d %d",
					rep.Chunk.Queries, rep.Opens.Queries, rep.NegativesInTopK, tc.chunkQ, tc.opensQ, tc.negatives)
			}
			if rep.Chunk.GoldNotInIndex+rep.Entry.GoldNotInIndex+rep.Opens.GoldNotInIndex != 0 {
				t.Errorf("other stores' gold counted as not in index: %+v %+v %+v", rep.Chunk, rep.Entry, rep.Opens)
			}
			if (len(fs.reqs) > 0) != tc.replayed {
				t.Errorf("replayed %d times, want replayed=%v", len(fs.reqs), tc.replayed)
			}
			if (c.reads > 0) != tc.reads {
				t.Errorf("corpus read %d times, want reads=%v", c.reads, tc.reads)
			}
			if rep.Queries != 1 || len(rep.PerQuery) != 1 || rep.PerQuery[0].OtherStoreGold != tc.perQuery {
				t.Errorf("queries %d per_query %+v, want one with other_store_gold %d", rep.Queries, rep.PerQuery, tc.perQuery)
			}
			qs, _ := Extract(tc.searches)
			if Replayable(qs) != tc.replayed {
				t.Errorf("Replayable = %v, want %v", Replayable(qs), tc.replayed)
			}
		})
	}
}
