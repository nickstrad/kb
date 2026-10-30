package search

import (
	"context"
	"math"
	"testing"

	"github.com/nickstrad/kb/internal/config"
	"github.com/nickstrad/kb/internal/embed/fake"
	"github.com/nickstrad/kb/internal/errs"
)

func explain(t *testing.T, s *Searcher, req Request) *Explanation {
	t.Helper()
	ex, err := s.Explain(context.Background(), req)
	if err != nil {
		t.Fatalf("Explain(%+v): %v", req, err)
	}
	return ex
}

func countStatus(ex *Explanation, status string) int {
	n := 0
	for _, h := range ex.Candidates {
		if h.Status == status {
			n++
		}
	}
	return n
}

// TestExplainDecisionTable: one row per way a candidate can be placed, and the trace header that
// goes with it.
func TestExplainDecisionTable(t *testing.T) {
	s, _ := testSearcher(t)
	for _, tc := range []struct {
		name        string
		req         Request
		wantMode    string
		wantMatch   string // "" = no FTS ran
		wantOp      string
		wantOrRetry bool
		check       func(t *testing.T, ex *Explanation)
	}{
		{
			name: "fts only", req: Request{Query: "quokka", Mode: ModeFTS, K: 8},
			wantMode: ModeFTS, wantMatch: `"quokka"`, wantOp: "AND",
			check: func(t *testing.T, ex *Explanation) {
				if len(ex.Candidates) != 1 {
					t.Fatalf("candidates = %d, want 1", len(ex.Candidates))
				}
				h := ex.Candidates[0]
				if h.FTS == nil || h.Vec != nil || len(h.RRF) != 1 || h.RRF[0].List != "fts" || h.Status != StatusReturned {
					t.Errorf("fts-only hit = %+v", h)
				}
			},
		},
		{
			name: "vec only", req: Request{Query: "container plumbing", Mode: ModeVec, K: 5},
			wantMode: ModeVec,
			check: func(t *testing.T, ex *Explanation) {
				if len(ex.Candidates) == 0 {
					t.Fatal("no candidates")
				}
				for _, h := range ex.Candidates {
					if h.FTS != nil || h.Vec == nil || h.Columns != nil || len(h.RRF) != 1 || h.RRF[0].List != "vec" {
						t.Errorf("vec-only hit = %+v", h)
					}
				}
			},
		},
		{
			name: "both lists", req: Request{Query: "zebracorn", Mode: ModeHybrid, K: 8},
			wantMode: ModeHybrid, wantMatch: `"zebracorn"`, wantOp: "AND",
			check: func(t *testing.T, ex *Explanation) {
				both := 0
				for _, h := range ex.Candidates {
					if h.FTS != nil && h.Vec != nil {
						both++
						if len(h.RRF) != 2 {
							t.Errorf("chunk %d in both lists has %d rrf terms", h.ChunkID, len(h.RRF))
						}
					}
				}
				if both == 0 {
					t.Error("no candidate is in both lists")
				}
			},
		},
		{
			name: "capped", req: Request{Query: "zebracorn", Mode: ModeFTS, K: 20},
			wantMode: ModeFTS, wantMatch: `"zebracorn"`, wantOp: "AND",
			check: func(t *testing.T, ex *Explanation) {
				// alpha.md has five zebracorn chunks and the cap is 3.
				if got := countStatus(ex, StatusReturned); got != 3 {
					t.Errorf("returned = %d, want 3", got)
				}
				if got := countStatus(ex, StatusCapDropped); got != 2 {
					t.Errorf("cap_dropped = %d, want 2", got)
				}
			},
		},
		{
			name: "below k", req: Request{Query: "widget", Mode: ModeFTS, K: 2},
			wantMode: ModeFTS, wantMatch: `"widget"`, wantOp: "AND",
			check: func(t *testing.T, ex *Explanation) {
				if got := countStatus(ex, StatusReturned); got != 2 {
					t.Errorf("returned = %d, want 2", got)
				}
				if countStatus(ex, StatusBelowK) == 0 {
					t.Error("no candidate is below_k")
				}
				for _, h := range ex.Candidates {
					if (h.Status == StatusReturned) != (h.ReturnedRank != nil) {
						t.Errorf("chunk %d: status %s with returned_rank %v", h.ChunkID, h.Status, h.ReturnedRank)
					}
				}
			},
		},
		{
			name: "or fallback", req: Request{Query: "quokka fluffernutter", Mode: ModeFTS, K: 8},
			wantMode: ModeFTS, wantMatch: `"quokka" OR "fluffernutter"`, wantOp: "OR", wantOrRetry: true,
			check: func(t *testing.T, ex *Explanation) {
				if len(ex.Candidates) != 1 || ex.Candidates[0].Columns == nil || ex.Candidates[0].Columns.Text == nil {
					t.Fatalf("OR candidate has no column scores: %+v", ex.Candidates)
				}
			},
		},
		{
			name: "no results", req: Request{Query: "fluffernutter", Mode: ModeFTS, K: 8},
			wantMode: ModeFTS, wantMatch: `"fluffernutter"`, wantOp: "AND",
			check: func(t *testing.T, ex *Explanation) {
				if ex.Candidates == nil || len(ex.Candidates) != 0 {
					t.Errorf("candidates = %#v, want empty non-nil", ex.Candidates)
				}
			},
		},
		{
			name: "unsanitisable query in hybrid has no fts header", req: Request{Query: "&&", Mode: ModeHybrid, K: 3},
			wantMode: ModeHybrid,
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			ex := explain(t, s, tc.req)
			if ex.Mode != tc.wantMode {
				t.Errorf("mode = %s, want %s", ex.Mode, tc.wantMode)
			}
			if tc.wantMatch == "" {
				if ex.FTS != nil {
					t.Errorf("fts header = %+v, want nil", ex.FTS)
				}
			} else if ex.FTS == nil || ex.FTS.Match != tc.wantMatch || ex.FTS.Op != tc.wantOp || ex.FTS.OrFallback != tc.wantOrRetry {
				t.Errorf("fts header = %+v, want match %q op %s or=%v", ex.FTS, tc.wantMatch, tc.wantOp, tc.wantOrRetry)
			}
			if ex.SettingsHash == "" || ex.Settings.RRFK != RRFConstant {
				t.Errorf("settings = %q %+v", ex.SettingsHash, ex.Settings)
			}
			if tc.check != nil {
				tc.check(t, ex)
			}
		})
	}
}

