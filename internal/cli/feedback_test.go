package cli

import (
	"context"
	"encoding/json"
	"fmt"
	"regexp"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/nickstrad/kb/internal/store"
	"github.com/nickstrad/kb/internal/store/sqlite"
)

// headerID is the search id from the first line of `kb search` human output ("search N · M hits"),
// which is what an agent reading `kb search ... | head` has to work with.
var headerID = regexp.MustCompile(`\Asearch ([1-9][0-9]*) · `)

// searchID runs one fts search against the test repo and returns the id from its first line.
func searchID(t *testing.T, args ...string) string {
	t.Helper()
	stdout, stderr, exit := runKB(t, append([]string{"search", "--mode", "fts"}, args...))
	if exit != 0 {
		t.Fatalf("kb search %v: exit %d; stderr=%s", args, exit, stderr)
	}
	m := headerID.FindStringSubmatch(stdout)
	if m == nil {
		t.Fatalf("kb search %v: first line carries no search id:\n%s", args, stdout)
	}
	return m[1]
}

// feedbackRows counts the stored verdict rows (store.FeedbackLog) satisfying every match.
func feedbackRows(t *testing.T, root string, match ...func(store.FeedbackRow) bool) int {
	t.Helper()
	st, err := sqlite.Open(context.Background(), testDBPath(root), sqlite.Options{ReadOnly: true})
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	defer st.Close()
	rows, err := st.FeedbackRows(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	n := 0
rows:
	for _, r := range rows {
		for _, m := range match {
			if !m(r) {
				continue rows
			}
		}
		n++
	}
	return n
}

// verdictOn matches the verdicts of one search at one rank, with the given usefulness.
func verdictOn(search string, rank int, useful bool) func(store.FeedbackRow) bool {
	return func(r store.FeedbackRow) bool {
		return strconv.FormatInt(r.SearchID, 10) == search && r.Rank == rank && r.Useful == useful
	}
}

// onSearch matches the verdicts of one search.
func onSearch(search string) func(store.FeedbackRow) bool {
	return func(r store.FeedbackRow) bool { return strconv.FormatInt(r.SearchID, 10) == search }
}

// TestFeedbackArgValidation: every malformed invocation is a usage error (exit 1) with a message
// that says how to fix it, and none of them writes a row.
func TestFeedbackArgValidation(t *testing.T) {
	root := newSearchTestRepo(t, testModel768, testDim768)
	useRoot(t, root)
	id := searchID(t, "widget", "--caller", "claude")

	for _, tc := range []struct {
		name string
		args []string
		want string
	}{
		{"no verdict", []string{id, "1"}, "exactly one of --useful, --not-useful or --none"},
		{"two verdicts", []string{id, "1", "--useful", "--not-useful"}, "exactly one of"},
		{"rank and none", []string{id, "1", "--none"}, "takes no rank"},
		{"useful without rank", []string{id, "--useful"}, "need the #rank of a hit"},
		{"no id and no last", []string{"--none"}, "or --last"},
		{"last plus id and rank", []string{"--last", id, "1", "--useful", "--caller", "claude"}, "at most a rank"},
		{"last useful without rank", []string{"--last", "--useful", "--caller", "claude"}, "need the #rank"},
		{"bad id", []string{"x", "1", "--useful"}, `search id must be a positive integer, got "x"`},
		{"zero rank", []string{id, "0", "--useful"}, `rank must be a positive integer, got "0"`},
		{"last with no caller", []string{"--last", "1", "--useful"}, "--last needs to know who searched"},
		{"last with caller unknown", []string{"--last", "--none", "--caller", "unknown"}, "--last needs to know who searched"},
		{"last for a caller with no search", []string{"--last", "--none", "--caller", "codex"}, `no search logged for caller "codex"`},
		{"unknown search", []string{"999", "--none"}, "no search with id 999"},
		{"rank out of range", []string{id, "3", "--useful"}, "rank 3 is out of range"},
		{"too many args", []string{id, "1", "2", "--useful"}, "at most 2 arg"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			stdout, stderr, exit := runKB(t, append([]string{"feedback"}, tc.args...))
			if exit != ExitUsage {
				t.Fatalf("exit = %d, want %d; stdout=%s stderr=%s", exit, ExitUsage, stdout, stderr)
			}
			if !strings.Contains(stderr, tc.want) {
				t.Errorf("stderr = %q, want it to contain %q", stderr, tc.want)
			}
		})
	}
	if n := feedbackRows(t, root); n != 0 {
		t.Errorf("feedback rows = %d after only rejected invocations, want 0", n)
	}
}

