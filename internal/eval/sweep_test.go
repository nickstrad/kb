package eval

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"math/rand/v2"
	"reflect"
	"slices"
	"strings"
	"testing"

	"github.com/BurntSushi/toml"

	"github.com/nickstrad/kb/internal/config"
	"github.com/nickstrad/kb/internal/errs"
	"github.com/nickstrad/kb/internal/search"
	"github.com/nickstrad/kb/internal/store"
)

// TestParseAxis is the --sweep syntax table: [search] and [prior] keys, minus k and stores; values are
// trimmed, non-empty and distinct.
func TestParseAxis(t *testing.T) {
	for _, tc := range []struct {
		spec string
		want Axis
		err  string
	}{
		{spec: "search.rrf_k=20,40,60", want: Axis{Key: "search.rrf_k", Values: []string{"20", "40", "60"}}},
		{spec: " search.bm25.title = 1 , 3 ", want: Axis{Key: "search.bm25.title", Values: []string{"1", "3"}}},
		{spec: "search.exact_list=false", want: Axis{Key: "search.exact_list", Values: []string{"false"}}},
		{spec: "search.rrf_k", err: "want key=v1,v2"},
		{spec: "=1,2", err: "want key=v1,v2"},
		{spec: "search.k=4,8", err: "cannot sweep search.k"},
		{spec: "search.stores=a,b", err: "cannot sweep search.stores"},
		{spec: "prior.max_boost=0.1,0.3", want: Axis{Key: "prior.max_boost", Values: []string{"0.1", "0.3"}}},
		{spec: "prior.enabled=false,true", want: Axis{Key: "prior.enabled", Values: []string{"false", "true"}}},
		{spec: "embed.model=none", err: "only [search] and [prior] keys sweep"},
		{spec: "eval.holdout=0.1", err: "only [search] and [prior] keys sweep"},
		{spec: "searchy.rrf_k=1", err: "only [search] and [prior] keys sweep"},
		{spec: "priory.max_boost=1", err: "only [search] and [prior] keys sweep"},
		{spec: "search.rrf_k=", err: "empty value"},
		{spec: "search.rrf_k=1,,2", err: "empty value"},
		{spec: "search.rrf_k=1, 1", err: `value "1" twice`},
	} {
		t.Run(tc.spec, func(t *testing.T) {
			got, err := ParseAxis(tc.spec)
			if tc.err != "" {
				if errs.KindOf(err) != errs.Usage || !strings.Contains(err.Error(), tc.err) || !strings.Contains(err.Error(), "--sweep") {
					t.Fatalf("err %v, want a Usage error naming --sweep and %q", err, tc.err)
				}
				return
			}
			if err != nil || !reflect.DeepEqual(got, tc.want) {
				t.Fatalf("got %+v, %v; want %+v", got, err, tc.want)
			}
		})
	}
}

func axis(t *testing.T, spec string) Axis {
	t.Helper()
	a, err := ParseAxis(spec)
	if err != nil {
		t.Fatal(err)
	}
	return a
}

// values is "1,2,…,n".
func values(n int) string {
	vs := make([]string, n)
	for i := range vs {
		vs[i] = fmt.Sprint(i + 1)
	}
	return strings.Join(vs, ",")
}

