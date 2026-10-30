// Package search is the hybrid searcher: a full-text list and a KNN list from a store.Retriever,
// fused with reciprocal rank fusion, capped to PerEntryCap chunks per entry, and logged through a
// Logger. It holds no SQL; the backend owns query syntax.
//
// Inputs: a Request and an embedder. Outputs: an Outcome (the Result plus everything the log
// records). Invariants (asserted): the fused list is totally ordered, duplicate-free and at most
// CandidateLimit long; the returned list is at most k long, at most PerEntryCap per entry and an
// order-preserving subsequence of the fused list.
package search

import (
	"context"
	"errors"
	"sort"
	"strings"
	"time"

	"github.com/nickstrad/kb/internal/assert"
	"github.com/nickstrad/kb/internal/embed"
	"github.com/nickstrad/kb/internal/errs"
	"github.com/nickstrad/kb/internal/store"
)

// Modes a search can run in. ModeFTSFallback is recorded when hybrid was asked for but the
// embedder was unreachable or the index has no vectors (ErrNoVectors).
const (
	ModeHybrid      = "hybrid"
	ModeFTS         = "fts"
	ModeVec         = "vec"
	ModeFTSFallback = "fts-fallback"
)

// Defaults and limits from the CLI contract and the search algorithm.
const (
	DefaultK       = 8
	MinK           = 1
	MaxK           = 20
	ListLimit      = 20 // candidates taken from each list
	CandidateLimit = 40 // rows written to search_candidates
	PerEntryCap    = 3  // diversity cap on the returned list
	RRFConstant    = 60
)

// vecOverFetch is how many neighbours the KNN fetches before a tag filter narrows them: the vector
// index cannot filter on the entry's tags, so without it a rare tag would come back nearly empty.
const vecOverFetch = 60

// ErrNoVectors means the index has no vectors: it is FTS only, or it was migrated to schema 3 and
// not reindexed yet. A hybrid search degrades to fts-fallback with it as Outcome.FallbackErr; a
// vec search returns it as a Conflict. It is not Unavailable: the embedder is fine.
var ErrNoVectors = errors.New("this index has no vectors yet; run `kb reindex --all` to embed it")

// Hit is one returned chunk. The JSON tags are the --json contract: the hits array of
// {search_id, query, mode, hits:[...]}. FTSRank and VecRank are null when the chunk appeared in
// only one of the two lists.
type Hit struct {
	Rank     int     `json:"rank"`
	RRFScore float64 `json:"rrf_score"`
	FTSRank  *int    `json:"fts_rank"`
	VecRank  *int    `json:"vec_rank"`
	Path     string  `json:"path"`
	Heading  string  `json:"heading"`
	Text     string  `json:"text"`
}

// Result is one whole search, shaped exactly like the --json output. Hits is never nil — an empty
// search returns an empty slice — so --json emits [] rather than null. NewResult guarantees that.
type Result struct {
	SearchID int64  `json:"search_id"`
	Query    string `json:"query"`
	Mode     string `json:"mode"`
	Hits     []Hit  `json:"hits"`
}

// NewResult starts an empty result for one query, with Hits initialised to the empty slice.
func NewResult(query, mode string) *Result {
	return &Result{Query: query, Mode: mode, Hits: []Hit{}}
}

// Request is one search as the CLI asked for it. Mode is one of the Mode constants, Caller comes
// from --caller / KB_CALLER / "unknown", and Tag is an optional tag filter.
type Request struct {
	Query  string
	Mode   string
	K      int
	Tag    string
	Caller string
}

// Logger writes one search to the log (store.SearchLog has it).
type Logger interface {
	LogSearch(ctx context.Context, r store.SearchRecord) (int64, error)
}

// Searcher runs searches against one index, using Embedder for the vector list. A nil Embedder
// means vector search is unavailable and hybrid degrades to fts-fallback.
type Searcher struct {
	Store    store.Retriever
	Log      Logger
	Embedder embed.Embedder
	// KBVersion is the kb binary's version, stored in searches.kb_version.
	KBVersion string
	// CorpusRev optionally names the corpus revision, stored in searches.corpus_rev.
	CorpusRev string
}

// Candidate is one chunk in the fused list. FTSRank/FTSScore are nil when the chunk was not in
// the FTS list and VecRank/VecDistance are nil when it was not in the vector list; at least one
// pair is always set. RRFScore is the reciprocal-rank-fusion score the list is sorted by. The
// path/heading/text fields are filled in once the candidates are looked up in the database, so
// the log can write search_candidates without re-reading anything.
type Candidate struct {
	ChunkID     int64
	FTSRank     *int
	FTSScore    *float64
	VecRank     *int
	VecDistance *float64
	RRFScore    float64

	EntryPath string
	Heading   string
	Text      string
}

