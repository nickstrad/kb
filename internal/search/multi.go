// Cross-store search (PLAN 3.2, docs/design/named-stores.md §4): the same request runs on every
// store with that store's own retriever and embedder and the primary store's settings, and the
// per-store fused lists are merged on ranks only.
//
// Merge. A candidate at position r (from 1) of a store's fused list scores 1/(rrf_k + r), rrf_k
// being the primary's. Scores (bm25, cosine, a store's own RRF) are never compared across stores,
// so any backend that returns an ordered list can join. A chunk's identity is (location root,
// entry path, ord): stores of one location that index the same file add their terms (an
// ensemble) and the merged candidate is the best-ranked store's; stores of different locations
// never merge. The merged list is sorted by score, then by the store order (the primary first,
// then canonical ref ascending), then path and ord, so it does not depend on the order the extra
// stores were named in. It is cut to the primary's candidate_limit, then the per-entry cap
// applies per (location root, path), then k.
package search

import (
	"context"
	"runtime/debug"
	"sort"
	"strconv"
	"sync"
	"time"

	"github.com/nickstrad/kb/internal/assert"
	"github.com/nickstrad/kb/internal/embed"
	"github.com/nickstrad/kb/internal/errs"
)

// MaxExtraStores is the most stores a search fans out to besides the primary (design N14).
const MaxExtraStores = 8

// Part is one store of a cross-store search. Ref is its canonical ref NAME@ABSROOT, "" for the
// primary store (the one that logs). Label is the tag its hits show (the ref as the user named
// it). Root is its location root, the scope of chunk identity. Searcher is set up on that store,
// with the primary's Settings. Mode, when set, replaces the request's mode for this store (an
// FTS-only store runs fts rather than falling back from hybrid).
type Part struct {
	Ref      string
	Label    string
	Root     string
	Mode     string
	Searcher *Searcher
}

// PartResult is one store's search, returned with its error rather than handled by the worker
// (CODING_STYLE §1.6). Err carries the store's ref in its context.
type PartResult struct {
	Value *Outcome
	Err   error
}

// MultiOutcome is a cross-store search: the merged Outcome (Mode, settings and trace are the
// primary's) and the extra stores that failed, each with its error, for the caller to warn about.
type MultiOutcome struct {
	*Outcome
	Skipped []Skipped
	// Degraded are the extra stores whose hybrid search fell back to FTS (their FallbackErr in
	// Err): they took part with their FTS list only.
	Degraded []Skipped
}

// Skipped is an extra store whose search failed: it did not take part in the merge.
type Skipped struct {
	Part Part
	Err  error
}

// OpSearchStores is the Op of the error SearchStores wraps an extra store's failure in, whose
// message starts "store REF: ": a caller that names the store itself shows the wrapped cause.
const OpSearchStores = "search.SearchStores"

// SearchStores runs req on every part concurrently and merges the results. parts[0] is the
// primary: its failure fails the whole search and cancels the others, because it is the store the
// user is working in and the one that logs. An extra store that fails is reported in Skipped and
// left out. With a single part the outcome is exactly that store's search. Nothing is logged:
// call parts[0].Searcher.Record with the result.
func SearchStores(ctx context.Context, parts []Part, req Request) (*MultiOutcome, error) {
	const op = "search.SearchStores"
	assert.Pre(len(parts) >= 1 && parts[0].Ref == "", "SearchStores: parts[0] must be the primary (Ref \"\")")
	if len(parts)-1 > MaxExtraStores {
		return nil, errs.New(errs.Usage, op, "%d extra stores; a search fans out to at most %d", len(parts)-1, MaxExtraStores)
	}
	start := time.Now()
	results := fanOut(ctx, parts, req)
	if err := results[0].Err; err != nil {
		return nil, err
	}
	if len(parts) == 1 {
		return &MultiOutcome{Outcome: results[0].Value}, nil
	}
	mo := &MultiOutcome{}
	outs := make([]*Outcome, len(parts))
	for i, r := range results {
		if r.Err != nil {
			mo.Skipped = append(mo.Skipped, Skipped{Part: parts[i], Err: r.Err})
			continue
		}
		outs[i] = r.Value
		if i > 0 && r.Value.FallbackErr != nil {
			mo.Degraded = append(mo.Degraded, Skipped{Part: parts[i], Err: r.Value.FallbackErr})
		}
	}
	mo.Outcome = Merge(parts, outs)
	mo.TotalMs = time.Since(start).Milliseconds()
	return mo, nil
}

