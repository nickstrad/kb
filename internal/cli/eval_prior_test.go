package cli

import (
	"context"
	"encoding/json"
	"fmt"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/nickstrad/kb/internal/errs"
	"github.com/nickstrad/kb/internal/eval"
	"github.com/nickstrad/kb/internal/store"
)

// steppingClock makes every reading of the kb clock one minute after the last, so each search
// and verdict of a test has its own logged second (the log keeps seconds) and their order is the
// order of the commands.
func steppingClock(t *testing.T) {
	t.Helper()
	orig := testDeps.now
	at := time.Date(2026, 10, 1, 9, 0, 0, 0, time.UTC)
	testDeps.now = func() time.Time { at = at.Add(time.Minute); return at }
	t.Cleanup(func() { testDeps.now = orig })
}

// evalPriorRepo is the two-chunk widget repo with the prior at its smallest gate (min_judged 1)
// but off: kb eval --set prior.enabled=true turns it on for a run. "widget" ranks the summary
// first and the Configuration chunk second; "configuring widget" finds only Configuration.
func evalPriorRepo(t *testing.T) string {
	t.Helper()
	root := newSearchTestRepo(t, testModel768, testDim768)
	useRoot(t, root)
	writeConfig(t, root, "[embed]\nmodel = \""+testModel768+"\"\n[prior]\nmin_judged = 1\n")
	steppingClock(t)
	return root
}

// evalPriorRun is kb eval --mode fts --json with flags; it returns the report (or the comparison's).
func evalPriorRun(t *testing.T, flags ...string) (eval.Report, *eval.Comparison) {
	t.Helper()
	stdout, stderr, exit := runKB(t, append([]string{"eval", "--mode", "fts", "--json"}, flags...))
	if exit != 0 {
		t.Fatalf("eval %v: exit %d: %s", flags, exit, stderr)
	}
	var doc struct {
		Comparison *eval.Comparison `json:"comparison"`
		Report     *eval.Report     `json:"report"`
	}
	if err := json.Unmarshal([]byte(stdout), &doc); err == nil && doc.Report != nil {
		return *doc.Report, doc.Comparison
	}
	var rep eval.Report
	if err := json.Unmarshal([]byte(stdout), &rep); err != nil {
		t.Fatalf("eval json: %v\n%s", err, stdout)
	}
	return rep, nil
}

// TestEvalPriorLeakage is PLAN 6.4's leakage table through kb eval: each replay sees only feedback
// recorded before its search, and never its own query's. "widget" (W) has gold Configuration at
// rank 2; "configuring widget" (C) has it at rank 1. A verdict on C given before W was searched
// lifts Configuration to rank 1 in W's replay with the prior on; given after W's search (even
// before W was judged), or on W itself, it does not. The prior's evidence is the same log the
// gold comes from; the replay's own searches are left out of it.
func TestEvalPriorLeakage(t *testing.T) {
	searchW := []string{"search", "widget", "--mode", "fts"}
	searchC := []string{"search", "configuring widget", "--mode", "fts"}
	for _, tc := range []struct {
		name   string
		steps  [][]string // in clock order; search ids follow the order of the searches
		active int        // replays whose prior was active
		wRank  int        // W's first gold rank with the prior on
	}{
		{"C judged before W's search", [][]string{searchC, {"feedback", "1", "1", "--useful"}, searchW, {"feedback", "2", "2", "--useful"}}, 1, 1},
		{"C judged after W's search, before W's verdict", [][]string{searchC, searchW, {"feedback", "1", "1", "--useful"}, {"feedback", "2", "2", "--useful"}}, 0, 2},
		{"C judged after W's verdict", [][]string{searchC, searchW, {"feedback", "2", "2", "--useful"}, {"feedback", "1", "1", "--useful"}}, 0, 2},
		{"only W judged: its own verdict never counts", [][]string{searchW, {"feedback", "1", "2", "--useful"}}, 0, 2},
		// W searched twice: the earlier search's verdict is gold of the same query, so it is left
		// out too, though it was recorded before the replayed (newest) search.
		{"W's earlier search judged before W's newest search", [][]string{searchW, {"feedback", "1", "2", "--useful"}, searchW}, 0, 2},
	} {
		t.Run(tc.name, func(t *testing.T) {
			evalPriorRepo(t)
			for _, s := range tc.steps {
				mustKB(t, s...)
			}
			off, _ := evalPriorRun(t)
			on, _ := evalPriorRun(t, "--set", "prior.enabled=true")
			if off.Prior != (eval.PriorUse{}) {
				t.Errorf("prior off: Prior = %+v, want zero", off.Prior)
			}
			replays := off.Chunk.Queries
			if on.Prior.Enabled != replays || on.Prior.Active != tc.active {
				t.Errorf("prior on: Prior = %+v, want enabled %d active %d", on.Prior, replays, tc.active)
			}
			w := perQuery(t, on, "widget")
			if w.Chunk == nil || w.Chunk.FirstRank != tc.wRank {
				t.Errorf("widget first gold rank with the prior = %+v, want %d", w.Chunk, tc.wRank)
			}
			if r := perQuery(t, off, "widget").Chunk.FirstRank; r != 2 {
				t.Errorf("widget first gold rank without the prior = %d, want 2", r)
			}
		})
	}
}

