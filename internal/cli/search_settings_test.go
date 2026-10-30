package cli

import (
	"strings"
	"testing"
)

// TestSearchUsesConfigSettings: [search] reaches the searcher. With rrf_k = 1 the top FTS hit
// scores exactly 1/(1+1); with the default rrf_k it would print 0.0164.
func TestSearchUsesConfigSettings(t *testing.T) {
	root := newSearchTestRepo(t, testModel768, testDim768)
	useRoot(t, root)
	writeConfig(t, root, "[embed]\nmodel = \""+testModel768+"\"\n[search]\nrrf_k = 1\n")

	stdout, stderr, exit := runKB(t, []string{"search", "widget", "--mode", "fts"})
	if exit != 0 {
		t.Fatalf("exit = %d, want 0; stderr=%s", exit, stderr)
	}
	if !strings.Contains(stdout, "#1  0.5000  ") {
		t.Errorf("top hit does not score 1/(rrf_k+1) = 0.5000 with rrf_k = 1:\n%s", stdout)
	}
}
