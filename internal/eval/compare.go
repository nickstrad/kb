package eval

import (
	"fmt"
	"math"

	"github.com/nickstrad/kb/internal/assert"
	"github.com/nickstrad/kb/internal/errs"
)

// tieEps is how close two per-query scores must be to tie: ranks and nDCG are computed from the
// same formulas on both sides, so only float noise separates "equal" scores.
const tieEps = 1e-9

// Comparison is a run against a baseline, query by query. Its JSON is part of the kb eval
// --compare --json contract.
type Comparison struct {
	Baseline Side `json:"baseline"`
	Current  Side `json:"current"`
	// Chunk is the headline; Entry and Opens are the other variants, paired the same way.
	Chunk    VariantDiff `json:"chunk"`
	Entry    VariantDiff `json:"entry"`
	Opens    VariantDiff `json:"opens"`
	Warnings []string    `json:"warnings"`
	// SmallSample: the paired chunk queries are fewer than the current [eval] min_judged.
	SmallSample bool `json:"small_sample"`
	MinJudged   int  `json:"min_judged"`
}

// Side identifies one run of a comparison.
type Side struct {
	Name         string `json:"name,omitempty"` // the baseline's name; empty for the current run
	CreatedAt    string `json:"created_at,omitempty"`
	KBVersion    string `json:"kb_version,omitempty"`
	SettingsHash string `json:"settings_hash"`
	Mode         string `json:"mode"`
	K            int    `json:"k"`
	EmbedModel   string `json:"embed_model"`
	EntryCount   int    `json:"entry_count"`
}

// VariantDiff compares one gold variant over the queries scored on both sides.
type VariantDiff struct {
	Paired   int          `json:"paired"`
	OnlyBase int          `json:"only_in_baseline"` // scored in the baseline, not in this run
	OnlyCur  int          `json:"only_in_current"`  // scored in this run, not in the baseline
	Metrics  []MetricDiff `json:"metrics"`          // hit_at_k, mrr, ndcg_at_k; empty when Paired is 0
}

// Metric is the metric named name (hit_at_k, mrr or ndcg_at_k); false when nothing was paired.
func (d VariantDiff) Metric(name string) (MetricDiff, bool) {
	for _, m := range d.Metrics {
		if m.Name == name {
			return m, true
		}
	}
	return MetricDiff{}, false
}

// MetricDiff is one metric over the paired queries. Delta is Current - Baseline, with a paired
// percentile bootstrap interval (resampling the per-query differences, as Bootstrap does).
// Wins, Losses and Ties count queries by the sign of the difference; P is the two-sided exact
// sign-test p-value over the wins and losses (1 when there are none).
type MetricDiff struct {
	Name     string  `json:"name"`
	Baseline float64 `json:"baseline"`
	Current  float64 `json:"current"`
	Delta    float64 `json:"delta"`
	Lo       float64 `json:"ci_low"`
	Hi       float64 `json:"ci_high"`
	Wins     int     `json:"wins"`
	Losses   int     `json:"losses"`
	Ties     int     `json:"ties"`
	P        float64 `json:"p"`
}

// Compare pairs the queries of base and cur by normalised query and compares the three variants.
// It errors (Usage) when the two ran at different k, whose metrics do not compare. Other
// differences (embed model, entry count, mode, other-store gold, fallback replays) are
// Warnings. cur is the run to judge: a Baseline that was not saved.
func Compare(base, cur *Baseline) (*Comparison, error) {
	if base.Report.K != cur.Report.K {
		return nil, errs.New(errs.Usage, "eval.Compare", "the baseline %q ran at k=%d and this run at k=%d; metrics at different k do not compare (use -k %d)",
			base.Name, base.Report.K, cur.Report.K, base.Report.K)
	}
	c := &Comparison{Baseline: side(base, true), Current: side(cur, false), MinJudged: cur.Report.MinJudged, Warnings: []string{}}
	rb, rc := &base.Report, &cur.Report
	c.Chunk = diffVariant(rb, rc, func(r Result) *Score { return r.Chunk }, rc.Resamples, rc.Seed)
	c.Entry = diffVariant(rb, rc, func(r Result) *Score { return r.Entry }, rc.Resamples, rc.Seed)
	c.Opens = diffVariant(rb, rc, func(r Result) *Score { return r.Opens }, rc.Resamples, rc.Seed)
	c.SmallSample = c.Chunk.Paired < c.MinJudged

	warn := func(format string, args ...any) { c.Warnings = append(c.Warnings, fmt.Sprintf(format, args...)) }
	if base.EmbedModel != cur.EmbedModel {
		warn("embed model differs: baseline %s, now %s", base.EmbedModel, cur.EmbedModel)
	}
	if base.EntryCount != cur.EntryCount {
		warn("entry count differs: baseline %d, now %d; the index changed since the baseline", base.EntryCount, cur.EntryCount)
	}
	if rb.OtherStore != rc.OtherStore {
		warn("gold from other stores changed (not scored): baseline %d chunks, %d opens, %d queries; now %d, %d, %d",
			rb.OtherStore.Chunks, rb.OtherStore.Opens, rb.OtherStore.Queries, rc.OtherStore.Chunks, rc.OtherStore.Opens, rc.OtherStore.Queries)
	}
	if rb.FallbackQueries > 0 || rc.FallbackQueries > 0 {
		warn("some replays ran --mode fts instead of the embedder (baseline %d, now %d); the comparison mixes modes", rb.FallbackQueries, rc.FallbackQueries)
	}
	if base.SettingsHash == cur.SettingsHash && rb.Mode == rc.Mode {
		warn("same settings and mode as the baseline: expect all ties unless the log or index changed")
	}
	return c, nil
}

