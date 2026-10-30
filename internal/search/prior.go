package search

import (
	"context"
	"fmt"
	"sort"
	"time"

	"github.com/nickstrad/kb/internal/assert"
	"github.com/nickstrad/kb/internal/errs"
	"github.com/nickstrad/kb/internal/prior"
	"github.com/nickstrad/kb/internal/store"
)

// PriorState is what the feedback prior did for one search (PLAN 6.2), for the trace and
// kb explain. The prior multiplies the fused score only when Active: prior.enabled, not turned
// off for the request (--no-prior) and at least MinJudged searches judged as of the search time.
type PriorState struct {
	Enabled   bool // [prior] enabled
	NoPrior   bool // Request.NoPrior: the caller turned it off for this search
	NoSource  bool // enabled, but the searcher has no judged log to read (an extra store of a cross-store search)
	Judged    int  // distinct judged searches counted (primary-store verdicts and opens before the search); 0 when the log was not read
	MinJudged int  // [prior] min_judged
	Active    bool
}

// Inactive says why the prior did not apply, "" when it did or when it is simply off
// (prior.enabled = false), which needs no explanation.
func (p PriorState) Inactive() string {
	if r := p.Reason(); r != "" {
		return "prior inactive: " + r
	}
	return ""
}

// Reason is Inactive without its "prior inactive: " prefix (the query-memory list reuses it).
func (p PriorState) Reason() string {
	switch {
	case !p.Enabled || p.Active:
		return ""
	case p.NoPrior:
		return "--no-prior"
	case p.NoSource:
		return "no judged log for this store"
	}
	return fmt.Sprintf("%d/%d judged searches", p.Judged, p.MinJudged)
}

// evidence is the judged log of one search, read once (Q130) and shared by the feedback prior and
// the query-memory list: the log as read, the prior model built from it and the gate's state.
type evidence struct {
	state  PriorState
	judged []store.JudgedSearch // nil when the log was not read
	model  prior.Model
	asOf   time.Time
}

// loadEvidence is the gate and the single read of a search: when prior.enabled and the request
// allows it and the searcher has a judged log, it reads the log as of req.AsOf (zero = the
// searcher's clock; only verdicts and opens recorded strictly before it count) and builds the
// model. A closed gate reads nothing.
func (s *Searcher) loadEvidence(ctx context.Context, req Request, p Params) (*evidence, error) {
	const op = "search.Search"
	ev := &evidence{state: PriorState{Enabled: p.Prior.Enabled, NoPrior: req.NoPrior, MinJudged: p.Prior.MinJudged}}
	if !ev.state.Enabled || ev.state.NoPrior {
		return ev, nil
	}
	if s.Prior == nil {
		ev.state.NoSource = true
		return ev, nil
	}
	ev.asOf = req.AsOf
	if ev.asOf.IsZero() {
		ev.asOf = s.now()
	}
	judged, err := s.Prior.Judged(ctx, store.JudgedFilter{})
	if err != nil {
		return nil, err
	}
	m, err := prior.Build(priorEvents(judged), p.Prior, ev.asOf)
	if err != nil {
		// config.Validate guarantees Build's params (prior.Validate accepts every validated [prior]).
		return nil, errs.Wrap(err, errs.Internal, op, "build the feedback prior: %v", err)
	}
	ev.judged, ev.model = judged, m
	ev.state.Judged, ev.state.Active = m.Judged(), m.Active()
	return ev, nil
}

// apply is the prior step of a search: when the gate is open it multiplies every candidate's RRF
// score by its (path, heading) multiplier, records the multiplier in Candidate.Prior and re-sorts
// cands (Fuse's order) in place. It runs after the supersede demotion and before the diversity
// cap. Both are multiplicative, so the scores do not depend on their order; the sort is by the
// final score either way. When the gate is closed it changes nothing (every Candidate.Prior stays nil).
func (ev *evidence) apply(cands []Candidate) {
	if !ev.state.Active || len(cands) == 0 {
		return
	}
	for i := range cands {
		mult := ev.model.Mult(cands[i].EntryPath, cands[i].Heading)
		cands[i].Prior = &mult
		cands[i].RRFScore *= mult
	}
	sort.Slice(cands, func(i, j int) bool { return fusedBefore(cands[i], cands[j]) })
	if assert.Enabled {
		for i := 1; i < len(cands); i++ {
			assert.Post(fusedBefore(cands[i-1], cands[i]), "applyPrior: cands[%d] (chunk %d) does not sort before cands[%d] (chunk %d)",
				i-1, cands[i-1].ChunkID, i, cands[i].ChunkID)
		}
	}
}

// priorEvents maps the judged log to prior evidence. A verdict is stamped with the time it was
// recorded and an open with the time of the read, not the search's time: a replay at the time of
// an earlier search (asOf) must not see a verdict given afterwards. The rank of a verdict is the
// rank it was given at; an open carries none (position bias applies to hits judged at a rank). A
// whole-search --none names no (path, heading), so it is no evidence for the prior. Verdicts on
// other stores' hits keep their Store, which prior.Build ignores.
func priorEvents(judged []store.JudgedSearch) []prior.Event {
	var out []prior.Event
	for _, j := range judged {
		for _, v := range j.Verdicts {
			kind := prior.NotUseful
			if v.Useful {
				kind = prior.Useful
			}
			out = append(out, prior.Event{SearchID: j.ID, EntryPath: v.EntryPath, Heading: v.Heading,
				Kind: kind, Rank: v.Rank, At: v.At, Store: v.Store})
		}
		for _, path := range j.Opens {
			out = append(out, prior.Event{SearchID: j.ID, EntryPath: path, Kind: prior.Open, At: j.OpenedAt[path]})
		}
	}
	return out
}
