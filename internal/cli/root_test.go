package cli

import (
	"strings"
	"testing"
)

// TestRootHelpMentionsShowAndOptionalFeedback: kb help is what agents learn kb from, so it must
// teach the search → show loop and present feedback as optional, not as a required step.
func TestRootHelpMentionsShowAndOptionalFeedback(t *testing.T) {
	out, errOut, code := run(t, "help")
	if code != 0 {
		t.Fatalf("kb help: exit %d, stderr %q", code, errOut)
	}
	for _, want := range []string{"kb search \"<query>\"", "kb show <path>", "kb feedback is optional"} {
		if !strings.Contains(out, want) {
			t.Errorf("kb help lacks %q:\n%s", want, out)
		}
	}
	for _, unwanted := range []string{"Judge every search", "Every search is three steps", "skill"} {
		if strings.Contains(out, unwanted) {
			t.Errorf("kb help still contains %q:\n%s", unwanted, out)
		}
	}
}
