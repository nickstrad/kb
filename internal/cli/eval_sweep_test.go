package cli

import (
	"encoding/json"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/nickstrad/kb/internal/config"
	"github.com/nickstrad/kb/internal/embed"
	"github.com/nickstrad/kb/internal/embed/fake"
	"github.com/nickstrad/kb/internal/eval"
)

// withCountingEmbedder makes the CLI embed with the fixture's fake model, counting the builds
// and the Embed calls.
func withCountingEmbedder(t *testing.T) (builds *int, calls *atomic.Int64) {
	t.Helper()
	builds, calls = new(int), new(atomic.Int64)
	orig := testDeps.embedder
	t.Cleanup(func() { testDeps.embedder = orig })
	testDeps.embedder = func(config.Embed) (embed.Embedder, error) {
		*builds++
		return countingEmbedder{fake.New(testModel768, testDim768), calls, new(atomic.Int64)}, nil
	}
	return builds, calls
}

// TestEvalSweepWiring: --sweep, --force, --mode, -k and --json reach the sweep; the grid is
// replayed in hybrid mode with each query embedded once for the whole sweep (vector reuse); the
// human form has its header and table; the log is never written.
func TestEvalSweepWiring(t *testing.T) {
	root := useRoot(t, newExplainRepo(t))
	queries := []string{"tmux logout", "docker volumes", "git rebase", "build cache layers"}
	for _, q := range queries {
		mustKB(t, "feedback", searchID(t, q), "1", "--useful")
	}
	before := feedbackRows(t, root, "")
	builds, calls := withCountingEmbedder(t)

	stdout, stderr, exit := runKB(t, []string{"eval", "--sweep", "search.rrf_k=10,60", "--sweep", "search.bm25.title=1,5", "--force", "-k", "5", "--json"})
	if exit != 0 {
		t.Fatalf("sweep: exit %d stderr %q", exit, stderr)
	}
	var rep eval.SweepReport
	if err := json.Unmarshal([]byte(stdout), &rep); err != nil {
		t.Fatalf("--json: %v\n%s", err, stdout)
	}
	// The current config (rrf_k 60, title 3) is not in the grid: 5 distinct settings replayed.
	if rep.Mode != "hybrid" || rep.K != 5 || rep.Settings != 4 || rep.Replays != 5 || rep.Judged != len(queries) ||
		len(rep.Train)+len(rep.Test) != len(queries) || len(rep.Test) != 1 || rep.Holdout != 0.3 || rep.MinJudged != 30 {
		t.Errorf("report %+v", rep)
	}
	if *builds != 1 || calls.Load() != int64(len(queries)) {
		t.Errorf("%d embedder builds, %d Embed calls for %d replays of %d queries; want 1 build and one call per query",
			*builds, calls.Load(), rep.Replays, len(queries))
	}
	if !strings.Contains(stderr, "warning: forced below eval.min_judged: 4 judged queries < 30") || strings.Contains(stdout, "warning: ") || len(rep.Warnings) != 1 {
		t.Errorf("the forced warning belongs on stderr only: %q", stderr)
	}

	stdout, _, exit = runKB(t, []string{"eval", "--sweep", "search.rrf_k=10", "--force", "--mode", "fts"})
	if exit != 0 || !strings.HasPrefix(stdout, "sweep · mode fts · k 8 · 1 settings (2 replays) · 4 judged queries: 3 to choose, 1 held out") ||
		!strings.Contains(stdout, "held-out Δ vs current") {
		t.Errorf("human sweep: exit %d\n%s", exit, stdout)
	}
	if after := feedbackRows(t, root, ""); after != before {
		t.Errorf("sweep changed the feedback rows: %d -> %d", before, after)
	}
	if next := searchID(t, "tmux logout"); next != "5" {
		t.Errorf("sweep logged its replays: the next search id is %s, want 5", next)
	}
}

