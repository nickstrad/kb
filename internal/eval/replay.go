package eval

import (
	"context"
	"slices"
	"time"

	"github.com/nickstrad/kb/internal/errs"
	"github.com/nickstrad/kb/internal/search"
	"github.com/nickstrad/kb/internal/store"
)

// Bootstrap defaults: 1000 resamples, a fixed seed, a 95% interval.
const (
	DefaultResamples = 1000
	DefaultSeed      = 1
	Alpha            = 0.05
)

// Searcher runs one search without logging it (*search.Searcher has it).
type Searcher interface {
	Search(ctx context.Context, req search.Request) (*search.Outcome, error)
}

// Options are one evaluation's settings. K must already be in [search.MinK, search.MaxK].
// Resamples and Seed left zero mean DefaultResamples and DefaultSeed. SettingsHash is only echoed.
type Options struct {
	Mode         string
	K            int
	MinJudged    int // below this many chunk-level queries the report is a small sample
	Resamples    int
	Seed         uint64
	SettingsHash string
	// Evidence is the judged log the replays' feedback prior and query memory read, when it is
	// wider than the searches evaluated (kb eval --since, --caller). A replay leaves out every
	// search of its query in it, not only the window's: a same-query search outside the window
	// carries the same gold (PLAN 6.4). nil when the prior reads the evaluated searches or nothing.
	Evidence []store.JudgedSearch
}

// Report is one evaluation; its JSON tags are the kb eval --json contract.
type Report struct {
	Mode         string       `json:"mode"`
	K            int          `json:"k"`
	SettingsHash string       `json:"settings_hash"`
	Searches     SearchCounts `json:"searches"`
	Queries      int          `json:"queries"` // distinct queries with gold of any kind, other stores' included
	// Chunk is the headline: useful verdicts, matched on (path, heading).
	Chunk Variant `json:"chunk"`
	// Entry is the same gold matched on path alone.
	Entry Variant `json:"entry"`
	// Opens is the weaker signal, entry-level, never mixed into Chunk or Entry.
	Opens Variant `json:"opens"`
	// OtherStore is gold on other stores' hits of cross-store searches: counted, never scored.
	OtherStore      OtherStoreGold `json:"other_store"`
	NegativesInTopK int            `json:"negatives_in_top_k"`
	MinJudged       int            `json:"min_judged"`
	SmallSample     bool           `json:"small_sample"`
	Resamples       int            `json:"resamples"`
	Seed            uint64         `json:"seed"`
	Confidence      float64        `json:"confidence"`
	FallbackQueries int            `json:"fts_fallback_queries"`
	FallbackReason  string         `json:"fts_fallback_reason,omitempty"`
	// Prior is what the feedback prior and the query memory did in the replays.
	Prior    PriorUse `json:"prior"`
	PerQuery []Result `json:"per_query"`
}

// PriorUse counts the replays the feedback prior (PLAN 6.2) and the query memory (6.3) took part
// in. Each replay sees the evidence as of its original search's time, less its own query's
// searches (Request.ExcludeEvidence), so the gate can be open for late queries and closed for
// early ones.
type PriorUse struct {
	Enabled int `json:"enabled"` // replays with [prior] enabled
	Active  int `json:"active"`  // ... whose gate was open: prior.min_judged judged searches before them
	Memory  int `json:"memory"`  // ... where the query memory placed at least one hit
}

// OtherStoreGold counts the gold a cross-store search recorded on other stores' hits. kb eval
// replays this store only (PLAN 3.2: the log is this store's; another store's ranking is not this
// store's settings to tune), so that gold is left out of every variant rather than checked
// against this index, where a same-named path is a different entry.
type OtherStoreGold struct {
	Chunks  int `json:"chunks"`  // distinct useful (store, path, heading)
	Opens   int `json:"opens"`   // distinct opened (store, path)
	Queries int `json:"queries"` // queries whose only gold is other stores': not replayed
}

// Variant is one way of matching hits to gold, over the queries that have such gold in the
// index. The estimates are nil when Queries is 0.
type Variant struct {
	Queries        int       `json:"queries"`
	GoldNotInIndex int       `json:"gold_not_in_index"` // gold items whose chunk or entry is gone
	QueriesDropped int       `json:"queries_dropped"`   // queries left with no gold in the index
	HitAtK         *Estimate `json:"hit_at_k"`
	MRR            *Estimate `json:"mrr"`
	NDCG           *Estimate `json:"ndcg_at_k"`
}

