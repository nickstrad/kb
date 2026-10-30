package cli

import (
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/nickstrad/kb/internal/search"
)

// TestSearchSpellNote: a typo that finds nothing is also searched as its correction, the hits come
// back and stderr says so; kb explain shows the fix, and search.spell=false turns it off.
func TestSearchSpellNote(t *testing.T) {
	useRoot(t, newExplainRepo(t))
	withFixtureEmbedder(t)

	paths, stderr, exit := synHits(t, "lgout")
	if exit != 0 || len(paths) == 0 {
		t.Fatalf("exit %d hits %v, want the logout entries", exit, paths)
	}
	if want := "note: also searched \"logout\" for \"lgout\"\n"; stderr != want {
		t.Errorf("stderr = %q, want %q", stderr, want)
	}

	stdout, stderr := mustKB(t, "explain", "lgout", "--mode", "fts")
	for _, want := range []string{`fts: match ("lgout" OR "logout") · AND`, "spell: lgout -> logout (1 edit, 3 chunks) (the FTS list had fewer than 3 rows)"} {
		if !strings.Contains(stdout, want) {
			t.Errorf("explain lacks %q:\n%s", want, stdout)
		}
	}
	if !strings.Contains(stderr, `note: also searched "logout"`) {
		t.Errorf("explain stderr = %q, want the spell note", stderr)
	}
	stdout, stderr = mustKB(t, "explain", "lgout", "--mode", "fts", "--set", "search.spell=false")
	if strings.Contains(stdout, "spell:") || stderr != "" || !strings.Contains(stdout, `fts: match "lgout"`) {
		t.Errorf("spell off still corrected:\n%s\nstderr %q", stdout, stderr)
	}
	// kb context runs the same Searcher, so it says so too (DEFAULTS #7).
	if _, stderr, _ := runKB(t, []string{"context", "lgout", "--json"}); !strings.Contains(stderr, `note: also searched "logout" for "lgout"`) {
		t.Errorf("context stderr = %q, want the spell note", stderr)
	}
	// A word the index has is never corrected, however few rows it finds.
	if _, stderr, _ := synHits(t, "keepalive"); stderr != "" {
		t.Errorf("known word got a note: %q", stderr)
	}
}

// TestSynonymsSuggest: a failed search reworded into one that was judged useful, a minute later
// on a pinned clock, is suggested as a group with its evidence, in both forms.
func TestSynonymsSuggest(t *testing.T) {
	useRoot(t, newExplainRepo(t))
	withFixtureEmbedder(t)
	at := time.Date(2026, 10, 30, 12, 0, 0, 0, time.UTC)
	orig := testDeps.now
	t.Cleanup(func() { testDeps.now = orig })
	testDeps.now = func() time.Time { return at }

	searchID(t, "signout", "--caller", "agent")
	at = at.Add(time.Minute)
	id := searchID(t, "logout", "--caller", "agent")
	mustKB(t, "feedback", id, "1", "--useful")

	stdout, _ := mustKB(t, "synonyms", "suggest")
	for _, want := range []string{
		"# kb synonyms suggest · 2 searches · 1 reworded pair (window 5m0s)\n",
		"# 1 pair, 1 caller (signout -> logout 1); chunks: logout 3, signout 0\nlogout, signout\n",
		"#   signout (1 search)\n",
	} {
		if !strings.Contains(stdout, want) {
			t.Errorf("suggest lacks %q:\n%s", want, stdout)
		}
	}
	for _, line := range strings.Split(strings.TrimSpace(stdout), "\n") {
		if line != "" && !strings.HasPrefix(line, "#") && line != "logout, signout" {
			t.Errorf("line %q is neither a comment nor a group: the output must paste as synonyms.txt", line)
		}
	}
	if syn := search.ParseSynonyms([]byte(stdout)); syn.Len() != 2 {
		t.Errorf("pasted output parses to %d terms, want the 2 of the group", syn.Len())
	}

	stdout, _ = mustKB(t, "synonyms", "suggest", "--json", "--window", "30s")
	var got search.Suggestions
	if err := json.Unmarshal([]byte(stdout), &got); err != nil {
		t.Fatalf("decode %q: %v", stdout, err)
	}
	if got.Window != "30s" || got.Pairs != 0 || len(got.Suggestions) != 0 || got.Searches != 2 {
		t.Errorf("30s window: %+v, want no pair", got)
	}
	// --gaps: 0 lists none (MaxGaps 0 alone would mean no cap), 1 caps; the log has one gap word.
	for _, tc := range []struct {
		gaps string
		want int
	}{{"0", 0}, {"1", 1}, {"5", 1}} {
		stdout, _ = mustKB(t, "synonyms", "suggest", "--json", "--gaps", tc.gaps)
		var g search.Suggestions
		if err := json.Unmarshal([]byte(stdout), &g); err != nil || g.Gaps == nil || len(g.Gaps) != tc.want {
			t.Errorf("--gaps %s: gaps %+v (%v), want %d", tc.gaps, g.Gaps, err, tc.want)
		}
	}
}

