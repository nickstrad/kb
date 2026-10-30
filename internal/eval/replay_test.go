package eval

import (
	"context"
	"errors"
	"math"
	"reflect"
	"strings"
	"testing"

	"github.com/nickstrad/kb/internal/errs"
	"github.com/nickstrad/kb/internal/search"
	"github.com/nickstrad/kb/internal/store"
)

// fakeSearcher answers each query text with scripted hits ("path#heading") and records requests.
type fakeSearcher struct {
	hits     map[string][]string
	fallback map[string]bool
	err      error
	reqs     []search.Request
}

func (f *fakeSearcher) Search(_ context.Context, req search.Request) (*search.Outcome, error) {
	f.reqs = append(f.reqs, req)
	if f.err != nil {
		return nil, f.err
	}
	res := search.NewResult(req.Query, req.Mode)
	for i, h := range f.hits[req.Query] {
		path, heading, _ := strings.Cut(h, "#")
		res.Hits = append(res.Hits, search.Hit{Rank: i + 1, Path: path, Heading: heading})
	}
	out := &search.Outcome{Result: res, Mode: req.Mode}
	if f.fallback[req.Query] {
		out.Mode, out.FallbackErr = search.ModeFTSFallback, errors.New("embedder down for "+req.Query)
	}
	return out, nil
}

// fakeCorpus is an index: path -> headings of its chunks. errPath fails its Entry read, errChunks
// its Chunks read.
type fakeCorpus struct {
	entries   map[string][]string
	errPath   string
	errChunks string
	reads     int
}

func (c *fakeCorpus) Entry(_ context.Context, path string) (*store.EntryRow, error) {
	c.reads++
	if path == c.errPath {
		return nil, errs.New(errs.Conflict, "fake.Entry", "locked")
	}
	if _, ok := c.entries[path]; !ok {
		return nil, nil
	}
	return &store.EntryRow{ID: int64(len(path)), Path: path}, nil
}

func (c *fakeCorpus) Chunks(_ context.Context, id int64) ([]store.ChunkRow, error) {
	for p, hs := range c.entries {
		if int64(len(p)) != id {
			continue
		}
		if p == c.errChunks {
			return nil, errors.New("disk")
		}
		out := make([]store.ChunkRow, len(hs))
		for i, h := range hs {
			out[i] = store.ChunkRow{Heading: h}
		}
		return out, nil
	}
	return nil, nil
}

// corpus has a.md (intro + H1), bb.md (H2), ccc.md (intro): distinct path lengths are the ids.
func corpus() *fakeCorpus {
	return &fakeCorpus{entries: map[string][]string{"a.md": {"", "H1"}, "bb.md": {"H2"}, "ccc.md": {""}}}
}