// TestExpandGrid: the product's order (first axis slowest) and the typed values that land in the
// config; bad keys and values fail as Usage errors naming --sweep; the cap is checked at, under
// and over MaxGrid.
func TestExpandGrid(t *testing.T) {
	base := config.Default()
	grid, err := ExpandGrid(base, []Axis{axis(t, "search.rrf_k=10,20"), axis(t, "search.bm25.title=1,2.5"), axis(t, "search.exact_list=false")}, MaxGrid)
	if err != nil {
		t.Fatal(err)
	}
	var sets []string
	for _, c := range grid {
		sets = append(sets, strings.Join(c.Set, " "))
	}
	want := []string{
		"search.rrf_k=10 search.bm25.title=1 search.exact_list=false",
		"search.rrf_k=10 search.bm25.title=2.5 search.exact_list=false",
		"search.rrf_k=20 search.bm25.title=1 search.exact_list=false",
		"search.rrf_k=20 search.bm25.title=2.5 search.exact_list=false",
	}
	if !slices.Equal(sets, want) {
		t.Errorf("grid order\n got %q\nwant %q", sets, want)
	}
	if s := grid[3].Config.Search; s.RRFK != 20 || s.BM25.Title != 2.5 || s.ExactList || s.BM25.Text != base.Search.BM25.Text {
		t.Errorf("combo 3 config %+v", s)
	}
	if base.Search.RRFK != config.Default().Search.RRFK {
		t.Error("ExpandGrid changed its base")
	}
	if g, err := ExpandGrid(base, []Axis{axis(t, "search.synonyms=my syn.txt")}, MaxGrid); err != nil || g[0].Config.Search.Synonyms != "my syn.txt" {
		t.Errorf("a bare string value: %v, %+v", err, g)
	}

	for _, tc := range []struct {
		name string
		axes []string
		max  int
		err  string // "" = ok
	}{
		{"at the cap", []string{"search.rrf_k=" + values(10), "search.vec_overfetch=" + values(20)}, MaxGrid, ""},
		{"one under the cap", []string{"search.rrf_k=" + values(199)}, MaxGrid, ""},
		{"one over the cap", []string{"search.rrf_k=" + values(201)}, MaxGrid, "more than 200 settings"},
		{"over the cap on the third axis", []string{"search.rrf_k=" + values(10), "search.vec_overfetch=" + values(10), "search.per_entry_cap=1,2,3"}, MaxGrid, "more than 200 settings"},
		{"a small cap", []string{"search.rrf_k=1,2,3"}, 2, "more than 2 settings"},
		{"key twice", []string{"search.rrf_k=1", "search.rrf_k=2"}, MaxGrid, "search.rrf_k is given twice"},
		{"unknown key", []string{"search.nope=1"}, MaxGrid, `--sweep search.nope=1: unknown key "search.nope"`},
		{"a table", []string{"search.bm25=1"}, MaxGrid, "is a table"},
		{"wrong type", []string{"search.rrf_k=abc"}, MaxGrid, "--sweep search.rrf_k=abc"},
		{"out of range", []string{"search.rrf_k=10,0"}, MaxGrid, "--sweep search.rrf_k=0"},
		{"bad bool", []string{"search.exact_list=maybe"}, MaxGrid, "--sweep search.exact_list=maybe"},
		{"bad value on the second axis", []string{"search.rrf_k=10", "search.bm25.title=-1"}, MaxGrid, "--sweep search.bm25.title=-1"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var axes []Axis
			for _, s := range tc.axes {
				axes = append(axes, axis(t, s))
			}
			g, err := ExpandGrid(base, axes, tc.max)
			if tc.err == "" {
				size := 1
				for _, a := range axes {
					size *= len(a.Values)
				}
				if err != nil || len(g) != size {
					t.Fatalf("%d combos, %v; want %d", len(g), err, size)
				}
				return
			}
			if errs.KindOf(err) != errs.Usage || !strings.Contains(err.Error(), tc.err) {
				t.Fatalf("err %v, want a Usage error with %q", err, tc.err)
			}
		})
	}
}

// TestGate: the min_judged boundary (29, 30, 31 against 30), --force, a threshold of 0, and an
// empty log that even --force cannot sweep.
// TestPriorGate is the [prior]-axis gate table (PLAN 6.4): judged queries around eval.min_judged
// (no --force escape), a grid that never enables the prior, and axes it does not concern.
func TestPriorGate(t *testing.T) {
	on := config.Default()
	on.Prior.Enabled = true
	off := config.Default()
	for _, tc := range []struct {
		name             string
		keys             []string
		grid             []config.Config
		judged, minJudge int
		err              string
	}{
		{"prior axis one below", []string{"prior.max_boost"}, []config.Config{on}, 29, 30, "--sweep prior.max_boost needs ≥30 judged queries (have 29); --force does not apply"},
		{"prior axis at the threshold", []string{"prior.max_boost"}, []config.Config{on}, 30, 30, ""},
		{"prior axis, none judged, threshold 0", []string{"prior.max_boost"}, []config.Config{on}, 0, 0, ""},
		{"second axis is prior", []string{"search.rrf_k", "prior.half_life_days"}, []config.Config{on}, 3, 30, "--sweep prior.half_life_days needs"},
		{"search axis below the threshold: Gate's call", []string{"search.rrf_k"}, []config.Config{off}, 3, 30, ""},
		{"prior never enabled", []string{"prior.max_boost"}, []config.Config{off, off}, 30, 30, "changes nothing while prior.enabled is false"},
		{"enabled in one grid point", []string{"prior.enabled"}, []config.Config{off, on}, 30, 30, ""},
		{"search axis, prior off", []string{"search.rrf_k"}, []config.Config{off}, 30, 30, ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			axes := make([]Axis, len(tc.keys))
			for i, k := range tc.keys {
				axes[i] = Axis{Key: k, Values: []string{"1"}}
			}
			grid := make([]Combo, len(tc.grid))
			for i, c := range tc.grid {
				grid[i] = Combo{Config: c}
			}
			err := PriorGate(axes, grid, tc.judged, tc.minJudge)
			if tc.err == "" {
				if err != nil {
					t.Fatalf("refused: %v", err)
				}
				return
			}
			if errs.KindOf(err) != errs.Usage || !strings.Contains(err.Error(), tc.err) {
				t.Fatalf("err %v, want a Usage error with %q", err, tc.err)
			}
		})
	}
}

