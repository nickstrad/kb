package render

import (
	"flag"
	"fmt"
	"math/rand"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

var update = flag.Bool("update", false, "rewrite the golden files")

func fixture() []Hit {
	return []Hit{
		{Rank: 1, Path: "data/tmux.md", Heading: "Logs", Updated: "2026-03-01",
			Summary: "How tmux logs are rotated.",
			Text:    "Rotate tmux logs with logrotate.\nSet copytruncate so the pane keeps writing.\nKeep seven days."},
		{Rank: 2, Path: "data/git.md", Heading: "", Updated: "2026-02-11",
			Summary: "Git recipes for rewriting history.",
			Text:    "Use git rebase -i to squash the last commits before you push."},
		{Rank: 3, Path: "data/deploy.md", Heading: "Rollback", Updated: "",
			Summary: "Deploy and rollback runbook.",
			Text:    "Roll back by redeploying the previous tag; never edit the live host by hand."},
		{Rank: 4, Path: "data/naming.md", Heading: "Rules", Updated: "2025-12-31",
			Summary: "",
			Text:    "Names are kebab-case."},
	}
}

func TestEstimateTokens(t *testing.T) {
	for _, tc := range []struct {
		in   string
		want int
	}{
		{"", 0}, {"a", 1}, {"abcd", 1}, {"abcde", 2}, {"héllo", 2}, {strings.Repeat("日", 8), 2}, {strings.Repeat("x", 9), 3},
	} {
		if got := EstimateTokens(tc.in); got != tc.want {
			t.Errorf("EstimateTokens(%q) = %d, want %d", tc.in, got, tc.want)
		}
	}
}

func TestRenderGolden(t *testing.T) {
	for _, b := range []int{0, 1, 8, 20, 40, 60, 90, 130, 400} {
		t.Run(fmt.Sprintf("budget_%d", b), func(t *testing.T) {
			res := Render(fixture(), b)
			got := fmt.Sprintf("levels=%v tokens=%d\n%s\n", res.Levels, res.Tokens, res.Text)
			path := filepath.Join("testdata", fmt.Sprintf("budget_%d.golden", b))
			if *update {
				if err := os.MkdirAll("testdata", 0o755); err != nil {
					t.Fatal(err)
				}
				if err := os.WriteFile(path, []byte(got), 0o644); err != nil {
					t.Fatal(err)
				}
			}
			want, err := os.ReadFile(path)
			if err != nil {
				t.Fatalf("read golden (run with -update): %v", err)
			}
			if got != string(want) {
				t.Errorf("budget %d output changed:\n--- got\n%s--- want\n%s", b, got, want)
			}
		})
	}
}

// TestRenderCutoffTable is the decision table for the elastic cut-off: each row pins the levels the
// greedy pass picks. Rows are built from block costs so they do not depend on exact wording.
func TestRenderCutoffTable(t *testing.T) {
	hits := fixture()
	headers := compose(hits, []int{1, 1, 1, 1})
	allSummary := compose(hits, []int{2, 2, 2, 2})
	oneFull := compose(hits, []int{3, 2, 2, 2})
	full := Render(hits, 0).Text
	tok := EstimateTokens
	for _, tc := range []struct {
		name   string
		budget int
		want   []int
	}{
		{"unlimited", 0, []int{3, 3, 3, 3}},
		{"nothing fits", 1, []int{0, 0, 0, 0}},
		{"tail alone exactly", tok(compose(hits, []int{0, 0, 0, 0})), []int{0, 0, 0, 0}},
		{"one header exactly", tok(compose(hits, []int{1, 0, 0, 0})), []int{1, 0, 0, 0}},
		{"headers exactly", tok(headers), []int{1, 1, 1, 1}},
		{"headers minus one drops a hit", tok(headers) - 1, []int{1, 1, 1, 0}},
		{"all summaries exactly", tok(allSummary), []int{2, 2, 2, 2}},
		{"all summaries minus one", tok(allSummary) - 1, []int{2, 2, 1, 1}},
		{"first full exactly", tok(oneFull), []int{3, 2, 2, 2}},
		{"first full minus one", tok(oneFull) - 1, []int{2, 2, 2, 2}},
		{"everything exactly", tok(full), []int{3, 3, 3, 3}},
		{"everything plus one", tok(full) + 1, []int{3, 3, 3, 3}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			res := Render(hits, tc.budget)
			if fmt.Sprint(res.Levels) != fmt.Sprint(tc.want) {
				t.Errorf("budget %d: levels %v, want %v\n%s", tc.budget, res.Levels, tc.want, res.Text)
			}
			if tc.budget > 0 && res.Tokens > tc.budget {
				t.Errorf("tokens %d exceed budget %d", res.Tokens, tc.budget)
			}
		})
	}
}