// TestEvaluateGoldInIndex is the gold-vs-index decision table: a renamed heading drops chunk gold
// but keeps the entry; a deleted entry drops both; an opened entry that is gone is counted apart.
func TestEvaluateGoldInIndex(t *testing.T) {
	for _, tc := range []struct {
		name                                     string
		search                                   store.JudgedSearch
		chunkQ, entryQ, opensQ                   int
		chunkMissing, entryMissing, opensMissing int
		chunkDropped, entryDropped, opensDropped int
		replayed                                 bool
	}{
		{name: "gold in index",
			search: store.JudgedSearch{Verdicts: []store.Verdict{useful(1, "a.md", "H1")}},
			chunkQ: 1, entryQ: 1, replayed: true},
		{name: "renamed heading: chunk gold gone, entry stays",
			search: store.JudgedSearch{Verdicts: []store.Verdict{useful(1, "a.md", "Old")}},
			entryQ: 1, chunkMissing: 1, chunkDropped: 1, replayed: true},
		{name: "deleted entry: nothing left, not replayed",
			search:       store.JudgedSearch{Verdicts: []store.Verdict{useful(1, "gone.md", "H"), useful(2, "gone.md", "")}},
			chunkMissing: 2, entryMissing: 1, chunkDropped: 1, entryDropped: 1},
		{name: "deleted entry, one gold item",
			search:       store.JudgedSearch{Verdicts: []store.Verdict{useful(1, "gone.md", "")}},
			chunkMissing: 1, entryMissing: 1, chunkDropped: 1, entryDropped: 1},
		{name: "one gold left of two",
			search: store.JudgedSearch{Verdicts: []store.Verdict{useful(1, "gone.md", "H"), useful(2, "bb.md", "H2")}},
			chunkQ: 1, entryQ: 1, chunkMissing: 1, entryMissing: 1, replayed: true},
		{name: "opened entry deleted",
			search:       store.JudgedSearch{Opens: []string{"gone.md"}},
			opensMissing: 1, opensDropped: 1},
		{name: "opened entry present: opens only",
			search: store.JudgedSearch{Opens: []string{"ccc.md"}},
			opensQ: 1, replayed: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			s := tc.search
			s.ID, s.Query = 1, "q"
			fs := &fakeSearcher{}
			rep, err := Evaluate(context.Background(), []store.JudgedSearch{s}, corpus(), fs, Options{Mode: "fts", K: 8})
			if err != nil {
				t.Fatal(err)
			}
			for _, v := range []struct {
				name                      string
				got                       Variant
				queries, missing, dropped int
			}{
				{"chunk", rep.Chunk, tc.chunkQ, tc.chunkMissing, tc.chunkDropped},
				{"entry", rep.Entry, tc.entryQ, tc.entryMissing, tc.entryDropped},
				{"opens", rep.Opens, tc.opensQ, tc.opensMissing, tc.opensDropped},
			} {
				if v.got.Queries != v.queries || v.got.GoldNotInIndex != v.missing || v.got.QueriesDropped != v.dropped {
					t.Errorf("%s: queries %d missing %d dropped %d, want %d %d %d", v.name,
						v.got.Queries, v.got.GoldNotInIndex, v.got.QueriesDropped, v.queries, v.missing, v.dropped)
				}
				if (v.got.Queries == 0) != (v.got.MRR == nil) {
					t.Errorf("%s: estimate %v with %d queries", v.name, v.got.MRR, v.got.Queries)
				}
			}
			if (len(fs.reqs) > 0) != tc.replayed {
				t.Errorf("replayed %d times, want replayed=%v", len(fs.reqs), tc.replayed)
			}
		})
	}
}

// TestEvaluateScores: the replay gets the options' mode and k and the newest text; scores per
// variant follow the hits; negatives are counted; opens never reach the useful variants.
func TestEvaluateScores(t *testing.T) {
	searches := []store.JudgedSearch{
		{ID: 1, Query: "Alpha", Verdicts: []store.Verdict{useful(2, "a.md", "H1"), notUseful(1, "bb.md", "H2")}},
		{ID: 2, Query: "alpha", Opens: []string{"ccc.md"}},
		{ID: 3, Query: "beta", Verdicts: []store.Verdict{useful(1, "bb.md", "H2")}},
		{ID: 4, Query: "gamma", None: true},
	}
	fs := &fakeSearcher{hits: map[string][]string{
		"alpha": {"bb.md#H2", "a.md#", "a.md#H1", "ccc.md#", "bb.md#H2"}, // the last is past k
		"beta":  {"a.md#H1"},
	}}
	rep, err := Evaluate(context.Background(), searches, corpus(), fs, Options{Mode: "hybrid", K: 3, MinJudged: 3, SettingsHash: "h"})
	if err != nil {
		t.Fatal(err)
	}
	wantReqs := []search.Request{{Query: "alpha", Mode: "hybrid", K: 3}, {Query: "beta", Mode: "hybrid", K: 3}}
	if !reflect.DeepEqual(fs.reqs, wantReqs) {
		t.Errorf("requests %+v, want %+v", fs.reqs, wantReqs)
	}
	alpha, beta := rep.PerQuery[0], rep.PerQuery[1]
	// alpha: gold a.md#H1 at rank 3 (chunk); a.md at rank 2 (entry); ccc.md beyond k=3 (opens).
	if !sameScore(*alpha.Chunk, Score{FirstRank: 3, Hit: true, RR: 1.0 / 3, NDCG: d3}) {
		t.Errorf("alpha chunk %+v", *alpha.Chunk)
	}
	if !sameScore(*alpha.Entry, Score{FirstRank: 2, Hit: true, RR: 0.5, NDCG: d2}) {
		t.Errorf("alpha entry %+v", *alpha.Entry)
	}
	if *alpha.Opens != (Score{}) {
		t.Errorf("alpha opens %+v: ccc.md is at rank 4 > k", *alpha.Opens)
	}
	if alpha.NegativesInTopK != 1 || beta.NegativesInTopK != 0 || rep.NegativesInTopK != 1 {
		t.Errorf("negatives %d %d %d", alpha.NegativesInTopK, beta.NegativesInTopK, rep.NegativesInTopK)
	}
	if beta.Opens != nil || *beta.Chunk != (Score{}) {
		t.Errorf("beta %+v %+v", beta.Opens, beta.Chunk)
	}
	if rep.Chunk.Queries != 2 || rep.Opens.Queries != 1 || rep.Queries != 2 {
		t.Errorf("queries chunk %d opens %d total %d", rep.Chunk.Queries, rep.Opens.Queries, rep.Queries)
	}
	if want := (1.0/3 + 0) / 2; math.Abs(rep.Chunk.MRR.Mean-want) > eps {
		t.Errorf("chunk mrr %g, want %g", rep.Chunk.MRR.Mean, want)
	}
	if rep.Chunk.HitAtK.Mean != 0.5 || rep.Entry.HitAtK.Mean != 0.5 {
		t.Errorf("hit@k chunk %g entry %g", rep.Chunk.HitAtK.Mean, rep.Entry.HitAtK.Mean)
	}
	if !rep.SmallSample || rep.Searches != (SearchCounts{Total: 4, Useful: 2, NoneOnly: 1, Unjudged: 1, WithOpens: 1}) {
		t.Errorf("small %v counts %+v", rep.SmallSample, rep.Searches)
	}
	if rep.Mode != "hybrid" || rep.K != 3 || rep.SettingsHash != "h" || rep.Resamples != 1000 ||
		rep.Seed != DefaultSeed || rep.Confidence != 1-Alpha || rep.FallbackQueries != 0 {
		t.Errorf("header %+v", rep)
	}
}