func TestGate(t *testing.T) {
	for _, tc := range []struct {
		name             string
		judged, minJudge int
		force            bool
		err              string
	}{
		{"one below", 29, 30, false, "need ≥30 judged queries (have 29); use --force"},
		{"at the threshold", 30, 30, false, ""},
		{"one above", 31, 30, false, ""},
		{"one below, forced", 29, 30, true, ""},
		{"one judged, forced", 1, 30, true, ""},
		{"threshold 0", 1, 0, false, ""},
		{"none", 0, 30, false, "no judged queries to sweep"},
		{"none, forced", 0, 30, true, "no judged queries to sweep"},
		{"none, threshold 0", 0, 0, false, "no judged queries to sweep"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			err := Gate(tc.judged, tc.minJudge, tc.force)
			if tc.err == "" {
				if err != nil {
					t.Fatalf("refused: %v", err)
				}
				return
			}
			if errs.KindOf(err) != errs.Usage || !strings.Contains(err.Error(), tc.err) {
				t.Fatalf("err %v, want a Usage error with %q", err, tc.err)
			}
		})
	}
}

func keys(n int) []string {
	ks := make([]string, n)
	for i := range ks {
		ks[i] = fmt.Sprintf("q%02d", i)
	}
	return ks
}

// TestSplitSizes is the held-out size table: round(holdout*n), kept within [1, n-1] once there
// are two queries and a holdout, none for n < 2 or holdout 0.
func TestSplitSizes(t *testing.T) {
	for _, tc := range []struct {
		n       int
		holdout float64
		test    int
	}{
		{0, 0.3, 0},
		{1, 0.3, 0},
		{1, 0.9, 0},
		{2, 0.3, 1},     // round(0.6) = 1
		{2, 0.01, 1},    // rounds to 0: raised to 1
		{2, 0.99, 1},    // rounds to 2: lowered to n-1
		{3, 0.3, 1},     // round(0.9)
		{10, 0, 0},      // nothing held out
		{10, 0.3, 3},    // the default
		{10, 0.25, 3},   // round(2.5) = 3 (half away from zero)
		{10, 0.24, 2},   // round(2.4)
		{10, 0.999, 9},  // capped at n-1
		{30, 0.3, 9},    // min_judged at the default holdout
		{100, 0.001, 1}, // a tiny holdout still holds one out
	} {
		t.Run(fmt.Sprintf("n=%d holdout=%g", tc.n, tc.holdout), func(t *testing.T) {
			ks := keys(tc.n)
			train, test := Split(ks, tc.holdout, DefaultSeed)
			if len(test) != tc.test || len(train) != tc.n-tc.test {
				t.Fatalf("train %d test %d, want %d/%d", len(train), len(test), tc.n-tc.test, tc.test)
			}
			all := append(slices.Clone(train), test...)
			slices.Sort(all)
			if !slices.Equal(all, ks) {
				t.Errorf("train ∪ test = %q, want every key once", all)
			}
			if !slices.IsSorted(train) || !slices.IsSorted(test) {
				t.Errorf("sides not sorted: %q %q", train, test)
			}
		})
	}
}

// TestSplitDeterministic: the split depends on the keys and the seed only, never on their order,
// and another seed gives another split.
func TestSplitDeterministic(t *testing.T) {
	ks := keys(40)
	train, test := Split(ks, 0.3, DefaultSeed)
	r := rand.New(rand.NewPCG(7, 7))
	for range 5 {
		shuffled := slices.Clone(ks)
		r.Shuffle(len(shuffled), func(i, j int) { shuffled[i], shuffled[j] = shuffled[j], shuffled[i] })
		tr, te := Split(shuffled, 0.3, DefaultSeed)
		if !slices.Equal(tr, train) || !slices.Equal(te, test) {
			t.Fatalf("the split changed with the input order: %q vs %q", te, test)
		}
	}
	if _, te := Split(ks, 0.3, 2); slices.Equal(te, test) {
		t.Errorf("seeds 1 and 2 hold out the same queries %q", te)
	}
	// The split is part of what a sweep reports, so its hash is pinned: a change would silently
	// move queries between the sides of every user's next sweep.
	if want := []string{"q01", "q02", "q06", "q08", "q09", "q11", "q13", "q15", "q20", "q23", "q28", "q29"}; !slices.Equal(test, want) {
		t.Errorf("held out %q, want %q", test, want)
	}
	// Not just the first or last keys: a hash order, not the input order.
	if slices.Equal(test, ks[:len(test)]) || slices.Equal(test, ks[len(ks)-len(test):]) {
		t.Errorf("held out %q: an input-order prefix or suffix", test)
	}
}

