package prior

import (
	"slices"
	"sort"
	"time"
)

// Index is a set of events prepared once for many Models at different instants: an eval replay
// (PLAN 6.4) asks for the model as of each replayed search's time, and building every one of them
// from the whole log would cost O(searches x events). NewIndex sorts the events by key once
// (O(E log E)); Model then answers the gate with a binary search and computes a multiplier only
// for the keys a search asks about, from that key's events alone.
//
// A Model from an Index is equal to Build over the same events with the excluded searches left
// out, bit for bit: per key it sums the same events in the same canonical order (less).
type Index struct {
	terms map[key][]Event    // Useful and NotUseful events of (path, heading), in less order
	opens map[string][]Event // Open events of a path, in less order
	// firstAt is, per counted search, the time of its earliest event; starts the same times
	// sorted, so the number of searches judged before asOf is one binary search.
	firstAt map[int64]time.Time
	starts  []time.Time
}

// NewIndex prepares events for Model. Events of other stores (Store != "") are dropped, as Build
// drops them. events is not modified or kept.
func NewIndex(events []Event) *Index {
	ix := &Index{terms: map[key][]Event{}, opens: map[string][]Event{}, firstAt: map[int64]time.Time{}}
	for _, e := range events {
		if e.Store != "" {
			continue
		}
		if t, ok := ix.firstAt[e.SearchID]; !ok || e.At.Before(t) {
			ix.firstAt[e.SearchID] = e.At
		}
		switch e.Kind {
		case Useful, NotUseful:
			k := key{e.EntryPath, e.Heading}
			ix.terms[k] = append(ix.terms[k], e)
		case Open:
			ix.opens[e.EntryPath] = append(ix.opens[e.EntryPath], e)
		}
	}
	byLess := func(evs []Event) { sort.Slice(evs, func(i, j int) bool { return less(evs[i], evs[j]) }) }
	for _, evs := range ix.terms {
		byLess(evs)
	}
	for _, evs := range ix.opens {
		byLess(evs)
	}
	ix.starts = make([]time.Time, 0, len(ix.firstAt))
	for _, t := range ix.firstAt {
		ix.starts = append(ix.starts, t)
	}
	slices.SortFunc(ix.starts, func(a, b time.Time) int { return a.Compare(b) })
	return ix
}

// Model is the prior as of asOf over the indexed events, leaving out every event of the searches
// in exclude (an eval replay leaves out the replayed query's own searches, whose verdicts are its
// gold). It equals Build(events without exclude's, p, asOf) and returns the same ErrBadParams.
// The model reads ix lazily, so ix must outlive it and must not change (it never does).
func (ix *Index) Model(p Params, asOf time.Time, exclude []int64) (Model, error) {
	if _, err := Build(nil, p, asOf); err != nil { // the same validation and error as Build
		return Model{}, err
	}
	out := map[int64]bool{}
	judged := sort.Search(len(ix.starts), func(i int) bool { return !ix.starts[i].Before(asOf) })
	for _, id := range exclude {
		if t, ok := ix.firstAt[id]; ok && !out[id] && t.Before(asOf) {
			judged--
		}
		out[id] = true
	}
	m := Model{judged: judged, boost: p.MaxBoost}
	m.active = p.Enabled && m.judged >= p.MinJudged
	if m.active {
		m.view = &view{ix: ix, asOf: asOf, halfLife: p.HalfLifeDays, exclude: out}
	}
	return m, nil
}

// view is a Model's lazy source: the sums of one key are computed when asked, from the key's
// events kept by Build's rule (before asOf, not excluded), in the order Build sums them.
type view struct {
	ix       *Index
	asOf     time.Time
	halfLife float64
	exclude  map[int64]bool
}

// kept reports whether Build would keep e (other stores' events never reach the index).
func (v *view) kept(e Event) bool { return e.At.Before(v.asOf) && !v.exclude[e.SearchID] }

// sums is (u, n) of the key without opens: Build's m.u[k] and m.n[k].
func (v *view) sums(path, heading string) (u, n float64) {
	for _, e := range v.ix.terms[key{path, heading}] {
		if !v.kept(e) {
			continue
		}
		if w := weight(e, v.halfLife, v.asOf); e.Kind == Useful {
			u += w
		} else {
			n += w
		}
	}
	return u, n
}

// opened is Build's m.open[path].
func (v *view) opened(path string) float64 {
	var o float64
	for _, e := range v.ix.opens[path] {
		if v.kept(e) {
			o += openWeight * weight(e, v.halfLife, v.asOf)
		}
	}
	return o
}
