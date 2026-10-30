package search

import (
	"context"
	"errors"
	"fmt"
	"math"
	"slices"
	"testing"
	"time"

	"github.com/nickstrad/kb/internal/config"
	"github.com/nickstrad/kb/internal/errs"
	"github.com/nickstrad/kb/internal/prior"
	"github.com/nickstrad/kb/internal/store"
	"github.com/nickstrad/kb/internal/store/sqlite"
)

var (
	priorEvidenceAt = time.Date(2026, 10, 1, 9, 0, 0, 0, time.UTC) // when the searches were logged
	priorNow        = time.Date(2026, 10, 2, 9, 0, 0, 0, time.UTC) // the searcher's clock
)

// logJudged logs n searches that each returned entry path at rank 1 and marks it useful (a
// verdict) one minute later: n judged searches in the primary's log.
func logJudged(t *testing.T, st *sqlite.Store, n int, path string) {
	t.Helper()
	ctx := context.Background()
	for range n {
		id, err := st.LogSearch(ctx, store.SearchRecord{TS: priorEvidenceAt, Query: "sprocket", Mode: "fts", K: 8, Caller: "c",
			Results: []store.LoggedHit{{ChunkID: 1, EntryPath: path, RRFScore: 1}}})
		if err != nil {
			t.Fatal(err)
		}
		if err := st.Feedback(ctx, id, 1, true, "", priorEvidenceAt.Add(time.Minute)); err != nil {
			t.Fatal(err)
		}
	}
}

// priorSearcher is a Searcher over st with [prior] set as given, reading and writing st's log.
func priorSearcher(t *testing.T, st *sqlite.Store, enabled bool, minJudged int) *Searcher {
	t.Helper()
	cfg := config.Default()
	cfg.Prior = config.Prior{Enabled: enabled, MinJudged: minJudged, HalfLifeDays: 90, MaxBoost: 0.2}
	settings, err := NewSettings(cfg)
	if err != nil {
		t.Fatal(err)
	}
	return &Searcher{Store: st, Log: st, Prior: st, Settings: settings, Now: func() time.Time { return priorNow }}
}

// TestPriorGate is the gate's decision table: enabled x judged searches around min_judged x
// --no-prior. Only an enabled prior with at least min_judged judged searches that the request did
// not turn off multiplies the scores; every other row leaves them alone, logs NULL and says why.
func TestPriorGate(t *testing.T) {
	const minJudged = 3
	for _, tc := range []struct {
		name     string
		enabled  bool
		judged   int
		noPrior  bool
		active   bool
		inactive string
	}{
		{"disabled, plenty judged", false, minJudged + 2, false, false, ""},
		{"enabled, none judged", true, 0, false, false, "prior inactive: 0/3 judged searches"},
		{"enabled, below min_judged", true, minJudged - 1, false, false, "prior inactive: 2/3 judged searches"},
		{"enabled, at min_judged", true, minJudged, false, true, ""},
		{"enabled, above min_judged", true, minJudged + 1, false, true, ""},
		{"enabled, at min_judged, --no-prior", true, minJudged, true, false, "prior inactive: --no-prior"},
		{"disabled, --no-prior", false, minJudged, true, false, ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			ctx := context.Background()
			st := supersedeStore(t, nil)
			logJudged(t, st, tc.judged, "new.md")
			s := priorSearcher(t, st, tc.enabled, minJudged)
			base, err := priorSearcher(t, st, false, minJudged).Search(ctx, Request{Query: "sprocket", Mode: ModeFTS})
			if err != nil {
				t.Fatal(err)
			}
			out, err := s.SearchAndLog(ctx, Request{Query: "sprocket", Mode: ModeFTS, NoPrior: tc.noPrior})
			if err != nil {
				t.Fatal(err)
			}
			if out.Prior.Active != tc.active || out.Prior.Inactive() != tc.inactive {
				t.Errorf("Prior = %+v (inactive %q), want active=%v inactive %q", out.Prior, out.Prior.Inactive(), tc.active, tc.inactive)
			}
			wantOrder := paths(base)
			if tc.active {
				wantOrder = []string{"new.md", "old.md", "other.md"} // the judged entry wins the tie
			}
			if got := paths(out); !slices.Equal(got, wantOrder) {
				t.Errorf("order = %v, want %v", got, wantOrder)
			}
			for i, c := range out.Candidates {
				if (c.Prior != nil) != tc.active {
					t.Errorf("candidate %d (%s): Prior = %v, want set=%v", i, c.EntryPath, c.Prior, tc.active)
				}
			}
			// The log holds the multiplier for an active search and NULL for every other.
			tab, err := st.QueryLog(ctx, "SELECT prior FROM search_candidates WHERE search_id = ? ORDER BY fused_rank", out.Result.SearchID)
			if err != nil {
				t.Fatal(err)
			}
			for i, row := range tab.Rows {
				if (row[0] != nil) != tc.active {
					t.Errorf("search_candidates row %d prior = %v, want set=%v", i, row[0], tc.active)
				}
			}
			res, err := st.QueryLog(ctx, "SELECT prior FROM search_results WHERE search_id = ? ORDER BY rank", out.Result.SearchID)
			if err != nil {
				t.Fatal(err)
			}
			if len(res.Rows) == 0 {
				t.Fatal("no results logged")
			}
			for i, row := range res.Rows {
				if (row[0] != nil) != tc.active {
					t.Errorf("search_results row %d prior = %v, want set=%v", i, row[0], tc.active)
				}
				if tc.active && *out.Result.Hits[i].Prior != row[0].(float64) {
					t.Errorf("hit %d prior %v differs from the logged %v", i, *out.Result.Hits[i].Prior, row[0])
				}
			}
		})
	}
}

