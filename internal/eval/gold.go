package eval

import (
	"cmp"
	"context"
	"fmt"
	"slices"
	"strings"

	"github.com/nickstrad/kb/internal/store"
)

// Chunk is chunk-level gold: an entry path and a heading, as the log recorded them. It is never
// keyed on chunk id, which a reindex hands to unrelated chunks.
type Chunk struct {
	Path    string `json:"path"`
	Heading string `json:"heading"`
}

func compareChunk(a, b Chunk) int {
	return cmp.Or(cmp.Compare(a.Path, b.Path), cmp.Compare(a.Heading, b.Heading))
}

// Query is every logged search of one normalised query, with the union of their signals.
type Query struct {
	Key       string  // Normalize(text): lower case, whitespace collapsed
	Text      string  // the newest search's text, which the replay runs
	SearchIDs []int64 // ascending
	Useful    []Chunk // useful verdicts: the gold; sorted, unique
	Negatives []Chunk // not-useful verdicts on chunks no search of the query marked useful
	Opens     []string
	// OtherUseful and OtherOpens count the distinct useful chunks and opens of other stores'
	// hits (a cross-store search logs them too). kb eval replays this store only, so they are
	// counted and never scored, nor checked against this store's index.
	OtherUseful int
	OtherOpens  int
}

// hasOwnGold reports whether q has gold of this store, so it is replayed.
func (q Query) hasOwnGold() bool { return len(q.Useful) > 0 || len(q.Opens) > 0 }

// Replayable reports whether any query has gold of this store: whether Evaluate will replay
// anything (and so needs an embedder).
func Replayable(qs []Query) bool { return slices.ContainsFunc(qs, Query.hasOwnGold) }

// SearchCounts says what happened to every search read from the log. Each search is in exactly
// one of Useful, NoneOnly, NotUsefulOnly and Unjudged; WithOpens overlaps them.
type SearchCounts struct {
	Total         int `json:"total"`
	Useful        int `json:"useful"`          // at least one useful verdict (on any store's hit): gold
	NoneOnly      int `json:"none"`            // judged --none, no useful verdict: excluded
	NotUsefulOnly int `json:"not_useful_only"` // only not-useful verdicts: negatives, no gold
	Unjudged      int `json:"unjudged"`        // no verdict at all: excluded
	WithOpens     int `json:"with_opens"`      // an entry was opened from it: the weaker signal
}

// Normalize is the grouping key of a query: lower case, runs of whitespace as one space, trimmed.
func Normalize(q string) string { return strings.Join(strings.Fields(strings.ToLower(q)), " ") }

// verdictClass is what a search's feedback makes it.
type verdictClass int

const (
	classUnjudged verdictClass = iota
	classUseful
	classNone
	classNotUsefulOnly
)

// classify decides a search's class. A useful verdict wins over --none (Feedback withdraws --none
// when a rank is marked useful, so both on record means a damaged or hand-edited log).
func classify(j store.JudgedSearch) verdictClass {
	useful, notUseful := false, false
	for _, v := range j.Verdicts {
		if v.Useful {
			useful = true
		} else {
			notUseful = true
		}
	}
	switch {
	case useful:
		return classUseful
	case j.None:
		return classNone
	case notUseful:
		return classNotUsefulOnly
	default:
		return classUnjudged
	}
}