// Result is one query's replay.
type Result struct {
	Query           string     `json:"query"` // the normalised key
	Text            string     `json:"text"`  // what was replayed
	SearchIDs       []int64    `json:"search_ids"`
	Gold            []GoldItem `json:"gold"`
	Opened          []OpenItem `json:"opened"`
	OtherStoreGold  int        `json:"other_store_gold"` // useful chunks and opens of other stores: not scored
	Mode            string     `json:"mode"`             // the mode the replay ran in; "" when not replayed
	Chunk           *Score     `json:"chunk"`
	Entry           *Score     `json:"entry"`
	Opens           *Score     `json:"opens"`
	NegativesInTopK int        `json:"negatives_in_top_k"`
}

// GoldItem is one useful-verdict chunk and whether the index still has it.
type GoldItem struct {
	Chunk
	InIndex bool `json:"in_index"`
}

// OpenItem is one opened entry and whether the index still has it.
type OpenItem struct {
	Path    string `json:"path"`
	InIndex bool   `json:"in_index"`
}

// Evaluate extracts gold from the logged searches, drops gold the index no longer has (counting
// it), replays every query with gold through s with opt.Mode and opt.K, and scores the three
// variants. It never writes. A failed replay stops the run; its kind is kept.
func Evaluate(ctx context.Context, searches []store.JudgedSearch, corpus Corpus, s Searcher, opt Options) (*Report, error) {
	rep, err := evaluate(ctx, searches, corpus, s, opt)
	if err != nil {
		return nil, errs.Wrap(err, errs.KindOf(err), "eval.Evaluate", "%v", err)
	}
	return rep, nil
}

func evaluate(ctx context.Context, searches []store.JudgedSearch, corpus Corpus, s Searcher, opt Options) (*Report, error) {
	p, err := prepare(ctx, searches, corpus)
	if err != nil {
		return nil, err
	}
	return p.run(ctx, s, opt, true)
}

// Prepared is a log whose gold has been checked against the index once, ready to be replayed with
// any number of Searchers (kb eval --sweep replays it once per setting).
type Prepared struct {
	queries []Query
	golds   []gold
	asOf    []time.Time // per query: its newest search's time, the replay's asOf (zero when not replayed)
	counts  SearchCounts
}

// Prepare extracts the gold of searches and checks it against corpus. It reads the index, never
// writes, and does not replay.
func Prepare(ctx context.Context, searches []store.JudgedSearch, corpus Corpus) (*Prepared, error) {
	p, err := prepare(ctx, searches, corpus)
	if err != nil {
		return nil, errs.Wrap(err, errs.KindOf(err), "eval.Prepare", "%v", err)
	}
	return p, nil
}

func prepare(ctx context.Context, searches []store.JudgedSearch, corpus Corpus) (*Prepared, error) {
	queries, counts := Extract(searches)
	p := &Prepared{queries: queries, golds: make([]gold, len(queries)), asOf: make([]time.Time, len(queries)), counts: counts}
	pres := newPresence(corpus)
	for i, q := range queries {
		g, err := resolveGold(ctx, pres, q)
		if err != nil {
			return nil, err
		}
		p.golds[i] = g
		if g.replayed() {
			if p.asOf[i], err = replayTime(q); err != nil {
				return nil, err
			}
		}
	}
	return p, nil
}

// replayTime is the instant a query is replayed as of: its newest search's logged time, the
// search whose text the replay runs. The log always holds RFC 3339 times (sqlite stamps them), so
// one that does not parse is a damaged log, not a kb bug.
func replayTime(q Query) (time.Time, error) {
	t, err := time.Parse(time.RFC3339, q.TS)
	if err != nil {
		id := q.SearchIDs[len(q.SearchIDs)-1]
		return time.Time{}, errs.Wrap(err, errs.Conflict, "eval.replay",
			"the search log is damaged: search %d has timestamp %q; correct or delete that row", id, q.TS).With("search", id)
	}
	return t, nil
}

