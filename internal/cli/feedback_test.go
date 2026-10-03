package cli

import (
	"encoding/json"
	"fmt"
	"path/filepath"
	"regexp"
	"strings"
	"testing"

	"github.com/nickstrad/kb/internal/embed/ollama"
	"github.com/nickstrad/kb/internal/store"
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

// feedbackRows counts search_feedback rows, optionally for one (search, rank).
func feedbackRows(t *testing.T, root, where string, args ...any) int {
	t.Helper()
	st, err := store.Open(filepath.Join(root, ".kb", "kb.sqlite"))
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	defer st.Close()
	q := "SELECT count(*) FROM search_feedback"
	if where != "" {
		q += " WHERE " + where
	}
	var n int
	if err := st.DB().QueryRow(q, args...).Scan(&n); err != nil {
		t.Fatalf("%s: %v", q, err)
	}
	return n
}

// TestFeedbackArgValidation: every malformed invocation is a usage error (exit 1) with a message
// that says how to fix it, and none of them writes a row.
func TestFeedbackArgValidation(t *testing.T) {
	root := newSearchTestRepo(t, ollama.DefaultModel, ollama.DefaultDim)
	t.Setenv("KB_ROOT", root)
	t.Setenv("KB_CALLER", "")
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
	if n := feedbackRows(t, root, ""); n != 0 {
		t.Errorf("feedback rows = %d after only rejected invocations, want 0", n)
	}
}

// TestFeedbackByID records a hit verdict and a whole-search verdict on a zero-result search, the
// case the old two-argument form could not express at all.
func TestFeedbackByID(t *testing.T) {
	root := newSearchTestRepo(t, ollama.DefaultModel, ollama.DefaultDim)
	t.Setenv("KB_ROOT", root)

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
	if n := feedbackRows(t, root, "search_id = ? AND rank = 0 AND useful = 0 AND note = ?", zero, "no widget-free docs"); n != 1 {
		t.Errorf("rank-0 rows for search %s = %d, want 1", zero, n)
	}

	// --none on the first search contradicts its useful rank 1 and is refused with the fix.
	_, stderr, exit = runKB(t, []string{"feedback", id, "--none"})
	if exit != ExitUsage || !strings.Contains(stderr, "kb feedback "+id+" 1 --not-useful") {
		t.Errorf("--none over a useful verdict: exit %d stderr %q, want the re-mark command", exit, stderr)
	}
}

// TestFeedbackLast: --last resolves the caller exactly as kb search does (flag, then $KB_CALLER),
// picks that caller's newest search, and says which search it judged.
func TestFeedbackLast(t *testing.T) {
	root := newSearchTestRepo(t, ollama.DefaultModel, ollama.DefaultDim)
	t.Setenv("KB_ROOT", root)
	t.Setenv("KB_CALLER", "")

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
	if n := feedbackRows(t, root, "search_id = ? AND rank = 1 AND useful = 1", claudeNew); n != 1 {
		t.Errorf("rank-1 useful rows on search %s = %d, want 1", claudeNew, n)
	}
	if n := feedbackRows(t, root, "search_id = ?", claudeOld); n != 0 {
		t.Errorf("the older claude search %s got %d feedback rows, want 0", claudeOld, n)
	}

	// Environment form, and --none on a zero-result search, which is how such a search gets judged.
	t.Setenv("KB_CALLER", "codex")
	stdout, stderr, exit = runKB(t, []string{"feedback", "--last", "--none", "--note", "nothing there"})
	if exit != 0 {
		t.Fatalf("feedback --last --none via KB_CALLER: exit %d; stderr=%s", exit, stderr)
	}
	if want := "recorded: search " + codexZero + " none (nothing useful) (newest search for codex, "; !strings.HasPrefix(stdout, want) {
		t.Errorf("stdout = %q, want prefix %q", stdout, want)
	}

	// The flag beats the environment, as in kb search: KB_CALLER=codex, --caller claude.
	stdout, stderr, exit = runKB(t, []string{"feedback", "--last", "1", "--not-useful", "--caller", "claude"})
	if exit != 0 {
		t.Fatalf("feedback --last with flag over env: exit %d; stderr=%s", exit, stderr)
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
	root := newSearchTestRepo(t, ollama.DefaultModel, ollama.DefaultDim)
	t.Setenv("KB_ROOT", root)

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

// TestFeedbackHelpReadsAsNextStep: kb help and kb search --help present feedback as the step after
// every search, and the feedback help names every verdict form and the --last limitation.
func TestFeedbackHelpReadsAsNextStep(t *testing.T) {
	for _, args := range [][]string{{"help"}, {"search", "--help"}} {
		stdout, stderr, exit := runKB(t, args)
		if exit != 0 {
			t.Fatalf("kb %v: exit = %d; stderr=%s", args, exit, stderr)
		}
		for _, want := range []string{"search N · M hits", "kb feedback N <rank> --useful", "--none"} {
			if !strings.Contains(stdout, want) {
				t.Errorf("kb %v help missing %q:\n%s", args, want, stdout)
			}
		}
	}

	stdout, stderr, exit := runKB(t, []string{"feedback", "--help"})
	if exit != 0 {
		t.Fatalf("exit = %d; stderr=%s", exit, stderr)
	}
	for _, want := range []string{"after every search", "--none", "--last", "same caller name", "kb feedback 42 --none"} {
		if !strings.Contains(stdout, want) {
			t.Errorf("feedback help missing %q:\n%s", want, stdout)
		}
	}
}