// TestEvalSweepGate: below [eval] min_judged the sweep is refused before the model is loaded;
// lowering the threshold in the config lets it run.
func TestEvalSweepGate(t *testing.T) {
	root := useRoot(t, newExplainRepo(t))
	mustKB(t, "feedback", searchID(t, "tmux logout"), "1", "--useful")
	builds, _ := withCountingEmbedder(t)
	stdout, stderr, exit := runKB(t, []string{"eval", "--sweep", "search.rrf_k=10,60"})
	if exit != ExitUsage || stdout != "" || !strings.Contains(stderr, "need ≥30 judged queries (have 1); use --force") || *builds != 0 {
		t.Errorf("exit %d stdout %q stderr %q, %d builds; want 1, the gate message, no model load", exit, stdout, stderr, *builds)
	}
	writeConfig(t, root, "[embed]\nmodel = \""+testModel768+"\"\n[eval]\nmin_judged = 1\nholdout = 0\n")
	if _, stderr, exit := runKB(t, []string{"eval", "--sweep", "search.rrf_k=10,60"}); exit != 0 || !strings.Contains(stderr, "nothing is held out (eval.holdout 0") {
		t.Errorf("min_judged 1, holdout 0: exit %d stderr %q", exit, stderr)
	}
}

// TestEvalSweepFlagErrors: flags --sweep does not combine with, and bad sweeps, are usage errors
// before any replay.
func TestEvalSweepFlagErrors(t *testing.T) {
	useRoot(t, newExplainRepo(t))
	for _, tc := range []struct {
		name string
		args []string
		want string
	}{
		{"with --compare", []string{"--sweep", "search.rrf_k=1", "--compare"}, "does not take --save-baseline or --compare"},
		{"with --save-baseline", []string{"--sweep", "search.rrf_k=1", "--save-baseline"}, "does not take --save-baseline or --compare"},
		{"with --per-query", []string{"--sweep", "search.rrf_k=1", "--per-query"}, "--per-query does not apply to --sweep"},
		{"--force alone", []string{"--force"}, "--force applies only to --sweep"},
		{"a NAME", []string{"x", "--sweep", "search.rrf_k=1"}, "a baseline NAME needs --save-baseline or --compare"},
		{"search.k", []string{"--sweep", "search.k=4,8"}, "cannot sweep search.k"},
		{"unknown key", []string{"--sweep", "search.nope=1"}, `--sweep search.nope=1: unknown key`},
		{"--set and --sweep one key", []string{"--set", "search.rrf_k=5", "--sweep", "search.rrf_k=1,2"}, "search.rrf_k is both --set and --sweep"},
		{"bad mode", []string{"--sweep", "search.rrf_k=1", "--mode", "x"}, `invalid --mode "x"`},
	} {
		t.Run(tc.name, func(t *testing.T) {
			_, stderr, exit := runKB(t, append([]string{"eval"}, tc.args...))
			if exit != ExitUsage || !strings.Contains(stderr, tc.want) {
				t.Errorf("exit %d stderr %q, want %d and %q", exit, stderr, ExitUsage, tc.want)
			}
		})
	}
}

// TestEvalSweepFallbackExits2: with the model unavailable the sweep's replays fall back to fts;
// the report is printed, the fallback is a warning and the exit is 2.
func TestEvalSweepFallbackExits2(t *testing.T) {
	root := newSearchTestRepo(t, testModel768, testDim768)
	useRoot(t, root)
	writeConfig(t, root, "[embed]\nmodel = \""+testModel768+"\"\n[eval]\nmin_judged = 1\n")
	countingEmbedders(t, false)
	mustKB(t, "feedback", searchID(t, "widget"), "1", "--useful")
	countingEmbedders(t, true)
	stdout, stderr, exit := runKB(t, []string{"eval", "--sweep", "search.rrf_k=10"})
	if exit != ExitEmbedder || !strings.HasPrefix(stdout, "sweep ·") || !strings.Contains(stderr, "of the replays ran --mode fts instead") {
		t.Errorf("exit %d\nstdout %q\nstderr %q", exit, stdout, stderr)
	}
}

