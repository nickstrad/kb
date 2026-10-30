package eval

import (
	"bytes"
	"cmp"
	"context"
	"crypto/sha256"
	"encoding/binary"
	"fmt"
	"math"
	"reflect"
	"slices"
	"strings"

	"github.com/BurntSushi/toml"

	"github.com/nickstrad/kb/internal/assert"
	"github.com/nickstrad/kb/internal/config"
	"github.com/nickstrad/kb/internal/errs"
	"github.com/nickstrad/kb/internal/search"
)

// MaxGrid is the most settings one sweep replays: each is a full replay of the judged log.
const MaxGrid = 200

// DefaultTop is how many settings a sweep reports with intervals and deltas.
const DefaultTop = 5

// Axis is one --sweep: a [search] key and the values to try, in the order given.
type Axis struct {
	Key    string   `json:"key"`
	Values []string `json:"values"`
}

// ParseAxis parses "search.key=v1,v2,…". Only [search] keys sweep, and not search.k (metrics at
// different k do not compare) or search.stores (kb eval replays this store only). Values are
// comma-separated, trimmed, non-empty and distinct; whether they suit the key is checked by
// ExpandGrid. Every failure is a Usage error.
func ParseAxis(spec string) (Axis, error) {
	fail := func(format string, args ...any) (Axis, error) {
		return Axis{}, errs.New(errs.Usage, "eval.ParseAxis", "--sweep %s: %s", spec, fmt.Sprintf(format, args...))
	}
	key, list, ok := strings.Cut(spec, "=")
	key = strings.TrimSpace(key)
	if !ok || key == "" {
		return fail("want key=v1,v2,…, such as search.rrf_k=20,40,60")
	}
	switch {
	case key == "search.k":
		return fail("cannot sweep search.k: metrics at different k do not compare; run kb eval -k N per k")
	case key == "search.stores":
		return fail("cannot sweep search.stores: kb eval replays this store only")
	case !strings.HasPrefix(key, "search."):
		return fail("only [search] keys sweep, such as search.rrf_k or search.bm25.title")
	}
	a := Axis{Key: key}
	for _, v := range strings.Split(list, ",") {
		v = strings.TrimSpace(v)
		if v == "" {
			return fail("empty value")
		}
		if slices.Contains(a.Values, v) {
			return fail("value %q twice", v)
		}
		a.Values = append(a.Values, v)
	}
	return a, nil
}

// Combo is one point of the grid: the assignments, one per axis, and the config they give.
type Combo struct {
	Set    []string      `json:"set"` // "search.rrf_k=20", in axis order
	Config config.Config `json:"-"`
}

// ExpandGrid is the cartesian product of axes applied to base, the first axis varying slowest.
// It refuses a key given twice and a grid of more than max settings before applying anything,
// then applies every combination (config.ApplyAs), so a bad value fails before any replay. Every
// failure is a Usage error.
func ExpandGrid(base config.Config, axes []Axis, max int) ([]Combo, error) {
	const op = "eval.ExpandGrid"
	assert.Pre(len(axes) > 0, "ExpandGrid: no axes")
	size := 1
	for i, a := range axes {
		assert.Pre(len(a.Values) > 0, "ExpandGrid: axis %s has no values", a.Key)
		for _, b := range axes[:i] {
			if b.Key == a.Key {
				return nil, errs.New(errs.Usage, op, "--sweep %s is given twice; list all its values in one --sweep", a.Key)
			}
		}
		size *= len(a.Values)
		if size > max {
			return nil, errs.New(errs.Usage, op, "the sweep grid has more than %d settings; use fewer values or fewer --sweep keys", max)
		}
	}
	out := make([]Combo, 0, size)
	idx := make([]int, len(axes))
	for {
		c := Combo{Config: base, Set: make([]string, len(axes))}
		for i, a := range axes {
			c.Set[i] = a.Key + "=" + a.Values[idx[i]]
			var err error
			if c.Config, err = config.ApplyAs(c.Config, "--sweep", c.Set[i]); err != nil {
				return nil, err
			}
		}
		out = append(out, c)
		// Odometer: the last axis turns fastest.
		i := len(axes) - 1
		for ; i >= 0; i-- {
			if idx[i]++; idx[i] < len(axes[i].Values) {
				break
			}
			idx[i] = 0
		}
		if i < 0 {
			assert.Post(len(out) == size, "ExpandGrid: %d combos, want %d", len(out), size)
			return out, nil
		}
	}
}