// Outcome is everything one search produced: the caller-facing Result plus the full candidate
// list, the per-stage timings and the counts that searches/search_results/search_candidates need.
// Mode is the mode that actually ran, which is ModeFTSFallback when a hybrid search lost its
// embedder; in that case FallbackErr says why and Search still returns a nil error.
type Outcome struct {
	Result     *Result
	Candidates []Candidate // the fused list, top CandidateLimit, in fused order
	Returned   []Candidate // the subset that survived the diversity cap and k (returned=1)
	Mode       string

	NFTS int
	NVec int

	FTSMs   int64
	VecMs   int64
	EmbedMs int64
	TotalMs int64

	EmbedModel  string
	TagFilter   string
	K           int
	FallbackErr error
}

// FTSRow and VecRow are the two ranked lists Fuse takes, as the store returns them.
type (
	FTSRow = store.FTSRow
	VecRow = store.VecRow
)

// Fuse merges the two lists with reciprocal rank fusion: a chunk scores 1/(RRFConstant+rank) for
// every list it appears in, rank starting at 1. The result is sorted by score descending and
// truncated to CandidateLimit. Ties are broken by FTS rank, then vector rank, then chunk id, all
// ascending (a missing rank sorts last), so the same two lists always fuse to the same order.
func Fuse(fts []FTSRow, vec []VecRow) []Candidate {
	byChunk := make(map[int64]*Candidate, len(fts)+len(vec))
	order := make([]int64, 0, len(fts)+len(vec))

	get := func(id int64) *Candidate {
		c, ok := byChunk[id]
		if !ok {
			c = &Candidate{ChunkID: id}
			byChunk[id] = c
			order = append(order, id)
		}
		return c
	}

	for i, r := range fts {
		rank := i + 1
		c := get(r.ChunkID)
		score := r.Score
		c.FTSRank = &rank
		c.FTSScore = &score
		c.RRFScore += 1 / float64(RRFConstant+rank)
	}
	for i, r := range vec {
		rank := i + 1
		c := get(r.ChunkID)
		distance := r.Distance
		c.VecRank = &rank
		c.VecDistance = &distance
		c.RRFScore += 1 / float64(RRFConstant+rank)
	}

	out := make([]Candidate, 0, len(order))
	for _, id := range order {
		out = append(out, *byChunk[id])
	}
	sort.Slice(out, func(i, j int) bool { return fusedBefore(out[i], out[j]) })
	if len(out) > CandidateLimit {
		out = out[:CandidateLimit]
	}
	if assert.Enabled {
		assertFused(out)
	}
	return out
}

// fusedBefore is Fuse's total order.
func fusedBefore(a, b Candidate) bool {
	if a.RRFScore != b.RRFScore {
		return a.RRFScore > b.RRFScore
	}
	if ra, rb := rankOrMax(a.FTSRank), rankOrMax(b.FTSRank); ra != rb {
		return ra < rb
	}
	if ra, rb := rankOrMax(a.VecRank), rankOrMax(b.VecRank); ra != rb {
		return ra < rb
	}
	return a.ChunkID < b.ChunkID
}

// assertFused checks Fuse's postconditions: strictly ordered (which also rules out duplicates
// adjacent after sorting), no duplicate chunk ids, within CandidateLimit, a rank in some list.
func assertFused(out []Candidate) {
	assert.Post(len(out) <= CandidateLimit, "Fuse: len=%d > CandidateLimit=%d", len(out), CandidateLimit)
	seen := make(map[int64]bool, len(out))
	for i, c := range out {
		assert.Post(!seen[c.ChunkID], "Fuse: chunk %d appears twice", c.ChunkID)
		seen[c.ChunkID] = true
		assert.Post(c.FTSRank != nil || c.VecRank != nil, "Fuse: chunk %d has neither rank", c.ChunkID)
		if i > 0 {
			assert.Post(fusedBefore(out[i-1], c), "Fuse: out[%d] (chunk %d) does not sort before out[%d] (chunk %d)",
				i-1, out[i-1].ChunkID, i, c.ChunkID)
		}
	}
}

// rankOrMax sorts a missing rank after every present one.
func rankOrMax(p *int) int {
	if p == nil {
		return int(^uint(0) >> 1)
	}
	return *p
}