// TestRenderSweep: the three endings (adopt the best, the current is best, overfitting), the
// held-out columns and a report with nothing held out.
func TestRenderSweep(t *testing.T) {
	est := func(m float64) eval.Estimate { return eval.Estimate{Mean: m, Lo: m - 0.1, Hi: m + 0.1} }
	split := func(m float64) *eval.SplitScore { return &eval.SplitScore{Queries: 3, MRR: est(m)} }
	diff := &eval.VariantDiff{Paired: 3, Metrics: []eval.MetricDiff{{Name: "mrr", Delta: 0.25, Lo: -0.05, Hi: 0.5, Wins: 2, Ties: 1, P: 0.5}}}
	base := func() eval.SweepReport {
		return eval.SweepReport{Mode: "hybrid", K: 8, Settings: 2, Replays: 3, Judged: 40, Holdout: 0.3, Seed: 1,
			Train: make([]string, 28), Test: make([]string, 12),
			Current: eval.SweepRow{SettingsHash: "abcdef0123456789", Current: true, Train: *split(0.5), Test: split(0.4)},
			Top: []eval.SweepRow{
				{Rank: 1, Set: []string{"search.rrf_k=10", "search.bm25.title=5"}, Train: *split(0.7), Test: split(0.65), TestVsCurrent: diff},
				{Rank: 2, Set: []string{"search.rrf_k=60", "search.bm25.title=3"}, Current: true, Train: *split(0.5), Test: split(0.4), TestVsCurrent: diff},
			},
			TOML: "[search]\nrrf_k = 10\n"}
	}
	adopt, isCurrent, overfit, noTest := base(), base(), base(), base()
	isCurrent.BestIsCurrent, isCurrent.TOML = true, ""
	overfit.Overfit = true
	noTest.Test, noTest.Current.Test = nil, nil
	for i := range noTest.Top {
		noTest.Top[i].Test, noTest.Top[i].TestVsCurrent = nil, nil
	}
	for _, tc := range []struct {
		name        string
		rep         eval.SweepReport
		want, never []string
	}{
		{"adopt", adopt, []string{
			"sweep · mode hybrid · k 8 · 2 settings (3 replays) · 40 judged queries: 28 to choose, 12 held out (eval.holdout 0.3, seed 1)",
			"current settings abcdef012345: mrr 0.500 [0.400, 0.600] chosen · 0.400 [0.300, 0.500] held out",
			"1    0.700 [0.600, 0.800]   0.650 [0.550, 0.750]   +0.250 [-0.050, +0.500]  2/0/1     0.500",
			"     search.rrf_k=10 search.bm25.title=5\n",
			"     search.rrf_k=60 search.bm25.title=3  (= current)",
			"best on the chosen queries: search.rrf_k=10 search.bm25.title=5\nto adopt it, add to /s/config.toml:\n[search]\nrrf_k = 10\n",
			"kb eval never writes the config"}, nil},
		{"current is best", isCurrent, []string{"best on the chosen queries: the current settings; nothing to change"}, []string{"to adopt it"}},
		{"overfit", overfit, []string{"does not win on the held-out ones: keep the current settings"}, []string{"to adopt it"}},
		{"nothing held out", noTest, []string{"chosen · - held out", "1    0.700 [0.600, 0.800]   -                      -                        -         -"}, nil},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got := renderSweep(&tc.rep, "/s/config.toml")
			for _, w := range tc.want {
				if !strings.Contains(got, w) {
					t.Errorf("lacks %q:\n%s", w, got)
				}
			}
			for _, n := range tc.never {
				if strings.Contains(got, n) {
					t.Errorf("has %q:\n%s", n, got)
				}
			}
		})
	}
}

// TestEvalSweepNoIndex: with no index the sweep is NotFound, after the grid is checked.
func TestEvalSweepNoIndex(t *testing.T) {
	useRoot(t, t.TempDir())
	if _, stderr, exit := runKB(t, []string{"eval", "--sweep", "search.rrf_k=10"}); exit != ExitUsage || !strings.Contains(stderr, "no index at") {
		t.Errorf("exit %d stderr %q", exit, stderr)
	}
}
