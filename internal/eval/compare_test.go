package eval

import (
	"bytes"
	"encoding/json"
	"errors"
	"math"
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/nickstrad/kb/internal/errs"
)

// TestSignTest: exact two-sided p-values, computed by hand as min(1, 2*sum_{i<=min(w,l)} C(n,i)/2^n).
func TestSignTest(t *testing.T) {
	for _, tc := range []struct {
		name         string
		wins, losses int
		want         float64
	}{
		{"no queries: no evidence", 0, 0, 1},
		{"one win: 2*(1/2) capped at 1", 1, 0, 1},
		{"one loss", 0, 1, 1},
		{"all ties are dropped before the test: same as n=0", 0, 0, 1},
		{"5 wins 0 losses: 2/32", 5, 0, 0.0625},
		{"5 losses 0 wins is symmetric", 0, 5, 0.0625},
		{"6 wins 0 losses: 2/64", 6, 0, 0.03125},
		{"9 wins 1 loss of 10: 2*(1+10)/1024", 9, 1, 22.0 / 1024},
		{"8 wins 2 losses of 10: 2*(1+10+45)/1024", 8, 2, 112.0 / 1024},
		{"7 wins 3 losses of 10: 2*(1+10+45+120)/1024", 7, 3, 352.0 / 1024},
		{"balanced 5/5: tail is over half, capped at 1", 5, 5, 1},
		{"2 wins 1 loss of 3: 2*(1+3)/8 capped", 2, 1, 1},
		{"3 wins 0 losses of 3: 2/8", 3, 0, 0.25},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got := SignTest(tc.wins, tc.losses)
			if math.Abs(got-tc.want) > 1e-12 {
				t.Errorf("SignTest(%d, %d) = %.15g, want %.15g", tc.wins, tc.losses, got, tc.want)
			}
		})
	}
	// Large n stays finite and in [0, 1]: 600 vs 400 is about 1e-10 (z ~ 6.3), 5000 vs 0 underflows to 0.
	if got := SignTest(600, 400); !(got > 1e-12 && got < 1e-8) {
		t.Errorf("SignTest(600, 400) = %g, want about 1e-10", got)
	}
	if got := SignTest(5000, 0); got < 0 || got > 1e-300 || math.IsNaN(got) {
		t.Errorf("SignTest(5000, 0) = %g, want ~0 and not NaN", got)
	}
}

// scored is a per-query result with a chunk score at the given first gold rank (0 = miss), k=8.
func scored(q string, rank int) Result {
	s := ScoreRanking(matchesAt(rank), 1, 8)
	return Result{Query: q, Text: q, Chunk: &s}
}

func matchesAt(rank int) []int {
	m := make([]int, 8)
	for i := range m {
		m[i] = -1
	}
	if rank > 0 {
		m[rank-1] = 0
	}
	return m
}

func baselineOf(name string, k int, rs ...Result) *Baseline {
	return &Baseline{
		Name: name, EmbedModel: "m", EntryCount: 10, SettingsHash: "h-" + name,
		Report: Report{Mode: "hybrid", K: k, MinJudged: 30, PerQuery: rs},
	}
}

func metric(t *testing.T, vd VariantDiff, name string) MetricDiff {
	t.Helper()
	for _, m := range vd.Metrics {
		if m.Name == name {
			return m
		}
	}
	t.Fatalf("no metric %q in %+v", name, vd)
	return MetricDiff{}
}