// Judged is the keys of the queries with chunk-level gold in the index: the queries the headline
// metrics score, in key order.
func (p *Prepared) Judged() []string {
	keys := []string{}
	for i, q := range p.queries {
		if len(p.golds[i].chunks) > 0 {
			keys = append(keys, q.Key)
		}
	}
	return keys
}

// run replays and scores; without intervals the variants carry only their query counts (a sweep
// scores each setting from PerQuery and bootstraps only the rows it prints).
func (p *Prepared) run(ctx context.Context, s Searcher, opt Options, intervals bool) (*Report, error) {
	if opt.Resamples == 0 {
		opt.Resamples = DefaultResamples
	}
	if opt.Seed == 0 {
		opt.Seed = DefaultSeed
	}
	rep := &Report{
		Mode: opt.Mode, K: opt.K, SettingsHash: opt.SettingsHash, Searches: p.counts, Queries: len(p.queries),
		MinJudged: opt.MinJudged, Resamples: opt.Resamples, Seed: opt.Seed, Confidence: 1 - Alpha,
		PerQuery: []Result{},
	}
	var chunkS, entryS, openS []Score
	own := searchesByKey(opt.Evidence)
	for i, q := range p.queries {
		g := p.golds[i]
		g.count(rep)
		rep.OtherStore.Chunks += q.OtherUseful
		rep.OtherStore.Opens += q.OtherOpens
		if !q.hasOwnGold() {
			rep.OtherStore.Queries++
		}
		res := Result{Query: q.Key, Text: q.Text, SearchIDs: q.SearchIDs, Gold: g.items, Opened: g.opened,
			OtherStoreGold: q.OtherUseful + q.OtherOpens}
		if g.replayed() {
			// As of the original search, without the query's own searches: no feedback given after
			// it, and none of the verdicts that are its gold, reach the prior or the query memory.
			out, err := s.Search(ctx, search.Request{Query: q.Text, Mode: opt.Mode, K: opt.K,
				AsOf: p.asOf[i], ExcludeEvidence: leaveOut(q.SearchIDs, own[q.Key])})
			if err != nil {
				return nil, errs.Wrap(err, errs.KindOf(err), "eval.replay", "replay %q: %v", q.Text, err).With("query", q.Text)
			}
			rep.Prior.count(out)
			res.Mode = out.Mode
			if out.Mode == search.ModeFTSFallback {
				rep.FallbackQueries++
				if rep.FallbackReason == "" && out.FallbackErr != nil {
					rep.FallbackReason = out.FallbackErr.Error()
				}
			}
			hits := out.Result.Hits
			res.Chunk = scoreIf(len(g.chunks), opt.K, hits, func(h search.Hit) int {
				return slices.Index(g.chunks, Chunk{Path: h.Path, Heading: h.Heading})
			}, &chunkS)
			res.Entry = scoreIf(len(g.paths), opt.K, hits, func(h search.Hit) int { return slices.Index(g.paths, h.Path) }, &entryS)
			res.Opens = scoreIf(len(g.opens), opt.K, hits, func(h search.Hit) int { return slices.Index(g.opens, h.Path) }, &openS)
			for _, h := range hits[:min(len(hits), opt.K)] {
				if slices.Contains(q.Negatives, Chunk{Path: h.Path, Heading: h.Heading}) {
					res.NegativesInTopK++
				}
			}
			rep.NegativesInTopK += res.NegativesInTopK
		}
		rep.PerQuery = append(rep.PerQuery, res)
	}
	if intervals {
		summarise(&rep.Chunk, chunkS, opt)
		summarise(&rep.Entry, entryS, opt)
		summarise(&rep.Opens, openS, opt)
	} else {
		rep.Chunk.Queries, rep.Entry.Queries, rep.Opens.Queries = len(chunkS), len(entryS), len(openS)
	}
	rep.SmallSample = rep.Chunk.Queries < opt.MinJudged
	return rep, nil
}

// gold is one query's gold split by what the index still has.
type gold struct {
	items  []GoldItem
	opened []OpenItem
	chunks []Chunk  // useful chunks in the index
	paths  []string // distinct paths of useful chunks, for the paths in the index
	opens  []string // opened paths in the index

	missingChunks, missingPaths, missingOpens int
}

