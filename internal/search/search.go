// Package search is the hybrid searcher: a full-text list, a KNN list and, when the query holds an
// identifier, an exact-identifier list from a store.Retriever, fused with weighted reciprocal rank
// fusion, capped to a number of chunks per entry, and logged through a Logger. It holds no SQL; the
// backend owns query syntax.
//
// Inputs: a Request, an embedder and Settings (the [search] Params plus the settings hash).
// Outputs: an Outcome (the Result, everything the log records and the trace: settings, FTS
// expression, OR retry, cap drops). Invariants (asserted): the fused list is totally ordered,
// duplicate-free and at most Params.CandidateLimit long; the returned list is at most k long, at
// most Params.PerEntryCap per entry and an order-preserving subsequence of the fused list.
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

// The k range from the CLI contract, and the default [search] values (DefaultParams; a test pins
// them to config.Default).
const (
	DefaultK       = 8
	MinK           = 1
	MaxK           = 20
	ListLimit      = 20 // candidates taken from each list
	CandidateLimit = 40 // rows written to search_candidates
	PerEntryCap    = 3  // diversity cap on the returned list
	RRFConstant    = 60
)

// vecOverFetch is the default number of neighbours the KNN fetches before a tag filter narrows
// them: the vector index cannot filter on tags, so without it a rare tag would come back nearly
// empty.
const vecOverFetch = 60

// ErrNoVectors means the index has no vectors: it is FTS only, or it was migrated to schema 3 and
// not reindexed yet. A hybrid search degrades to fts-fallback with it as Outcome.FallbackErr; a
// vec search returns it as a Conflict. It is not Unavailable: the embedder is fine.
var ErrNoVectors = errors.New("this index has no vectors yet; run `kb reindex --all` to embed it")