// TestRenderSingleHit: one hit is rendered like any other count (a "no hits" shortcut must not
// swallow it): unlimited gives level 3 and the full block; a budget that fits only the header
// gives level 1 and the tail.
func TestRenderSingleHit(t *testing.T) {
	h := fixture()[0]
	if r := Render([]Hit{h}, 0); fmt.Sprint(r.Levels) != "[3]" || r.Text != block(h, 3) {
		t.Fatalf("one hit, unlimited: %v %q", r.Levels, r.Text)
	}
	one := compose([]Hit{h}, []int{1})
	if r := Render([]Hit{h}, EstimateTokens(one)); fmt.Sprint(r.Levels) != "[1]" || r.Text != one {
		t.Fatalf("one hit, header budget: %v %q", r.Levels, r.Text)
	}
}

// TestComposeTail pins the tail wording for each mix of hidden and condensed hits.
func TestComposeTail(t *testing.T) {
	hits := fixture()
	for _, tc := range []struct {
		levels []int
		want   string
	}{
		{[]int{3, 3, 3, 3}, ""},
		{[]int{2, 2, 0, 0}, "(+2 hits not shown, 2 condensed; raise --budget)"},
		{[]int{2, 2, 2, 2}, "(+4 condensed; raise --budget)"},
		{[]int{3, 3, 3, 0}, "(+1 hits not shown; raise --budget)"},
	} {
		got := compose(hits, tc.levels)
		if tc.want == "" {
			if strings.Contains(got, "raise --budget") {
				t.Errorf("%v: unexpected tail in %q", tc.levels, got)
			}
			continue
		}
		if !strings.HasSuffix(got, "\n\n"+tc.want) {
			t.Errorf("%v: tail of %q, want %q", tc.levels, got, tc.want)
		}
	}
}

func TestRenderEmptyAndNegative(t *testing.T) {
	if r := Render(nil, 100); r.Text != "" || len(r.Levels) != 0 {
		t.Errorf("no hits: %+v", r)
	}
	if got, want := Render(fixture(), -5).Text, Render(fixture(), 0).Text; got != want {
		t.Errorf("a negative budget must act as unlimited")
	}
}

func TestLevelThreeNeverShorterThanTwo(t *testing.T) {
	h := Hit{Rank: 1, Path: "a.md", Summary: strings.Repeat("long summary ", 10), Text: "short"}
	if block(h, 3) != block(h, 2) {
		t.Errorf("level 3 should fall back to the longer summary block:\n%q", block(h, 3))
	}
}

func TestHeaderShapes(t *testing.T) {
	for _, tc := range []struct {
		h    Hit
		want string
	}{
		{Hit{Rank: 2, Path: "a.md"}, "[2] a.md"},
		{Hit{Rank: 2, Path: "a.md", Heading: "H"}, "[2] a.md › H"},
		{Hit{Rank: 2, Path: "a.md", Updated: "2026-01-02"}, "[2] a.md · updated 2026-01-02"},
		{Hit{Rank: 2, Path: "a.md", Heading: "H", Updated: "2026-01-02"}, "[2] a.md › H · updated 2026-01-02"},
	} {
		if got := header(tc.h); got != tc.want {
			t.Errorf("header = %q, want %q", got, tc.want)
		}
	}
}