// TestExplainMatchesSearch: explain reports the same candidates, order and returned set as a
// plain search, and its RRF terms add up to the score the list was sorted by.
func TestExplainMatchesSearch(t *testing.T) {
	s, _ := testSearcher(t)
	for _, req := range []Request{
		{Query: "zebracorn", Mode: ModeHybrid, K: 20},
		{Query: "quokka widget", Mode: ModeHybrid, K: 4},
		{Query: "container plumbing", Mode: ModeVec, K: 6},
	} {
		out, err := s.Search(context.Background(), req)
		if err != nil {
			t.Fatal(err)
		}
		ex := explain(t, s, req)
		if len(ex.Candidates) != len(out.Candidates) {
			t.Fatalf("%q: %d candidates, want %d", req.Query, len(ex.Candidates), len(out.Candidates))
		}
		for i, h := range ex.Candidates {
			if h.ChunkID != out.Candidates[i].ChunkID || h.RRFScore != out.Candidates[i].RRFScore {
				t.Errorf("%q cand %d: %d/%v, want %d/%v", req.Query, i, h.ChunkID, h.RRFScore, out.Candidates[i].ChunkID, out.Candidates[i].RRFScore)
			}
			sum := 0.0
			for _, term := range h.RRF {
				sum += term.Term
			}
			if !almostEqual(sum, h.RRFScore) {
				t.Errorf("%q chunk %d: rrf terms sum to %v, score is %v", req.Query, h.ChunkID, sum, h.RRFScore)
			}
		}
		if got := countStatus(ex, StatusReturned); got != len(out.Returned) {
			t.Errorf("%q: %d returned, want %d", req.Query, got, len(out.Returned))
		}
	}
}