// Hit is one returned chunk. The JSON tags are the --json contract: the hits array of
// {search_id, query, mode, hits:[...]}. FTSRank and VecRank are null when the chunk was not in that
// list; ExactRank, its place in the exact-identifier list, is omitted when it was not there (the
// list runs only for a query with an identifier in it).
type Hit struct {
	Rank int `json:"rank"`
	// Store is the store that returned the hit in a cross-store search, as the user named it
	// (--stores, search.stores); omitted for the primary store's hits.
	Store     string  `json:"store,omitempty"`
	RRFScore  float64 `json:"rrf_score"`
	FTSRank   *int    `json:"fts_rank"`
	VecRank   *int    `json:"vec_rank"`
	ExactRank *int    `json:"exact_rank,omitempty"`
	Path      string  `json:"path"`
	Heading   string  `json:"heading"`
	Text      string  `json:"text"`
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
// from --caller, else [agent] caller in the config, else "unknown", and Tag is an optional tag filter.
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
// means vector search is unavailable and hybrid degrades to fts-fallback. Settings left zero
// means DefaultSettings.
type Searcher struct {
	Store    store.Retriever
	Log      Logger
	Embedder embed.Embedder
	Settings Settings
	// KBVersion is the kb binary's version, stored in searches.kb_version.
	KBVersion string
	// CorpusRev optionally names the corpus revision, stored in searches.corpus_rev.
	CorpusRev string
	// Now is the clock Record stamps the search with (searches.ts); nil means time.Now.
	Now func() time.Time
	// Queries, when set, shares query vectors between searchers: a cross-store search embeds
	// the query once per distinct (model, query text with its prefix). nil embeds every time.
	Queries *QueryMemo
	// Synonyms expand the query terms of the main full-text list (PLAN 5.5); nil or empty means no
	// expansion. The exact-identifier list and the vector query never use them.
	Synonyms *Synonyms
}

// now is s.Now, or the wall clock.
func (s *Searcher) now() time.Time {
	if s.Now == nil {
		return time.Now()
	}
	return s.Now()
}

// Candidate is one chunk in the fused list. FTSRank/FTSScore are nil when the chunk was not in
// the FTS list, VecRank/VecDistance when it was not in the vector list and ExactRank/ExactScore
// when it was not in the exact-identifier list; at least one pair is always set. RRFScore is the reciprocal-rank-fusion score the list is sorted by.
// CapDropped is true when the diversity cap skipped the chunk because its entry already filled
// its places. The path/heading/text fields are filled in once the candidates are looked up in the
// database, so the log can write search_candidates without re-reading anything. Ord is the
// chunk's position in its entry; with the entry path and the store's location root it is the
// chunk's identity across stores (Merge). Store is "" for the primary store's chunks; Merge sets
// it to the canonical ref of the store a merged candidate came from.
type Candidate struct {
	Store       string
	ChunkID     int64
	FTSRank     *int
	FTSScore    *float64
	VecRank     *int
	VecDistance *float64
	ExactRank   *int
	ExactScore  *float64
	RRFScore    float64
	CapDropped  bool

	EntryPath string
	Ord       int
	Heading   string
	Text      string
}

// Outcome is everything one search produced: the caller-facing Result plus the full candidate
// list, the per-stage timings and the counts that searches/search_results/search_candidates need.
// Mode is the mode that actually ran, which is ModeFTSFallback when a hybrid search lost its
// embedder; in that case FallbackErr says why and Search still returns a nil error. Settings,
// FTSMatch and FTSFallbackToOr are the trace: what the search ran with, the FTS expression that
// produced the FTS list ("" when none ran) and whether it is the OR retry; ExactTerms, ExactMatch
// and ExactFallbackToOr are the same for the exact-identifier list (ExactTerms nil and ExactMatch
// "" when it did not run: no identifier in the query, exact_list off, or vec mode).
type Outcome struct {
	Result     *Result
	Candidates []Candidate // the fused list, top Params.CandidateLimit, in fused order
	Returned   []Candidate // the subset that survived the diversity cap and k (returned=1)
	Mode       string

	NFTS   int
	NVec   int
	NExact int

	FTSMs   int64
	VecMs   int64
	EmbedMs int64
	TotalMs int64

	EmbedModel  string
	TagFilter   string
	K           int
	FallbackErr error

	Settings          Settings
	FTSMatch          string
	FTSSynonyms       map[string][]string // query term -> its synonym alternatives, when FTSMatch is expanded
	FTSAlts           map[string][]string // the alternatives FTSMatch was built with: FTSSynonyms plus Spell's corrections
	Spell             []SpellFix          // the words spell correction added an alternative to (PLAN 5.6); nil when it did not run or found none
	FTSFallbackToOr   bool
	ExactTerms        []string
	ExactMatch        string
	ExactFallbackToOr bool
}

// FTSRow and VecRow are the two ranked lists Fuse takes, as the store returns them.
type (
	FTSRow = store.FTSRow
	VecRow = store.VecRow
)

// Fuse merges the lists with weighted reciprocal rank fusion: a chunk scores w/(p.RRFK+rank) for
// every list it appears in, rank starting at 1 and w that list's weight (p.FTSWeight, p.VecWeight,
// p.ExactWeight). The result is sorted by score descending and truncated to p.CandidateLimit. Ties
// are broken by FTS rank, then vector rank, then exact rank, then chunk id, all ascending (a
// missing rank sorts last), so the same lists and params always fuse to the same order; with no
// exact list the order is the two-list order. A list of weight 0 still contributes its chunks and
// ranks, but no score.
func Fuse(fts []FTSRow, vec []VecRow, exact []FTSRow, p Params) []Candidate {
	byChunk := make(map[int64]*Candidate, len(fts)+len(vec)+len(exact))
	order := make([]int64, 0, len(fts)+len(vec)+len(exact))

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
		c.RRFScore += p.FTSWeight / float64(p.RRFK+rank)
	}
	for i, r := range vec {
		rank := i + 1
		c := get(r.ChunkID)
		distance := r.Distance
		c.VecRank = &rank
		c.VecDistance = &distance
		c.RRFScore += p.VecWeight / float64(p.RRFK+rank)
	}
	for i, r := range exact {
		rank := i + 1
		c := get(r.ChunkID)
		score := r.Score
		c.ExactRank = &rank
		c.ExactScore = &score
		c.RRFScore += p.ExactWeight / float64(p.RRFK+rank)
	}

	out := make([]Candidate, 0, len(order))
	for _, id := range order {
		out = append(out, *byChunk[id])
	}
	sort.Slice(out, func(i, j int) bool { return fusedBefore(out[i], out[j]) })
	if len(out) > p.CandidateLimit {
		out = out[:p.CandidateLimit]
	}
	if assert.Enabled {
		assertFused(out, p.CandidateLimit)
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
	if ra, rb := rankOrMax(a.ExactRank), rankOrMax(b.ExactRank); ra != rb {
		return ra < rb
	}
	return a.ChunkID < b.ChunkID
}

// assertFused checks Fuse's postconditions: strictly ordered (which also rules out duplicates
// adjacent after sorting), no duplicate chunk ids, within limit, a rank in some list.
func assertFused(out []Candidate, limit int) {
	assert.Post(len(out) <= limit, "Fuse: len=%d > CandidateLimit=%d", len(out), limit)
	seen := make(map[int64]bool, len(out))
	for i, c := range out {
		assert.Post(!seen[c.ChunkID], "Fuse: chunk %d appears twice", c.ChunkID)
		seen[c.ChunkID] = true
		assert.Post(c.FTSRank != nil || c.VecRank != nil || c.ExactRank != nil, "Fuse: chunk %d has no rank", c.ChunkID)
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
// entry already holds perEntryCap places in the returned list, and the walk stops at k. The
// candidate list keeps every chunk, so recall analysis can see what the cap dropped; each chunk
// the cap skipped gets CapDropped set in cands. A chunk missing from entryOf is skipped, not
// marked: without an entry it can neither be capped nor displayed.
func applyCap(cands []Candidate, entryOf map[int64]string, k, perEntryCap int) []Candidate {
	return capBy(cands, entryByChunk(entryOf), k, perEntryCap)
}

// entryByChunk is capBy's entry identity within one store: the path of the chunk's entry.
func entryByChunk(entryOf map[int64]string) func(Candidate) (string, bool) {
	return func(c Candidate) (string, bool) {
		path, ok := entryOf[c.ChunkID]
		return path, ok
	}
}

// capBy is the diversity cap over any entry identity: entryOf names a candidate's entry, false
// when it has none (skipped, not marked). applyCap keys by path within one store; Merge keys by
// (location root, path) across stores.
func capBy(cands []Candidate, entryOf func(Candidate) (string, bool), k, perEntryCap int) []Candidate {
	assert.Pre(k >= MinK && k <= MaxK, "applyCap: k=%d outside [MinK,MaxK]", k)
	assert.Pre(perEntryCap >= 1, "applyCap: perEntryCap=%d must be positive", perEntryCap)
	out := make([]Candidate, 0, k)
	perEntry := make(map[string]int)
	for i, c := range cands {
		if len(out) >= k {
			break
		}
		entry, ok := entryOf(c)
		if !ok {
			continue
		}
		if perEntry[entry] >= perEntryCap {
			cands[i].CapDropped = true
			continue
		}
		perEntry[entry]++
		out = append(out, c)
	}
	if assert.Enabled {
		assertCapped(cands, out, entryOf, k, perEntryCap)
	}
	return out
}

// assertCapped checks capBy's postconditions: at most k, at most perEntryCap per entry, an
// order-preserving subsequence of cands, and no returned chunk marked CapDropped.
func assertCapped(cands, out []Candidate, entryOf func(Candidate) (string, bool), k, perEntryCap int) {
	assert.Post(len(out) <= k, "applyCap: len(out)=%d > k=%d", len(out), k)
	perEntry := make(map[string]int)
	j := 0
	for _, c := range out {
		entry, _ := entryOf(c)
		perEntry[entry]++
		assert.Post(perEntry[entry] <= perEntryCap, "applyCap: entry %q appears more than PerEntryCap=%d times",
			entry, perEntryCap)
		assert.Post(!c.CapDropped, "applyCap: returned chunk %d is marked CapDropped", c.ChunkID)
		for j < len(cands) && (cands[j].Store != c.Store || cands[j].ChunkID != c.ChunkID) {
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
	settings, err := s.settings()
	if err != nil {
		return nil, err
	}
	p := settings.Params

	out := &Outcome{
		Mode:       mode,
		TagFilter:  req.Tag,
		K:          k,
		Candidates: []Candidate{},
		Returned:   []Candidate{},
		Settings:   settings,
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
			vec, err = s.Store.KNN(ctx, store.KNNQuery{Vec: qvec, K: p.ListLimit, Tag: req.Tag, OverFetch: p.VecOverFetch})
			out.VecMs = time.Since(vecStart).Milliseconds()
			if err != nil {
				return nil, err
			}
		}
	}

	var fts []FTSRow
	if out.Mode != ModeVec {
		ftsStart := time.Now()
		fields := strings.Fields(req.Query)
		syn := s.Synonyms.Expand(fields)
		res, retried, err := s.lexicalList(ctx, fields, syn, req.Tag, p, false)
		if err != nil {
			return nil, err
		}
		res, retried, alts, err := s.spell(ctx, out, fields, syn, req.Tag, p, res, retried)
		if err != nil {
			return nil, err
		}
		out.FTSMs = time.Since(ftsStart).Milliseconds()
		fts = res.Rows
		out.FTSMatch = res.Match
		if res.Match != "" {
			out.FTSSynonyms = syn
			out.FTSAlts = alts
		}
		out.FTSFallbackToOr = retried
	}
	var exact []FTSRow
	if out.Mode != ModeVec && p.ExactList {
		if terms := ExactTerms(req.Query); len(terms) > 0 {
			exactStart := time.Now()
			res, retried, err := s.lexicalList(ctx, terms, nil, req.Tag, p, true)
			out.FTSMs += time.Since(exactStart).Milliseconds() // FTSMs times the lexical lists
			if err != nil {
				return nil, err
			}
			exact = res.Rows
			out.ExactTerms = terms
			out.ExactMatch = res.Match
			out.ExactFallbackToOr = retried
		}
	}
	out.NFTS = len(fts)
	out.NVec = len(vec)
	out.NExact = len(exact)

	cands := Fuse(fts, vec, exact, p)
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
			cands[i].Ord = m.Ord
			cands[i].Heading = m.Heading
			cands[i].Text = m.Text
		}
	}
	returned := applyCap(cands, entryOf, k, p.PerEntryCap)
	if len(cands) > 0 {
		out.Candidates = cands
	}
	if len(returned) > 0 {
		out.Returned = returned
	}

	out.Result = resultOf(req.Query, out.Mode, returned, nil)
	out.TotalMs = time.Since(start).Milliseconds()
	return out, nil
}

// resultOf is the Result for the returned candidates, ranked from 1. label maps a candidate's
// store (its canonical ref) to the tag the hit shows; nil leaves every hit untagged.
func resultOf(query, mode string, returned []Candidate, label func(store string) string) *Result {
	result := NewResult(query, mode)
	for i, c := range returned {
		h := Hit{
			Rank:      i + 1,
			RRFScore:  c.RRFScore,
			FTSRank:   c.FTSRank,
			VecRank:   c.VecRank,
			ExactRank: c.ExactRank,
			Path:      c.EntryPath,
			Heading:   c.Heading,
			Text:      c.Text,
		}
		if label != nil && c.Store != "" {
			h.Store = label(c.Store)
		}
		result.Hits = append(result.Hits, h)
	}
	return result
}

// lexicalList runs the full-text list (exact: the exact-identifier list over the identifier index):
// all terms ANDed first and, if that matches nothing and there is more than one term, ORed (one
// unknown word should not empty a good query). retried reports that the OR query is the one that
// produced res. alts are the synonym alternatives of terms (nil for none), applied to both queries.
func (s *Searcher) lexicalList(ctx context.Context, terms []string, alts map[string][]string, tag string, p Params, exact bool) (res store.FTSResult, retried bool, err error) {
	weights := p.BM25
	q := store.FTSQuery{Terms: terms, Op: store.OpAnd, Tag: tag, Limit: p.ListLimit, Weights: &weights, Exact: exact, Alts: alts}
	if res, err = s.Store.FTS(ctx, q); err != nil {
		return store.FTSResult{}, false, err
	}
	if len(res.Rows) == 0 && res.Match != "" && len(terms) > 1 {
		q.Op = store.OpOr
		if res, err = s.Store.FTS(ctx, q); err != nil {
			return store.FTSResult{}, false, err
		}
		return res, true, nil
	}
	return res, false, nil
}

// spell is the spell-correction step of the main FTS list (spell.go): when spellWanted, it plans
// the corrections and, if there are any, runs the list once more with them as alternatives, with
// the operator the first run ended with (an OR retry stays OR, so no row it found is lost to an
// AND that the corrections now satisfy), and keeps the first run's rows (keepOriginal). It records
// the fixes in out.Spell and returns the list, OR flag and alternatives to use; with nothing to
// correct they are the first run's, unchanged.
func (s *Searcher) spell(ctx context.Context, out *Outcome, fields []string, alts map[string][]string, tag string, p Params,
	first store.FTSResult, retried bool) (store.FTSResult, bool, map[string][]string, error) {
	v, hasVocab := s.Store.(store.Vocab)
	if !spellWanted(p.Spell, hasVocab, first.Match, len(first.Rows)) {
		return first, retried, alts, nil
	}
	plan, err := s.planSpell(ctx, v, fields, alts)
	if err != nil || len(plan.fixes) == 0 {
		return first, retried, alts, err
	}
	var res store.FTSResult
	if retried {
		weights := p.BM25
		res, err = s.Store.FTS(ctx, store.FTSQuery{Terms: fields, Op: store.OpOr, Tag: tag, Limit: p.ListLimit, Weights: &weights, Alts: plan.alts})
	} else {
		res, retried, err = s.lexicalList(ctx, fields, plan.alts, tag, p, false)
	}
	if err != nil {
		return store.FTSResult{}, false, nil, err
	}
	res.Rows = keepOriginal(first.Rows, res.Rows, p.ListLimit)
	out.Spell = plan.fixes
	return res, retried, plan.alts, nil
}

// embedQuery embeds the query with the embedder's query prefix, timing it on its own because it
// is by far the slowest stage (~0.4 s warm). A nil Embedder is Unavailable.
func (s *Searcher) embedQuery(ctx context.Context, query string) ([]float32, int64, error) {
	const op = "search.Search"
	if s.Embedder == nil {
		return nil, 0, errs.Wrap(embed.ErrUnavailable, errs.Unavailable, op, "no embedder is configured")
	}
	start := time.Now()
	vecs, err := s.Queries.embed(ctx, s.Embedder, embed.QueryText(s.Embedder, query))
	ms := time.Since(start).Milliseconds()
	if err != nil {
		return nil, ms, errs.Wrap(err, errs.KindOf(err), op, "embed query: %v", err)
	}
	if len(vecs) != 1 || len(vecs[0]) == 0 {
		return nil, ms, errs.New(errs.Internal, op, "embed query: embedder returned %d vectors, want 1", len(vecs))
	}
	return vecs[0], ms, nil
}

// unavailable reports whether an embedder error means "cannot be used right now", which a hybrid
// search answers by falling back to FTS.
func unavailable(err error) bool {
	return errs.KindOf(err) == errs.Unavailable
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