// fanOut runs every part's search in its own goroutine and collects each result in its slot. A
// failed primary cancels the rest: their results no longer matter. A worker that panics hands the
// panic to this goroutine (errs.WorkerPanic), which raises it again once every worker is done, so
// the top-level recover reports it as an internal error (exit 3) rather than Go crashing with 2.
func fanOut(ctx context.Context, parts []Part, req Request) []PartResult {
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()
	results := make([]PartResult, len(parts))
	panics := make([]*errs.WorkerPanic, len(parts))
	var wg sync.WaitGroup
	for i, p := range parts {
		wg.Add(1)
		go func() {
			defer wg.Done()
			defer func() {
				if r := recover(); r != nil { // handed over below, never swallowed (CODING_STYLE §1.5)
					panics[i] = &errs.WorkerPanic{Value: r, Stack: debug.Stack()}
					cancel()
				}
			}()
			preq := req
			if p.Mode != "" {
				preq.Mode = p.Mode
			}
			out, err := p.Searcher.Search(ctx, preq)
			if err != nil {
				if p.Ref != "" {
					err = errs.Wrap(err, errs.KindOf(err), OpSearchStores, "store %s: %v", p.Ref, err).With("store", p.Ref)
				}
				if i == 0 {
					cancel()
				}
			}
			results[i] = PartResult{Value: out, Err: err}
		}()
	}
	wg.Wait()
	for _, p := range panics {
		if p != nil {
			panic(*p)
		}
	}
	return results
}

// mergeKey is a chunk's identity across stores.
type mergeKey struct {
	root, path string
	ord        int
}

// merged is one identity while merging: its summed score, and the candidate of the store that
// ranked it best (lowest rank, then the earlier store in merge order).
type merged struct {
	key      mergeKey
	score    float64
	best     Candidate
	bestRank int
	bestPart int // index into the parts in merge order
	nParts   int // how many stores returned it
}

// Merge merges the stores' fused lists (outs[i] is parts[i]'s outcome; nil for a store that
// failed, never for the primary) into one Outcome, as the package doc describes. The primary's
// outcome supplies the mode, settings, trace and k; NFTS, NVec and NExact are summed over the
// stores.
// Every merged candidate carries the canonical ref of its store in Store ("" = primary) and the
// merged score in RRFScore; its ranks and raw scores are that store's.
func Merge(parts []Part, outs []*Outcome) *Outcome {
	assert.Pre(len(parts) == len(outs) && len(outs) >= 1 && outs[0] != nil && parts[0].Ref == "",
		"Merge: %d parts, %d outcomes; the primary's outcome must be first", len(parts), len(outs))
	primary := outs[0]
	p := primary.Settings.Params
	order := mergeOrder(parts)

	byKey := map[mergeKey]*merged{}
	var keys []*merged
	nfts, nvec, nexact := 0, 0, 0
	for pos, i := range order {
		out := outs[i]
		if out == nil {
			continue
		}
		nfts, nvec, nexact = nfts+out.NFTS, nvec+out.NVec, nexact+out.NExact
		for r, c := range out.Candidates {
			if c.EntryPath == "" { // no entry row (removed since): it cannot be shown or capped
				continue
			}
			rank := r + 1
			k := mergeKey{parts[i].Root, c.EntryPath, c.Ord}
			m, ok := byKey[k]
			if !ok {
				m = &merged{key: k, bestRank: rank, bestPart: pos}
				m.best = c
				byKey[k] = m
				keys = append(keys, m)
			} else if rank < m.bestRank { // equal rank: the earlier store (in merge order) keeps it
				m.best, m.bestRank, m.bestPart = c, rank, pos
			}
			m.score += 1 / float64(p.RRFK+rank)
			m.nParts++
		}
	}
	sort.Slice(keys, func(a, b int) bool { return mergedBefore(keys[a], keys[b]) })
	if len(keys) > p.CandidateLimit {
		keys = keys[:p.CandidateLimit]
	}

	cands := make([]Candidate, len(keys))
	for j, m := range keys {
		c := m.best
		c.Store = parts[order[m.bestPart]].Ref
		c.RRFScore = m.score
		c.CapDropped = false
		cands[j] = c
	}
	if assert.Enabled {
		assertMerged(keys, p.CandidateLimit)
	}
	rootOf := map[string]string{}
	labelOf := map[string]string{}
	for _, pt := range parts {
		rootOf[pt.Ref], labelOf[pt.Ref] = pt.Root, pt.Label
	}
	returned := capBy(cands, func(c Candidate) (string, bool) {
		return rootOf[c.Store] + "\x00" + c.EntryPath, true
	}, primary.K, p.PerEntryCap)

	out := *primary
	out.Candidates, out.Returned = cands, returned
	out.NFTS, out.NVec, out.NExact = nfts, nvec, nexact
	out.Result = resultOf(primary.Result.Query, primary.Mode, returned, func(ref string) string { return labelOf[ref] })
	return &out
}