func perQuery(t *testing.T, r eval.Report, key string) eval.Result {
	t.Helper()
	for _, q := range r.PerQuery {
		if q.Query == key {
			return q
		}
	}
	t.Fatalf("no query %q in %+v", key, r.PerQuery)
	return eval.Result{}
}

// TestEvalComparePriorOn is PLAN 6.4's accept case: kb eval --set prior.enabled=true --compare
// prints the deltas against a baseline without the prior, and says how many replays the prior
// was active in. Here it lifts one query's gold from rank 2 to 1: MRR +0.25 over 2 queries.
func TestEvalComparePriorOn(t *testing.T) {
	evalPriorRepo(t)
	mustKB(t, "search", "configuring widget", "--mode", "fts")
	mustKB(t, "feedback", "1", "1", "--useful")
	mustKB(t, "search", "widget", "--mode", "fts")
	mustKB(t, "feedback", "2", "2", "--useful")
	mustKB(t, "eval", "--mode", "fts", "--save-baseline")

	stdout, stderr, exit := runKB(t, []string{"eval", "--mode", "fts", "--set", "prior.enabled=true", "--compare"})
	if exit != 0 || stderr != "" {
		t.Fatalf("compare: exit %d stderr %q\n%s", exit, stderr, stdout)
	}
	for _, want := range []string{
		"chunk: 2 paired queries (0 only in the baseline, 0 only now)\n",
		"  mrr          0.750    1.000   +0.250  ",
		"prior: active in 1/2 replays · query memory placed hits in 0 (each replay as of its search's time, without its own query's feedback)\n",
	} {
		if !strings.Contains(stdout, want) {
			t.Errorf("compare lacks %q:\n%s", want, stdout)
		}
	}
	_, cmp := evalPriorRun(t, "--set", "prior.enabled=true", "--compare")
	if m := metricOf(t, cmp.Chunk, "mrr"); m.Wins != 1 || m.Losses != 0 || m.Ties != 1 || m.Delta != 0.25 {
		t.Errorf("mrr diff %+v, want +0.25 with 1/0/1", m)
	}
}

// TestEvalPriorQueryMemory (Q140): --set prior.query_memory=true replays with the query memory: a
// past search with the same words in another order (another eval query) remembers its useful hit.
// The store the replays run on can locate chunks, so the list is active.
func TestEvalPriorQueryMemory(t *testing.T) {
	root := evalPriorRepo(t)
	writeConfig(t, root, "[embed]\nmodel = \""+testModel768+"\"\n[prior]\nmin_judged = 1\n[search.list_weights]\nmemory = 2\n")
	mustKB(t, "search", "widget configuring", "--mode", "fts")
	mustKB(t, "feedback", "1", "1", "--useful")
	mustKB(t, "search", "configuring widget", "--mode", "fts")
	mustKB(t, "feedback", "2", "1", "--useful")

	without, _ := evalPriorRun(t, "--set", "prior.enabled=true")
	with, _ := evalPriorRun(t, "--set", "prior.enabled=true", "--set", "prior.query_memory=true")
	if without.Prior.Memory != 0 || with.Prior.Memory != 1 || with.Prior.Active != 1 {
		t.Errorf("memory use: without %+v, with %+v; want 0, then 1 (the later query remembers the earlier)", without.Prior, with.Prior)
	}
	if without.SettingsHash == with.SettingsHash {
		t.Error("query_memory does not change the settings hash")
	}
}