// TestCompareDeltas: deltas, wins/losses/ties and p-values from hand-computed per-query scores.
func TestCompareDeltas(t *testing.T) {
	for _, tc := range []struct {
		name       string
		base, cur  []Result
		paired     int
		w, l, ties int // on mrr
		baseMRR    float64
		curMRR     float64
		p          float64
	}{
		{"no queries", nil, nil, 0, 0, 0, 0, 0, 0, 0},
		{"one query, improved", []Result{scored("a", 2)}, []Result{scored("a", 1)}, 1, 1, 0, 0, 0.5, 1, 1},
		{"all ties", []Result{scored("a", 1), scored("b", 0)}, []Result{scored("a", 1), scored("b", 0)}, 2, 0, 0, 2, 0.5, 0.5, 1},
		{"all wins (5): p 2/32", []Result{scored("a", 0), scored("b", 0), scored("c", 4), scored("d", 3), scored("e", 2)},
			[]Result{scored("a", 1), scored("b", 2), scored("c", 3), scored("d", 2), scored("e", 1)},
			5, 5, 0, 0, (0 + 0 + 0.25 + 1.0/3 + 0.5) / 5, (1 + 0.5 + 1.0/3 + 0.5 + 1) / 5, 0.0625},
		{"mixed: 2 wins, 1 loss, 1 tie", []Result{scored("a", 2), scored("b", 1), scored("c", 1), scored("d", 0)},
			[]Result{scored("a", 1), scored("b", 2), scored("c", 1), scored("d", 1)},
			4, 2, 1, 1, (0.5 + 1 + 1 + 0) / 4, (1 + 0.5 + 1 + 1) / 4, 1},
		{"rank 2 to 3 is a loss in mrr", []Result{scored("a", 2)}, []Result{scored("a", 3)}, 1, 0, 1, 0, 0.5, 1.0 / 3, 1},
	} {
		t.Run(tc.name, func(t *testing.T) {
			c, err := Compare(baselineOf("b", 8, tc.base...), baselineOf("now", 8, tc.cur...))
			if err != nil {
				t.Fatal(err)
			}
			if c.Chunk.Paired != tc.paired || c.Chunk.OnlyBase != 0 || c.Chunk.OnlyCur != 0 {
				t.Fatalf("chunk %+v, want %d paired", c.Chunk, tc.paired)
			}
			if tc.paired == 0 {
				if len(c.Chunk.Metrics) != 0 {
					t.Errorf("metrics of nothing: %+v", c.Chunk.Metrics)
				}
				return
			}
			m := metric(t, c.Chunk, "mrr")
			if m.Wins != tc.w || m.Losses != tc.l || m.Ties != tc.ties {
				t.Errorf("w/l/t %d/%d/%d, want %d/%d/%d", m.Wins, m.Losses, m.Ties, tc.w, tc.l, tc.ties)
			}
			if math.Abs(m.Baseline-tc.baseMRR) > 1e-12 || math.Abs(m.Current-tc.curMRR) > 1e-12 || math.Abs(m.Delta-(tc.curMRR-tc.baseMRR)) > 1e-12 {
				t.Errorf("means %g -> %g (delta %g), want %g -> %g", m.Baseline, m.Current, m.Delta, tc.baseMRR, tc.curMRR)
			}
			if math.Abs(m.P-tc.p) > 1e-12 {
				t.Errorf("p = %g, want %g", m.P, tc.p)
			}
			if m.Lo > m.Delta+1e-12 || m.Hi < m.Delta-1e-12 {
				t.Errorf("interval [%g, %g] excludes the delta %g", m.Lo, m.Hi, m.Delta)
			}
			if len(c.Chunk.Metrics) != 3 || c.Chunk.Metrics[0].Name != "hit_at_k" || c.Chunk.Metrics[2].Name != "ndcg_at_k" {
				t.Errorf("metrics %+v, want hit_at_k, mrr, ndcg_at_k", c.Chunk.Metrics)
			}
		})
	}
}