// Gate refuses a sweep on too few judged queries (fewer than minJudged, unless forced): with a
// small log the best setting is mostly noise. A log with none is refused even when forced.
func Gate(judged, minJudged int, force bool) error {
	const op = "eval.Gate"
	if judged == 0 {
		return errs.New(errs.Usage, op, "no judged queries to sweep; mark hits with kb feedback <search-id> <rank> --useful")
	}
	if judged < minJudged && !force {
		return errs.New(errs.Usage, op, "need ≥%d judged queries (have %d); use --force", minJudged, judged)
	}
	return nil
}

// Split divides the judged query keys into the keys that choose a setting (train) and the keys
// held out to report it (test), both sorted. The order is by SHA-256 of (seed, key), so the same
// keys and seed always split the same way, and a key's side does not depend on the order the
// log returned it. The test side is round(holdout*n) keys, at least 1 and at most n-1 when
// holdout > 0 and n >= 2, so both sides are never empty; with n < 2 or holdout 0 nothing is held
// out. holdout must be in [0, 1). The keys are distinct (Extract groups queries by key).
func Split(keys []string, holdout float64, seed uint64) (train, test []string) {
	assert.Pre(holdout >= 0 && holdout < 1, "Split: holdout=%g outside [0, 1)", holdout)
	n := len(keys)
	nTest := 0
	if holdout > 0 && n >= 2 {
		nTest = min(max(int(math.Round(holdout*float64(n))), 1), n-1)
	}
	type ranked struct {
		key string
		h   [sha256.Size]byte
	}
	rs := make([]ranked, n)
	var seedBytes [8]byte
	binary.LittleEndian.PutUint64(seedBytes[:], seed)
	for i, k := range keys {
		rs[i] = ranked{key: k, h: sha256.Sum256(append(seedBytes[:], k...))}
	}
	slices.SortFunc(rs, func(a, b ranked) int { return cmp.Or(bytes.Compare(a.h[:], b.h[:]), cmp.Compare(a.key, b.key)) })
	test, train = make([]string, 0, nTest), make([]string, 0, n-nTest)
	for i, r := range rs {
		if i < nTest {
			test = append(test, r.key)
		} else {
			train = append(train, r.key)
		}
	}
	slices.Sort(train)
	slices.Sort(test)
	if assert.Enabled {
		assert.Post(len(train)+len(test) == n, "Split: %d+%d keys, want %d", len(train), len(test), n)
		assert.Post(slices.IsSorted(train) && slices.IsSorted(test), "Split: a side is not sorted")
		inTest := keySet(test)
		for _, k := range train {
			assert.Post(!inTest[k], "Split: key %q on both sides", k)
		}
	}
	return train, test
}

// SweepOptions are a sweep's settings. Resamples and Seed left zero mean DefaultResamples and
// DefaultSeed (Seed also seeds the split); Top zero means DefaultTop.
type SweepOptions struct {
	Mode      string
	K         int
	MinJudged int
	Holdout   float64
	Resamples int
	Seed      uint64
	Top       int
}

// SweepReport is one sweep; its JSON tags are the kb eval --sweep --json contract.
type SweepReport struct {
	Mode       string  `json:"mode"`
	K          int     `json:"k"`
	Axes       []Axis  `json:"axes"`
	Settings   int     `json:"settings"` // grid size
	Replays    int     `json:"replays"`  // distinct settings replayed, the current ones included
	Judged     int     `json:"judged"`
	MinJudged  int     `json:"min_judged"`
	Holdout    float64 `json:"holdout"`
	Seed       uint64  `json:"seed"`
	Resamples  int     `json:"resamples"`
	Confidence float64 `json:"confidence"`
	// Train chose the setting; Test is held out to report it. Keys are normalised queries.
	Train []string `json:"train"`
	Test  []string `json:"test"`
	// Current is the config the sweep varies (after any --set); Top the best settings on Train,
	// best first; Grid every setting in grid order.
	Current SweepRow    `json:"current"`
	Top     []SweepRow  `json:"top"`
	Grid    []GridPoint `json:"grid"`
	// BestIsCurrent: no setting beats the current one on Train.
	BestIsCurrent bool `json:"best_is_current"`
	// Overfit: the best setting beats the current one on Train but not on Test.
	Overfit bool `json:"overfit"`
	// TOML is the best setting's swept keys as a config snippet; "" when it is the current one.
	TOML            string   `json:"toml"`
	FallbackQueries int      `json:"fts_fallback_queries"`
	FallbackReason  string   `json:"fts_fallback_reason,omitempty"`
	Warnings        []string `json:"warnings"`
}

