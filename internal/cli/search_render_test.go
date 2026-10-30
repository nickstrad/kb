package cli

import (
	"regexp"
	"strings"
	"testing"

	"github.com/nickstrad/kb/internal/render"
)

// TestSearchBudgetElastic: --budget shrinks the human output (tail line, no chunk text), the
// first line and footer stay, [output] budget is the default, and the flag overrides it.
func TestSearchBudgetElastic(t *testing.T) {
	t.Parallel()
	root := newSearchTestRepo(t, testModel768, testDim768)
	useRoot(t, root)

	full, stderr, exit := runKB(t, []string{"search", "widget", "--mode", "fts"})
	if exit != 0 {
		t.Fatalf("exit = %d; stderr=%s", exit, stderr)
	}
	if !strings.Contains(full, "details about configuring the widget") || strings.Contains(full, "raise --budget") {
		t.Fatalf("unlimited output should carry chunk text and no tail:\n%s", full)
	}
	if regexp.MustCompile(`\b0\.\d{4}\b`).MatchString(full) {
		t.Errorf("human output carries the RRF score:\n%s", full)
	}

	small, _, exit := runKB(t, []string{"search", "widget", "--mode", "fts", "--budget", "25"})
	if exit != 0 {
		t.Fatalf("exit = %d", exit)
	}
	if !strings.HasPrefix(small, "search ") || !strings.Contains(small, "search_id=") ||
		!strings.Contains(small, "raise --budget") || strings.Contains(small, "details about configuring") {
		t.Errorf("--budget 25 output is not condensed with a tail:\n%s", small)
	}
	// Body = everything between the first line and the footer; it must fit the budget.
	body := bodyOf(small)
	if got := render.EstimateTokens(body); got > 25 {
		t.Errorf("body is %d tokens, over --budget 25:\n%s", got, body)
	}

	writeConfig(t, root, "[embed]\nmodel = \""+testModel768+"\"\n[output]\nbudget = 25\n")
	fromConfig, _, _ := runKB(t, []string{"search", "widget", "--mode", "fts"})
	if bodyOf(fromConfig) != bodyOf(small) {
		t.Errorf("[output] budget = 25 differs from --budget 25:\n%s\nvs\n%s", fromConfig, small)
	}
	override, _, _ := runKB(t, []string{"search", "widget", "--mode", "fts", "--budget", "0"})
	if !strings.Contains(override, "details about configuring the widget") {
		t.Errorf("--budget 0 did not override [output] budget:\n%s", override)
	}
}

// bodyOf drops the "search N · M hits" line and the search_id footer.
func bodyOf(s string) string {
	lines := strings.Split(strings.TrimRight(s, "\n"), "\n")
	return strings.Join(lines[1:len(lines)-1], "\n")
}

func TestSearchNegativeBudgetIsUsageError(t *testing.T) {
	t.Parallel()
	root := newSearchTestRepo(t, testModel768, testDim768)
	useRoot(t, root)
	stdout, stderr, exit := runKB(t, []string{"search", "widget", "--mode", "fts", "--budget", "-1"})
	if exit != 1 || stdout != "" || !strings.Contains(stderr, "--budget") {
		t.Errorf("exit %d stdout %q stderr %q; want exit 1, no stdout, an error naming --budget", exit, stdout, stderr)
	}
}
