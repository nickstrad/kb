package search

import (
	"context"
	"errors"

	"github.com/nickstrad/kb/internal/assert"
	"github.com/nickstrad/kb/internal/errs"
	"github.com/nickstrad/kb/internal/store"
)

// ErrLogFailed marks "the search ran, but writing it to the log did not". SearchAndLog wraps it
// around the failure, so the CLI can still print the hits, warn and exit 0.
var ErrLogFailed = errors.New("search log")

// Record writes one completed search through s.Log and returns the new search id, which it also
// stores in o.Result.SearchID. It records the mode that actually ran (o.Mode, "fts-fallback" when
// hybrid lost its embedder). A stage that did not run in that mode is recorded as NULL, not 0, so
// averages over a stage count only the searches that ran it: fts-fallback embedded (and timed the
// attempt) but ran no vector list.
func (s *Searcher) Record(ctx context.Context, req Request, o *Outcome) (int64, error) {
	assert.Pre(s.Log != nil && o != nil, "Record: nil Log or Outcome")
	mode := o.Mode
	if mode == "" {
		mode = req.Mode
	}
	if mode == "" {
		mode = ModeHybrid
	}
	caller := req.Caller
	if caller == "" {
		caller = "unknown"
	}
	stage := func(ran bool, ms int64) *int64 {
		if !ran {
			return nil
		}
		return &ms
	}
	rec := store.SearchRecord{
		TS:         s.now(),
		Query:      req.Query,
		Mode:       mode,
		K:          o.K,
		Tag:        o.TagFilter,
		EmbedModel: o.EmbedModel,
		NFTS:       o.NFTS,
		NVec:       o.NVec,
		FTSMs:      stage(mode != ModeVec, o.FTSMs),
		VecMs:      stage(mode == ModeHybrid || mode == ModeVec, o.VecMs),
		EmbedMs:    stage(mode != ModeFTS, o.EmbedMs),
		TotalMs:    o.TotalMs,
		Caller:     caller,
		KBVersion:  s.KBVersion,
		CorpusRev:  s.CorpusRev,
		// The per-store settings the search ran with (Outcome.Settings), not the defaults.
		Settings:     string(o.Settings.JSON),
		SettingsHash: o.Settings.Hash,
		FTSMatch:     o.FTSMatch,
		Results:      loggedHits(o.Returned),
		Candidates:   loggedHits(o.Candidates),
	}
	id, err := s.Log.LogSearch(ctx, rec)
	if err != nil {
		return 0, err
	}
	if o.Result != nil {
		o.Result.SearchID = id
	}
	return id, nil
}

func loggedHits(cs []Candidate) []store.LoggedHit {
	out := make([]store.LoggedHit, len(cs))
	for i, c := range cs {
		out[i] = store.LoggedHit{
			Store: c.Store, ChunkID: c.ChunkID, EntryPath: c.EntryPath, Heading: c.Heading,
			FTSRank: c.FTSRank, FTSScore: c.FTSScore, VecRank: c.VecRank, VecDistance: c.VecDistance,
			ExactRank: c.ExactRank, ExactScore: c.ExactScore,
			RRFScore: c.RRFScore,
		}
	}
	return out
}

// SearchAndLog runs the search, then records it. A search that fell back to FTS is still logged;
// a search that failed is not (there is no outcome).
//
// Partial success: when the search succeeded but the log write failed, the outcome is returned
// with an error wrapping ErrLogFailed, and out.Result.SearchID stays 0:
//
//	out, err := s.SearchAndLog(ctx, req)
//	if err != nil && !errors.Is(err, search.ErrLogFailed) {
//		return err // the search itself failed; out is nil
//	}
func (s *Searcher) SearchAndLog(ctx context.Context, req Request) (*Outcome, error) {
	out, err := s.Search(ctx, req)
	if err != nil {
		return nil, err
	}
	if _, err := s.Record(ctx, req, out); err != nil {
		return out, errs.Wrap(errors.Join(ErrLogFailed, err), errs.KindOf(err), "search.SearchAndLog", "%v", err)
	}
	return out, nil
}