// TestSynonymsSuggestErrors: bad flags are usage errors (exit 1) and a store with no index is
// NotFound, all before anything is printed on stdout.
func TestSynonymsSuggestErrors(t *testing.T) {
	useRoot(t, newExplainRepo(t))
	for _, tc := range []struct {
		args []string
		want string
	}{
		{[]string{"synonyms", "suggest", "--window", "0s"}, "invalid --window 0s"},
		{[]string{"synonyms", "suggest", "--min", "0"}, "invalid --min 0"},
		{[]string{"synonyms", "suggest", "--gaps", "-1"}, "invalid --gaps -1"},
		{[]string{"synonyms", "suggest", "--since", "yesterday"}, `invalid --since "yesterday"`},
		{[]string{"synonyms", "suggest", "extra"}, "unknown command"},
	} {
		stdout, stderr, exit := runKB(t, tc.args)
		if exit != ExitUsage || stdout != "" || !strings.Contains(stderr, tc.want) {
			t.Errorf("%v: exit %d stdout %q stderr %q, want exit %d and %q", tc.args, exit, stdout, stderr, ExitUsage, tc.want)
		}
	}

	useRoot(t, t.TempDir())
	stdout, stderr, exit := runKB(t, []string{"synonyms", "suggest"})
	if exit == 0 || stdout != "" || !strings.Contains(stderr, "no index") {
		t.Errorf("no index: exit %d stdout %q stderr %q", exit, stdout, stderr)
	}
}

// TestRenderSuggestions: the human form's branches: no groups, one group (plural/singular nouns,
// both From directions, the typo note) and the gaps block.
func TestRenderSuggestions(t *testing.T) {
	group := func(typo bool) search.Suggestion {
		return search.Suggestion{
			Terms: [2]string{"login", "signin"}, Line: "login, signin", Pairs: 2, Callers: 1, Typo: typo,
			From: map[string]int{"login": 2, "signin": 1}, Doc: map[string]int{"login": 5, "signin": 0},
		}
	}
	for _, tc := range []struct {
		name        string
		in          search.Suggestions
		want, never []string
	}{
		{"no groups", search.Suggestions{Window: "5m0s", Searches: 1, Pairs: 1, Gaps: []search.Gap{}},
			[]string{"# kb synonyms suggest · 1 searches · 1 reworded pair (window 5m0s)\n", "# no groups to suggest\n"},
			[]string{"paste the groups", "gaps:"}},
		{"group both directions", search.Suggestions{Window: "5m0s", Searches: 4, Pairs: 2, Suggestions: []search.Suggestion{group(false)}},
			[]string{"2 reworded pairs", "# paste the groups you approve into syn.txt (kb never writes it)\n",
				"# 2 pairs, 1 caller (login -> signin 2, signin -> login 1); chunks: login 5, signin 0\nlogin, signin\n"},
			[]string{"a typo", "no groups"}},
		{"typo note", search.Suggestions{Window: "1m", Suggestions: []search.Suggestion{group(true)}},
			[]string{"signin 0; a typo: search.spell already corrects it\nlogin, signin\n"}, nil},
		{"gaps", search.Suggestions{Window: "1m", Gaps: []search.Gap{{Term: "zzz", Searches: 1}, {Term: "qqq", Searches: 3}}},
			[]string{"# gaps: words of searches", "#   zzz (1 search)\n", "#   qqq (3 searches)\n"}, nil},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got := renderSuggestions(tc.in, "syn.txt")
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
			for _, line := range strings.Split(strings.TrimSpace(got), "\n") {
				if line != "" && !strings.HasPrefix(line, "#") && line != "login, signin" {
					t.Errorf("non-comment, non-group line %q", line)
				}
			}
		})
	}
}