// TestExplainColumnRelation pins what bm25 does guarantee about single-column scores. FTS5 sums
// the column-weighted term frequencies and then saturates, x/(x+c) per phrase, a concave
// increasing function that is 0 at 0. So for every matched chunk, in magnitude:
// max(columns) <= total <= sum(columns). The sum is not equal: that is the documented caveat.
func TestExplainColumnRelation(t *testing.T) {
	s, _ := testSearcher(t)
	const eps = 1e-9
	sawStrict := false
	for _, q := range []string{"widget", "entry", "section", "beta", "zebracorn widget", "beta widget"} {
		ex := explain(t, s, Request{Query: q, Mode: ModeFTS, K: MaxK})
		if len(ex.Candidates) == 0 {
			t.Fatalf("%q: no candidates", q)
		}
		for _, h := range ex.Candidates {
			if h.FTS == nil || h.Columns == nil || h.Columns.Text == nil || h.Columns.Heading == nil || h.Columns.Title == nil {
				t.Fatalf("%q chunk %d: missing column scores: %+v", q, h.ChunkID, h)
			}
			a := []float64{-*h.Columns.Text, -*h.Columns.Heading, -*h.Columns.Title}
			total, sum, max := -h.FTS.BM25, 0.0, 0.0
			for _, v := range a {
				if v < -eps {
					t.Errorf("%q chunk %d: column magnitude %v is negative (bm25 is <= 0)", q, h.ChunkID, v)
				}
				sum += v
				max = math.Max(max, v)
			}
			if total < max-eps || total > sum+eps {
				t.Errorf("%q chunk %d: total %v outside [max %v, sum %v]", q, h.ChunkID, total, max, sum)
			}
			if total < sum-eps {
				sawStrict = true
			}
		}
	}
	if !sawStrict {
		t.Error("no chunk had total < sum of columns: the test no longer shows the saturation caveat")
	}
}

// TestExplainColumnSpecifics: a title term scores in the title column, a heading term in the
// heading column, and a term in neither scores 0 there.
func TestExplainColumnSpecifics(t *testing.T) {
	s, _ := testSearcher(t)

	ex := explain(t, s, Request{Query: "beta", Mode: ModeFTS, K: MaxK})
	if len(ex.Candidates) == 0 {
		t.Fatal("no candidates for beta")
	}
	for _, h := range ex.Candidates {
		if h.Columns.Title == nil || *h.Columns.Title >= 0 {
			t.Errorf("chunk %d: title column = %v, want a negative score (title is \"Beta Entry\")", h.ChunkID, h.Columns.Title)
		}
		if *h.Columns.Heading != 0 {
			t.Errorf("chunk %d: heading column = %v, want 0 (no heading contains beta)", h.ChunkID, *h.Columns.Heading)
		}
	}

	ex = explain(t, s, Request{Query: "section", Mode: ModeFTS, K: MaxK})
	for _, h := range ex.Candidates {
		if h.Heading != "" && *h.Columns.Heading >= 0 {
			t.Errorf("chunk %d (heading %q): heading column = %v, want negative", h.ChunkID, h.Heading, *h.Columns.Heading)
		}
	}
}