// TestCompareHitAndNDCG: hit@k and nDCG pair the same queries; a rank moving inside the top k is a
// tie for hit@k but a win for nDCG.
func TestCompareHitAndNDCG(t *testing.T) {
	c, err := Compare(baselineOf("b", 8, scored("a", 3), scored("b", 0)), baselineOf("n", 8, scored("a", 1), scored("b", 5)))
	if err != nil {
		t.Fatal(err)
	}
	hit, ndcg := metric(t, c.Chunk, "hit_at_k"), metric(t, c.Chunk, "ndcg_at_k")
	if hit.Wins != 1 || hit.Losses != 0 || hit.Ties != 1 || hit.Baseline != 0.5 || hit.Current != 1 {
		t.Errorf("hit %+v, want 1 win (b), 1 tie (a), 0.5 -> 1", hit)
	}
	if ndcg.Wins != 2 || ndcg.Ties != 0 {
		t.Errorf("ndcg %+v, want 2 wins", ndcg)
	}
}

// TestCompareSelfIsAllTies: a run against itself ties every query and has zero deltas, p = 1.
func TestCompareSelfIsAllTies(t *testing.T) {
	b := baselineOf("b", 8, scored("a", 1), scored("b", 3), scored("c", 0))
	c, err := Compare(b, b)
	if err != nil {
		t.Fatal(err)
	}
	for _, m := range c.Chunk.Metrics {
		if m.Wins != 0 || m.Losses != 0 || m.Ties != 3 || m.Delta != 0 || m.Lo != 0 || m.Hi != 0 || m.P != 1 {
			t.Errorf("%s against itself: %+v", m.Name, m)
		}
	}
	if len(c.Warnings) != 1 || !strings.Contains(c.Warnings[0], "same settings and mode") {
		t.Errorf("warnings %q, want the same-settings note", c.Warnings)
	}
}

// TestCompareUnpaired: a query on one side only is counted, not paired; a variant scored on one
// side only (the gold left the index) is unpaired too.
func TestCompareUnpaired(t *testing.T) {
	entryOnly := func(q string) Result { r := scored(q, 1); r.Entry, r.Chunk = r.Chunk, nil; return r }
	base := baselineOf("b", 8, scored("a", 1), scored("gone", 2), scored("both-but-dropped", 1), entryOnly("e"))
	cur := baselineOf("n", 8, scored("a", 2), scored("new1", 1), scored("new2", 1), entryOnly("e"),
		func() Result { r := scored("both-but-dropped", 1); r.Chunk = nil; return r }())
	c, err := Compare(base, cur)
	if err != nil {
		t.Fatal(err)
	}
	if c.Chunk.Paired != 1 || c.Chunk.OnlyBase != 2 || c.Chunk.OnlyCur != 2 {
		t.Errorf("chunk %+v, want 1 paired, 2 only in the baseline (gone, both-but-dropped), 2 only now", c.Chunk)
	}
	if c.Entry.Paired != 1 || c.Entry.OnlyBase != 0 || c.Entry.OnlyCur != 0 {
		t.Errorf("entry %+v, want the one entry-only query paired", c.Entry)
	}
	if c.Opens.Paired+c.Opens.OnlyBase+c.Opens.OnlyCur != 0 || c.Opens.Metrics == nil {
		t.Errorf("opens %+v, want empty with a non-nil metrics list", c.Opens)
	}
	if !c.SmallSample {
		t.Error("1 paired query < min_judged 30 is a small sample")
	}
	// Empty either side: all of the other side is unpaired.
	c, err = Compare(baselineOf("b", 8), baselineOf("n", 8, scored("x", 1), scored("y", 1)))
	if err != nil || c.Chunk.Paired != 0 || c.Chunk.OnlyCur != 2 || c.Chunk.OnlyBase != 0 {
		t.Errorf("empty baseline: %+v, %v", c.Chunk, err)
	}
	c, err = Compare(baselineOf("b", 8, scored("x", 1)), baselineOf("n", 8))
	if err != nil || c.Chunk.Paired != 0 || c.Chunk.OnlyBase != 1 || c.Chunk.OnlyCur != 0 {
		t.Errorf("empty current: %+v, %v", c.Chunk, err)
	}
}