func resolveGold(ctx context.Context, pres *presence, q Query) (gold, error) {
	g := gold{items: make([]GoldItem, 0, len(q.Useful)), opened: make([]OpenItem, 0, len(q.Opens))}
	seen := map[string]bool{}
	for _, c := range q.Useful {
		headings, err := pres.load(ctx, c.Path) // nil: the entry is gone
		if err != nil {
			return gold{}, err
		}
		ok := headings[c.Heading] // false for a renamed heading
		g.items = append(g.items, GoldItem{Chunk: c, InIndex: ok})
		if ok {
			g.chunks = append(g.chunks, c)
		} else {
			g.missingChunks++
		}
		if seen[c.Path] {
			continue
		}
		seen[c.Path] = true
		if headings != nil {
			g.paths = append(g.paths, c.Path)
		} else {
			g.missingPaths++
		}
	}
	for _, p := range q.Opens {
		in, err := pres.entry(ctx, p)
		if err != nil {
			return gold{}, err
		}
		g.opened = append(g.opened, OpenItem{Path: p, InIndex: in})
		if in {
			g.opens = append(g.opens, p)
		} else {
			g.missingOpens++
		}
	}
	return g, nil
}

// searchesByKey is the ids of log's searches by Normalize(query): the query a search belongs to,
// as Extract groups them.
func searchesByKey(log []store.JudgedSearch) map[string][]int64 {
	by := map[string][]int64{}
	for _, j := range log {
		if k := Normalize(j.Query); k != "" {
			by[k] = append(by[k], j.ID)
		}
	}
	return by
}

// leaveOut is the searches a replay leaves out of the prior's evidence: the query's own searches
// in the evaluated window plus its searches in the evidence log (more when the window is
// filtered), sorted and unique. With nothing beyond the window it is ids itself.
func leaveOut(ids, more []int64) []int64 {
	if len(more) == 0 {
		return ids
	}
	out := slices.Concat(ids, more)
	slices.Sort(out)
	return slices.Compact(out)
}

// replayed reports whether the query has gold in the index, so it is replayed and scored.
func (g gold) replayed() bool { return len(g.chunks)+len(g.paths)+len(g.opens) > 0 }

// count adds one replay's prior and memory state.
func (u *PriorUse) count(out *search.Outcome) {
	if out.Prior.Enabled {
		u.Enabled++
	}
	if out.Prior.Active {
		u.Active++
	}
	if out.Memory.Items > 0 {
		u.Memory++
	}
}

// count adds the query's missing gold and drops to the report.
func (g gold) count(rep *Report) {
	rep.Chunk.GoldNotInIndex += g.missingChunks
	rep.Entry.GoldNotInIndex += g.missingPaths
	rep.Opens.GoldNotInIndex += g.missingOpens
	if len(g.items) > 0 && len(g.chunks) == 0 {
		rep.Chunk.QueriesDropped++
	}
	if len(g.items) > 0 && len(g.paths) == 0 {
		rep.Entry.QueriesDropped++
	}
	if len(g.opened) > 0 && len(g.opens) == 0 {
		rep.Opens.QueriesDropped++
	}
}

// scoreIf scores hits against nGold gold items when there are any, appending to acc.
func scoreIf(nGold, k int, hits []search.Hit, match func(search.Hit) int, acc *[]Score) *Score {
	if nGold == 0 {
		return nil
	}
	m := make([]int, len(hits))
	for i, h := range hits {
		m[i] = match(h)
	}
	s := ScoreRanking(m, nGold, k)
	*acc = append(*acc, s)
	return &s
}

// summarise fills a variant's means and intervals from its per-query scores.
func summarise(v *Variant, scores []Score, opt Options) {
	v.Queries = len(scores)
	if len(scores) == 0 {
		return
	}
	hit, rr, ndcg := make([]float64, len(scores)), make([]float64, len(scores)), make([]float64, len(scores))
	for i, s := range scores {
		if s.Hit {
			hit[i] = 1
		}
		rr[i], ndcg[i] = s.RR, s.NDCG
	}
	est := func(xs []float64) *Estimate {
		e := Bootstrap(xs, opt.Resamples, Alpha, opt.Seed)
		return &e
	}
	v.HitAtK, v.MRR, v.NDCG = est(hit), est(rr), est(ndcg)
}
