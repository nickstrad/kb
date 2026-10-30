// Package prior computes the feedback prior of PLAN 6.1: a smoothed per-(entry_path, heading)
// score from judged-search verdicts, turned into a bounded multiplier for the fused RRF score.
// It is pure: no I/O, no store, no clock. The caller passes the events and the instant "asOf".
//
// Model, for one key (path, heading) and an event i with age_i = asOf - at_i in days:
//
//	w_i  = 0.5^(age_i/half_life) * pos_i      pos = min(3, sqrt(rank)) for positives, 1 for negatives
//	u    = Σ w_i over useful verdicts of the key + 0.5·Σ w_i over opens of the entry (any heading)
//	n    = Σ w_i over not-useful verdicts of the key
//	p    = (u + 2) / (u + n + 4)                      Beta(2,2) smoothing; no evidence gives 0.5
//	mult = clamp(1 + max_boost·(2p − 1), 1 − max_boost, 1 + max_boost)
//
// Unjudged results contribute nothing (an unjudged hit is not a negative). Events with
// at >= asOf are ignored (no leakage), as are verdicts on other stores' hits (Store != "").
// The prior is inactive (every multiplier 1) unless Params.Enabled and at least Params.MinJudged
// distinct searches carry a counted verdict or open.
package prior

import (
	"errors"
	"fmt"
	"math"
	"sort"
	"time"

	"github.com/nickstrad/kb/internal/assert"
)

// Beta smoothing constants (a, b): a uniform-ish Beta(2,2) prior. Fixed, not configurable.
const (
	smoothA = 2.0
	smoothB = 2.0
	// openWeight is the share of a useful verdict an open counts for.
	openWeight = 0.5
	// maxPosWeight caps the position-bias correction min(3, sqrt(rank)).
	maxPosWeight = 3.0
)

// Params mirrors the `[prior]` config section (PLAN §2.3).
type Params struct {
	Enabled      bool
	MinJudged    int     // distinct judged searches needed before the prior applies
	HalfLifeDays float64 // evidence halves every HalfLifeDays
	MaxBoost     float64 // multiplier stays within [1-MaxBoost, 1+MaxBoost]
	QueryMemory  bool    // read by 6.3, unused here
}

// DefaultParams are the PLAN §2.3 defaults (everything off).
func DefaultParams() Params {
	return Params{Enabled: false, MinJudged: 50, HalfLifeDays: 90, MaxBoost: 0.2, QueryMemory: false}
}

// Validate reports whether the numbers can drive the model. It accepts every [prior] value that
// config.Validate accepts (half_life_days >= 1 there), so a validated config always builds; max_boost
// 1 is fine because p is never 0 or 1, so the multiplier stays inside (0, 2).
func (p Params) Validate() error {
	switch {
	case p.MinJudged < 0:
		return fmt.Errorf("prior: min_judged %d is negative", p.MinJudged)
	case !(p.HalfLifeDays > 0) || math.IsInf(p.HalfLifeDays, 0):
		return fmt.Errorf("prior: half_life_days %v must be a positive number", p.HalfLifeDays)
	case !(p.MaxBoost >= 0 && p.MaxBoost <= 1): // the range config.Validate accepts for prior.max_boost
		return fmt.Errorf("prior: max_boost %v must be in [0, 1]", p.MaxBoost)
	}
	return nil
}

// Kind is the sort of evidence an Event carries.
type Kind int

const (
	// Useful is a useful verdict on (EntryPath, Heading).
	Useful Kind = iota
	// NotUseful is a not-useful verdict on (EntryPath, Heading).
	NotUseful
	// Open means the caller opened the entry; it lifts every heading of EntryPath. Heading is ignored.
	Open
)

// Event is one piece of judged-search evidence.
type Event struct {
	SearchID  int64 // identifies the search, so distinct judged searches can be counted
	EntryPath string
	Heading   string
	Kind      Kind
	Rank      int // 1-based rank of the hit in its search; values < 1 count as 1
	At        time.Time
	Store     string // "" = this store; anything else is another store's hit and is ignored
}

// ErrBadParams wraps Params.Validate failures from Build.
var ErrBadParams = errors.New("prior: invalid params")