func TestCompareSmallSampleThreshold(t *testing.T) {
	for _, tc := range []struct {
		min, paired int
		want        bool
	}{{30, 29, true}, {2, 2, false}, {0, 0, false}, {3, 2, true}} {
		var rs []Result
		for i := range tc.paired {
			rs = append(rs, scored(string(rune('a'+i)), 1))
		}
		b := baselineOf("n", 8, rs...)
		b.Report.MinJudged = tc.min
		c, err := Compare(baselineOf("b", 8, rs...), b)
		if err != nil || c.SmallSample != tc.want || c.MinJudged != tc.min {
			t.Errorf("min %d paired %d: small=%v (%v), want %v", tc.min, tc.paired, c.SmallSample, err, tc.want)
		}
	}
}

// TestCompareWarnings: each difference between the runs that makes a comparison weaker is a
// warning; a different k is refused.
func TestCompareWarnings(t *testing.T) {
	mk := func(f func(*Baseline)) *Baseline {
		b := baselineOf("n", 8, scored("a", 1))
		f(b)
		return b
	}
	for _, tc := range []struct {
		name string
		cur  *Baseline
		want string // "" = no warning at all
	}{
		{"embed model", mk(func(b *Baseline) { b.EmbedModel = "other" }), "embed model differs: baseline m, now other"},
		{"entry count", mk(func(b *Baseline) { b.EntryCount = 11 }), "entry count differs: baseline 10, now 11"},
		{"other-store gold", mk(func(b *Baseline) { b.Report.OtherStore.Chunks = 2 }), "gold from other stores changed"},
		{"fallback now", mk(func(b *Baseline) { b.Report.FallbackQueries = 1 }), "(baseline 0, now 1)"},
		{"same settings", mk(func(b *Baseline) { b.SettingsHash = "h-b" }), "same settings and mode"},
		{"different settings, same everything else", mk(func(*Baseline) {}), ""},
		{"different mode is the point, no warning", mk(func(b *Baseline) { b.SettingsHash = "h-b"; b.Report.Mode = "fts" }), ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			c, err := Compare(baselineOf("b", 8, scored("a", 1)), tc.cur)
			if err != nil {
				t.Fatal(err)
			}
			if tc.want == "" {
				if len(c.Warnings) != 0 {
					t.Errorf("warnings %q, want none", c.Warnings)
				}
				return
			}
			if len(c.Warnings) != 1 || !strings.Contains(c.Warnings[0], tc.want) {
				t.Errorf("warnings %q, want one with %q", c.Warnings, tc.want)
			}
		})
	}
	base := baselineOf("b", 8, scored("a", 1))
	base.Report.FallbackQueries = 1
	if c, _ := Compare(base, baselineOf("n", 8, scored("a", 1))); len(c.Warnings) != 1 || !strings.Contains(c.Warnings[0], "(baseline 1, now 0)") {
		t.Errorf("fallback in the baseline: %q", c.Warnings)
	}
	_, err := Compare(baselineOf("b", 8), baselineOf("n", 5))
	if errs.KindOf(err) != errs.Usage || !strings.Contains(err.Error(), "k=8") || !strings.Contains(err.Error(), "-k 8") {
		t.Errorf("different k: %v, want a Usage error naming both and the fix", err)
	}
}

// TestCompareJSONContract: the field names of --compare --json.
func TestCompareJSONContract(t *testing.T) {
	c, _ := Compare(baselineOf("b", 8, scored("a", 1)), baselineOf("n", 8, scored("a", 2)))
	data, err := json.Marshal(c)
	if err != nil {
		t.Fatal(err)
	}
	for _, key := range []string{`"baseline":{`, `"current":{`, `"chunk":{"paired":1,"only_in_baseline":0,"only_in_current":0,"metrics":[{"name":"hit_at_k"`,
		`"wins":`, `"losses":`, `"ties":`, `"p":`, `"ci_low":`, `"warnings":[`, `"small_sample":true`, `"min_judged":30`, `"settings_hash":"h-b"`} {
		if !strings.Contains(string(data), key) {
			t.Errorf("JSON lacks %s:\n%s", key, data)
		}
	}
}

