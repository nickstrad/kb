package search

import (
	"context"
	"sort"
	"strings"
	"time"

	"github.com/nickstrad/kb/internal/errs"
	"github.com/nickstrad/kb/internal/prior"
	"github.com/nickstrad/kb/internal/store"
)

// memoryMaxGold bounds the remembered (path, heading) pairs looked up in the index for one search:
// the highest-u ones, before the list is cut to Params.ListLimit.
const memoryMaxGold = 100

// MemoryRow is one row of the query-memory list, in rank order: a chunk of the current index and
// the u (decayed, position-weighted useful mass) that placed it.
type MemoryRow struct {
	ChunkID int64
	Score   float64
}

// MemoryState is what the query-memory list did for one search (PLAN 6.3), for the trace and
// kb explain.
type MemoryState struct {
	Enabled bool   // [prior] query_memory
	Active  bool   // the list ran (it may still be empty)
	Reason  string // why it did not run, when Enabled and not Active
	Key     string // the normalised query key ("" when the query has none)
	// Searches is the number of earlier judged searches with this key (as of the search time);
	// Items the rows of the list; Skipped the remembered hits that no longer resolve to a chunk
	// (the entry or heading is gone, or the tag filter excludes it).
	Searches, Items, Skipped int
}

// Inactive is the explain text for a memory list that was asked for and did not run, "" otherwise.
func (m MemoryState) Inactive() string {
	if !m.Enabled || m.Active {
		return ""
	}
	return "memory inactive: " + m.Reason
}

// MemoryKey is the normalised form of a query that decides which past searches it remembers: the
// lowercase, sorted, unique words of the fields the full-text list would search (the same field
// split as the MATCH expression: punctuation separates words, bare operators and fields with no
// letter or digit vanish), joined by a space. "" when nothing is left.
func MemoryKey(query string) string {
	seen := map[string]bool{}
	var words []string
	for _, f := range strings.Fields(query) {
		for _, w := range ftsWords(f) {
			if w = strings.ToLower(w); !seen[w] {
				seen[w] = true
				words = append(words, w)
			}
		}
	}
	sort.Strings(words)
	return strings.Join(words, " ")
}

// memoryList builds the query-memory list: from the evidence's judged searches with the same key,
// the (path, heading) pairs of useful verdicts (and entries opened from them, by their first
// chunk) ranked by u, resolved to chunks of the current index. It runs under the prior's gate
// plus [prior] query_memory and a non-zero list weight (a weight-0 list would add candidates
// that score nothing); a remembered hit whose entry or heading is gone is skipped.
func (s *Searcher) memoryList(ctx context.Context, ev *evidence, req Request, p Params) ([]MemoryRow, MemoryState, error) {
	st := MemoryState{Enabled: p.Prior.QueryMemory}
	if !st.Enabled {
		return nil, st, nil
	}
	switch loc, ok := s.Store.(store.ChunkLocator); {
	case !ev.state.Active:
		st.Reason = "prior is not active"
		if why := ev.state.Reason(); why != "" {
			st.Reason = why
		}
		return nil, st, nil
	case p.MemoryWeight == 0:
		st.Reason = "search.list_weights.memory is 0"
		return nil, st, nil
	case !ok:
		st.Reason = "the store cannot locate chunks"
		return nil, st, nil
	default:
		return s.memoryRows(ctx, loc, ev, req, p, st)
	}
}

// memoryRows is the I/O shell of the list: it picks the remembered pairs (rememberedGold), looks
// them up in the index (the one store read) and keeps the ones that resolve (memoryRowsOf).
func (s *Searcher) memoryRows(ctx context.Context, loc store.ChunkLocator, ev *evidence, req Request, p Params, st MemoryState) ([]MemoryRow, MemoryState, error) {
	if st.Key = MemoryKey(req.Query); st.Key == "" {
		st.Reason = "the query has no searchable words"
		return nil, st, nil
	}
	st.Active = true
	golds, searches, err := rememberedGold(ev.judged, st.Key, p.Prior, ev.asOf)
	if err != nil {
		return nil, st, err
	}
	st.Searches = searches
	if len(golds) == 0 {
		return nil, st, nil
	}
	refs := make([]store.ChunkRef, len(golds))
	for i, g := range golds {
		refs[i] = g.ref
	}
	ids, err := loc.LocateChunks(ctx, refs, req.Tag)
	if err != nil {
		return nil, st, err
	}
	rows, skipped := memoryRowsOf(golds, ids, p.ListLimit)
	st.Items, st.Skipped = len(rows), skipped
	return rows, st, nil
}