// TestEvaluateSmallSampleBoundary: the caveat is on below min_judged and off at it; 0 turns it off.
func TestEvaluateSmallSampleBoundary(t *testing.T) {
	searches := []store.JudgedSearch{
		{ID: 1, Query: "a", Verdicts: []store.Verdict{useful(1, "a.md", "H1")}},
		{ID: 2, Query: "b", Verdicts: []store.Verdict{useful(1, "a.md", "H1")}},
	}
	for _, tc := range []struct {
		min  int
		want bool
	}{{0, false}, {1, false}, {2, false}, {3, true}} {
		rep, err := Evaluate(context.Background(), searches, corpus(), &fakeSearcher{}, Options{Mode: "fts", K: 8, MinJudged: tc.min})
		if err != nil {
			t.Fatal(err)
		}
		if rep.SmallSample != tc.want {
			t.Errorf("min_judged %d: small sample %v, want %v", tc.min, rep.SmallSample, tc.want)
		}
		if rep.Chunk.HitAtK.Mean != 0 || rep.Chunk.MRR.Mean != 0 { // the fake returns no hits
			t.Errorf("no hits scored hit@k %g mrr %g", rep.Chunk.HitAtK.Mean, rep.Chunk.MRR.Mean)
		}
	}
}

// TestEvaluateFallback counts replays that ran in fts-fallback and keeps the first reason.
func TestEvaluateFallback(t *testing.T) {
	searches := []store.JudgedSearch{
		{ID: 1, Query: "a", Verdicts: []store.Verdict{useful(1, "a.md", "H1")}},
		{ID: 2, Query: "b", Verdicts: []store.Verdict{useful(1, "a.md", "H1")}},
	}
	searches = append(searches, store.JudgedSearch{ID: 3, Query: "c", Verdicts: []store.Verdict{useful(1, "a.md", "H1")}})
	fs := &fakeSearcher{fallback: map[string]bool{"b": true, "c": true}}
	rep, err := Evaluate(context.Background(), searches, corpus(), fs, Options{Mode: "hybrid", K: 8})
	if err != nil {
		t.Fatal(err)
	}
	if rep.FallbackQueries != 2 || rep.FallbackReason != "embedder down for b" ||
		rep.PerQuery[0].Mode != search.ModeHybrid || rep.PerQuery[1].Mode != search.ModeFTSFallback {
		t.Errorf("fallback %d %q %q %q", rep.FallbackQueries, rep.FallbackReason, rep.PerQuery[0].Mode, rep.PerQuery[1].Mode)
	}
}