// TestEvalPriorSweepRefused: a sweep of a [prior] key below eval.min_judged is refused, --force
// or not; a [prior] axis the grid never enables is refused too; both before the model loads.
func TestEvalPriorSweepRefused(t *testing.T) {
	evalPriorRepo(t)
	mustKB(t, "search", "widget", "--mode", "fts")
	mustKB(t, "feedback", "1", "2", "--useful")
	for _, tc := range []struct {
		args []string
		want string
	}{
		{[]string{"--sweep", "prior.max_boost=0.1,0.3", "--set", "prior.enabled=true", "--force"}, "--sweep prior.max_boost needs ≥30 judged queries (have 1); --force does not apply"},
		{[]string{"--sweep", "prior.enabled=false,true", "--force"}, "--sweep prior.enabled needs ≥30 judged queries"},
		{[]string{"--sweep", "prior.max_boost=0.1,0.3", "--set", "eval.min_judged=1"}, "changes nothing while prior.enabled is false"},
	} {
		_, stderr, exit := runKB(t, append([]string{"eval", "--mode", "fts"}, tc.args...))
		if exit != ExitUsage || !strings.Contains(stderr, tc.want) {
			t.Errorf("%v: exit %d stderr %q, want usage exit and %q", tc.args, exit, stderr, tc.want)
		}
	}
	// At the gate, a [prior] sweep runs.
	stdout, stderr, exit := runKB(t, []string{"eval", "--mode", "fts", "--set", "eval.min_judged=1", "--set", "prior.enabled=true", "--sweep", "prior.max_boost=0.1,0.3"})
	if exit != 0 || !strings.Contains(stdout, "prior.max_boost=") {
		t.Errorf("sweep at the gate: exit %d stderr %q\n%s", exit, stderr, stdout)
	}
}

// judgeCThenW logs the leakage table's first row: "configuring widget" judged before "widget" is
// searched; with callers as given.
func judgeCThenW(t *testing.T, callerC, callerW string) {
	t.Helper()
	mustKB(t, "search", "configuring widget", "--mode", "fts", "--caller", callerC)
	mustKB(t, "feedback", "1", "1", "--useful")
	mustKB(t, "search", "widget", "--mode", "fts", "--caller", callerW)
	mustKB(t, "feedback", "2", "2", "--useful")
}

// TestEvalPriorReadsWholeLog: kb eval --caller (or --since) scores that window's queries, but the
// prior learns from the whole log, as kb search does: another caller's verdict lifts W's gold.
func TestEvalPriorReadsWholeLog(t *testing.T) {
	evalPriorRepo(t)
	judgeCThenW(t, "other", "me")
	for _, flags := range [][]string{{"--caller", "me"}, {"--since", "2026-10-01"}, {}} {
		rep, _ := evalPriorRun(t, append(flags, "--set", "prior.enabled=true")...)
		if w := perQuery(t, rep, "widget"); w.Chunk == nil || w.Chunk.FirstRank != 1 || rep.Prior.Active != 1 {
			t.Errorf("%v: widget %+v, prior %+v; want rank 1 lifted by the other caller's verdict", flags, w.Chunk, rep.Prior)
		}
	}
}

// judgedFault is a store whose Judged fails from the nth call on.
type judgedFault struct {
	store.Store
	n   *int
	nth int
}

func (f judgedFault) Judged(ctx context.Context, flt store.JudgedFilter) ([]store.JudgedSearch, error) {
	if *f.n++; *f.n >= f.nth {
		return nil, errs.New(errs.Conflict, "test", "log read exploded")
	}
	return f.Store.(store.JudgedLog).Judged(ctx, flt)
}

// TestEvalPriorSecondReadFails: with --caller the prior's whole-log read is a second read; its
// failure stops eval and sweep with its kind (exit 1), before any report.
func TestEvalPriorSecondReadFails(t *testing.T) {
	evalPriorRepo(t)
	judgeCThenW(t, "other", "me")
	orig := testDeps.openStore
	t.Cleanup(func() { testDeps.openStore = orig })
	n := 0
	testDeps.openStore = func(ctx context.Context, path string, ro bool) (store.Store, error) {
		st, err := orig(ctx, path, ro)
		if err != nil {
			return nil, err
		}
		n = 0
		return judgedFault{Store: st, n: &n, nth: 2}, nil
	}
	for _, args := range [][]string{
		{"eval", "--mode", "fts", "--caller", "me", "--set", "prior.enabled=true"},
		{"eval", "--mode", "fts", "--caller", "me", "--set", "prior.enabled=true", "--set", "eval.min_judged=1", "--sweep", "prior.max_boost=0.1,0.3"},
	} {
		stdout, stderr, exit := runKB(t, args)
		if exit != ExitUsage || stdout != "" || !strings.Contains(stderr, "log read exploded") {
			t.Errorf("%v: exit %d stdout %q stderr %q; want 1, no report, the cause", args, exit, stdout, stderr)
		}
	}
	// Without the prior there is no second read.
	if _, stderr, exit := runKB(t, []string{"eval", "--mode", "fts", "--caller", "me"}); exit != 0 {
		t.Errorf("prior off: exit %d: %s", exit, stderr)
	}
}