func side(b *Baseline, named bool) Side {
	s := Side{SettingsHash: b.SettingsHash, Mode: b.Report.Mode, K: b.Report.K, EmbedModel: b.EmbedModel, EntryCount: b.EntryCount}
	if named {
		s.Name, s.KBVersion = b.Name, b.KBVersion
		if !b.CreatedAt.IsZero() {
			s.CreatedAt = b.CreatedAt.Format("2006-01-02")
		}
	}
	return s
}

// diffVariant pairs the queries scored in a variant on both sides.
func diffVariant(base, cur *Report, pick func(Result) *Score, resamples int, seed uint64) VariantDiff {
	if resamples == 0 {
		resamples = DefaultResamples
	}
	if seed == 0 {
		seed = DefaultSeed
	}
	byKey := map[string]*Score{}
	for _, r := range base.PerQuery {
		if s := pick(r); s != nil {
			byKey[r.Query] = s
		}
	}
	var vd VariantDiff
	var bs, cs []Score
	seen := map[string]bool{}
	for _, r := range cur.PerQuery {
		s := pick(r)
		if s == nil {
			continue
		}
		seen[r.Query] = true
		b, ok := byKey[r.Query]
		if !ok {
			vd.OnlyCur++
			continue
		}
		bs, cs = append(bs, *b), append(cs, *s)
	}
	for k := range byKey {
		if !seen[k] {
			vd.OnlyBase++
		}
	}
	vd.Paired = len(bs)
	if vd.Paired == 0 {
		vd.Metrics = []MetricDiff{}
		return vd
	}
	val := func(name string, f func(Score) float64) MetricDiff {
		bx, cx := make([]float64, len(bs)), make([]float64, len(bs))
		for i := range bs {
			bx[i], cx[i] = f(bs[i]), f(cs[i])
		}
		return metricDiff(name, bx, cx, resamples, seed)
	}
	hit := func(s Score) float64 {
		if s.Hit {
			return 1
		}
		return 0
	}
	vd.Metrics = []MetricDiff{
		val("hit_at_k", hit),
		val("mrr", func(s Score) float64 { return s.RR }),
		val("ndcg_at_k", func(s Score) float64 { return s.NDCG }),
	}
	return vd
}

// metricDiff compares paired per-query values (equal length, > 0).
func metricDiff(name string, base, cur []float64, resamples int, seed uint64) MetricDiff {
	assert.Pre(len(base) > 0 && len(base) == len(cur), "metricDiff: %d baseline values, %d current", len(base), len(cur))
	d := MetricDiff{Name: name, Baseline: mean(base), Current: mean(cur)}
	diffs := make([]float64, len(base))
	for i := range base {
		diffs[i] = cur[i] - base[i]
		switch {
		case diffs[i] > tieEps:
			d.Wins++
		case diffs[i] < -tieEps:
			d.Losses++
		default:
			d.Ties++
		}
	}
	est := Bootstrap(diffs, resamples, Alpha, seed)
	d.Delta, d.Lo, d.Hi = est.Mean, est.Lo, est.Hi
	d.P = SignTest(d.Wins, d.Losses)
	return d
}

// SignTest is the two-sided exact sign-test p-value for wins and losses (ties already dropped):
// under the null each non-tied query is a win with probability 1/2, so
// p = min(1, 2 * P(X <= min(wins, losses))) for X ~ Binomial(wins+losses, 1/2). With no
// non-tied query there is no evidence either way and p is 1.
func SignTest(wins, losses int) float64 {
	assert.Pre(wins >= 0 && losses >= 0, "SignTest: wins=%d losses=%d", wins, losses)
	n := wins + losses // n == 0: one term, 2*1 capped at 1
	lgN, _ := math.Lgamma(float64(n) + 1)
	tail := 0.0
	for i := 0; i <= min(wins, losses); i++ {
		lgI, _ := math.Lgamma(float64(i) + 1)
		lgNI, _ := math.Lgamma(float64(n-i) + 1)
		tail += math.Exp(lgN - lgI - lgNI - float64(n)*math.Ln2) // C(n,i) / 2^n, in log space
	}
	return min(1, 2*tail)
}