// TestExplainUsesEffectiveSettings: weights and rrf_k come from Settings, not the defaults. A
// zero column weight scores every matched chunk 0 in that column.
func TestExplainUsesEffectiveSettings(t *testing.T) {
	s, _ := testSearcher(t)
	s.Settings = settingsWith(t, func(c *config.Search) {
		c.RRFK = 1
		c.BM25.Heading = 0
		c.BM25.Title = 7
		c.ListWeights.FTS = 2
	})
	ex := explain(t, s, Request{Query: "beta", Mode: ModeFTS, K: MaxK})
	if ex.Settings.RRFK != 1 || ex.Settings.BM25Heading != 0 || ex.Settings.BM25Title != 7 || ex.Settings.FTSWeight != 2 {
		t.Errorf("settings = %+v", ex.Settings)
	}
	if ex.SettingsHash == "" || ex.SettingsHash == mustDefaultHash(t) {
		t.Errorf("hash %q must differ from the default hash", ex.SettingsHash)
	}
	top := ex.Candidates[0]
	if want := 1.0; top.RRF[0].Term != want || top.RRF[0].Weight != 2 { // weight 2 over rrf_k 1 + rank 1
		t.Errorf("rrf term = %+v, want weight 2 over (1+1) = %v", top.RRF[0], want)
	}
	for _, h := range ex.Candidates {
		if *h.Columns.Heading != 0 {
			t.Errorf("chunk %d: heading column = %v, want 0 with weight 0", h.ChunkID, *h.Columns.Heading)
		}
	}
	// The title column at weight 7 is stronger than at the default 3.
	def, _ := testSearcher(t)
	dex := explain(t, def, Request{Query: "beta", Mode: ModeFTS, K: MaxK})
	if *ex.Candidates[0].Columns.Title >= *dex.Candidates[0].Columns.Title {
		t.Errorf("title column at weight 7 (%v) is not stronger than at weight 3 (%v)",
			*ex.Candidates[0].Columns.Title, *dex.Candidates[0].Columns.Title)
	}
}

func mustDefaultHash(t *testing.T) string {
	t.Helper()
	st, err := DefaultSettings()
	if err != nil {
		t.Fatal(err)
	}
	return st.Hash
}

// TestExplainVecOnlyCandidateStillGetsColumns: with list_limit 1 a chunk can be in the vector
// list, miss the FTS list, and still match the expression; its columns are then set though its
// fts rank is null. (Boundary: the smallest legal list_limit.)
func TestExplainVecOnlyCandidateStillGetsColumns(t *testing.T) {
	s, _ := testSearcher(t)
	s.Settings = settingsWith(t, func(c *config.Search) { c.ListLimit = 1 })
	ex := explain(t, s, Request{Query: "widget", Mode: ModeHybrid, K: MaxK})
	var vecOnly *ExplainHit
	for i, h := range ex.Candidates {
		if h.FTS == nil && h.Vec != nil {
			vecOnly = &ex.Candidates[i]
		}
	}
	if vecOnly == nil {
		t.Fatalf("no vec-only candidate in %+v", ex.Candidates)
	}
	if vecOnly.Columns == nil || vecOnly.Columns.Text == nil {
		t.Errorf("vec-only candidate matching the expression has no columns: %+v", vecOnly)
	}
}

// TestExplainErrors: a blank query is a Usage error, as in Search, and an unknown mode too.
func TestExplainErrors(t *testing.T) {
	s, _ := testSearcher(t)
	for _, req := range []Request{{Query: "  "}, {Query: "x", Mode: "bogus"}} {
		_, err := s.Explain(context.Background(), req)
		if err == nil || errs.KindOf(err) != errs.Usage {
			t.Errorf("Explain(%+v) = %v, want a Usage error", req, err)
		}
	}
}

// TestExplainFallbackReason: a hybrid search that lost its embedder explains itself.
func TestExplainFallbackReason(t *testing.T) {
	s, _ := testSearcher(t)
	s.Embedder = failingEmbedder{Embedder: fake.New("fake-embed", 768), err: errs.New(errs.Unavailable, "test", "model missing")}
	ex := explain(t, s, Request{Query: "quokka", Mode: ModeHybrid, K: 3})
	if ex.Mode != ModeFTSFallback || ex.FallbackReason == "" {
		t.Errorf("mode %s reason %q, want fts-fallback with a reason", ex.Mode, ex.FallbackReason)
	}
	if ex.FTS == nil || len(ex.Candidates) == 0 {
		t.Errorf("fallback explained nothing: %+v", ex)
	}
}

// TestExplainDoesNotLog: explaining writes nothing to the search log.
func TestExplainDoesNotLog(t *testing.T) {
	s, _ := testSearcher(t)
	explain(t, s, Request{Query: "zebracorn", Mode: ModeHybrid, K: 8})
	var n int
	if err := dbOf(s).QueryRow("SELECT count(*) FROM searches").Scan(&n); err != nil {
		t.Fatal(err)
	}
	if n != 0 {
		t.Errorf("searches rows = %d, want 0", n)
	}
}