// Extract groups the searches by Normalize(query) and keeps the groups with gold: a useful
// verdict, or an open (kept apart: an open is the weaker signal), of this store or of another
// (Query.OtherUseful, OtherOpens: counted only). A verdict on another store's hit is never this
// store's gold nor a negative: a same-named path here is a different entry. Queries come back
// sorted by Key, so every later step is deterministic. Searches whose query normalises to "" are
// skipped.
func Extract(searches []store.JudgedSearch) ([]Query, SearchCounts) {
	var counts SearchCounts
	type acc struct {
		q         Query
		newest    int64
		useful    map[Chunk]bool
		negatives map[Chunk]bool
		opens     map[string]bool
		other     map[otherGold]bool
	}
	groups := map[string]*acc{}
	for _, j := range searches {
		counts.Total++
		switch classify(j) {
		case classUseful:
			counts.Useful++
		case classNone:
			counts.NoneOnly++
		case classNotUsefulOnly:
			counts.NotUsefulOnly++
		default:
			counts.Unjudged++
		}
		if len(j.Opens) > 0 {
			counts.WithOpens++
		}
		key := Normalize(j.Query)
		if key == "" {
			continue
		}
		a := groups[key]
		if a == nil {
			a = &acc{q: Query{Key: key}, useful: map[Chunk]bool{}, negatives: map[Chunk]bool{}, opens: map[string]bool{}, other: map[otherGold]bool{}}
			groups[key] = a
		}
		a.q.SearchIDs = append(a.q.SearchIDs, j.ID)
		if a.q.Text == "" || j.ID > a.newest {
			a.q.Text, a.newest = j.Query, j.ID
		}
		for _, v := range j.Verdicts {
			c := Chunk{Path: v.EntryPath, Heading: v.Heading}
			if v.Store != "" {
				if v.Useful {
					a.other[otherGold{store: v.Store, chunk: c}] = true
				}
				continue // another store's negative cannot match this store's replay
			}
			if v.Useful {
				a.useful[c] = true
			} else {
				a.negatives[c] = true
			}
		}
		for _, p := range j.Opens {
			a.opens[p] = true
		}
		for _, o := range j.OtherOpens {
			a.other[otherGold{store: o.Store, chunk: Chunk{Path: o.Path}, open: true}] = true
		}
	}
	var out []Query
	for _, a := range groups {
		if len(a.useful) == 0 && len(a.opens) == 0 && len(a.other) == 0 {
			continue
		}
		for g := range a.other {
			if g.open {
				a.q.OtherOpens++
			} else {
				a.q.OtherUseful++
			}
		}
		for c := range a.negatives {
			if a.useful[c] {
				delete(a.negatives, c) // a chunk useful for any search of the query is gold
			}
		}
		a.q.Useful = sortedKeys(a.useful, compareChunk)
		a.q.Negatives = sortedKeys(a.negatives, compareChunk)
		a.q.Opens = sortedKeys(a.opens, strings.Compare)
		slices.Sort(a.q.SearchIDs)
		out = append(out, a.q)
	}
	slices.SortFunc(out, func(x, y Query) int { return strings.Compare(x.Key, y.Key) })
	return out, counts
}

// otherGold is one distinct gold item of another store: a useful chunk, or an opened entry.
type otherGold struct {
	store string
	chunk Chunk
	open  bool
}

func sortedKeys[K comparable](m map[K]bool, cmp func(a, b K) int) []K {
	out := make([]K, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	slices.SortFunc(out, cmp)
	return out
}

// Corpus is the part of the index gold is checked against (store.Index has it).
type Corpus interface {
	Entry(ctx context.Context, path string) (*store.EntryRow, error)
	Chunks(ctx context.Context, entryID int64) ([]store.ChunkRow, error)
}

// presence answers "is this gold still in the index?", reading each entry once.
type presence struct {
	corpus   Corpus
	headings map[string]map[string]bool // path -> headings of its chunks; nil = no such entry
}

func newPresence(c Corpus) *presence {
	return &presence{corpus: c, headings: map[string]map[string]bool{}}
}

func (p *presence) load(ctx context.Context, path string) (map[string]bool, error) {
	if h, ok := p.headings[path]; ok {
		return h, nil
	}
	e, err := p.corpus.Entry(ctx, path)
	if err != nil {
		return nil, fmt.Errorf("read entry %s: %w", path, err)
	}
	var h map[string]bool
	if e != nil {
		chunks, err := p.corpus.Chunks(ctx, e.ID)
		if err != nil {
			return nil, fmt.Errorf("read chunks of %s: %w", path, err)
		}
		h = make(map[string]bool, len(chunks))
		for _, c := range chunks {
			h[c.Heading] = true
		}
	}
	p.headings[path] = h
	return h, nil
}

// entry reports whether path is indexed.
func (p *presence) entry(ctx context.Context, path string) (bool, error) {
	h, err := p.load(ctx, path)
	return h != nil, err
}