// TestRankPoints is the selection order: higher train MRR, then nDCG, then hit rate (a difference
// of exactly tieEps is a tie), then the current settings, then grid order (stable sort).
func TestRankPoints(t *testing.T) {
	p := func(mrr, ndcg, hit float64, current bool) point {
		return point{mrr: mrr, ndcg: ndcg, hit: hit, current: current}
	}
	for _, tc := range []struct {
		name string
		a, b point
		want int
	}{
		{"higher mrr first", p(0.6, 0, 0, false), p(0.5, 1, 1, true), -1},
		{"lower mrr last", p(0.5, 1, 1, true), p(0.6, 0, 0, false), 1},
		{"mrr tie: higher ndcg", p(0.5, 0.7, 0, false), p(0.5, 0.6, 1, true), -1},
		{"mrr tie: lower ndcg", p(0.5, 0.6, 1, true), p(0.5, 0.7, 0, false), 1},
		{"mrr, ndcg tie: higher hit", p(0.5, 0.5, 0.9, false), p(0.5, 0.5, 0.8, true), -1},
		{"mrr, ndcg tie: lower hit", p(0.5, 0.5, 0.8, true), p(0.5, 0.5, 0.9, false), 1},
		{"all tie: current first", p(0.5, 0.5, 0.5, true), p(0.5, 0.5, 0.5, false), -1},
		{"all tie: current first, reversed", p(0.5, 0.5, 0.5, false), p(0.5, 0.5, 0.5, true), 1},
		{"all tie, neither current: grid order", p(0.5, 0.5, 0.5, false), p(0.5, 0.5, 0.5, false), 0},
		{"mrr higher by exactly tieEps: tie", p(tieEps, 0.4, 0, false), p(0, 0.5, 0, false), 1},
		{"mrr lower by exactly tieEps: tie", p(0, 0.5, 0, false), p(tieEps, 0.4, 0, false), -1},
		{"mrr higher by 2*tieEps: decides", p(2*tieEps, 0.4, 0, false), p(0, 0.5, 0, false), -1},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := rankPoints(tc.a, tc.b); got != tc.want {
				t.Fatalf("rankPoints = %d, want %d", got, tc.want)
			}
		})
	}
}

// sweepWorld is a log of n judged queries ("q00"…), each with gold a.md#H1, and a searcher
// factory whose ranking of that gold is rank(params, query): 0 leaves it out of the top k.
type sweepWorld struct {
	n         int
	rank      func(p search.Params, q string) int
	err       func(p search.Params) error
	fallback  bool
	factories int
	searches  int
}

func (w *sweepWorld) prepared(t *testing.T) *Prepared {
	t.Helper()
	var log []store.JudgedSearch
	for i, k := range keys(w.n) {
		log = append(log, store.JudgedSearch{ID: int64(i + 1), Query: strings.ToUpper(k), Verdicts: []store.Verdict{useful(1, "a.md", "H1")}})
	}
	// One more query whose only gold is an open: replayed, never judged.
	log = append(log, store.JudgedSearch{ID: int64(w.n + 1), Query: "opened", Opens: []string{"ccc.md"}})
	p, err := Prepare(context.Background(), stamped(log), corpus())
	if err != nil {
		t.Fatal(err)
	}
	return p
}

func (w *sweepWorld) newSearcher(s search.Settings) Searcher {
	w.factories++
	return &worldSearcher{w: w, p: s.Params}
}

type worldSearcher struct {
	w *sweepWorld
	p search.Params
}

func (s *worldSearcher) Search(_ context.Context, req search.Request) (*search.Outcome, error) {
	s.w.searches++
	if s.w.err != nil {
		if err := s.w.err(s.p); err != nil {
			return nil, err
		}
	}
	res := search.NewResult(req.Query, req.Mode)
	r := s.w.rank(s.p, Normalize(req.Query))
	for i := 1; i <= req.K; i++ {
		h := search.Hit{Rank: i, Path: "bb.md", Heading: "H2"}
		if i == r {
			h.Path, h.Heading = "a.md", "H1"
		}
		res.Hits = append(res.Hits, h)
	}
	out := &search.Outcome{Result: res, Mode: req.Mode}
	if s.w.fallback {
		out.Mode, out.FallbackErr = search.ModeFTSFallback, errors.New("model down")
	}
	return out, nil
}

func (w *sweepWorld) sweep(t *testing.T, opt SweepOptions, specs ...string) (*SweepReport, error) {
	t.Helper()
	base := config.Default()
	var axes []Axis
	for _, s := range specs {
		axes = append(axes, axis(t, s))
	}
	grid, err := ExpandGrid(base, axes, MaxGrid)
	if err != nil {
		t.Fatal(err)
	}
	if opt.Mode == "" {
		opt.Mode, opt.K, opt.MinJudged, opt.Holdout = "fts", 8, 30, 0.3
	}
	return Sweep(context.Background(), w.prepared(t), base, axes, grid, w.newSearcher, opt)
}

func mustSweep(t *testing.T, w *sweepWorld, opt SweepOptions, specs ...string) *SweepReport {
	t.Helper()
	rep, err := w.sweep(t, opt, specs...)
	if err != nil {
		t.Fatal(err)
	}
	return rep
}

// byRRFK ranks the gold at rank[rrf_k] for every query (the default 60 is the current config).
func byRRFK(rank map[int]int) func(search.Params, string) int {
	return func(p search.Params, _ string) int { return rank[p.RRFK] }
}

func sets(rows []SweepRow) []string {
	var out []string
	for _, r := range rows {
		out = append(out, strings.Join(r.Set, " "))
	}
	return out
}