func sampleBaseline(name string) Baseline {
	b := baselineOf(name, 8, scored("tmux logout", 2))
	b.KBVersion, b.CreatedAt = "v-test", time.Date(2026, 10, 30, 12, 0, 0, 0, time.UTC)
	b.Settings = json.RawMessage(`{"search":{"rrf_k":60}}`)
	b.Report.OtherStore = OtherStoreGold{Chunks: 1, Opens: 2, Queries: 3}
	return *b
}

// TestBaselineRoundTrip: what is saved is what loads, including the per-query ranks and the
// other-store counts; saving again replaces the file; no temp file stays.
func TestBaselineRoundTrip(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "eval") // not created yet
	b := sampleBaseline("v1.0")
	if err := SaveBaseline(dir, b); err != nil {
		t.Fatal(err)
	}
	got, err := LoadBaseline(dir, "v1.0")
	if err != nil {
		t.Fatal(err)
	}
	b.Version = BaselineVersion
	if got.Version != BaselineVersion || got.Name != "v1.0" || got.KBVersion != "v-test" || !got.CreatedAt.Equal(b.CreatedAt) ||
		got.EmbedModel != "m" || got.EntryCount != 10 || got.SettingsHash != "h-v1.0" || compact(got.Settings) != `{"search":{"rrf_k":60}}` ||
		got.Report.K != 8 || got.Report.OtherStore != b.Report.OtherStore ||
		len(got.Report.PerQuery) != 1 || got.Report.PerQuery[0].Chunk.FirstRank != 2 {
		t.Errorf("round trip:\n got %+v\nwant %+v", got, b)
	}
	// Comparing a loaded baseline with itself is all ties.
	c, err := Compare(got, got)
	if err != nil || c.Chunk.Paired != 1 || metric(t, c.Chunk, "mrr").Ties != 1 {
		t.Errorf("self compare after load: %+v, %v", c.Chunk, err)
	}
	b.EntryCount = 99
	if err := SaveBaseline(dir, b); err != nil {
		t.Fatal(err)
	}
	if got, _ := LoadBaseline(dir, "v1.0"); got.EntryCount != 99 {
		t.Errorf("second save kept entry_count %d, want 99", got.EntryCount)
	}
	ents, _ := os.ReadDir(dir)
	if len(ents) != 1 || ents[0].Name() != "v1.0.json" {
		t.Errorf("eval dir has %v, want only v1.0.json", ents)
	}
	data, _ := os.ReadFile(filepath.Join(dir, "v1.0.json"))
	if !strings.Contains(string(data), `"version": 1`) {
		t.Errorf("file lacks the version:\n%s", data)
	}
}

// TestLoadBaselineRefusals: every unusable file or name is a well-formed error with the fix.
func TestLoadBaselineRefusals(t *testing.T) {
	dir := t.TempDir()
	write := func(name, body string) { os.WriteFile(filepath.Join(dir, name+".json"), []byte(body), 0o644) }
	write("future", `{"version": 2, "name": "future"}`)
	write("old", `{"version": 0}`)
	write("noversion", `{"name": "x"}`)
	write("garbage", `not json`)
	write("damaged", `{"version": 1, "report": {"k": "eight"}}`)
	os.Mkdir(filepath.Join(dir, "isdir.json"), 0o755)
	for _, tc := range []struct {
		name string
		kind errs.Kind
		want string
	}{
		{"future", errs.Conflict, "is version 2; this kb reads version 1; rewrite it with kb eval --save-baseline future"},
		{"old", errs.Conflict, "is version 0"},
		{"noversion", errs.Conflict, "is version 0"},
		{"garbage", errs.Conflict, "is not a baseline"},
		{"damaged", errs.Conflict, "is damaged"},
		{"missing", errs.NotFound, `no baseline "missing"`},
		{"isdir", errs.Usage, "cannot read"},
		{"../escape", errs.Usage, "invalid baseline name"},
		{"", errs.Usage, "invalid baseline name"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			_, err := LoadBaseline(dir, tc.name)
			if err == nil || errs.KindOf(err) != tc.kind || !strings.Contains(err.Error(), tc.want) {
				t.Errorf("err %v (kind %v), want %v containing %q", err, errs.KindOf(err), tc.kind, tc.want)
			}
		})
	}
}