type key struct{ path, heading string }

// Model is a computed prior. The zero Model is inactive and returns 1 everywhere.
type Model struct {
	active bool
	judged int
	boost  float64
	u, n   map[key]float64
	open   map[string]float64
}

// Build computes the Model from events as of asOf. The result does not depend on the order of
// events. It returns ErrBadParams (wrapped) when p fails Validate.
func Build(events []Event, p Params, asOf time.Time) (Model, error) {
	if err := p.Validate(); err != nil {
		return Model{}, fmt.Errorf("%w: %v", ErrBadParams, err)
	}
	kept := make([]Event, 0, len(events))
	searches := map[int64]struct{}{}
	for _, e := range events {
		if e.Store != "" || !e.At.Before(asOf) {
			continue
		}
		kept = append(kept, e)
		searches[e.SearchID] = struct{}{}
	}
	m := Model{
		judged: len(searches),
		boost:  p.MaxBoost,
		u:      map[key]float64{},
		n:      map[key]float64{},
		open:   map[string]float64{},
	}
	m.active = p.Enabled && m.judged >= p.MinJudged
	if !m.active {
		return m, nil
	}
	// Float addition is not associative: sum in a canonical order so the result is order-independent.
	sort.Slice(kept, func(i, j int) bool { return less(kept[i], kept[j]) })
	for _, e := range kept {
		w := weight(e, p.HalfLifeDays, asOf)
		switch e.Kind {
		case Useful:
			m.u[key{e.EntryPath, e.Heading}] += w
		case NotUseful:
			m.n[key{e.EntryPath, e.Heading}] += w
		case Open:
			m.open[e.EntryPath] += openWeight * w
		}
	}
	return m, nil
}

func less(a, b Event) bool {
	switch {
	case a.EntryPath != b.EntryPath:
		return a.EntryPath < b.EntryPath
	case a.Heading != b.Heading:
		return a.Heading < b.Heading
	case a.Kind != b.Kind:
		return a.Kind < b.Kind
	case !a.At.Equal(b.At):
		return a.At.Before(b.At)
	case a.Rank != b.Rank:
		return a.Rank < b.Rank
	}
	return a.SearchID < b.SearchID
}

// weight is the decayed, position-corrected weight of one event.
func weight(e Event, halfLifeDays float64, asOf time.Time) float64 {
	ageDays := asOf.Sub(e.At).Hours() / 24
	w := math.Exp2(-ageDays / halfLifeDays)
	if e.Kind != NotUseful {
		rank := max(e.Rank, 1)
		w *= math.Min(maxPosWeight, math.Sqrt(float64(rank)))
	}
	return w
}

// Active reports whether the gate is open (enabled and enough judged searches).
func (m Model) Active() bool { return m.active }

// Judged is the number of distinct counted searches with a verdict or open, whether or not the
// gate is open (6.2 prints it as "prior inactive: 12/50 judged searches").
func (m Model) Judged() int { return m.judged }

// Useful returns u for the key: the decayed, position-weighted useful mass including opens. It
// is 0 when the model is inactive. 6.3 uses it to rank remembered hits.
func (m Model) Useful(entryPath, heading string) float64 {
	if !m.active {
		return 0
	}
	return m.u[key{entryPath, heading}] + m.open[entryPath]
}

// Mult returns the multiplier for (entryPath, heading): 1 when inactive or without evidence,
// otherwise within [1-MaxBoost, 1+MaxBoost].
func (m Model) Mult(entryPath, heading string) float64 {
	if !m.active {
		return 1
	}
	k := key{entryPath, heading}
	u := m.u[k] + m.open[entryPath]
	n := m.n[k]
	p := (u + smoothA) / (u + n + smoothA + smoothB)
	mult := clamp(1+m.boost*(2*p-1), 1-m.boost, 1+m.boost)
	assert.Post(mult >= 1-m.boost && mult <= 1+m.boost, "prior.Mult: %v outside [1-%v, 1+%v]", mult, m.boost, m.boost)
	return mult
}

func clamp(v, lo, hi float64) float64 { return math.Max(lo, math.Min(hi, v)) }