// TestSweepPicksBestAndReportsHeldOut: the best setting on the training queries comes first, is
// scored on the held-out ones against the current config with Compare, and its swept keys come
// back as TOML. Every setting is replayed once; the current one is not replayed again when the
// grid holds it.
func TestSweepPicksBestAndReportsHeldOut(t *testing.T) {
	w := &sweepWorld{n: 40, rank: byRRFK(map[int]int{10: 1, 30: 3, 60: 2})}
	rep := mustSweep(t, w, SweepOptions{}, "search.rrf_k=30,60,10")
	if got := sets(rep.Top); !slices.Equal(got, []string{"search.rrf_k=10", "search.rrf_k=60", "search.rrf_k=30"}) {
		t.Fatalf("top %q", got)
	}
	if rep.Settings != 3 || rep.Replays != 3 || w.factories != 3 || w.searches != 3*41 {
		t.Errorf("settings %d replays %d factories %d searches %d; want 3, 3, 3, 3×41 (the grid holds the current rrf_k)", rep.Settings, rep.Replays, w.factories, w.searches)
	}
	if rep.Judged != 40 || len(rep.Train) != 28 || len(rep.Test) != 12 || rep.Holdout != 0.3 || rep.Seed != DefaultSeed {
		t.Errorf("judged %d, split %d/%d, holdout %g, seed %d", rep.Judged, len(rep.Train), len(rep.Test), rep.Holdout, rep.Seed)
	}
	best := rep.Top[0]
	if best.Rank != 1 || best.Current || best.Train.Queries != 28 || best.Train.MRR.Mean != 1 || best.Train.HitAtK.Mean != 1 ||
		best.Test == nil || best.Test.MRR.Mean != 1 || best.Test.Queries != 12 {
		t.Errorf("best %+v", best)
	}
	m := metricOf(best.TestVsCurrent, "mrr")
	if m.Baseline != 0.5 || m.Current != 1 || m.Delta != 0.5 || m.Wins != 12 || m.Losses != 0 || m.P >= 0.001 {
		t.Errorf("held-out mrr vs current %+v", m)
	}
	if tm := metricOf(best.TrainVsCurrent, "mrr"); tm.Wins != 28 {
		t.Errorf("train mrr vs current %+v", tm)
	}
	if !rep.Top[1].Current || rep.Top[1].Rank != 2 || rep.Top[1].TestVsCurrent == nil || metricOf(rep.Top[1].TestVsCurrent, "mrr").Ties != 12 || rep.Current.Rank != 0 || !rep.Current.Current || rep.Current.TestVsCurrent != nil || rep.Current.TrainVsCurrent != nil {
		t.Errorf("current rows: top[1] %+v, current %+v", rep.Top[1], rep.Current)
	}
	if rep.Current.Train.MRR.Mean != 0.5 || rep.Current.Test.MRR.Mean != 0.5 {
		t.Errorf("current scores %+v %+v", rep.Current.Train, rep.Current.Test)
	}
	if rep.BestIsCurrent || rep.Overfit || len(rep.Warnings) != 0 || rep.TOML != "[search]\nrrf_k = 10\n" {
		t.Errorf("best is current %v, overfit %v, warnings %q, toml %q", rep.BestIsCurrent, rep.Overfit, rep.Warnings, rep.TOML)
	}
	if len(rep.Grid) != 3 || math.Abs(rep.Grid[0].TrainMRR-1.0/3) > 1e-12 || rep.Grid[2].TrainMRR != 1 || rep.Grid[2].TestMRR == nil || *rep.Grid[2].TestMRR != 1 ||
		rep.Grid[0].TrainNDCG != 0.5 || rep.Grid[0].TrainHit != 1 {
		t.Errorf("grid %+v", rep.Grid)
	}
}

// TestSweepTopIsCapped: at most opt.Top rows, best first.
func TestSweepTopIsCapped(t *testing.T) {
	w := &sweepWorld{n: 30, rank: func(p search.Params, _ string) int { return p.RRFK }}
	rep := mustSweep(t, w, SweepOptions{}, "search.rrf_k="+values(8))
	// Literal 5: the default is pinned here, so changing DefaultTop fails this row.
	if got := sets(rep.Top); len(got) != 5 || got[0] != "search.rrf_k=1" || got[4] != "search.rrf_k=5" || len(rep.Warnings) != 0 {
		t.Errorf("top %q", got)
	}
	opt := SweepOptions{Mode: "fts", K: 8, MinJudged: 30, Holdout: 0.3, Top: 2}
	if rep := mustSweep(t, w, opt, "search.rrf_k="+values(8)); len(rep.Top) != 2 || len(rep.Grid) != 8 {
		t.Errorf("Top 2: %d rows, grid %d", len(rep.Top), len(rep.Grid))
	}
}

// TestSweepTopCollapsesEqualSettings: values that differ as text but hash alike (1 and 1.0 for a
// float key) are one setting: Top lists it once (the first in grid order), the Grid keeps both
// points, and Replays counts the distinct settings.
func TestSweepTopCollapsesEqualSettings(t *testing.T) {
	w := &sweepWorld{n: 30, rank: byRRFK(map[int]int{60: 2})}
	rep := mustSweep(t, w, SweepOptions{}, "search.bm25.title=3,1.0,1,2")
	got := sets(rep.Top)
	if slices.Contains(got, "search.bm25.title=1") || !slices.Contains(got, "search.bm25.title=1.0") || len(got) != 3 {
		t.Errorf("top %q; want 3 rows, 1.0 listed once and 1 collapsed into it", got)
	}
	for i, r := range rep.Top {
		if r.Rank != i+1 {
			t.Errorf("top[%d] rank %d", i, r.Rank)
		}
	}
	if len(rep.Grid) != 4 || rep.Settings != 4 || rep.Grid[1].SettingsHash != rep.Grid[2].SettingsHash {
		t.Errorf("grid %+v settings %d", rep.Grid, rep.Settings)
	}
}