// TestEvaluateErrors: a failed replay or index read stops the run and keeps its kind.
func TestEvaluateErrors(t *testing.T) {
	searches := []store.JudgedSearch{{ID: 1, Query: "q", Verdicts: []store.Verdict{useful(1, "a.md", "H1")}}}
	for _, tc := range []struct {
		name   string
		corpus *fakeCorpus
		s      *fakeSearcher
		kind   errs.Kind
	}{
		{"replay unavailable", corpus(), &fakeSearcher{err: errs.New(errs.Unavailable, "embed", "down")}, errs.Unavailable},
		{"entry read locked", &fakeCorpus{entries: corpus().entries, errPath: "a.md"}, &fakeSearcher{}, errs.Conflict},
		{"chunks read fails", &fakeCorpus{entries: corpus().entries, errChunks: "a.md"}, &fakeSearcher{}, errs.Internal},
	} {
		t.Run(tc.name, func(t *testing.T) {
			rep, err := Evaluate(context.Background(), searches, tc.corpus, tc.s, Options{Mode: "fts", K: 8})
			if err == nil || rep != nil {
				t.Fatalf("got %+v, %v; want an error", rep, err)
			}
			var e *errs.Error
			if !errors.As(err, &e) || errs.KindOf(err) != tc.kind {
				t.Errorf("err %v: kind %v, want %v", err, errs.KindOf(err), tc.kind)
			}
		})
	}
	// An opened path whose read fails stops the run too.
	opens := []store.JudgedSearch{{ID: 1, Query: "q", Opens: []string{"a.md"}}}
	if _, err := Evaluate(context.Background(), opens, &fakeCorpus{entries: corpus().entries, errPath: "a.md"}, &fakeSearcher{}, Options{Mode: "fts", K: 8}); errs.KindOf(err) != errs.Conflict {
		t.Errorf("opens read: %v", err)
	}
}

// TestEvaluateEmpty: no gold means no replay, no estimates and an empty, non-nil per-query list.
func TestEvaluateEmpty(t *testing.T) {
	fs := &fakeSearcher{}
	rep, err := Evaluate(context.Background(), []store.JudgedSearch{{ID: 1, Query: "q", None: true}}, corpus(), fs, Options{Mode: "fts", K: 8, MinJudged: 1})
	if err != nil {
		t.Fatal(err)
	}
	if len(fs.reqs) != 0 || rep.PerQuery == nil || len(rep.PerQuery) != 0 || rep.Chunk.MRR != nil || !rep.SmallSample {
		t.Errorf("empty report %+v (requests %d)", rep, len(fs.reqs))
	}
}

// TestEvaluateDeterministic: the same inputs give the same report, intervals included, whatever
// the order of the log; the corpus is read once per path.
func TestEvaluateDeterministic(t *testing.T) {
	searches := []store.JudgedSearch{
		{ID: 1, Query: "a", Verdicts: []store.Verdict{useful(1, "a.md", "H1")}},
		{ID: 2, Query: "b", Verdicts: []store.Verdict{useful(1, "bb.md", "H2"), useful(2, "a.md", "")}},
		{ID: 3, Query: "c", Verdicts: []store.Verdict{useful(1, "ccc.md", "")}},
	}
	hits := map[string][]string{"a": {"bb.md#H2", "a.md#H1"}, "b": {"a.md#"}, "c": {"ccc.md#"}}
	c := corpus()
	first, err := Evaluate(context.Background(), searches, c, &fakeSearcher{hits: hits}, Options{Mode: "fts", K: 8})
	if err != nil {
		t.Fatal(err)
	}
	if c.reads != 3 {
		t.Errorf("corpus read %d times, want once per path (3)", c.reads)
	}
	reversed := []store.JudgedSearch{searches[2], searches[1], searches[0]}
	second, err := Evaluate(context.Background(), reversed, corpus(), &fakeSearcher{hits: hits}, Options{Mode: "fts", K: 8})
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(first, second) {
		t.Errorf("reports differ:\n%+v\n%+v", first, second)
	}
	if first.Chunk.MRR.Lo == first.Chunk.MRR.Hi {
		t.Errorf("degenerate interval %+v for a mixed sample", first.Chunk.MRR)
	}
}

// sameScore compares scores with eps on the floats.
func sameScore(a, b Score) bool {
	return a.FirstRank == b.FirstRank && a.Hit == b.Hit && math.Abs(a.RR-b.RR) <= eps && math.Abs(a.NDCG-b.NDCG) <= eps
}