func TestValidBaselineName(t *testing.T) {
	for name, want := range map[string]bool{
		"default": true, "v1.2": true, "a": true, "0-day_x": true, strings.Repeat("a", 64): true,
		"": false, ".hidden": false, "-x": false, "Upper": false, "a/b": false, "..": false, "a b": false,
		strings.Repeat("a", 65): false, "x.json\n": false,
	} {
		if got := ValidBaselineName(name); got != want {
			t.Errorf("ValidBaselineName(%q) = %v, want %v", name, got, want)
		}
	}
}

// TestSaveBaselineErrors: an invalid name and an unwritable directory are Usage errors.
func TestSaveBaselineErrors(t *testing.T) {
	if err := SaveBaseline(t.TempDir(), Baseline{Name: "Bad/Name"}); errs.KindOf(err) != errs.Usage || err == nil {
		t.Errorf("bad name: %v", err)
	}
	file := filepath.Join(t.TempDir(), "afile")
	os.WriteFile(file, nil, 0o644)
	err := SaveBaseline(filepath.Join(file, "eval"), Baseline{Name: "x"}) // parent is a file
	var e *errs.Error
	if !errors.As(err, &e) || e.Kind != errs.Usage || !strings.Contains(err.Error(), "cannot create") {
		t.Errorf("mkdir under a file: %v", err)
	}
	dir := t.TempDir()
	os.Chmod(dir, 0o555)
	defer os.Chmod(dir, 0o755)
	if os.Geteuid() != 0 {
		if err := SaveBaseline(dir, Baseline{Name: "x"}); errs.KindOf(err) != errs.Usage || err == nil || !strings.Contains(err.Error(), "cannot write") {
			t.Errorf("read-only dir: %v", err)
		}
	}
	// A directory where the file should go: the rename fails and the temp file is removed.
	dir = t.TempDir()
	os.Mkdir(filepath.Join(dir, "x.json"), 0o755)
	os.WriteFile(filepath.Join(dir, "x.json", "keep"), nil, 0o644) // non-empty: rename over it fails
	if err := SaveBaseline(dir, Baseline{Name: "x"}); err == nil || !strings.Contains(err.Error(), "cannot write") {
		t.Errorf("rename onto a directory: %v", err)
	}
	if ents, _ := os.ReadDir(dir); len(ents) != 1 {
		t.Errorf("a temp file stayed: %v", ents)
	}
}

func compact(raw json.RawMessage) string {
	var buf bytes.Buffer
	if err := json.Compact(&buf, raw); err != nil {
		return err.Error()
	}
	return buf.String()
}

// TestCompareSides: the baseline side carries its name, version and date; the current run has no
// name; a zero date prints nothing.
func TestCompareSides(t *testing.T) {
	b := sampleBaseline("base")
	cur := baselineOf("now", 8, scored("a", 1))
	cur.KBVersion, cur.CreatedAt = "v-now", b.CreatedAt
	c, err := Compare(&b, cur)
	if err != nil {
		t.Fatal(err)
	}
	if want := (Side{Name: "base", CreatedAt: "2026-10-30", KBVersion: "v-test", SettingsHash: "h-base", Mode: "hybrid", K: 8, EmbedModel: "m", EntryCount: 10}); c.Baseline != want {
		t.Errorf("baseline side %+v, want %+v", c.Baseline, want)
	}
	if want := (Side{SettingsHash: "h-now", Mode: "hybrid", K: 8, EmbedModel: "m", EntryCount: 10}); c.Current != want {
		t.Errorf("current side %+v, want %+v", c.Current, want)
	}
	b.CreatedAt = time.Time{}
	if c, _ := Compare(&b, cur); c.Baseline.CreatedAt != "" {
		t.Errorf("zero date printed %q", c.Baseline.CreatedAt)
	}
}