// TestSweepTiesKeepCurrent: a setting that only ties the current config is not recommended; among
// tied non-current settings, the grid's order decides.
func TestSweepTiesKeepCurrent(t *testing.T) {
	w := &sweepWorld{n: 30, rank: byRRFK(map[int]int{10: 1, 20: 1, 60: 1})}
	rep := mustSweep(t, w, SweepOptions{}, "search.rrf_k=10,60,20")
	if !rep.BestIsCurrent || !rep.Top[0].Current || rep.TOML != "" || rep.Overfit {
		t.Errorf("tied with current: top %q, best is current %v, toml %q", sets(rep.Top), rep.BestIsCurrent, rep.TOML)
	}
	for _, order := range []string{"20,10", "10,20"} {
		w := &sweepWorld{n: 30, rank: byRRFK(map[int]int{10: 1, 20: 1, 60: 2})}
		rep := mustSweep(t, w, SweepOptions{}, "search.rrf_k="+order)
		if want := "search.rrf_k=" + order[:2]; rep.Top[0].Set[0] != want || rep.BestIsCurrent {
			t.Errorf("grid %s: best %q, want %s", order, rep.Top[0].Set, want)
		}
	}
}

// TestSweepCurrentOutsideGrid: when the grid does not hold the current settings, its best point is
// recommended only if it strictly beats them on train; worse or tied keeps the current (no TOML,
// no overfit warning). Before the W10 pass a worse grid point was recommended.
func TestSweepCurrentOutsideGrid(t *testing.T) {
	for _, tc := range []struct {
		name     string
		rank     map[int]int
		wantBest bool
	}{
		{"current strictly better", map[int]int{10: 3, 60: 1}, true},
		{"tied with current", map[int]int{10: 1, 60: 1}, true},
		{"grid point strictly better", map[int]int{10: 1, 60: 3}, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			w := &sweepWorld{n: 40, rank: byRRFK(tc.rank)}
			rep := mustSweep(t, w, SweepOptions{}, "search.rrf_k=10")
			if rep.BestIsCurrent != tc.wantBest || (rep.TOML == "") != tc.wantBest || rep.Overfit || len(rep.Warnings) != 0 {
				t.Errorf("best is current %v (want %v), toml %q, overfit %v, warnings %q", rep.BestIsCurrent, tc.wantBest, rep.TOML, rep.Overfit, rep.Warnings)
			}
		})
	}
}

// TestSweepOverfit: a setting that wins on the training queries but loses on the held-out ones is
// flagged, with a warning, and still listed first (selection never looks at the held-out side).
func TestSweepOverfit(t *testing.T) {
	train, _ := Split(keys(40), 0.3, DefaultSeed)
	inTrain := keySet(train)
	w := &sweepWorld{n: 40, rank: func(p search.Params, q string) int {
		switch {
		case p.RRFK == 60:
			return 2
		case inTrain[q]:
			return 1
		}
		return 3
	}}
	rep := mustSweep(t, w, SweepOptions{}, "search.rrf_k=10")
	if tm := rep.Grid[0].TestMRR; tm == nil || math.Abs(*tm-1.0/3) > 1e-12 || rep.Grid[0].TrainMRR != 1 {
		t.Errorf("grid point %+v: train mrr 1, held-out 1/3", rep.Grid[0])
	}
	if !rep.Overfit || rep.Top[0].Set[0] != "search.rrf_k=10" || len(rep.Warnings) != 1 ||
		!strings.Contains(rep.Warnings[0], "does not beat the current settings on the 12 held-out queries (mrr -0.167): likely overfitting") {
		t.Errorf("overfit %v, top %q, warnings %q", rep.Overfit, sets(rep.Top), rep.Warnings)
	}
	// A held-out tie is not a win either.
	w.rank = func(p search.Params, q string) int {
		if p.RRFK != 60 && inTrain[q] {
			return 1
		}
		return 2
	}
	if rep := mustSweep(t, w, SweepOptions{}, "search.rrf_k=10"); !rep.Overfit {
		t.Error("a held-out tie is not flagged")
	}
}

