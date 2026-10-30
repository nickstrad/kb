package search

import (
	"context"
	"strings"

	"github.com/nickstrad/kb/internal/errs"
	"github.com/nickstrad/kb/internal/store"
)

// Statuses of an explained candidate: it was returned, the diversity cap skipped it, or the walk
// stopped at k (or the chunk could not be mapped to an entry) before reaching it.
const (
	StatusReturned   = "returned"
	StatusCapDropped = "cap_dropped"
	StatusBelowK     = "below_k"
)

// Explanation is one search laid open: the settings it ran with, the FTS expression, and every
// fused candidate (not only the returned hits) with the numbers that placed it. The JSON tags are
// the `kb explain --json` contract. FTS is nil when no FTS list ran (vec mode, or a query that
// sanitises to no terms); Exact is nil when no exact-identifier list ran (no identifier in the
// query, search.exact_list off, or vec mode); FallbackReason is "" unless the mode is fts-fallback.
type Explanation struct {
	Query          string        `json:"query"`
	Mode           string        `json:"mode"`
	K              int           `json:"k"`
	SettingsHash   string        `json:"settings_hash"`
	Settings       ExplainParams `json:"settings"`
	FTS            *ExplainMatch `json:"fts"`
	Exact          *ExplainMatch `json:"exact"`
	FallbackReason string        `json:"fallback_reason,omitempty"`
	NFTS           int           `json:"n_fts"`
	NVec           int           `json:"n_vec"`
	NExact         int           `json:"n_exact"`
	Candidates     []ExplainHit  `json:"candidates"`
}

// ExplainParams are the effective [search] numbers that shape ranking.
type ExplainParams struct {
	RRFK           int     `json:"rrf_k"`
	FTSWeight      float64 `json:"fts_weight"`
	VecWeight      float64 `json:"vec_weight"`
	ExactWeight    float64 `json:"exact_weight"`
	ExactList      bool    `json:"exact_list"`
	BM25Text       float64 `json:"bm25_text"`
	BM25Heading    float64 `json:"bm25_heading"`
	BM25Title      float64 `json:"bm25_title"`
	ListLimit      int     `json:"list_limit"`
	CandidateLimit int     `json:"candidate_limit"`
	PerEntryCap    int     `json:"per_entry_cap"`
}

// ExplainMatch is the expression that produced a lexical list (FTS or exact) and whether it is the
// OR retry (the AND query matched nothing).
type ExplainMatch struct {
	Match      string `json:"match"`
	Op         string `json:"op"` // "AND" or "OR"
	OrFallback bool   `json:"or_fallback"`
}

// ExplainHit is one fused candidate. Rank is its place in the fused list (1-based);
// ReturnedRank is its place among the returned hits (nil unless Status is "returned").
type ExplainHit struct {
	Rank         int            `json:"rank"`
	ChunkID      int64          `json:"chunk_id"`
	Path         string         `json:"path"`
	Heading      string         `json:"heading"`
	Status       string         `json:"status"`
	ReturnedRank *int           `json:"returned_rank"`
	RRFScore     float64        `json:"rrf_score"`
	RRF          []ExplainTerm  `json:"rrf_terms"`
	FTS          *ExplainFTS    `json:"fts"`
	Vec          *ExplainVec    `json:"vec"`
	Exact        *ExplainFTS    `json:"exact"`
	Columns      *ExplainColumn `json:"columns"`
}

// ExplainTerm is one list's addend of the RRF score: Weight/(RRFK+Rank).
type ExplainTerm struct {
	List   string  `json:"list"` // "fts", "vec" or "exact"
	Weight float64 `json:"weight"`
	Rank   int     `json:"rank"`
	Term   float64 `json:"term"`
}

// ExplainFTS is the chunk's place in the FTS list or the exact-identifier list; BM25 is the
// backend score (negative, lower is better).
type ExplainFTS struct {
	Rank int     `json:"rank"`
	BM25 float64 `json:"bm25"`
}

// ExplainVec is the chunk's place in the vector list.
type ExplainVec struct {
	Rank     int     `json:"rank"`
	Distance float64 `json:"distance"`
}

// ExplainColumn is the chunk re-scored with one column's weight alone (the other two at 0), using
// the same expression as the search. A score is nil when the chunk does not match that
// expression (a vector-only candidate can fail the AND query). FTS5 bm25 sums the weighted term
// frequencies of all columns before saturating them, so these scores are diagnostic: for a
// matched chunk, with a = the single-column magnitudes and t = the total magnitude,
// max(a) <= t <= sum(a), and in general t < sum(a).
type ExplainColumn struct {
	Text    *float64 `json:"text"`
	Heading *float64 `json:"heading"`
	Title   *float64 `json:"title"`
}