// TestPriorMultipliesScore: the multiplier is the 6.1 model's, applied to the fused score (and the
// supersede penalty): score = fused x penalty x prior, whichever the order of the two.
func TestPriorMultipliesScore(t *testing.T) {
	ctx := context.Background()
	for _, tc := range []struct {
		name  string
		edges map[string][]store.EdgeInput
	}{
		{"no supersede", nil},
		{"with supersede", map[string][]store.EdgeInput{"new.md": supersedes("old.md")}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			st := supersedeStore(t, tc.edges)
			logJudged(t, st, 2, "old.md")
			s := priorSearcher(t, st, true, 1)
			plain, err := s.Search(ctx, Request{Query: "sprocket", Mode: ModeFTS, NoPrior: true})
			if err != nil {
				t.Fatal(err)
			}
			out, err := s.Search(ctx, Request{Query: "sprocket", Mode: ModeFTS})
			if err != nil {
				t.Fatal(err)
			}
			want := map[string]float64{}
			for _, c := range plain.Candidates {
				want[c.EntryPath] = c.RRFScore
			}
			for _, c := range out.Candidates {
				m := 1.0
				if c.EntryPath == "old.md" {
					// Two useful verdicts, one minute after 23 hours before the clock: u = 2 x 0.5^(23/24/90) x 1.
					u := 2 * math.Exp2(-(23*time.Hour+59*time.Minute).Hours()/24/90)
					p := (u + 2) / (u + 4)
					m = 1 + 0.2*(2*p-1)
				}
				if c.Prior == nil || math.Abs(*c.Prior-m) > 1e-9 {
					t.Fatalf("%s: Prior = %v, want %v", c.EntryPath, c.Prior, m)
				}
				if got := want[c.EntryPath] * m; math.Abs(c.RRFScore-got) > 1e-12 {
					t.Errorf("%s: score %v, want plain %v x %v = %v", c.EntryPath, c.RRFScore, want[c.EntryPath], m, got)
				}
			}
			if tc.edges != nil && (!out.Candidates[len(out.Candidates)-1].Superseded && !out.Candidates[0].Superseded) {
				// the superseded entry still carries both effects
				t.Errorf("no candidate is marked superseded: %v", paths(out))
			}
		})
	}
}

