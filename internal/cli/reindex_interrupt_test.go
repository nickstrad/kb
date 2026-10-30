package cli

import (
	"bytes"
	"context"
	"path/filepath"
	"strings"
	"testing"
)

// TestReindexInterrupted is Q21: a reindex whose context is cancelled (SIGINT/SIGTERM via main's
// NotifyContext) exits 1 with one friendly "interrupted … rerun" line instead of the driver's
// "context canceled" from whichever statement was running. The cancel is in place before the run
// starts, so the first entry's write is what sees it: deterministic, no signal or sleep needed.
func TestReindexInterrupted(t *testing.T) {
	root := newTempRoot(t)
	useNoneModel(t, root)
	writeFile(t, filepath.Join(root, "data", "a.md"), validFrontMatter("A", "a summary"))

	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	var out, errBuf bytes.Buffer
	code := execute(ctx, testDeps, []string{"reindex", "--all"}, &out, &errBuf)
	if code != ExitUsage {
		t.Fatalf("exit = %d, want %d; stderr:\n%s", code, ExitUsage, errBuf.String())
	}
	stderr := errBuf.String()
	if !strings.Contains(stderr, "interrupted") || !strings.Contains(stderr, "rerun kb reindex") {
		t.Errorf("stderr = %q; want the interrupted/rerun message", stderr)
	}
	if strings.Contains(stderr, "context canceled") {
		t.Errorf("stderr leaks the driver detail: %q", stderr)
	}
}