// TestOverfit: the decision is a win on train (MRR delta above tieEps) and no win held out (delta
// at most tieEps). Both boundaries are exact: a delta of exactly tieEps is a tie on either side.
func TestOverfit(t *testing.T) {
	above := math.Nextafter(tieEps, 1)
	for _, tc := range []struct {
		name          string
		trainD, testD float64
		want          bool
	}{
		{"train exactly tieEps: a tie, not a win", tieEps, -0.2, false},
		{"train just above tieEps, held out loses", above, -0.2, true},
		{"train wins, held-out delta exactly tieEps: a tie", 0.1, tieEps, true},
		{"train wins, held-out delta just above tieEps: a win", 0.1, above, false},
		{"train wins, held out wins", 0.1, 0.2, false},
		{"train loses, held out loses", -0.1, -0.2, false},
		{"train tie, held out tie", 0, 0, false},
	} {
		if got := overfit(tc.trainD, tc.testD); got != tc.want {
			t.Errorf("%s: overfit(%g, %g) = %v, want %v", tc.name, tc.trainD, tc.testD, got, tc.want)
		}
	}
}

// TestSweepNothingHeldOut: holdout 0 chooses and reports on every query, with a warning; a forced
// sweep below min_judged says so.
func TestSweepNothingHeldOut(t *testing.T) {
	w := &sweepWorld{n: 5, rank: byRRFK(map[int]int{10: 1, 60: 2})}
	rep := mustSweep(t, w, SweepOptions{Mode: "fts", K: 8, MinJudged: 30, Holdout: 0}, "search.rrf_k=10")
	if len(rep.Test) != 0 || rep.Top[0].Test != nil || rep.Top[0].TestVsCurrent != nil || rep.Current.Test != nil || rep.Grid[0].TestMRR != nil || rep.Overfit {
		t.Errorf("holdout 0: %+v", rep.Top[0])
	}
	want := []string{
		"forced below eval.min_judged: 5 judged queries < 30; the best setting is mostly noise",
		"nothing is held out (eval.holdout 0, 5 judged queries): the setting was chosen and scored on the same queries, so its numbers are optimistic",
	}
	if !slices.Equal(rep.Warnings, want) || rep.TOML == "" {
		t.Errorf("warnings %q toml %q", rep.Warnings, rep.TOML)
	}
	// One judged query: nothing can be held out even at the default holdout.
	w = &sweepWorld{n: 1, rank: byRRFK(map[int]int{10: 1, 60: 2})}
	if rep := mustSweep(t, w, SweepOptions{Mode: "fts", K: 8, MinJudged: 0, Holdout: 0.3}, "search.rrf_k=10"); len(rep.Train) != 1 || len(rep.Warnings) != 1 {
		t.Errorf("n=1: train %q warnings %q", rep.Train, rep.Warnings)
	}
}

// TestSweepFailures: a failed replay (of the current config or of a grid setting) stops the sweep
// and keeps its kind; fallback replays are counted, not errors.
func TestSweepFailures(t *testing.T) {
	for _, failOn := range []int{60, 10} {
		w := &sweepWorld{n: 3, rank: byRRFK(nil), err: func(p search.Params) error {
			if p.RRFK == failOn {
				return errs.New(errs.Conflict, "fake.Search", "locked")
			}
			return nil
		}}
		_, err := w.sweep(t, SweepOptions{}, "search.rrf_k=10")
		if errs.KindOf(err) != errs.Conflict || !strings.Contains(err.Error(), "locked") {
			t.Errorf("failing rrf_k=%d: %v, want the Conflict", failOn, err)
		}
		// A failed grid setting is named; the current config's replay has no setting to name.
		if named := strings.Contains(err.Error(), "--sweep setting search.rrf_k=10"); named != (failOn == 10) {
			t.Errorf("failing rrf_k=%d: %v, setting named %v", failOn, err, named)
		}
	}
	w := &sweepWorld{n: 3, rank: byRRFK(nil), fallback: true}
	rep := mustSweep(t, w, SweepOptions{}, "search.rrf_k=10")
	if rep.FallbackQueries != 4 || rep.FallbackReason != "model down" {
		t.Errorf("fallback %d %q", rep.FallbackQueries, rep.FallbackReason)
	}
}

// TestSweepFallbackOnlyInGrid: a fallback in a grid replay but not the current one is reported.
func TestSweepFallbackOnlyInGrid(t *testing.T) {
	w := &sweepWorld{n: 3, rank: byRRFK(nil)}
	fs := &fallbackOn{sweepWorld: w, rrfk: 10}
	base := config.Default()
	a := axis(t, "search.rrf_k=10")
	grid, _ := ExpandGrid(base, []Axis{a}, MaxGrid)
	rep, err := Sweep(context.Background(), w.prepared(t), base, []Axis{a}, grid, fs.newSearcher,
		SweepOptions{Mode: "fts", K: 8, MinJudged: 0, Holdout: 0.3})
	if err != nil || rep.FallbackQueries != 4 || rep.FallbackReason != "model down" {
		t.Errorf("err %v fallback %d %q", err, rep.FallbackQueries, rep.FallbackReason)
	}
}

type fallbackOn struct {
	*sweepWorld
	rrfk int
}

func (f *fallbackOn) newSearcher(s search.Settings) Searcher {
	w := *f.sweepWorld
	w.fallback = s.Params.RRFK == f.rrfk
	return &worldSearcher{w: &w, p: s.Params}
}