// TestEvalPriorSweepUsesPrior: a sweep of prior.enabled replays the enabled point with the prior
// (the evidence is loaded when any grid point enables it): it wins, at MRR 1.
func TestEvalPriorSweepUsesPrior(t *testing.T) {
	evalPriorRepo(t)
	judgeCThenW(t, "c", "c")
	stdout, stderr, exit := runKB(t, []string{"eval", "--mode", "fts", "--json", "--set", "eval.min_judged=1", "--set", "eval.holdout=0",
		"--sweep", "prior.enabled=false,true"})
	if exit != 0 {
		t.Fatalf("sweep: exit %d: %s", exit, stderr)
	}
	var rep eval.SweepReport
	if err := json.Unmarshal([]byte(stdout), &rep); err != nil {
		t.Fatal(err)
	}
	if len(rep.Top) != 2 || !slices.Equal(rep.Top[0].Set, []string{"prior.enabled=true"}) || rep.Top[0].Train.MRR.Mean != 1 || rep.Top[1].Train.MRR.Mean != 0.75 {
		t.Errorf("top rows %+v: want prior.enabled=true first at MRR 1, then false at 0.75", rep.Top)
	}
}

// TestPriorLine is the eval report's prior line: none while the prior is off, the counts when on,
// and a hint when the gate never opened.
func TestPriorLine(t *testing.T) {
	const head = "prior: active in %d/%d replays · query memory placed hits in %d (each replay as of its search's time, without its own query's feedback)\n"
	const hint = "prior inactive in every replay: fewer than prior.min_judged judged searches before each one\n"
	for _, tc := range []struct {
		u    eval.PriorUse
		want string
	}{
		{eval.PriorUse{}, ""},
		{eval.PriorUse{Enabled: 1}, fmt.Sprintf(head, 0, 1, 0) + hint},
		{eval.PriorUse{Enabled: 2, Active: 1}, fmt.Sprintf(head, 1, 2, 0)},
		{eval.PriorUse{Enabled: 3, Active: 3, Memory: 2}, fmt.Sprintf(head, 3, 3, 2)},
	} {
		if got := priorLine(tc.u); got != tc.want {
			t.Errorf("priorLine(%+v) = %q, want %q", tc.u, got, tc.want)
		}
	}
}

// TestEvalPriorWindowLeavesOutQuery (PLAN 6.4, W13 pass): kb eval --caller (or --since) scores a
// window of the log, but the prior reads the whole log, so a search of the same query outside
// the window must be left out too: its verdict is the same gold. Here "widget" is judged by
// caller "other" and then by "me"; replaying "me"'s search must not see "other"'s verdict, with
// or without the window, in kb eval and in a sweep.
func TestEvalPriorWindowLeavesOutQuery(t *testing.T) {
	evalPriorRepo(t)
	mustKB(t, "search", "widget", "--mode", "fts", "--caller", "other")
	mustKB(t, "feedback", "1", "2", "--useful")
	mustKB(t, "search", "widget", "--mode", "fts", "--caller", "me")
	mustKB(t, "feedback", "2", "2", "--useful")
	for _, flags := range [][]string{{"--caller", "me"}, {}} { // --since takes the same whole-log branch as --caller
		rep, _ := evalPriorRun(t, append(flags, "--set", "prior.enabled=true")...)
		if w := perQuery(t, rep, "widget"); w.Chunk == nil || w.Chunk.FirstRank != 2 || rep.Prior.Active != 0 {
			t.Errorf("%v: widget %+v, prior %+v; want rank 2 and the gate closed (the query's own verdicts are out)", flags, w.Chunk, rep.Prior)
		}
	}
	stdout, stderr, exit := runKB(t, []string{"eval", "--mode", "fts", "--json", "--caller", "me", "--set", "eval.min_judged=1",
		"--set", "eval.holdout=0", "--set", "prior.enabled=true", "--sweep", "prior.max_boost=0.1,0.3"})
	if exit != 0 {
		t.Fatalf("sweep: exit %d: %s", exit, stderr)
	}
	var sr eval.SweepReport
	if err := json.Unmarshal([]byte(stdout), &sr); err != nil {
		t.Fatal(err)
	}
	for _, r := range sr.Top {
		if r.Train.MRR.Mean != 0.5 {
			t.Errorf("sweep row %v: train MRR %v, want 0.5 (rank 2: no leak)", r.Set, r.Train.MRR.Mean)
		}
	}
}