// applyCap is the diversity cap: walking the fused list in order, a chunk is skipped once its
// entry already holds PerEntryCap places in the returned list, and the walk stops at k. The
// candidate list itself is unaffected, so recall analysis can see what the cap dropped. A chunk
// missing from entryOf is skipped: without an entry it can neither be capped nor displayed.
func applyCap(cands []Candidate, entryOf map[int64]string, k int) []Candidate {
	assert.Pre(k >= MinK && k <= MaxK, "applyCap: k=%d outside [MinK,MaxK]", k)
	out := make([]Candidate, 0, k)
	perEntry := make(map[string]int)
	for _, c := range cands {
		if len(out) >= k {
			break
		}
		path, ok := entryOf[c.ChunkID]
		if !ok {
			continue
		}
		if perEntry[path] >= PerEntryCap {
			continue
		}
		perEntry[path]++
		out = append(out, c)
	}
	if assert.Enabled {
		assertCapped(cands, out, entryOf, k)
	}
	return out
}

// assertCapped checks applyCap's postconditions: at most k, at most PerEntryCap per entry, and
// an order-preserving subsequence of cands.
func assertCapped(cands, out []Candidate, entryOf map[int64]string, k int) {
	assert.Post(len(out) <= k, "applyCap: len(out)=%d > k=%d", len(out), k)
	perEntry := make(map[string]int)
	j := 0
	for _, c := range out {
		perEntry[entryOf[c.ChunkID]]++
		assert.Post(perEntry[entryOf[c.ChunkID]] <= PerEntryCap, "applyCap: entry %q appears more than PerEntryCap=%d times",
			entryOf[c.ChunkID], PerEntryCap)
		for j < len(cands) && cands[j].ChunkID != c.ChunkID {
			j++
		}
		assert.Post(j < len(cands), "applyCap: chunk %d is not in cands in order", c.ChunkID)
		j++
	}
}

// Search runs one search and reports everything it produced, including what the log records. It
// writes nothing itself.
//
// Mode handling: hybrid runs both lists; fts and vec run one. A hybrid search whose embedder is
// missing or unavailable degrades to FTS, reports ModeFTSFallback and puts the reason in
// Outcome.FallbackErr; that is not an error, because a usable answer was produced. The same holds
// when the index has no vectors (FallbackErr is ErrNoVectors and the query is not embedded). A vec
// search in either situation returns the error.
//
// A blank query is a Usage error in every mode: the vector list would otherwise embed nothing and
// log k arbitrary chunks. A query that is not blank but sanitises to no FTS terms is allowed; the
// FTS list is skipped and the vector list still runs.
func (s *Searcher) Search(ctx context.Context, req Request) (*Outcome, error) {
	out, err := s.search(ctx, req)
	if err != nil {
		return nil, errs.Wrap(err, errs.KindOf(err), "search.Search", "%v", err).With("query", req.Query, "mode", req.Mode)
	}
	if assert.Enabled {
		assertOutcome(out)
	}
	return out, nil
}

// assertOutcome checks Search's postconditions on the mode and the hits.
func assertOutcome(out *Outcome) {
	switch out.Mode {
	case ModeHybrid, ModeFTS, ModeVec, ModeFTSFallback:
	default:
		assert.Post(false, "Search: unknown mode %q", out.Mode)
	}
	assert.Post((out.Mode == ModeFTSFallback) == (out.FallbackErr != nil), "Search: mode %q with FallbackErr %v", out.Mode, out.FallbackErr)
	assert.Post(out.Result != nil && out.Result.Hits != nil, "Search: nil Result or Hits")
	for i, h := range out.Result.Hits {
		assert.Post(h.Rank == i+1, "Search: hit %d has rank %d", i, h.Rank)
	}
}