// TestTOMLSnippet: the swept keys with the values the config holds, nested tables included.
func TestTOMLSnippet(t *testing.T) {
	grid, err := ExpandGrid(config.Default(), []Axis{axis(t, "search.rrf_k=20"), axis(t, "search.bm25.title=5"),
		axis(t, "search.exact_list=false"), axis(t, "search.synonyms=syn.txt"), axis(t, "search.bm25.text=2")}, MaxGrid)
	if err != nil {
		t.Fatal(err)
	}
	want := "[search]\nexact_list = false\nrrf_k = 20\nsynonyms = \"syn.txt\"\n[search.bm25]\ntext = 2.0\ntitle = 5.0\n"
	if got := tomlSnippet(grid[0]); got != want {
		t.Errorf("snippet\n%s\nwant\n%s", got, want)
	}
	var back config.Config
	if _, err := toml.Decode(want, &back); err != nil || back.Search.RRFK != 20 || back.Search.BM25.Title != 5 {
		t.Errorf("the snippet does not decode back: %v %+v", err, back.Search)
	}
}

// TestSweepJSONContract: the field names of --sweep --json.
func TestSweepJSONContract(t *testing.T) {
	w := &sweepWorld{n: 4, rank: byRRFK(map[int]int{10: 1, 60: 2})}
	rep := mustSweep(t, w, SweepOptions{}, "search.rrf_k=10")
	data, err := json.Marshal(rep)
	if err != nil {
		t.Fatal(err)
	}
	for _, key := range []string{`"mode":"fts"`, `"k":8`, `"axes":[{"key":"search.rrf_k","values":["10"]}]`, `"settings":1`, `"replays":2`,
		`"judged":4`, `"min_judged":30`, `"holdout":0.3`, `"seed":1`, `"resamples":1000`, `"confidence":0.95`, `"train":[`, `"test":[`,
		`"current":{"rank":0,"set":[]`, `"top":[{"rank":1,"set":["search.rrf_k=10"]`, `"settings_hash":`, `"train_vs_current":{"paired":3`,
		`"test_vs_current":{"paired":1`, `"test":{"queries":1`, `"grid":[{"set":["search.rrf_k=10"]`, `"train_mrr":1`, `"test_mrr":1`, `"best_is_current":false`,
		`"overfit":false`, `"toml":"[search]\nrrf_k = 10\n"`, `"fts_fallback_queries":0`, `"warnings":[`, `"mrr":{"mean":`} {
		if !strings.Contains(string(data), key) {
			t.Errorf("JSON lacks %s:\n%s", key, data)
		}
	}
}

// TestPrepareAndMetricEdges: a failed index read is Prepare's error with its kind; a comparison
// with nothing paired has no metrics to name.
func TestPrepareAndMetricEdges(t *testing.T) {
	c := corpus()
	c.errPath = "a.md"
	_, err := Prepare(context.Background(), stamped([]store.JudgedSearch{{ID: 1, Query: "q", Verdicts: []store.Verdict{useful(1, "a.md", "H1")}}}), c)
	if errs.KindOf(err) != errs.Conflict || !strings.Contains(err.Error(), "locked") {
		t.Errorf("Prepare on a failing index: %v", err)
	}
	if _, ok := (VariantDiff{}).Metric("mrr"); ok {
		t.Error("an empty comparison named a metric")
	}
}

// FuzzParseAxis: an accepted --sweep spec has a [search] key other than search.k/stores and
// trimmed, non-empty, distinct values, and re-parses to itself from its canonical form; a
// refused one is a Usage error naming --sweep.
func FuzzParseAxis(f *testing.F) {
	for _, s := range []string{"search.rrf_k=20,40,60", " search.bm25.title = 1 , 3 ", "search.rrf_k", "=1,2", "search.k=4,8",
		"search.stores=a,b", "embed.model=none", "search.rrf_k=", "search.rrf_k=1,,2", "search.rrf_k=1, 1", "search.x=a=b,c", "search.é= x"} {
		f.Add(s)
	}
	f.Fuzz(func(t *testing.T, spec string) {
		a, err := ParseAxis(spec)
		if err != nil {
			if errs.KindOf(err) != errs.Usage || !strings.Contains(err.Error(), "--sweep") {
				t.Fatalf("ParseAxis(%q) = %v (kind %v), want Usage naming --sweep", spec, err, errs.KindOf(err))
			}
			return
		}
		if !strings.HasPrefix(a.Key, "search.") || a.Key == "search.k" || a.Key == "search.stores" || a.Key != strings.TrimSpace(a.Key) || len(a.Values) == 0 {
			t.Fatalf("ParseAxis(%q) accepted %+v", spec, a)
		}
		seen := map[string]bool{}
		for _, v := range a.Values {
			if v == "" || v != strings.TrimSpace(v) || seen[v] || strings.Contains(v, ",") {
				t.Fatalf("ParseAxis(%q): bad value %q in %q", spec, v, a.Values)
			}
			seen[v] = true
		}
		again, err := ParseAxis(a.Key + "=" + strings.Join(a.Values, ","))
		if err != nil || !reflect.DeepEqual(again, a) {
			t.Fatalf("round trip of %+v = %+v, %v", a, again, err)
		}
	})
}
