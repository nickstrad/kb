package search

import (
	"context"
	"fmt"
	"math"
	"testing"

	"github.com/nickstrad/kb/internal/config"
)

// The boundary tables of the configurable params, on top of the default-value tables in
// boundary_test.go: each knob at its minimum, just below/at/above a non-default value, and at the
// config maximum.

// TestListLimitParamBoundary: each list holds at most Params.ListLimit rows; MaxListLimit is the
// largest the vector index accepts and one more is an error, which is why config bounds it.
func TestListLimitParamBoundary(t *testing.T) {
	s := rankedSearcher(t, 7, 0)
	for _, tc := range []struct {
		name  string
		limit int
		want  int
	}{
		{"list_limit 1 (min)", 1, 1},
		{"list_limit 6 = matches-1", 6, 6},
		{"list_limit 7 = matches", 7, 7},
		{"list_limit 8 = matches+1", 8, 7},
		{"list_limit MaxListLimit", config.MaxListLimit, 7},
	} {
		t.Run(tc.name, func(t *testing.T) {
			s2 := *s
			s2.Settings = settingsWith(t, func(c *config.Search) { c.ListLimit = tc.limit })
			out, err := s2.Search(context.Background(), Request{Query: "widget", Mode: ModeHybrid, K: MaxK})
			if err != nil {
				t.Fatal(err)
			}
			if out.NFTS != tc.want || out.NVec != tc.want {
				t.Fatalf("n_fts=%d n_vec=%d, want %d each", out.NFTS, out.NVec, tc.want)
			}
		})
	}
	t.Run("MaxListLimit+1 is refused by the vector index", func(t *testing.T) {
		s2 := *s
		s2.Settings = settingsWith(t, func(*config.Search) {})
		s2.Settings.Params.ListLimit = config.MaxListLimit + 1
		if _, err := s2.Search(context.Background(), Request{Query: "widget", Mode: ModeVec, K: MaxK}); err == nil {
			t.Fatal("KNN with k = MaxListLimit+1 succeeded; config.MaxListLimit no longer matches sqlite-vec")
		}
	})
}

// TestVecOverFetchParamBoundary: with a tag, the KNN fetches max(ListLimit, VecOverFetch) rows
// before filtering. At the minimum (1) ListLimit decides; at 30 a tagged chunk at rank 30 is
// found and one at 31 is not.
func TestVecOverFetchParamBoundary(t *testing.T) {
	for _, tc := range []struct {
		name      string
		overFetch int
		rank      int
		wantNVec  int
	}{
		{"overfetch 1 (min), tagged at ListLimit", 1, ListLimit, 1},
		{"overfetch 1 (min), tagged at ListLimit+1", 1, ListLimit + 1, 0},
		{"overfetch 30, tagged at 29", 30, 29, 1},
		{"overfetch 30, tagged at 30", 30, 30, 1},
		{"overfetch 30, tagged at 31", 30, 31, 0},
		{"overfetch MaxVecOverfetch, tagged last", config.MaxVecOverfetch, 35, 1},
	} {
		t.Run(tc.name, func(t *testing.T) {
			s := rankedSearcher(t, 35, tc.rank)
			s.Settings = settingsWith(t, func(c *config.Search) { c.VecOverfetch = tc.overFetch })
			out, err := s.Search(context.Background(), Request{Query: "q", Mode: ModeVec, K: MaxK, Tag: "t"})
			if err != nil {
				t.Fatal(err)
			}
			if out.NVec != tc.wantNVec {
				t.Fatalf("n_vec = %d, want %d", out.NVec, tc.wantNVec)
			}
		})
	}
}

// TestCandidateLimitParamBoundary: Fuse cuts at Params.CandidateLimit, from 1 up to the config
// maximum.
func TestCandidateLimitParamBoundary(t *testing.T) {
	fts, vec := distinctLists(10)
	for _, tc := range []struct {
		name  string
		limit int
		want  int
	}{
		{"candidate_limit 1 (min)", 1, 1},
		{"candidate_limit 9 = total-1", 9, 9},
		{"candidate_limit 10 = total", 10, 10},
		{"candidate_limit 11 = total+1", 11, 10},
		{"candidate_limit MaxCandidateLimit", config.MaxCandidateLimit, 10},
	} {
		t.Run(tc.name, func(t *testing.T) {
			p := DefaultParams()
			p.CandidateLimit = tc.limit
			if got := Fuse(fts, vec, nil, p); len(got) != tc.want {
				t.Fatalf("len(Fuse) = %d, want %d", len(got), tc.want)
			}
		})
	}
}

// distinctLists is n chunks split across the two lists, none in both.
func distinctLists(n int) ([]FTSRow, []VecRow) {
	var fts []FTSRow
	var vec []VecRow
	for i := 1; i <= n; i++ {
		if i%2 == 0 {
			fts = append(fts, FTSRow{ChunkID: int64(i)})
		} else {
			vec = append(vec, VecRow{ChunkID: int64(i)})
		}
	}
	return fts, vec
}