// TestPriorAsOf: the evidence is the log as of Request.AsOf (default: the searcher's clock). A
// verdict recorded at or after it is invisible, so replaying a search at its original time never
// sees the feedback it later received.
func TestPriorAsOf(t *testing.T) {
	ctx := context.Background()
	st := supersedeStore(t, nil)
	logJudged(t, st, 2, "new.md") // verdicts at priorEvidenceAt + 1 minute
	s := priorSearcher(t, st, true, 2)
	verdictAt := priorEvidenceAt.Add(time.Minute)
	for _, tc := range []struct {
		name   string
		asOf   time.Time
		active bool
		judged int
	}{
		{"default clock is after the evidence", time.Time{}, true, 2},
		{"just after the verdicts", verdictAt.Add(time.Second), true, 2},
		{"exactly at the verdicts", verdictAt, false, 0},
		{"before the searches ran", priorEvidenceAt.Add(-time.Hour), false, 0},
	} {
		t.Run(tc.name, func(t *testing.T) {
			out, err := s.Search(ctx, Request{Query: "sprocket", Mode: ModeFTS, AsOf: tc.asOf})
			if err != nil {
				t.Fatal(err)
			}
			if out.Prior.Active != tc.active || out.Prior.Judged != tc.judged {
				t.Errorf("Prior = %+v, want active=%v judged=%d", out.Prior, tc.active, tc.judged)
			}
		})
	}
}

// TestPriorWithoutRelevantEvidenceKeepsOrder is the property: when the judged entries are not among
// the candidates, an active prior is 1 everywhere and the order, scores and returned list are the
// prior-off search's.
func TestPriorWithoutRelevantEvidenceKeepsOrder(t *testing.T) {
	ctx := context.Background()
	for _, path := range []string{"elsewhere.md", "other-store/x.md", "new.md "} {
		t.Run(fmt.Sprintf("evidence on %q", path), func(t *testing.T) {
			st := supersedeStore(t, nil)
			logJudged(t, st, 3, path)
			s := priorSearcher(t, st, true, 3)
			off, err := s.Search(ctx, Request{Query: "sprocket", Mode: ModeFTS, NoPrior: true})
			if err != nil {
				t.Fatal(err)
			}
			on, err := s.Search(ctx, Request{Query: "sprocket", Mode: ModeFTS})
			if err != nil {
				t.Fatal(err)
			}
			if !on.Prior.Active {
				t.Fatalf("the gate is closed: %+v", on.Prior)
			}
			if !slices.Equal(paths(on), paths(off)) || len(on.Returned) != len(off.Returned) {
				t.Errorf("order changed: on %v off %v", paths(on), paths(off))
			}
			for i, c := range on.Candidates {
				if c.Prior == nil || *c.Prior != 1 || c.RRFScore != off.Candidates[i].RRFScore {
					t.Errorf("candidate %d (%s): prior %v score %v, want 1 and %v", i, c.EntryPath, c.Prior, c.RRFScore, off.Candidates[i].RRFScore)
				}
			}
		})
	}
}

