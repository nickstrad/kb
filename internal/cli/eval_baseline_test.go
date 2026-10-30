package cli

import (
	"bytes"
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"

	"github.com/nickstrad/kb/internal/errs"
	"github.com/nickstrad/kb/internal/eval"
	"github.com/nickstrad/kb/internal/store"
)

// judgedRepo is an explain repo with one useful verdict, so there is one query to score.
func judgedRepo(t *testing.T) string {
	t.Helper()
	root := useRoot(t, newExplainRepo(t))
	id := searchID(t, "tmux logout")
	mustKB(t, "feedback", id, "1", "--useful")
	return root
}

func baselineFile(root, name string) string {
	return filepath.Join(testWS(root).StoreDir, "eval", name+".json")
}

// TestEvalBaselineFlow: save, compare with itself (all ties), compare a --set variant, a named
// baseline, --json; the file holds the settings, the hash, the model and the per-query ranks, and
// the log is untouched.
func TestEvalBaselineFlow(t *testing.T) {
	root := judgedRepo(t)
	before := feedbackRows(t, root)

	stdout, stderr, exit := runKB(t, []string{"eval", "--mode", "fts", "--save-baseline"})
	if exit != 0 || stderr != "" || !strings.Contains(stdout, `saved baseline "default" (1 queries, settings `) {
		t.Fatalf("save: exit %d stderr %q\n%s", exit, stderr, stdout)
	}
	var b eval.Baseline
	data, err := os.ReadFile(baselineFile(root, "default"))
	if err != nil {
		t.Fatal(err)
	}
	if err := json.Unmarshal(data, &b); err != nil {
		t.Fatal(err)
	}
	if b.Version != eval.BaselineVersion || b.Name != "default" || b.KBVersion != Version || b.EmbedModel == "" ||
		b.EntryCount < 1 || b.SettingsHash == "" || !strings.Contains(string(b.Settings), `"rrf_k"`) ||
		b.Report.Mode != "fts" || b.Report.K != 8 || len(b.Report.PerQuery) != 1 || b.Report.PerQuery[0].Chunk.FirstRank != 1 ||
		b.CreatedAt.IsZero() {
		t.Errorf("baseline %+v", b)
	}

	// Itself: every query ties.
	stdout, stderr, exit = runKB(t, []string{"eval", "--mode", "fts", "--compare"})
	if exit != 0 || !strings.HasPrefix(stderr, "warning: same settings and mode") || strings.Contains(stdout, "warning") {
		t.Fatalf("compare: exit %d stderr %q: want the same-settings warning on stderr only\n%s", exit, stderr, stdout)
	}
	for _, want := range []string{`compare · baseline "default"`, "chunk: 1 paired queries (0 only in the baseline, 0 only now)",
		"hit@8", "mrr", "ndcg@8", "0/0/1", "sign-test p", "small sample: 1 paired queries < eval.min_judged 30"} {
		if !strings.Contains(stdout, want) {
			t.Errorf("compare output lacks %q:\n%s", want, stdout)
		}
	}
	if strings.Contains(stdout, "entry:") && !strings.Contains(stdout, "entry: 1 paired") {
		t.Errorf("entry table malformed:\n%s", stdout)
	}

	// A variant: a different hash, no same-settings warning, the baseline file untouched.
	stdout, stderr, exit = runKB(t, []string{"eval", "--mode", "fts", "--set", "search.rrf_k=20", "--compare"})
	if exit != 0 || strings.Contains(stderr, "same settings") {
		t.Errorf("variant compare: exit %d\n%s", exit, stdout)
	}
	if after, _ := os.ReadFile(baselineFile(root, "default")); string(after) != string(data) {
		t.Error("--compare rewrote the baseline")
	}

	// --json: comparison and the report; a named baseline; the saved variant carries its own hash.
	if _, stderr, exit := runKB(t, []string{"eval", "--mode", "fts", "--set", "search.rrf_k=20", "--save-baseline", "rrf20", "--json"}); exit != 0 || stderr != "" {
		t.Fatalf("save named: %d %s", exit, stderr)
	}
	stdout, _, exit = runKB(t, []string{"eval", "--mode", "fts", "--compare", "rrf20", "--json"})
	var doc struct {
		Comparison eval.Comparison `json:"comparison"`
		Report     eval.Report     `json:"report"`
	}
	if err := json.Unmarshal([]byte(stdout), &doc); err != nil || exit != 0 {
		t.Fatalf("compare --json: exit %d %v\n%s", exit, err, stdout)
	}
	if doc.Comparison.Baseline.Name != "rrf20" || doc.Comparison.Baseline.SettingsHash == doc.Comparison.Current.SettingsHash ||
		doc.Comparison.Chunk.Paired != 1 || len(doc.Comparison.Chunk.Metrics) != 3 || doc.Report.Chunk.Queries != 1 {
		t.Errorf("comparison %+v", doc.Comparison)
	}

	if after := feedbackRows(t, root); after != before {
		t.Errorf("eval changed the feedback rows: %d -> %d", before, after)
	}
	if ents, _ := os.ReadDir(filepath.Dir(baselineFile(root, "x"))); len(ents) != 2 {
		t.Errorf("eval dir %v, want default.json and rrf20.json only", ents)
	}
}