// SweepRow is one setting with intervals: chunk-level metrics on each side, and its paired
// comparison with the current config (Compare's chunk variant) on each side.
type SweepRow struct {
	Rank           int          `json:"rank"` // 1 = best on train; 0 for the current config
	Set            []string     `json:"set"`
	SettingsHash   string       `json:"settings_hash"`
	Current        bool         `json:"current"` // the same settings as the current config
	Train          SplitScore   `json:"train"`
	Test           *SplitScore  `json:"test"`             // nil when nothing is held out
	TrainVsCurrent *VariantDiff `json:"train_vs_current"` // nil on the current row
	TestVsCurrent  *VariantDiff `json:"test_vs_current"`  // nil on the current row or with no test
}

// SplitScore is the chunk-level metrics over one side of the split.
type SplitScore struct {
	Queries int      `json:"queries"`
	HitAtK  Estimate `json:"hit_at_k"`
	MRR     Estimate `json:"mrr"`
	NDCG    Estimate `json:"ndcg_at_k"`
}

// GridPoint is one setting's means, without intervals.
type GridPoint struct {
	Set          []string `json:"set"`
	SettingsHash string   `json:"settings_hash"`
	TrainMRR     float64  `json:"train_mrr"`
	TrainNDCG    float64  `json:"train_ndcg_at_k"`
	TrainHit     float64  `json:"train_hit_at_k"`
	TestMRR      *float64 `json:"test_mrr"` // nil when nothing is held out
}

// point is a replayed setting during selection.
type point struct {
	index   int // grid order
	hash    string
	current bool
	rep     *Report
	mrr     float64 // train means
	ndcg    float64
	hit     float64
}

// Sweep replays p once for the current config base and once per distinct setting of grid (two
// combos with equal settings share a replay), with Searchers from newSearcher. The caller makes
// them share one query-vector memo (search.QueryMemo), so each query is embedded once whatever
// the grid size. The judged queries are Split by opt.Holdout; the settings are ranked on the
// train side by chunk MRR, then nDCG, then hit rate (ties within 1e-9), then the current settings
// first, then grid order; the top ones (one row per distinct setting) are reported on both sides against the current config
// with Compare. It never writes, and its warnings are data. The judged log must not be empty:
// callers run Gate first.
func Sweep(ctx context.Context, p *Prepared, base config.Config, axes []Axis, grid []Combo,
	newSearcher func(search.Settings) Searcher, opt SweepOptions) (*SweepReport, error) {
	rep, err := sweep(ctx, p, base, axes, grid, newSearcher, opt)
	if err != nil {
		return nil, errs.Wrap(err, errs.KindOf(err), "eval.Sweep", "%v", err)
	}
	return rep, nil
}