// TestPriorOnlyThisStoreCounts: verdicts and opens on another store's hits are not evidence, and a
// searcher with no judged log (an extra store of a cross-store search) never applies the prior.
func TestPriorOnlyThisStoreCounts(t *testing.T) {
	ctx := context.Background()
	st := supersedeStore(t, nil)
	for range 3 {
		id, err := st.LogSearch(ctx, store.SearchRecord{TS: priorEvidenceAt, Query: "sprocket", Mode: "fts", K: 8, Caller: "c",
			Results: []store.LoggedHit{{Store: "notes@/x", ChunkID: 1, EntryPath: "new.md", RRFScore: 1}}})
		if err != nil {
			t.Fatal(err)
		}
		if err := st.Feedback(ctx, id, 1, true, "", priorEvidenceAt.Add(time.Minute)); err != nil {
			t.Fatal(err)
		}
	}
	s := priorSearcher(t, st, true, 1)
	out, err := s.Search(ctx, Request{Query: "sprocket", Mode: ModeFTS})
	if err != nil {
		t.Fatal(err)
	}
	if out.Prior.Active || out.Prior.Judged != 0 {
		t.Errorf("verdicts on another store's hits counted: %+v", out.Prior)
	}
	logJudged(t, st, 1, "new.md")
	s.Prior = nil
	out, err = s.Search(ctx, Request{Query: "sprocket", Mode: ModeFTS})
	if err != nil {
		t.Fatal(err)
	}
	if out.Prior.Active || !out.Prior.NoSource || out.Prior.Inactive() != "prior inactive: no judged log for this store" {
		t.Errorf("without a judged log: %+v (%q)", out.Prior, out.Prior.Inactive())
	}
}

type failingJudged struct{ err error }

func (f failingJudged) Judged(context.Context, store.JudgedFilter) ([]store.JudgedSearch, error) {
	return nil, f.err
}

// TestPriorErrors: a log that cannot be read fails the search with its kind (a degraded answer
// would hide the prior's absence), and Build rejecting params that validation let through is a
// kb bug: Internal. A closed gate never reads the log.
func TestPriorErrors(t *testing.T) {
	ctx := context.Background()
	st := supersedeStore(t, nil)
	s := priorSearcher(t, st, true, 1)

	s.Prior = failingJudged{errs.New(errs.Unavailable, "test", "log gone")}
	if _, err := s.Search(ctx, Request{Query: "sprocket", Mode: ModeFTS}); errs.KindOf(err) != errs.Unavailable {
		t.Errorf("unreadable log: err = %v (kind %v), want Unavailable", err, errs.KindOf(err))
	}
	if _, err := s.Search(ctx, Request{Query: "sprocket", Mode: ModeFTS, NoPrior: true}); err != nil {
		t.Errorf("--no-prior read the log: %v", err)
	}

	bad := DefaultParams()
	bad.Prior = prior.Params{Enabled: true, MinJudged: 1, HalfLifeDays: 0, MaxBoost: 0.2}
	s.Prior = st
	if _, err := s.loadEvidence(ctx, Request{}, bad); errs.KindOf(err) != errs.Internal || !errors.Is(err, prior.ErrBadParams) {
		t.Errorf("Build rejecting params: err = %v (kind %v), want Internal wrapping ErrBadParams", err, errs.KindOf(err))
	}
}

// TestPriorOpensCount: an open is evidence too, at the time of the read.
func TestPriorOpensCount(t *testing.T) {
	ctx := context.Background()
	st := supersedeStore(t, nil)
	id, err := st.LogSearch(ctx, store.SearchRecord{TS: priorEvidenceAt, Query: "sprocket", Mode: "fts", K: 8, Caller: "c",
		Results: []store.LoggedHit{{ChunkID: 1, EntryPath: "new.md", RRFScore: 1}}})
	if err != nil || id == 0 {
		t.Fatal(id, err)
	}
	if ok, err := st.RecordOpen(ctx, store.OpenRecord{EntryPath: "new.md", At: priorEvidenceAt.Add(time.Minute), Window: time.Hour}); err != nil || !ok {
		t.Fatalf("RecordOpen: %v %v", ok, err)
	}
	s := priorSearcher(t, st, true, 1)
	out, err := s.Search(ctx, Request{Query: "sprocket", Mode: ModeFTS})
	if err != nil {
		t.Fatal(err)
	}
	if !out.Prior.Active || out.Prior.Judged != 1 {
		t.Fatalf("Prior = %+v, want active on one opened search", out.Prior)
	}
	if got := paths(out); got[0] != "new.md" {
		t.Errorf("order = %v, want the opened entry first", got)
	}
	out, err = s.Search(ctx, Request{Query: "sprocket", Mode: ModeFTS, AsOf: priorEvidenceAt.Add(time.Minute)})
	if err != nil {
		t.Fatal(err)
	}
	if out.Prior.Active {
		t.Errorf("an open at the as-of instant counted: %+v", out.Prior)
	}
}