// memoryGold is one remembered hit: where the log recorded it and the u that ranks it.
type memoryGold struct {
	ref store.ChunkRef
	u   float64
}

// rememberedGold is the query-memory decision, over values: from the judged searches whose query
// has key, this store's evidence before asOf, the (path, heading) of each useful verdict, plus the
// entry (any heading) of each entry opened but never judged useful. They rank by u, the decayed,
// position-weighted mass a model over that evidence alone gives them, then by path and heading,
// and at most memoryMaxGold are kept. searches is the number of distinct judged searches with
// the key. p must be valid (config.Validate guarantees it).
func rememberedGold(judged []store.JudgedSearch, key string, p prior.Params, asOf time.Time) (golds []memoryGold, searches int, err error) {
	var same []store.JudgedSearch
	for _, j := range judged {
		if MemoryKey(j.Query) == key {
			same = append(same, j)
		}
	}
	// This key's evidence as the prior counts it: this store's events before asOf only.
	var events []prior.Event
	for _, e := range priorEvents(same) {
		if e.Store == "" && e.At.Before(asOf) {
			events = append(events, e)
		}
	}
	// A model over it alone: u is what the earlier identical searches learned.
	p.Enabled, p.MinJudged = true, 0
	m, err := prior.Build(events, p, asOf)
	if err != nil { // not reached with validated params (loadEvidence builds with the same ones); kept as a guard, tested with invalid params
		return nil, 0, errs.Wrap(err, errs.Internal, "search.Search", "build the query memory: %v", err)
	}
	have := map[store.ChunkRef]bool{}
	usefulEntry := map[string]bool{}
	for _, e := range events {
		if e.Kind != prior.Useful {
			continue
		}
		usefulEntry[e.EntryPath] = true
		if ref := (store.ChunkRef{Path: e.EntryPath, Heading: e.Heading}); !have[ref] {
			have[ref] = true
			golds = append(golds, memoryGold{ref, m.Useful(e.EntryPath, e.Heading)})
		}
	}
	for _, e := range events {
		if ref := (store.ChunkRef{Path: e.EntryPath, AnyHeading: true}); e.Kind == prior.Open && !usefulEntry[e.EntryPath] && !have[ref] {
			have[ref] = true
			golds = append(golds, memoryGold{ref, m.Opened(e.EntryPath)})
		}
	}
	// A path has an any-heading ref only when it has no other, so the order below is total.
	sort.Slice(golds, func(i, j int) bool {
		a, b := golds[i], golds[j]
		switch {
		case a.u != b.u:
			return a.u > b.u
		case a.ref.Path != b.ref.Path:
			return a.ref.Path < b.ref.Path
		}
		return a.ref.Heading < b.ref.Heading
	})
	if len(golds) > memoryMaxGold {
		golds = golds[:memoryMaxGold]
	}
	return golds, m.Judged(), nil
}

// memoryRowsOf keeps, in gold order, the golds that ids resolves to a chunk, at most limit of
// them; skipped counts the golds that no longer resolve (a gold past the limit is neither).
func memoryRowsOf(golds []memoryGold, ids map[store.ChunkRef]int64, limit int) (rows []MemoryRow, skipped int) {
	for _, g := range golds {
		switch id, ok := ids[g.ref]; {
		case !ok:
			skipped++
		case len(rows) < limit:
			rows = append(rows, MemoryRow{ChunkID: id, Score: g.u})
		}
	}
	return rows, skipped
}