func randomHits(r *rand.Rand) []Hit {
	words := func(n int) string {
		var b strings.Builder
		for i := 0; i < n; i++ {
			b.WriteString(strings.Repeat("w", 1+r.Intn(9)) + " ")
		}
		return strings.TrimSpace(b.String())
	}
	hits := make([]Hit, r.Intn(9))
	for i := range hits {
		hits[i] = Hit{Rank: i + 1, Path: "data/" + words(1) + ".md", Text: words(r.Intn(200))}
		if r.Intn(2) == 0 {
			hits[i].Heading = words(1 + r.Intn(3))
		}
		if r.Intn(2) == 0 {
			hits[i].Summary = words(r.Intn(80))
		}
		if r.Intn(2) == 0 {
			hits[i].Updated = "2026-01-02"
		}
	}
	return hits
}

// TestRenderProperties: for random hits, the output never exceeds the budget, and as the budget
// grows no hit loses detail and the shown-hit count never falls.
func TestRenderProperties(t *testing.T) {
	r := rand.New(rand.NewSource(7))
	for iter := 0; iter < 200; iter++ {
		hits := randomHits(r)
		var prev []int
		prevTokens := -1
		for b := 1; b <= 1500; b += 1 + r.Intn(40) {
			res := Render(hits, b)
			if res.Tokens > b || EstimateTokens(res.Text) > b {
				t.Fatalf("iter %d budget %d: %d tokens exceed it", iter, b, res.Tokens)
			}
			for i := range prev {
				if res.Levels[i] < prev[i] {
					t.Fatalf("iter %d: budget %d lowered hit %d from level %d to %d", iter, b, i, prev[i], res.Levels[i])
				}
			}
			if res.Tokens < prevTokens {
				t.Fatalf("iter %d: budget %d used %d tokens, fewer than the %d a smaller budget used", iter, b, res.Tokens, prevTokens)
			}
			prev, prevTokens = res.Levels, res.Tokens
		}
		// A budget at least the unlimited size renders the unlimited text.
		unl := Render(hits, 0)
		for i, lv := range unl.Levels {
			if lv != levelFull {
				t.Fatalf("iter %d: unlimited rendered hit %d at level %d, want %d", iter, i, lv, levelFull)
			}
		}
		if got := Render(hits, unl.Tokens).Text; got != unl.Text {
			t.Fatalf("iter %d: budget %d (exact fit) differs from unlimited", iter, unl.Tokens)
		}
	}
}

func FuzzRender(f *testing.F) {
	f.Add("data/a.md", "H", "sum", "text body here", 10, 3)
	f.Add("", "", "", "", 1, 0)
	f.Add("é.md", "日本", "日本語", strings.Repeat("日", 700), 40, 5)
	f.Fuzz(func(t *testing.T, path, heading, summary, text string, budget, n int) {
		if n < 0 || n > 12 {
			n = n % 12
			if n < 0 {
				n = -n
			}
		}
		hits := make([]Hit, n)
		for i := range hits {
			hits[i] = Hit{Rank: i + 1, Path: path, Heading: heading, Summary: summary, Text: text}
		}
		res := Render(hits, budget)
		if budget > 0 && res.Tokens > budget {
			t.Fatalf("tokens %d exceed budget %d", res.Tokens, budget)
		}
		if budget > 0 {
			if bigger := Render(hits, budget+1+budget/3); sum(bigger.Levels) < sum(res.Levels) {
				t.Fatalf("a larger budget lowered total detail: %v -> %v", res.Levels, bigger.Levels)
			}
		}
	})
}

func sum(a []int) (s int) {
	for _, v := range a {
		s += v
	}
	return
}