func sweep(ctx context.Context, p *Prepared, base config.Config, axes []Axis, grid []Combo,
	newSearcher func(search.Settings) Searcher, opt SweepOptions) (*SweepReport, error) {
	assert.Pre(len(grid) > 0, "Sweep: empty grid")
	if opt.Resamples == 0 {
		opt.Resamples = DefaultResamples
	}
	if opt.Seed == 0 {
		opt.Seed = DefaultSeed
	}
	if opt.Top == 0 {
		opt.Top = DefaultTop
	}
	judged := p.Judged()
	assert.Pre(len(judged) > 0, "Sweep: no judged queries; callers run Gate first")
	train, test := Split(judged, opt.Holdout, opt.Seed)
	sr := &SweepReport{
		Mode: opt.Mode, K: opt.K, Axes: axes, Settings: len(grid), Judged: len(judged), MinJudged: opt.MinJudged,
		Holdout: opt.Holdout, Seed: opt.Seed, Resamples: opt.Resamples, Confidence: 1 - Alpha,
		Train: train, Test: test, Top: []SweepRow{}, Grid: make([]GridPoint, 0, len(grid)), Warnings: []string{},
	}
	inTrain, inTest := keySet(train), keySet(test)
	evalOpt := Options{Mode: opt.Mode, K: opt.K, MinJudged: opt.MinJudged, Resamples: opt.Resamples, Seed: opt.Seed}

	replays := map[string]*Report{} // by settings hash
	replay := func(cfg config.Config) (*Report, string, error) {
		st, err := search.NewSettings(cfg)
		if err != nil { // unreachable: Internal from encoding a config that validation already accepted
			return nil, "", err
		}
		if r, ok := replays[st.Hash]; ok {
			return r, st.Hash, nil
		}
		o := evalOpt
		o.SettingsHash = st.Hash
		r, err := p.run(ctx, newSearcher(st), o, false)
		if err != nil {
			return nil, "", err
		}
		replays[st.Hash] = r
		return r, st.Hash, nil
	}

	curRep, curHash, err := replay(base)
	if err != nil {
		return nil, err
	}
	sr.FallbackQueries, sr.FallbackReason = curRep.FallbackQueries, curRep.FallbackReason
	points := make([]point, len(grid))
	for i, c := range grid {
		r, h, err := replay(c.Config)
		if err != nil {
			return nil, errs.Wrap(err, errs.KindOf(err), "eval.Sweep", "--sweep setting %s: %v", strings.Join(c.Set, " "), err).With("set", c.Set)
		}
		pt := point{index: i, hash: h, current: h == curHash, rep: r}
		pt.mrr, pt.ndcg, pt.hit = means(r, inTrain)
		points[i] = pt
		gp := GridPoint{Set: c.Set, SettingsHash: h, TrainMRR: pt.mrr, TrainNDCG: pt.ndcg, TrainHit: pt.hit}
		if len(test) > 0 {
			m, _, _ := means(r, inTest)
			gp.TestMRR = &m
		}
		sr.Grid = append(sr.Grid, gp)
		if r.FallbackQueries > sr.FallbackQueries {
			sr.FallbackQueries, sr.FallbackReason = r.FallbackQueries, r.FallbackReason
		}
	}
	sr.Replays = len(replays)

	ranked := slices.Clone(points)
	slices.SortStableFunc(ranked, rankPoints)
	sr.Current = row(curRep, curRep, inTrain, inTest, nil, curHash, true, opt)
	// Grid points with one settings hash (60 and 60.0) are one setting: Top lists the first.
	seen := map[string]bool{}
	for _, pt := range ranked {
		if len(sr.Top) == opt.Top {
			break
		}
		if seen[pt.hash] {
			continue
		}
		seen[pt.hash] = true
		r := row(curRep, pt.rep, inTrain, inTest, grid[pt.index].Set, pt.hash, pt.current, opt)
		r.Rank = len(sr.Top) + 1
		sr.Top = append(sr.Top, r)
	}

	// The current settings may be outside the grid: then the best grid point is recommended only
	// when it strictly beats them on train (a tie keeps the current, as within the grid).
	cur := point{current: true}
	cur.mrr, cur.ndcg, cur.hit = means(curRep, inTrain)
	best := sr.Top[0]
	sr.BestIsCurrent = best.Current || rankPoints(ranked[0], cur) >= 0
	if !sr.BestIsCurrent {
		sr.TOML = tomlSnippet(grid[ranked[0].index])
	}
	warn := func(format string, args ...any) { sr.Warnings = append(sr.Warnings, fmt.Sprintf(format, args...)) }
	if len(judged) < opt.MinJudged {
		warn("forced below eval.min_judged: %d judged queries < %d; the best setting is mostly noise", len(judged), opt.MinJudged)
	}
	if len(test) == 0 {
		warn("nothing is held out (eval.holdout %g, %d judged queries): the setting was chosen and scored on the same queries, so its numbers are optimistic",
			opt.Holdout, len(judged))
	} else if !sr.BestIsCurrent {
		trainD := metricOf(best.TrainVsCurrent, "mrr").Delta
		testD := metricOf(best.TestVsCurrent, "mrr").Delta
		if overfit(trainD, testD) {
			sr.Overfit = true
			warn("the best setting on the %d training queries does not beat the current settings on the %d held-out queries (mrr %+.3f): likely overfitting; keep the current settings",
				len(train), len(test), testD)
		}
	}
	return sr, nil
}

// overfit reports whether the best setting beats the current one on train (its MRR delta trainD
// is more than tieEps) but not on the held-out queries (testD at most tieEps). Only MRR decides:
// a best that ties MRR and wins nDCG is not checked.
func overfit(trainD, testD float64) bool {
	return trainD > tieEps && testD <= tieEps
}

// rankPoints orders settings best first: train MRR, nDCG, hit rate (higher first; within tieEps
// a tie), then the current settings, then grid order (the sort is stable).
func rankPoints(a, b point) int {
	for _, m := range [][2]float64{{a.mrr, b.mrr}, {a.ndcg, b.ndcg}, {a.hit, b.hit}} {
		if d := m[0] - m[1]; d > tieEps {
			return -1
		} else if d < -tieEps {
			return 1
		}
	}
	switch {
	case a.current && !b.current:
		return -1
	case b.current && !a.current:
		return 1
	}
	return 0
}

// means are the chunk-level means over the queries in keys, at least one of which is scored
// (every judged query is).
func means(r *Report, keys map[string]bool) (mrr, ndcg, hit float64) {
	n := 0
	for _, q := range r.PerQuery {
		if q.Chunk == nil || !keys[q.Query] {
			continue
		}
		n++
		mrr += q.Chunk.RR
		ndcg += q.Chunk.NDCG
		if q.Chunk.Hit {
			hit++
		}
	}
	assert.Pre(n > 0, "means: no scored query among %d keys", len(keys))
	return mrr / float64(n), ndcg / float64(n), hit / float64(n)
}