// mergeOrder is the order stores take part in the merge and break ties in: the primary first,
// then the extra stores by canonical ref. It returns indexes into parts.
func mergeOrder(parts []Part) []int {
	order := make([]int, len(parts))
	for i := range order {
		order[i] = i
	}
	sort.SliceStable(order[1:], func(a, b int) bool { return parts[order[1+a]].Ref < parts[order[1+b]].Ref })
	if assert.Enabled {
		for j := 1; j < len(order); j++ {
			assert.Pre(parts[order[j]].Ref != "" && (j == 1 || parts[order[j-1]].Ref != parts[order[j]].Ref),
				"Merge: extra store %d has an empty or duplicate ref %q", order[j], parts[order[j]].Ref)
		}
	}
	return order
}

// mergedBefore is Merge's total order: score descending, then the best store in merge order, then
// path and ord. Two identities with the same best store differ in path or ord (a store has one
// root), so no two distinct identities compare equal.
func mergedBefore(a, b *merged) bool {
	if a.score != b.score {
		return a.score > b.score
	}
	if a.bestPart != b.bestPart {
		return a.bestPart < b.bestPart
	}
	if a.key.path != b.key.path {
		return a.key.path < b.key.path
	}
	return a.key.ord < b.key.ord
}

// assertMerged checks Merge's postconditions (TESTING.md §3.5): at most limit, strictly ordered,
// no identity twice, and each store's relative order kept among the identities only it returned.
func assertMerged(keys []*merged, limit int) {
	assert.Post(len(keys) <= limit, "Merge: len=%d > CandidateLimit=%d", len(keys), limit)
	seen := map[mergeKey]bool{}
	lastRank := map[int]int{}
	for i, m := range keys {
		assert.Post(!seen[m.key], "Merge: %s#%d of %s appears twice", m.key.path, m.key.ord, m.key.root)
		seen[m.key] = true
		if i > 0 {
			assert.Post(mergedBefore(keys[i-1], m), "Merge: out[%d] does not sort before out[%d]", i-1, i)
		}
		if m.nParts == 1 {
			assert.Post(m.bestRank > lastRank[m.bestPart], "Merge: store %d's rank %d comes after its rank %d", m.bestPart, m.bestRank, lastRank[m.bestPart])
			lastRank[m.bestPart] = m.bestRank
		}
	}
}

// QueryMemo shares query vectors between the searchers of one cross-store search, so the query
// is embedded once per distinct (model, dim, query text with its prefix). It is safe for
// concurrent use; a failure is shared too (the same model would fail the same way).
type QueryMemo struct {
	mu sync.Mutex
	m  map[string]*memoEntry
}

type memoEntry struct {
	once sync.Once
	vecs [][]float32
	err  error
}

// embed embeds the one text with e, through the memo when q is not nil.
func (q *QueryMemo) embed(ctx context.Context, e embed.Embedder, text string) ([][]float32, error) {
	if q == nil {
		return e.Embed(ctx, []string{text})
	}
	key := e.Model() + "\x00" + strconv.Itoa(e.Dim()) + "\x00" + text
	q.mu.Lock()
	if q.m == nil {
		q.m = map[string]*memoEntry{}
	}
	ent, ok := q.m[key]
	if !ok {
		ent = &memoEntry{}
		q.m[key] = ent
	}
	q.mu.Unlock()
	ent.once.Do(func() { ent.vecs, ent.err = e.Embed(ctx, []string{text}) })
	return ent.vecs, ent.err
}