// TestEvalBaselineFailures: each bad use is one error, exit 1, with nothing written.
func TestEvalBaselineFailures(t *testing.T) {
	root := judgedRepo(t)
	write := func(name, body string) {
		t.Helper()
		if err := os.MkdirAll(filepath.Dir(baselineFile(root, name)), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(baselineFile(root, name), []byte(body), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	mustKB(t, "eval", "--mode", "fts", "--save-baseline", "k8")
	write("future", `{"version": 2}`)
	for _, tc := range []struct {
		name string
		args []string
		want string
	}{
		{"no baseline to compare", []string{"--compare"}, `no baseline "default"`},
		{"version refused", []string{"--compare", "future"}, "is version 2; this kb reads version 1"},
		{"different k", []string{"-k", "5", "--compare", "k8"}, "ran at k=8 and this run at k=5"},
		{"both flags", []string{"--save-baseline", "--compare"}, "exclusive"},
		{"bad name on save", []string{"--save-baseline", "Bad/Name"}, `invalid baseline name "Bad/Name"`},
		{"bad name on compare", []string{"--compare", "../x"}, "invalid baseline name"},
		{"bad --set key", []string{"--set", "search.nope=1", "--compare", "k8"}, "search.nope"},
		{"bad --set value", []string{"--set", "search.rrf_k=zero", "--save-baseline", "v"}, "search.rrf_k"},
		{"--set without =", []string{"--set", "search.rrf_k", "--compare", "k8"}, "search.rrf_k"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			stdout, stderr, exit := runKB(t, append([]string{"eval", "--mode", "fts"}, tc.args...))
			if exit != ExitUsage || stdout != "" || !strings.Contains(stderr, tc.want) {
				t.Errorf("exit %d stdout %q stderr %q; want exit 1, no output, %q", exit, stdout, stderr, tc.want)
			}
		})
	}
	if _, err := os.Stat(baselineFile(root, "v")); err == nil {
		t.Error("a failed save left a baseline")
	}
}

// TestEvalFallbackDoesNotSaveBaseline: a run whose replays fell back to fts exits 2, prints its
// report, and writes no baseline; --compare still prints.
func TestEvalFallbackDoesNotSaveBaseline(t *testing.T) {
	root := newSearchTestRepo(t, testModel768, testDim768)
	useRoot(t, root)
	countingEmbedders(t, false)
	id := searchID(t, "widget")
	mustKB(t, "feedback", id, "1", "--useful")
	mustKB(t, "eval", "--save-baseline", "ok")
	countingEmbedders(t, true)
	stdout, stderr, exit := runKB(t, []string{"eval", "--save-baseline", "fb"})
	if exit != ExitEmbedder || !strings.Contains(stdout, "queries=1") || !strings.Contains(stderr, `baseline "fb" not saved`) {
		t.Errorf("exit %d\nstdout %q\nstderr %q", exit, stdout, stderr)
	}
	if _, err := os.Stat(baselineFile(root, "fb")); err == nil {
		t.Error("a fallback run saved a baseline")
	}
	stdout, stderr, exit = runKB(t, []string{"eval", "--compare", "ok"})
	if exit != ExitEmbedder || !strings.Contains(stdout, "compare ·") || !strings.Contains(stderr, "warning: some replays ran --mode fts instead") {
		t.Errorf("compare under fallback: exit %d\n%s\n%s", exit, stdout, stderr)
	}
}

// TestRenderCompare: the table rows, the empty case and the optional lines.
func TestRenderCompare(t *testing.T) {
	m := func(name string, w, l, ties int, p float64) eval.MetricDiff {
		return eval.MetricDiff{Name: name, Baseline: 0.5, Current: 0.75, Delta: 0.25, Lo: 0.05, Hi: 0.45, Wins: w, Losses: l, Ties: ties, P: p}
	}
	c := &eval.Comparison{
		Baseline: eval.Side{Name: "base", CreatedAt: "2026-10-30", KBVersion: "v1", SettingsHash: "aaaaaaaaaaaaaaaa", Mode: "hybrid", K: 8, EmbedModel: "m", EntryCount: 10},
		Current:  eval.Side{SettingsHash: "bbbbbbbbbbbbbbbb", Mode: "fts", K: 5, EmbedModel: "m2", EntryCount: 11},
		Chunk: eval.VariantDiff{Paired: 40, OnlyBase: 1, OnlyCur: 2, Metrics: []eval.MetricDiff{
			m("hit_at_k", 5, 1, 34, 0.21875), m("mrr", 8, 2, 30, 0.109375), m("ndcg_at_k", 8, 2, 30, 0.109375)}},
		Opens:       eval.VariantDiff{Paired: 0, OnlyBase: 1, OnlyCur: 1, Metrics: []eval.MetricDiff{}},
		Warnings:    []string{"embed model differs: baseline m, now m2"},
		SmallSample: true, MinJudged: 50,
	}
	got := renderCompare(c, &eval.Report{K: 5}, false)
	for _, want := range []string{
		`compare · baseline "base" (2026-10-30 · kb v1 · mode hybrid · settings aaaaaaaaaaaa · m · 10 entries)`,
		"now (mode fts · settings bbbbbbbbbbbb · m2 · 11 entries) · k 5",
		"chunk: 40 paired queries (1 only in the baseline, 2 only now)",
		"hit@5        0.500    0.750   +0.250  [+0.050, +0.450]  5/1/34    0.219",
		"mrr          0.500    0.750", "ndcg@5", "8/2/30    0.109",
		"opens: 0 paired queries (1 only in the baseline, 1 only now)",
		"small sample: 40 paired queries < eval.min_judged 50",
	} {
		if !strings.Contains(got, want) {
			t.Errorf("lacks %q:\n%s", want, got)
		}
	}
	if strings.Contains(got, "entry:") || strings.Contains(got, "per query") || strings.Contains(got, "no scored queries") || strings.Contains(got, "warning") {
		t.Errorf("unexpected sections:\n%s", got)
	}
	// Only a variant with paired queries gets a metrics table (opens has none here).
	if n := strings.Count(got, "95% CI of delta"); n != 1 {
		t.Errorf("%d metric tables, want 1 (chunk only):\n%s", n, got)
	}
	empty := renderCompare(&eval.Comparison{Chunk: eval.VariantDiff{Metrics: []eval.MetricDiff{}}}, &eval.Report{K: 8}, true)
	if !strings.Contains(empty, "chunk: no scored queries on either side") || !strings.Contains(empty, "per query:") || strings.Contains(empty, "small sample") {
		t.Errorf("empty comparison:\n%s", empty)
	}
}

// faultStore wraps a store with scripted failures for the paths a real store cannot reach.
type faultStore struct {
	store.Store
	judged, entry, count error
}

func (f faultStore) Judged(ctx context.Context, flt store.JudgedFilter) ([]store.JudgedSearch, error) {
	if f.judged != nil {
		return nil, f.judged
	}
	return f.Store.(store.JudgedLog).Judged(ctx, flt)
}

func (f faultStore) Entry(ctx context.Context, path string) (*store.EntryRow, error) {
	if f.entry != nil {
		return nil, f.entry
	}
	return f.Store.Entry(ctx, path)
}

func (f faultStore) EntryCount(ctx context.Context) (int, error) {
	if f.count != nil {
		return 0, f.count
	}
	return f.Store.(entryCounter).EntryCount(ctx)
}

// plainStore hides everything but store.Store, so it is neither a JudgedLog nor an entryCounter.
type plainStore struct{ store.Store }

// TestEvalStoreFaults (Q86): errors from the store reach the user with their kind, and a store
// without the optional interfaces is handled.
func TestEvalStoreFaults(t *testing.T) {
	judgedRepo(t)
	boom := errs.New(errs.Conflict, "test", "disk exploded")
	orig := testDeps.openStore
	t.Cleanup(func() { testDeps.openStore = orig })
	wrap := func(f func(store.Store) store.Store) {
		testDeps.openStore = func(ctx context.Context, path string, ro bool) (store.Store, error) {
			st, err := orig(ctx, path, ro)
			if err != nil {
				return nil, err
			}
			return f(st), nil
		}
	}
	for _, tc := range []struct {
		name string
		wrap func(store.Store) store.Store
		code int
		want string
	}{
		{"judged read fails", func(s store.Store) store.Store { return faultStore{Store: s, judged: boom} }, ExitUsage, "disk exploded"},
		{"gold check fails", func(s store.Store) store.Store { return faultStore{Store: s, entry: boom} }, ExitUsage, "disk exploded"},
		{"entry count fails", func(s store.Store) store.Store { return faultStore{Store: s, count: boom} }, ExitUsage, "disk exploded"},
		{"no judged log", func(s store.Store) store.Store { return plainStore{s} }, ExitInternal, "internal error"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			wrap(tc.wrap)
			stdout, stderr, exit := runKB(t, []string{"eval", "--mode", "fts"})
			if exit != tc.code || stdout != "" || !strings.Contains(stderr, tc.want) {
				t.Errorf("exit %d stdout %q stderr %q; want %d, no report, %q", exit, stdout, stderr, tc.code, tc.want)
			}
		})
	}
}

// TestEvalEnvironmentFailures (Q86): failures around the index and the baseline directory are
// the command's errors, never a report or a half-written baseline.
func TestEvalEnvironmentFailures(t *testing.T) {
	root := judgedRepo(t)
	if _, stderr, exit := runKB(t, []string{"eval", "--set", "embed.model=no-such-model", "--compare"}); exit != ExitUsage || !strings.Contains(stderr, "no-such-model") {
		t.Errorf("--set to an unknown model: exit %d %q", exit, stderr)
	}

	// eval/ is a file: the save fails after the report was printed.
	dir := filepath.Dir(baselineFile(root, "x"))
	if err := os.WriteFile(dir, []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
	stdout, stderr, exit := runKB(t, []string{"eval", "--mode", "fts", "--save-baseline"})
	if exit != ExitUsage || !strings.Contains(stderr, "cannot create") || !strings.Contains(stdout, "queries=1") || strings.Contains(stdout, "saved baseline") {
		t.Errorf("save into a blocked eval dir: exit %d\n%s\n%s", exit, stdout, stderr)
	}
	os.Remove(dir)

	orig := testDeps.openStore
	t.Cleanup(func() { testDeps.openStore = orig })
	testDeps.openStore = func(context.Context, string, bool) (store.Store, error) {
		return nil, errs.New(errs.Conflict, "test", "database damaged")
	}
	if stdout, stderr, exit := runKB(t, []string{"eval"}); exit != ExitUsage || stdout != "" || !strings.Contains(stderr, "database damaged") {
		t.Errorf("open failure: exit %d %q %q", exit, stdout, stderr)
	}
}

// TestEvalCompareExactListOff is the natural 4.5 × 2.4 use: save a baseline with the exact list on,
// then `kb eval --set search.exact_list=false --compare` replays the identifier query without it.
// The baseline's settings carry exact_list=true, the variant has another hash, the gold that the
// exact list put at rank 1 drops, and the comparison counts it as a loss.
func TestEvalCompareExactListOff(t *testing.T) {
	root := useRoot(t, newExplainRepoWith(t, identEntries))
	stdout, _, exit := runKB(t, []string{"search", "--mode", "fts", "--json", "reindex_test"})
	var res struct {
		SearchID int64 `json:"search_id"`
		Hits     []struct {
			Rank      int
			Path      string
			ExactRank *int `json:"exact_rank"`
		}
	}
	if err := json.Unmarshal([]byte(stdout), &res); err != nil || exit != 0 {
		t.Fatalf("search: exit %d %v\n%s", exit, err, stdout)
	}
	if len(res.Hits) == 0 || res.Hits[0].Path != "store.md" || res.Hits[0].ExactRank == nil {
		t.Fatalf("hits %+v: want store.md first, from the exact list", res.Hits)
	}
	mustKB(t, "feedback", strconv.FormatInt(res.SearchID, 10), "1", "--useful")
	mustKB(t, "eval", "--mode", "fts", "--save-baseline", "exact-on")
	data, err := os.ReadFile(baselineFile(root, "exact-on"))
	if err != nil || !strings.Contains(compactJSON(t, data), `"exact_list":true`) {
		t.Fatalf("baseline settings lack exact_list=true (%v):\n%s", err, data)
	}

	stdout, stderr, exit := runKB(t, []string{"eval", "--mode", "fts", "--set", "search.exact_list=false", "--compare", "exact-on", "--json"})
	var doc struct {
		Comparison eval.Comparison `json:"comparison"`
		Report     eval.Report     `json:"report"`
	}
	if err := json.Unmarshal([]byte(stdout), &doc); err != nil || exit != 0 || stderr != "" {
		t.Fatalf("compare: exit %d %v stderr %q\n%s", exit, err, stderr, stdout)
	}
	c := doc.Comparison
	if c.Baseline.SettingsHash == c.Current.SettingsHash || c.Chunk.Paired != 1 {
		t.Fatalf("comparison sides %+v / %+v, paired %d: want different hashes, 1 pair", c.Baseline, c.Current, c.Chunk.Paired)
	}
	for _, w := range c.Warnings {
		if strings.Contains(w, "same settings") {
			t.Errorf("warning %q on a variant", w)
		}
	}
	mrr := metricOf(t, c.Chunk, "mrr")
	if mrr.Baseline != 1 || mrr.Current >= 1 || mrr.Losses != 1 || mrr.Wins != 0 {
		t.Errorf("mrr %+v: want baseline 1, current < 1, one loss (the exact list put the gold first)", mrr)
	}
	if got := doc.Report.PerQuery[0].Chunk.FirstRank; got <= 1 {
		t.Errorf("first gold rank without the exact list = %d, want > 1", got)
	}
}

func metricOf(t *testing.T, vd eval.VariantDiff, name string) eval.MetricDiff {
	t.Helper()
	for _, m := range vd.Metrics {
		if m.Name == name {
			return m
		}
	}
	t.Fatalf("no metric %s in %+v", name, vd)
	return eval.MetricDiff{}
}

func compactJSON(t *testing.T, data []byte) string {
	t.Helper()
	var buf bytes.Buffer
	if err := json.Compact(&buf, data); err != nil {
		t.Fatal(err)
	}
	return buf.String()
}