// TestFeedbackByID records a hit verdict, and a whole-search verdict on a zero-result search.
func TestFeedbackByID(t *testing.T) {
	root := newSearchTestRepo(t, testModel768, testDim768)
	useRoot(t, root)

	id := searchID(t, "widget")
	stdout, stderr, exit := runKB(t, []string{"feedback", id, "1", "--useful"})
	if exit != 0 {
		t.Fatalf("feedback --useful: exit %d; stderr=%s", exit, stderr)
	}
	if want := "recorded: search " + id + " rank 1 useful=1\n"; stdout != want {
		t.Errorf("stdout = %q, want %q", stdout, want)
	}

	zero := searchID(t, "not-in-the-fixture")
	_, stderr, exit = runKB(t, []string{"feedback", zero, "1", "--not-useful"})
	if exit != ExitUsage || !strings.Contains(stderr, "judge it with --none") {
		t.Errorf("ranked verdict on a zero-result search: exit %d stderr %q, want a usage error suggesting --none", exit, stderr)
	}
	stdout, stderr, exit = runKB(t, []string{"feedback", zero, "--none", "--note", "no widget-free docs"})
	if exit != 0 {
		t.Fatalf("feedback --none: exit %d; stderr=%s", exit, stderr)
	}
	if want := "recorded: search " + zero + " none (nothing useful)\n"; stdout != want {
		t.Errorf("stdout = %q, want %q", stdout, want)
	}
	if n := feedbackRows(t, root, verdictOn(zero, 0, false), func(r store.FeedbackRow) bool { return r.Note == "no widget-free docs" }); n != 1 {
		t.Errorf("rank-0 rows for search %s = %d, want 1", zero, n)
	}

	// --none on the first search contradicts its useful rank 1 and is refused with the fix.
	_, stderr, exit = runKB(t, []string{"feedback", id, "--none"})
	if exit != ExitUsage || !strings.Contains(stderr, "kb feedback "+id+" 1 --not-useful") {
		t.Errorf("--none over a useful verdict: exit %d stderr %q, want the re-mark command", exit, stderr)
	}
}

// TestFeedbackLast: --last resolves the caller exactly as kb search does (flag, then [agent] caller),
// picks that caller's newest search, and says which search it judged.
func TestFeedbackLast(t *testing.T) {
	root := newSearchTestRepo(t, testModel768, testDim768)
	useRoot(t, root)

	claudeOld := searchID(t, "widget", "--caller", "claude")
	claudeNew := searchID(t, "widget configuring", "--caller", "claude")
	codexZero := searchID(t, "not-in-the-fixture", "--caller", "codex")
	if claudeOld == claudeNew {
		t.Fatalf("two searches got the same id %s", claudeOld)
	}

	// Flag form: codex searched last overall, but claude's newest is what --caller claude means.
	// "widget configuring" matches one chunk only (FTS ANDs the terms), so rank 1 is its hit.
	stdout, stderr, exit := runKB(t, []string{"feedback", "--last", "1", "--useful", "--caller", "claude"})
	if exit != 0 {
		t.Fatalf("feedback --last --caller claude: exit %d; stderr=%s", exit, stderr)
	}
	wantPrefix := "recorded: search " + claudeNew + " rank 1 useful=1 (newest search for claude, "
	if !strings.HasPrefix(stdout, wantPrefix) || !strings.HasSuffix(stdout, `: "widget configuring")`+"\n") {
		t.Errorf("stdout = %q, want %q… ending in the query", stdout, wantPrefix)
	}
	if n := feedbackRows(t, root, verdictOn(claudeNew, 1, true)); n != 1 {
		t.Errorf("rank-1 useful rows on search %s = %d, want 1", claudeNew, n)
	}
	if n := feedbackRows(t, root, onSearch(claudeOld)); n != 0 {
		t.Errorf("the older claude search %s got %d feedback rows, want 0", claudeOld, n)
	}

	// Config form, and --none on a zero-result search, which is how such a search gets judged.
	writeConfig(t, root, "[agent]\ncaller = \"codex\"\n")
	stdout, stderr, exit = runKB(t, []string{"feedback", "--last", "--none", "--note", "nothing there"})
	if exit != 0 {
		t.Fatalf("feedback --last --none via [agent] caller: exit %d; stderr=%s", exit, stderr)
	}
	if want := "recorded: search " + codexZero + " none (nothing useful) (newest search for codex, "; !strings.HasPrefix(stdout, want) {
		t.Errorf("stdout = %q, want prefix %q", stdout, want)
	}

	// The flag beats the config, as in kb search: caller = codex, --caller claude.
	stdout, stderr, exit = runKB(t, []string{"feedback", "--last", "1", "--not-useful", "--caller", "claude"})
	if exit != 0 {
		t.Fatalf("feedback --last with flag over config: exit %d; stderr=%s", exit, stderr)
	}
	if want := "recorded: search " + claudeNew + " rank 1 useful=0 "; !strings.HasPrefix(stdout, want) {
		t.Errorf("stdout = %q, want prefix %q", stdout, want)
	}
}