func TestTrim(t *testing.T) {
	t.Run("under limit is unchanged", func(t *testing.T) {
		s := strings.Repeat("a", 600)
		if got := Trim(s, 600); got != s {
			t.Errorf("Trim changed a string at exactly the limit")
		}
	})

	t.Run("cuts at the last space before the limit", func(t *testing.T) {
		s := strings.Repeat("a", 595) + " " + strings.Repeat("b", 10) // len 606
		want := strings.Repeat("a", 595) + " …"
		if got := Trim(s, 600); got != want {
			t.Errorf("Trim(...) = %q, want %q", got, want)
		}
	})

	t.Run("hard cut when there is no space", func(t *testing.T) {
		s := strings.Repeat("a", 700)
		got := Trim(s, 600)
		want := strings.Repeat("a", 600) + " …"
		if got != want {
			t.Errorf("Trim(...) = %q, want %q", got, want)
		}
	})

	t.Run("does not backtrack more than eighty runes", func(t *testing.T) {
		s := "See " + strings.Repeat("x", 700)
		want := "See " + strings.Repeat("x", 596) + " …"
		if got := Trim(s, 600); got != want {
			t.Errorf("Trim(...) = %q, want a hard cut %q", got, want)
		}
	})

	t.Run("multibyte text respects rune boundaries", func(t *testing.T) {
		s := strings.Repeat("é", 700) // each é is one rune, two UTF-8 bytes
		got := Trim(s, 600)
		if !strings.HasSuffix(got, " …") {
			t.Fatalf("Trim did not mark the cut: %q", got)
		}
		body := strings.TrimSuffix(got, " …")
		if n := len([]rune(body)); n != 600 {
			t.Errorf("trimmed body has %d runes, want 600", n)
		}
		// Every rune must still be a valid é, i.e. no byte was sliced out of the middle of one.
		for _, r := range body {
			if r != 'é' {
				t.Fatalf("Trim corrupted a multibyte rune: got %q in body", r)
			}
		}
	})

	t.Run("empty text", func(t *testing.T) {
		if got := Trim("", 600); got != "" {
			t.Errorf("Trim(\"\", 600) = %q, want \"\"", got)
		}
	})

	t.Run("a space exactly eighty runes before the limit is used", func(t *testing.T) {
		s := strings.Repeat("a", 520) + " " + strings.Repeat("b", 100) // space at index 520 = limit-80
		want := strings.Repeat("a", 520) + " …"
		if got := Trim(s, 600); got != want {
			t.Errorf("Trim(...) = %q, want %q", got, want)
		}
	})

	t.Run("a space eighty-one runes before the limit is not used", func(t *testing.T) {
		s := strings.Repeat("a", 519) + " " + strings.Repeat("b", 100) // space at index 519 = limit-81
		want := strings.Repeat("a", 519) + " " + strings.Repeat("b", 80) + " …"
		if got := Trim(s, 600); got != want {
			t.Errorf("Trim(...) = %q, want a hard cut %q", got, want)
		}
	})

	t.Run("cuts at the last newline before the limit", func(t *testing.T) {
		s := strings.Repeat("a", 590) + "\n" + strings.Repeat("b", 20)
		want := strings.Repeat("a", 590) + " …"
		if got := Trim(s, 600); got != want {
			t.Errorf("Trim(...) = %q, want %q", got, want)
		}
	})

	t.Run("limit under eighty cuts at the last space", func(t *testing.T) {
		if got, want := Trim("ab cdefgh", 5), "ab …"; got != want {
			t.Errorf("Trim(\"ab cdefgh\", 5) = %q, want %q", got, want)
		}
	})

	t.Run("limit under eighty with no space hard cuts", func(t *testing.T) {
		if got, want := Trim("abcdefgh", 5), "abcde …"; got != want {
			t.Errorf("Trim(\"abcdefgh\", 5) = %q, want %q", got, want)
		}
	})
}

// TestLimitsPinned pins the rune caps of the render levels; a change here changes every
// rendered block, so it must be a deliberate edit of this test too.
func TestLimitsPinned(t *testing.T) {
	if TextLimit != 600 {
		t.Errorf("TextLimit = %d, want 600", TextLimit)
	}
	if SummaryLimit != 300 {
		t.Errorf("SummaryLimit = %d, want 300", SummaryLimit)
	}
}