// TestPriorInactiveMessage pins the wording kb explain shows.
func TestPriorInactiveMessage(t *testing.T) {
	for _, tc := range []struct {
		st   PriorState
		want string
	}{
		{PriorState{Enabled: true, Judged: 12, MinJudged: 50}, "prior inactive: 12/50 judged searches"},
		{PriorState{Enabled: true, Judged: 50, MinJudged: 50, Active: true}, ""},
		{PriorState{Enabled: false, Judged: 12, MinJudged: 50}, ""},
		{PriorState{Enabled: true, NoPrior: true, Judged: 0, MinJudged: 50}, "prior inactive: --no-prior"},
		{PriorState{Enabled: true, NoSource: true, MinJudged: 50}, "prior inactive: no judged log for this store"},
	} {
		if got := tc.st.Inactive(); got != tc.want {
			t.Errorf("%+v.Inactive() = %q, want %q", tc.st, got, tc.want)
		}
	}
}

// TestExplainPrior: Explain reports the gate (inactive reason, counts) and, when active, each
// candidate's multiplier next to the RRF terms.
func TestExplainPrior(t *testing.T) {
	ctx := context.Background()
	st := supersedeStore(t, nil)
	logJudged(t, st, 2, "new.md")
	s := priorSearcher(t, st, true, 3)
	ex, err := s.Explain(ctx, Request{Query: "sprocket", Mode: ModeFTS})
	if err != nil {
		t.Fatal(err)
	}
	if want := (ExplainPrior{Enabled: true, Judged: 2, MinJudged: 3, HalfLifeDays: 90, MaxBoost: 0.2, Inactive: "prior inactive: 2/3 judged searches"}); ex.Prior != want {
		t.Errorf("inactive: Prior = %+v, want %+v", ex.Prior, want)
	}
	for _, h := range ex.Candidates {
		if h.Prior != nil {
			t.Errorf("%s: prior %v while inactive", h.Path, *h.Prior)
		}
	}
	s = priorSearcher(t, st, true, 2)
	if ex, err = s.Explain(ctx, Request{Query: "sprocket", Mode: ModeFTS}); err != nil {
		t.Fatal(err)
	}
	if !ex.Prior.Active || ex.Prior.Inactive != "" || ex.Prior.Judged != 2 {
		t.Errorf("active: Prior = %+v", ex.Prior)
	}
	if ex.Candidates[0].Path != "new.md" || ex.Candidates[0].Prior == nil || *ex.Candidates[0].Prior <= 1 {
		t.Errorf("top candidate = %+v, want new.md boosted", ex.Candidates[0])
	}
}

// TestPriorSingleCandidate: the multiplier applies to a list of one (and an empty list is a no-op
// that still reports the gate).
func TestPriorSingleCandidate(t *testing.T) {
	ctx := context.Background()
	st := supersedeStore(t, nil)
	logJudged(t, st, 2, "other.md")
	s := priorSearcher(t, st, true, 2)
	out, err := s.Search(ctx, Request{Query: "unrelated", Mode: ModeFTS})
	if err != nil {
		t.Fatal(err)
	}
	if len(out.Candidates) != 1 || out.Candidates[0].Prior == nil || *out.Candidates[0].Prior <= 1 {
		t.Fatalf("candidates = %+v, want the one chunk boosted", out.Candidates)
	}
	out, err = s.Search(ctx, Request{Query: "nonexistentword", Mode: ModeFTS})
	if err != nil {
		t.Fatal(err)
	}
	if len(out.Candidates) != 0 || !out.Prior.Active || out.Prior.Judged != 2 {
		t.Errorf("empty result: %d candidates, Prior %+v; want none and an active gate", len(out.Candidates), out.Prior)
	}
}