// row builds a SweepRow for rep against the current config's replay cur.
func row(cur, rep *Report, inTrain, inTest map[string]bool, set []string, hash string, current bool, opt SweepOptions) SweepRow {
	r := SweepRow{Set: set, SettingsHash: hash, Current: current, Train: splitScore(rep, inTrain, opt)}
	if r.Set == nil {
		r.Set = []string{}
	}
	if len(inTest) > 0 {
		s := splitScore(rep, inTest, opt)
		r.Test = &s
	}
	if rep == cur && set == nil { // the current row itself
		return r
	}
	r.TrainVsCurrent = chunkDiff(cur, rep, inTrain)
	if len(inTest) > 0 {
		r.TestVsCurrent = chunkDiff(cur, rep, inTest)
	}
	return r
}

// splitScore bootstraps the chunk metrics over the queries in keys (at least one is scored).
func splitScore(r *Report, keys map[string]bool, opt SweepOptions) SplitScore {
	var hit, rr, ndcg []float64
	for _, q := range r.PerQuery {
		if q.Chunk == nil || !keys[q.Query] {
			continue
		}
		h := 0.0
		if q.Chunk.Hit {
			h = 1
		}
		hit, rr, ndcg = append(hit, h), append(rr, q.Chunk.RR), append(ndcg, q.Chunk.NDCG)
	}
	return SplitScore{Queries: len(rr),
		HitAtK: Bootstrap(hit, opt.Resamples, Alpha, opt.Seed),
		MRR:    Bootstrap(rr, opt.Resamples, Alpha, opt.Seed),
		NDCG:   Bootstrap(ndcg, opt.Resamples, Alpha, opt.Seed)}
}

// chunkDiff is Compare's chunk variant of rep against cur over the queries in keys.
func chunkDiff(cur, rep *Report, keys map[string]bool) *VariantDiff {
	c, err := Compare(&Baseline{Report: subset(cur, keys)}, &Baseline{Report: subset(rep, keys)})
	assert.Post(err == nil, "Sweep: Compare of one k failed: %v", err)
	return &c.Chunk
}

// subset is r with only the queries in keys, enough for Compare.
func subset(r *Report, keys map[string]bool) Report {
	s := Report{Mode: r.Mode, K: r.K, MinJudged: r.MinJudged, Resamples: r.Resamples, Seed: r.Seed, PerQuery: []Result{}}
	for _, q := range r.PerQuery {
		if keys[q.Query] {
			s.PerQuery = append(s.PerQuery, q)
		}
	}
	return s
}

// metricOf is the named metric of d, which always has the three (a sweep pairs every judged
// query of a side, and a side is never empty when compared).
func metricOf(d *VariantDiff, name string) MetricDiff {
	m, ok := d.Metric(name)
	assert.Post(ok, "Sweep: no %s in the comparison", name)
	return m
}

func keySet(keys []string) map[string]bool {
	m := make(map[string]bool, len(keys))
	for _, k := range keys {
		m[k] = true
	}
	return m
}

// tomlSnippet is c's swept keys as config TOML, with the values c ends up holding (so a bare word
// comes out quoted and 5 as 5.0 where the key is a float).
func tomlSnippet(c Combo) string {
	doc := map[string]any{}
	for _, a := range c.Set {
		key, _, _ := strings.Cut(a, "=")
		segs := strings.Split(key, ".")
		m := doc
		for _, s := range segs[:len(segs)-1] {
			next, ok := m[s].(map[string]any)
			if !ok {
				next = map[string]any{}
				m[s] = next
			}
			m = next
		}
		m[segs[len(segs)-1]] = valueAt(reflect.ValueOf(c.Config), segs)
	}
	var buf bytes.Buffer
	enc := toml.NewEncoder(&buf)
	enc.Indent = ""
	err := enc.Encode(doc)
	assert.Post(err == nil, "Sweep: encode the TOML snippet: %v", err)
	return buf.String()
}

// valueAt walks v by toml tags; the path was already resolved by config.ApplyAs.
func valueAt(v reflect.Value, segs []string) any {
	for _, s := range segs {
		t, i := v.Type(), 0
		for i < t.NumField() && t.Field(i).Tag.Get("toml") != s {
			i++
		}
		assert.Post(i < t.NumField(), "Sweep: no key %s in %s", s, t)
		v = v.Field(i)
	}
	return v.Interface()
}