// TestFeedbackNoneCountsInStats: the point of --none is that kb stats sees the search as judged
// and not a hit. One fts search with zero results, judged --none, is one judged search with a 0%
// hit rate and an MRR of 0 (100% coverage, 100% zero-result), where before it was unjudged and its
// hit rate NULL.
func TestFeedbackNoneCountsInStats(t *testing.T) {
	root := newSearchTestRepo(t, testModel768, testDim768)
	useRoot(t, root)

	zero := searchID(t, "not-in-the-fixture", "--caller", "claude")
	overview := func() map[string]any {
		t.Helper()
		stdout, stderr, exit := runKB(t, []string{"stats", "overview", "--json"})
		if exit != 0 {
			t.Fatalf("stats overview: exit %d; stderr=%s", exit, stderr)
		}
		var parsed struct {
			Rows []map[string]any `json:"rows"`
		}
		if err := json.Unmarshal([]byte(stdout), &parsed); err != nil {
			t.Fatalf("unmarshal overview: %v\n%s", err, stdout)
		}
		if len(parsed.Rows) != 1 {
			t.Fatalf("overview rows = %d, want 1 (one fts search): %v", len(parsed.Rows), parsed.Rows)
		}
		return parsed.Rows[0]
	}

	before := overview()
	if before["judged"] != float64(0) || before["hit_rate_pct"] != nil {
		t.Errorf("before --none: judged %v hit_rate_pct %v, want 0 and null", before["judged"], before["hit_rate_pct"])
	}

	if _, stderr, exit := runKB(t, []string{"feedback", zero, "--none"}); exit != 0 {
		t.Fatalf("feedback --none: exit %d; stderr=%s", exit, stderr)
	}
	after := overview()
	for col, want := range map[string]any{
		"mode": "fts", "searches": float64(1), "judged": float64(1), "coverage_pct": 100.0,
		"hit_rate_pct": 0.0, "mrr": 0.0, "zero_result_pct": 100.0,
	} {
		if fmt.Sprint(after[col]) != fmt.Sprint(want) {
			t.Errorf("after --none: %s = %v, want %v", col, after[col], want)
		}
	}
}

// TestFeedbackHelpNamesEveryVerdict: kb help and kb search --help show the feedback command forms
// as optional, and the feedback help names every verdict form and the --last limitation.
func TestFeedbackHelpNamesEveryVerdict(t *testing.T) {
	for _, args := range [][]string{{"help"}, {"search", "--help"}} {
		stdout, stderr, exit := runKB(t, args)
		if exit != 0 {
			t.Fatalf("kb %v: exit = %d; stderr=%s", args, exit, stderr)
		}
		for _, want := range []string{"search N · M hits", "kb feedback N <rank> --useful", "--none", "optional"} {
			if !strings.Contains(stdout, want) {
				t.Errorf("kb %v help missing %q:\n%s", args, want, stdout)
			}
		}
	}

	stdout, stderr, exit := runKB(t, []string{"feedback", "--help"})
	if exit != 0 {
		t.Fatalf("exit = %d; stderr=%s", exit, stderr)
	}
	for _, want := range []string{"optional", "--none", "--last", "same caller name", "kb feedback 42 --none"} {
		if !strings.Contains(stdout, want) {
			t.Errorf("feedback help missing %q:\n%s", want, stdout)
		}
	}
}

// TestLogUsesInjectedClock (Q78, Q81): searches, verdicts and opens are stamped with deps.now, so
// a test can place them at an exact time.
func TestLogUsesInjectedClock(t *testing.T) {
	root := openRepo(t)
	at := time.Date(2025, 1, 2, 3, 4, 5, 0, time.UTC)
	orig := testDeps.now
	t.Cleanup(func() { testDeps.now = orig })
	testDeps.now = func() time.Time { return at }
	id := searchID(t, "foo")
	mustKB(t, "feedback", id, "1", "--useful")
	mustKB(t, "show", "data/foo.md")
	if n := feedbackRows(t, root, func(r store.FeedbackRow) bool { return r.TS == "2025-01-02T03:04:05Z" }); n != 1 {
		t.Errorf("%d verdicts stamped with the injected clock, want 1", n)
	}
	st, err := sqlite.Open(context.Background(), testDBPath(root), sqlite.Options{ReadOnly: true})
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	got, err := st.Judged(context.Background(), store.JudgedFilter{Since: at})
	if err != nil || len(got) != 1 || got[0].TS != "2025-01-02T03:04:05Z" || len(got[0].Opens) != 1 {
		t.Errorf("Judged since the injected time = %+v, %v; want the one search, stamped then, with its open", got, err)
	}
}