// TestPerEntryCapParamBoundary: one entry keeps at most perEntryCap places; the dropped ones are
// marked. 1 is the minimum; 5 is checked one below, at and above; a cap above k leaves k.
func TestPerEntryCapParamBoundary(t *testing.T) {
	for _, tc := range []struct {
		name              string
		chunks, cap       int
		want, wantDropped int
	}{
		{"cap 1 (min), 3 chunks", 3, 1, 1, 2},
		{"cap 5, 4 chunks", 4, 5, 4, 0},
		{"cap 5, 5 chunks", 5, 5, 5, 0},
		{"cap 5, 6 chunks", 6, 5, 5, 1},
		{"cap far above k: the walk stops at k, nothing dropped", MaxK + 5, 1 << 30, MaxK, 0},
	} {
		t.Run(tc.name, func(t *testing.T) {
			cands := make([]Candidate, tc.chunks)
			entryOf := map[int64]string{}
			for i := range cands {
				cands[i] = Candidate{ChunkID: int64(i + 1)}
				entryOf[int64(i+1)] = "a.md"
			}
			got := applyCap(cands, entryOf, MaxK, tc.cap)
			if len(got) != tc.want {
				t.Fatalf("len(applyCap) = %d, want %d", len(got), tc.want)
			}
			dropped := 0
			for _, c := range cands {
				if c.CapDropped {
					dropped++
				}
			}
			if dropped != tc.wantDropped {
				t.Fatalf("dropped = %d, want %d", dropped, tc.wantDropped)
			}
		})
	}
}

// TestRRFKParamBoundary: the score of rank r is w/(rrf_k+r), exactly, from the minimum 1 to the
// config maximum.
func TestRRFKParamBoundary(t *testing.T) {
	for _, k := range []int{1, 2, RRFConstant - 1, RRFConstant, RRFConstant + 1, config.MaxRRFK} {
		t.Run(fmt.Sprint("rrf_k ", k), func(t *testing.T) {
			p := DefaultParams()
			p.RRFK = k
			got := Fuse([]FTSRow{{ChunkID: 1}, {ChunkID: 2}}, []VecRow{{ChunkID: 1}}, nil, p)
			if want := 1/float64(k+1) + 1/float64(k+1); got[0].RRFScore != want {
				t.Errorf("chunk 1 = %v, want %v", got[0].RRFScore, want)
			}
			if want := 1 / float64(k+2); got[1].RRFScore != want {
				t.Errorf("chunk 2 = %v, want %v", got[1].RRFScore, want)
			}
		})
	}
}

// TestListWeightParamBoundary: a list's weight (fts, vec, exact) multiplies its RRF term, from 0 (no score) to the
// config maximum (still finite).
func TestListWeightParamBoundary(t *testing.T) {
	for _, tc := range []struct {
		name            string
		fts, vec, exact float64
	}{
		{"all 0 (min)", 0, 0, 0},
		{"fts 0", 0, 1, 1},
		{"vec 0", 1, 0, 1},
		{"exact 0", 1, 1, 0},
		{"fts 0.5, vec 2, exact 4", 0.5, 2, 4},
		{"all MaxWeight", config.MaxWeight, config.MaxWeight, config.MaxWeight},
	} {
		t.Run(tc.name, func(t *testing.T) {
			p := DefaultParams()
			p.FTSWeight, p.VecWeight, p.ExactWeight, p.RRFK = tc.fts, tc.vec, tc.exact, 1
			got := Fuse([]FTSRow{{ChunkID: 1}}, []VecRow{{ChunkID: 1}, {ChunkID: 2}}, []FTSRow{{ChunkID: 3}, {ChunkID: 1}}, p)
			byID := map[int64]float64{}
			for _, c := range got {
				byID[c.ChunkID] = c.RRFScore
			}
			if want := tc.fts/2 + tc.vec/2 + tc.exact/3; byID[1] != want || math.IsInf(byID[1], 0) {
				t.Errorf("chunk 1 = %v, want %v", byID[1], want)
			}
			if want := tc.vec / 3; byID[2] != want {
				t.Errorf("chunk 2 = %v, want %v", byID[2], want)
			}
			if want := tc.exact / 2; byID[3] != want {
				t.Errorf("chunk 3 = %v, want %v", byID[3], want)
			}
		})
	}
}

// TestBM25ParamBoundary: the column weights reach the FTS query. All 0 (the minimum) scores every
// row 0; MaxWeight keeps scores finite; title-only weight scores a title-only hit and not a
// body-only one.
func TestBM25ParamBoundary(t *testing.T) {
	s, _ := testSearcher(t)
	ftsScores := func(t *testing.T, query string, edit func(*config.BM25)) []float64 {
		t.Helper()
		s2 := *s
		s2.Settings = settingsWith(t, func(c *config.Search) { edit(&c.BM25) })
		out, err := s2.Search(context.Background(), Request{Query: query, Mode: ModeFTS, K: MaxK})
		if err != nil {
			t.Fatal(err)
		}
		var scores []float64
		for _, c := range out.Candidates {
			scores = append(scores, *c.FTSScore)
		}
		if len(scores) == 0 {
			t.Fatalf("%q matched nothing", query)
		}
		return scores
	}
	t.Run("all 0 (min)", func(t *testing.T) {
		for _, sc := range ftsScores(t, "widget", func(b *config.BM25) { *b = config.BM25{} }) {
			if sc != 0 {
				t.Fatalf("score %v, want 0", sc)
			}
		}
	})
	t.Run("all MaxWeight", func(t *testing.T) {
		for _, sc := range ftsScores(t, "widget", func(b *config.BM25) {
			*b = config.BM25{Text: config.MaxWeight, Heading: config.MaxWeight, Title: config.MaxWeight}
		}) {
			if math.IsInf(sc, 0) || math.IsNaN(sc) || sc >= 0 {
				t.Fatalf("score %v, want finite and negative", sc)
			}
		}
	})
	t.Run("title only", func(t *testing.T) {
		// "Gamma" is in gamma's title column (every chunk) and in its first chunk's text.
		titleOnly := ftsScores(t, "Gamma", func(b *config.BM25) { *b = config.BM25{Title: 1} })
		textOnly := ftsScores(t, "quokka", func(b *config.BM25) { *b = config.BM25{Title: 1} })
		if titleOnly[0] >= 0 || textOnly[0] != 0 {
			t.Errorf("title-weighted: Gamma %v (want < 0), quokka %v (want 0)", titleOnly[0], textOnly[0])
		}
	})
}