// Explain runs the search exactly as Search does (and, deliberately, does not log it: the search
// log feeds stats and eval, which a diagnostic must not skew), then re-scores the candidate set
// one bm25 column at a time. The three extra FTS calls go straight to the store, restricted to
// the candidate ids and using the expression and operator the search itself ended with; they do
// not go through ftsList, whose OR retry would fire on an empty result.
func (s *Searcher) Explain(ctx context.Context, req Request) (*Explanation, error) {
	out, err := s.Search(ctx, req)
	if err != nil {
		return nil, err
	}
	p := out.Settings.Params
	ex := &Explanation{
		Query:        req.Query,
		Mode:         out.Mode,
		K:            out.K,
		SettingsHash: out.Settings.Hash,
		Settings: ExplainParams{
			RRFK: p.RRFK, FTSWeight: p.FTSWeight, VecWeight: p.VecWeight, ExactWeight: p.ExactWeight, ExactList: p.ExactList,
			BM25Text: p.BM25.Text, BM25Heading: p.BM25.Heading, BM25Title: p.BM25.Title,
			ListLimit: p.ListLimit, CandidateLimit: p.CandidateLimit, PerEntryCap: p.PerEntryCap,
		},
		NFTS:       out.NFTS,
		NVec:       out.NVec,
		NExact:     out.NExact,
		Candidates: make([]ExplainHit, 0, len(out.Candidates)),
	}
	if out.FallbackErr != nil {
		ex.FallbackReason = out.FallbackErr.Error()
	}
	op := store.OpAnd
	if out.FTSMatch != "" {
		name := "AND"
		if out.FTSFallbackToOr {
			op, name = store.OpOr, "OR"
		}
		ex.FTS = &ExplainMatch{Match: out.FTSMatch, Op: name, OrFallback: out.FTSFallbackToOr}
	}
	if out.ExactMatch != "" {
		name := "AND"
		if out.ExactFallbackToOr {
			name = "OR"
		}
		ex.Exact = &ExplainMatch{Match: out.ExactMatch, Op: name, OrFallback: out.ExactFallbackToOr}
	}

	var cols map[int64]*ExplainColumn
	if ex.FTS != nil && len(out.Candidates) > 0 {
		ids := make([]int64, len(out.Candidates))
		for i, c := range out.Candidates {
			ids[i] = c.ChunkID
		}
		if cols, err = s.columnScores(ctx, strings.Fields(req.Query), op, req.Tag, ids, p.BM25); err != nil {
			return nil, err
		}
	}

	returnedRank := make(map[int64]int, len(out.Returned))
	for i, c := range out.Returned {
		returnedRank[c.ChunkID] = i + 1
	}
	for i, c := range out.Candidates {
		h := ExplainHit{
			Rank: i + 1, ChunkID: c.ChunkID, Path: c.EntryPath, Heading: c.Heading,
			Status: StatusBelowK, RRFScore: c.RRFScore, RRF: []ExplainTerm{}, Columns: cols[c.ChunkID],
		}
		if r, ok := returnedRank[c.ChunkID]; ok {
			h.Status, h.ReturnedRank = StatusReturned, &r
		} else if c.CapDropped {
			h.Status = StatusCapDropped
		}
		if c.FTSRank != nil {
			h.FTS = &ExplainFTS{Rank: *c.FTSRank, BM25: *c.FTSScore}
			h.RRF = append(h.RRF, ExplainTerm{List: "fts", Weight: p.FTSWeight, Rank: *c.FTSRank,
				Term: p.FTSWeight / float64(p.RRFK+*c.FTSRank)})
		}
		if c.VecRank != nil {
			h.Vec = &ExplainVec{Rank: *c.VecRank, Distance: *c.VecDistance}
			h.RRF = append(h.RRF, ExplainTerm{List: "vec", Weight: p.VecWeight, Rank: *c.VecRank,
				Term: p.VecWeight / float64(p.RRFK+*c.VecRank)})
		}
		if c.ExactRank != nil {
			h.Exact = &ExplainFTS{Rank: *c.ExactRank, BM25: *c.ExactScore}
			h.RRF = append(h.RRF, ExplainTerm{List: "exact", Weight: p.ExactWeight, Rank: *c.ExactRank,
				Term: p.ExactWeight / float64(p.RRFK+*c.ExactRank)})
		}
		ex.Candidates = append(ex.Candidates, h)
	}
	return ex, nil
}

// columnScores re-scores ids three times, each with one column's weight alone. A column whose
// weight is 0 scores every matched chunk 0.
func (s *Searcher) columnScores(ctx context.Context, terms []string, op store.FTSOp, tag string, ids []int64, w store.BM25Weights) (map[int64]*ExplainColumn, error) {
	cols := make(map[int64]*ExplainColumn, len(ids))
	single := []struct {
		w    store.BM25Weights
		slot func(*ExplainColumn) **float64
	}{
		{store.BM25Weights{Text: w.Text}, func(c *ExplainColumn) **float64 { return &c.Text }},
		{store.BM25Weights{Heading: w.Heading}, func(c *ExplainColumn) **float64 { return &c.Heading }},
		{store.BM25Weights{Title: w.Title}, func(c *ExplainColumn) **float64 { return &c.Title }},
	}
	for i := range single {
		weights := single[i].w
		res, err := s.Store.FTS(ctx, store.FTSQuery{Terms: terms, Op: op, Tag: tag, Limit: len(ids), Weights: &weights, ChunkIDs: ids})
		if err != nil {
			return nil, errs.Wrap(err, errs.KindOf(err), "search.Explain", "%v", err)
		}
		for _, r := range res.Rows {
			c := cols[r.ChunkID]
			if c == nil {
				c = &ExplainColumn{}
				cols[r.ChunkID] = c
			}
			score := r.Score
			*single[i].slot(c) = &score
		}
	}
	return cols, nil
}