// TestCompareBootstrapSettings: the interval uses the run's resamples and seed (defaults when
// zero), so the same two runs always give the same interval.
func TestCompareBootstrapSettings(t *testing.T) {
	base := baselineOf("b", 8, scored("a", 3), scored("b", 2), scored("c", 0), scored("d", 1))
	cur := baselineOf("n", 8, scored("a", 1), scored("b", 2), scored("c", 4), scored("d", 5))
	diffs := []float64{1 - 1.0/3, 0, 0.25, 0.2 - 1}
	for _, tc := range []struct {
		resamples int
		seed      uint64
		wantRes   int
		wantSeed  uint64
	}{{0, 0, DefaultResamples, DefaultSeed}, {50, 7, 50, 7}, {50, 0, 50, DefaultSeed}, {0, 9, DefaultResamples, 9}} {
		cur.Report.Resamples, cur.Report.Seed = tc.resamples, tc.seed
		c, err := Compare(base, cur)
		if err != nil {
			t.Fatal(err)
		}
		want := Bootstrap(diffs, tc.wantRes, Alpha, tc.wantSeed)
		if m := metric(t, c.Chunk, "mrr"); m.Lo != want.Lo || m.Hi != want.Hi || math.Abs(m.Delta-want.Mean) > 1e-12 {
			t.Errorf("resamples %d seed %d: [%g, %g] delta %g, want [%g, %g] delta %g", tc.resamples, tc.seed, m.Lo, m.Hi, m.Delta, want.Lo, want.Hi, want.Mean)
		}
	}
}

// TestSaveBaselineFileMode: the file is 0644 whatever the umask, and an unencodable settings
// field is an Internal error that leaves nothing behind.
func TestSaveBaselineFileMode(t *testing.T) {
	dir := t.TempDir()
	old := syscall.Umask(0o077)
	defer syscall.Umask(old)
	if err := SaveBaseline(dir, sampleBaseline("m")); err != nil {
		t.Fatal(err)
	}
	if fi, err := os.Stat(filepath.Join(dir, "m.json")); err != nil || fi.Mode().Perm() != 0o644 {
		t.Errorf("baseline file: %v %v, want mode 0644", fi, err)
	}
	b := sampleBaseline("bad")
	b.Settings = json.RawMessage(`{not json`)
	if err := SaveBaseline(dir, b); errs.KindOf(err) != errs.Internal || err == nil {
		t.Errorf("unencodable settings: %v", err)
	}
	if ents, _ := os.ReadDir(dir); len(ents) != 1 {
		t.Errorf("dir has %v, want m.json only", ents)
	}
}

// TestMetricDiffTieBoundary: a per-query difference of exactly ±tieEps is a tie; just past it is a
// win or a loss (the §2.1 boundary of the tie rule).
func TestMetricDiffTieBoundary(t *testing.T) {
	for _, tc := range []struct {
		name         string
		diff         float64
		wins, losses int
	}{
		{"+tieEps ties", tieEps, 0, 0},
		{"-tieEps ties", -tieEps, 0, 0},
		{"above +tieEps wins", 2 * tieEps, 1, 0},
		{"below -tieEps loses", -2 * tieEps, 0, 1},
	} {
		d := metricDiff("m", []float64{0}, []float64{tc.diff}, 10, DefaultSeed)
		if d.Wins != tc.wins || d.Losses != tc.losses || d.Ties != 1-tc.wins-tc.losses {
			t.Errorf("%s: w/l/t = %d/%d/%d, want %d/%d/%d", tc.name, d.Wins, d.Losses, d.Ties, tc.wins, tc.losses, 1-tc.wins-tc.losses)
		}
	}
}
