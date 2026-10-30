//go:build e2e

package e2e

import (
	"os/exec"
	"path/filepath"
	"sort"
	"strings"
	"testing"
)

// auditSkip lists leaf commands that appear in no journey, each with the reason. A stale entry
// (the command is covered after all, or no longer exists) fails the test.
var auditSkip = map[string]string{
	"help":       "cobra's built-in; its output is the reference text, owned by the cli golden tests",
	"completion": "cobra's generated shell scripts; the subcommands are bash, fish, powershell and zsh",
	"model pull": "needs the network; the nightly model job pulls a real model",
}

// helpChildren reads the "Available Commands:" section of `kb <path> --help`.
func helpChildren(t *testing.T, kb, path string) []string {
	t.Helper()
	args := append(strings.Fields(path), "--help")
	cmd := exec.Command(kb, args...)
	cmd.Env = []string{"HOME=" + t.TempDir(), "PATH="}
	out, err := cmd.Output()
	if err != nil {
		t.Fatalf("kb %s: %v", strings.Join(args, " "), err)
	}
	var kids []string
	in := false
	for _, ln := range strings.Split(string(out), "\n") {
		switch {
		case ln == "Available Commands:":
			in = true
		case in && strings.TrimSpace(ln) == "":
			in = false
		case in:
			kids = append(kids, strings.Fields(ln)[0])
		}
	}
	return kids
}

// commandTree walks `kb --help` down to the leaves. tree maps a path ("" is the root) to its
// children; leaves lists the paths with none.
func commandTree(t *testing.T, kb string) (tree map[string][]string, leaves []string) {
	t.Helper()
	tree = map[string][]string{}
	var walk func(path string)
	walk = func(path string) {
		kids := helpChildren(t, kb, path)
		tree[path] = kids
		if len(kids) == 0 && path != "" {
			leaves = append(leaves, path)
		}
		for _, k := range kids {
			child := k
			if path != "" {
				child = path + " " + k
			}
			walk(child)
		}
	}
	walk("")
	sort.Strings(leaves)
	return tree, leaves
}

// TestEveryCommandHasAJourney fails when a leaf command of the real command tree appears as
// "$ kb <command> ..." in no journey (PLAN E5, TESTING §11.7).
func TestEveryCommandHasAJourney(t *testing.T) {
	kb := kbBinary(t)
	tree, leaves := commandTree(t, kb)
	if len(leaves) < 20 {
		t.Fatalf("found %d leaf commands %v; the help parser is broken", len(leaves), leaves)
	}
	covered := map[string]bool{}
	for _, toks := range allJourneyCommands(t) {
		covered[cmdPath(toks, tree)] = true
	}
	isLeaf := map[string]bool{}
	for _, l := range leaves {
		isLeaf[l] = true
		top := strings.Fields(l)[0]
		_, skipped := auditSkip[l]
		_, topSkipped := auditSkip[top]
		switch {
		case covered[l] && skipped:
			t.Errorf("auditSkip lists %q but a journey covers it; delete the entry", l)
		case !covered[l] && !skipped && !topSkipped:
			t.Errorf("command %q appears in no journey: add a %q line to one (or list it in auditSkip with a reason)", l, "$ kb "+l)
		}
	}
	for s := range auditSkip {
		if !isLeaf[s] && len(tree[s]) == 0 {
			t.Errorf("auditSkip lists %q, which is not a command", s)
		}
	}
}

// allJourneyCommands is the kb commands of every journey file.
func allJourneyCommands(t *testing.T) [][]string {
	t.Helper()
	files, err := filepath.Glob(filepath.Join("testdata", "journeys", "*.txt"))
	if err != nil || len(files) == 0 {
		t.Fatalf("no journeys found: %v", err)
	}
	var out [][]string
	for _, f := range files {
		out = append(out, journeyCommands(t, strings.TrimSuffix(filepath.Base(f), ".txt"))...)
	}
	return out
}