func (s *Searcher) search(ctx context.Context, req Request) (*Outcome, error) {
	const op = "search.Search"
	start := time.Now()
	if strings.TrimSpace(req.Query) == "" {
		return nil, errs.New(errs.Usage, op, "empty query")
	}
	mode := req.Mode
	if mode == "" {
		mode = ModeHybrid
	}
	switch mode {
	case ModeHybrid, ModeFTS, ModeVec:
	default:
		return nil, errs.New(errs.Usage, op, "unknown mode %q (want hybrid, fts or vec)", req.Mode)
	}
	k := ClampK(req.K)

	out := &Outcome{
		Mode:       mode,
		TagFilter:  req.Tag,
		K:          k,
		Candidates: []Candidate{},
		Returned:   []Candidate{},
	}

	// Vector side first: whether it works decides the mode finally recorded. Missing vectors are
	// checked before embedding, so a not-yet-reindexed index does not pay for an embedding.
	var vec []VecRow
	if mode == ModeHybrid || mode == ModeVec {
		hasVec, err := s.Store.HasVectors(ctx)
		if err != nil {
			return nil, err
		}
		if !hasVec {
			if mode == ModeVec {
				return nil, errs.Wrap(ErrNoVectors, errs.Conflict, op, "vector search: %v", ErrNoVectors)
			}
			out.Mode = ModeFTSFallback
			out.FallbackErr = ErrNoVectors
		}
	}
	if out.Mode == ModeHybrid || out.Mode == ModeVec {
		qvec, embedMs, err := s.embedQuery(ctx, req.Query)
		out.EmbedMs = embedMs
		if err != nil {
			if !unavailable(err) || mode == ModeVec {
				return nil, err
			}
			out.Mode = ModeFTSFallback
			out.FallbackErr = err
		} else {
			out.EmbedModel = s.Embedder.Model()
			vecStart := time.Now()
			vec, err = s.Store.KNN(ctx, store.KNNQuery{Vec: qvec, K: ListLimit, Tag: req.Tag, OverFetch: vecOverFetch})
			out.VecMs = time.Since(vecStart).Milliseconds()
			if err != nil {
				return nil, err
			}
		}
	}

	var fts []FTSRow
	if out.Mode != ModeVec {
		ftsStart := time.Now()
		rows, err := s.ftsList(ctx, strings.Fields(req.Query), req.Tag)
		out.FTSMs = time.Since(ftsStart).Milliseconds()
		if err != nil {
			return nil, err
		}
		fts = rows
	}
	out.NFTS = len(fts)
	out.NVec = len(vec)

	cands := Fuse(fts, vec)
	ids := make([]int64, len(cands))
	for i, c := range cands {
		ids[i] = c.ChunkID
	}
	meta, err := s.Store.ChunkMeta(ctx, ids)
	if err != nil {
		return nil, err
	}
	entryOf := make(map[int64]string, len(meta))
	for id, m := range meta {
		entryOf[id] = m.EntryPath
	}
	for i := range cands {
		if m, ok := meta[cands[i].ChunkID]; ok {
			cands[i].EntryPath = m.EntryPath
			cands[i].Heading = m.Heading
			cands[i].Text = m.Text
		}
	}
	if len(cands) > 0 {
		out.Candidates = cands
	}
	returned := applyCap(cands, entryOf, k)
	if len(returned) > 0 {
		out.Returned = returned
	}

	result := NewResult(req.Query, out.Mode)
	for i, c := range returned {
		result.Hits = append(result.Hits, Hit{
			Rank:     i + 1,
			RRFScore: c.RRFScore,
			FTSRank:  c.FTSRank,
			VecRank:  c.VecRank,
			Path:     c.EntryPath,
			Heading:  c.Heading,
			Text:     c.Text,
		})
	}
	out.Result = result
	out.TotalMs = time.Since(start).Milliseconds()
	return out, nil
}

// ftsList runs the full-text list: all terms ANDed first and, if that matches nothing and there
// is more than one term, ORed (one unknown word should not empty a good query).
func (s *Searcher) ftsList(ctx context.Context, terms []string, tag string) ([]FTSRow, error) {
	q := store.FTSQuery{Terms: terms, Op: store.OpAnd, Tag: tag, Limit: ListLimit}
	res, err := s.Store.FTS(ctx, q)
	if err != nil {
		return nil, err
	}
	if len(res.Rows) == 0 && res.Match != "" && len(terms) > 1 {
		q.Op = store.OpOr
		if res, err = s.Store.FTS(ctx, q); err != nil {
			return nil, err
		}
	}
	return res.Rows, nil
}

// embedQuery embeds the query with the embedder's query prefix, timing it on its own because it
// is by far the slowest stage (~0.4 s warm). A nil Embedder is Unavailable.
func (s *Searcher) embedQuery(ctx context.Context, query string) ([]float32, int64, error) {
	const op = "search.Search"
	if s.Embedder == nil {
		return nil, 0, errs.Wrap(embed.ErrUnavailable, errs.Unavailable, op, "no embedder is configured")
	}
	start := time.Now()
	vecs, err := s.Embedder.Embed(ctx, []string{embed.QueryText(s.Embedder, query)})
	ms := time.Since(start).Milliseconds()
	if err != nil {
		k := errs.KindOf(err)
		if errors.Is(err, embed.ErrUnavailable) {
			k = errs.Unavailable
		}
		return nil, ms, errs.Wrap(err, k, op, "embed query: %v", err)
	}
	if len(vecs) != 1 || len(vecs[0]) == 0 {
		return nil, ms, errs.New(errs.Internal, op, "embed query: embedder returned %d vectors, want 1", len(vecs))
	}
	return vecs[0], ms, nil
}

// unavailable reports whether an embedder error means "cannot be used right now", which a hybrid
// search answers by falling back to FTS.
func unavailable(err error) bool {
	return errs.KindOf(err) == errs.Unavailable || errors.Is(err, embed.ErrUnavailable)
}

// ClampK forces k into the documented 1..20 range. DefaultK is the flag's default, not a fallback
// applied here: an explicit -k 0 clamps up to MinK.
func ClampK(k int) int {
	if k < MinK {
		return MinK
	}
	if k > MaxK {
		return MaxK
	}
	return k
}
