package reindex

import (
	"bytes"
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/nickstrad/kb/internal/config"
	"github.com/nickstrad/kb/internal/embed/fake"
)

func writeRel(t *testing.T, root, rel, content string) {
	t.Helper()
	p := filepath.Join(root, filepath.FromSlash(rel))
	if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(p, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
}

// TestAllInPlaceSources: files without front matter are indexed with the fallback title and
// summary; a symlink out of the root is warned about and not indexed; removing a file removes its
// entry on the next run; a store with no sources warns and prunes everything.
func TestAllInPlaceSources(t *testing.T) {
	root, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	writeRel(t, root, "README.md", "# kb\n\nInstall kb with go install.\n\n## Usage\n\nRun it.\n")
	writeRel(t, root, "docs/plan.md", "No heading here, just a plan.\n")
	writeRel(t, root, "data/a.md", "---\ntitle: A\nsummary: S\n---\n## X\n\nbody\n")
	outside := t.TempDir()
	writeRel(t, outside, "secret.md", "# Secret\n")
	if err := os.Symlink(filepath.Join(outside, "secret.md"), filepath.Join(root, "docs", "leak.md")); err != nil {
		t.Fatal(err)
	}
	sources := []config.Source{
		{Path: "data", Layout: config.LayoutEntries},
		{Glob: "README.md", Layout: config.LayoutFiles},
		{Glob: "docs/**/*.md", Layout: config.LayoutFiles},
	}
	base := fake.New("fake-embed", testDim)
	s := newTestStore(t, base)
	ctx := context.Background()
	run := func(srcs []config.Source) (Summary, string, string) {
		t.Helper()
		var stdout, stderr bytes.Buffer
		sum, err := All(ctx, Options{Root: root, Sources: srcs, Store: s, Embedder: base, Stdout: &stdout, Stderr: &stderr})
		if err != nil {
			t.Fatalf("All: %v", err)
		}
		return sum, stdout.String(), stderr.String()
	}

	sum, _, stderr := run(sources)
	if sum.Reindexed != 3 || sum.Failed != 0 {
		t.Fatalf("summary %+v, want 3 reindexed, 0 failed; stderr:\n%s", sum, stderr)
	}
	if !strings.Contains(stderr, "warning: skipped docs/leak.md: symlink to") {
		t.Errorf("stderr lacks the symlink warning:\n%s", stderr)
	}
	row, err := s.Entry(ctx, "README.md")
	if err != nil || row == nil || row.Title != "kb" || row.Summary != "Install kb with go install." || row.Kind != "file" {
		t.Fatalf("README.md row = %+v, %v", row, err)
	}
	chunks, err := s.Chunks(ctx, row.ID)
	if err != nil || len(chunks) != 3 || chunks[0].Text != "kb: Install kb with go install." || !strings.HasPrefix(chunks[2].Text, "kb › Usage\n") {
		t.Fatalf("README.md chunks = %+v, %v", chunks, err)
	}
	if row, _ := s.Entry(ctx, "docs/plan.md"); row == nil || row.Title != "Plan" {
		t.Errorf("docs/plan.md row = %+v, want the humanised title", row)
	}
	if row, _ := s.Entry(ctx, "docs/leak.md"); row != nil {
		t.Errorf("the out-of-root symlink was indexed: %+v", row)
	}

	// Removing a file removes its entry.
	if err := os.Remove(filepath.Join(root, "docs", "plan.md")); err != nil {
		t.Fatal(err)
	}
	sum, stdout, _ := run(sources)
	if sum.Removed != 1 || !strings.Contains(stdout, "removed docs/plan.md (no longer on disk)") {
		t.Errorf("after rm: %+v\n%s", sum, stdout)
	}

	// No sources: a warning, and the derived index follows the (empty) corpus.
	sum, _, stderr = run(nil)
	if sum.Scanned != 0 || sum.Removed != 2 || !strings.Contains(stderr, "no [[sources]]") {
		t.Errorf("no sources: %+v, stderr %q", sum, stderr)
	}
	rows, err := s.Entries(ctx)
	if err != nil || len(rows) != 0 {
		t.Errorf("rows after a run with no sources = %v, %v", rows, err)
	}
}

// TestAllOverlappingSources: two sources claiming one file abort the run before anything is
// written, as a Usage error naming both.
func TestAllOverlappingSources(t *testing.T) {
	root := t.TempDir()
	writeRel(t, root, "docs/a.md", "# A\n")
	base := fake.New("fake-embed", testDim)
	s := newTestStore(t, base)
	_, err := All(context.Background(), Options{Root: root, Store: s, Embedder: base, Sources: []config.Source{
		{Glob: "docs/*.md", Layout: config.LayoutFiles}, {Glob: "**/a.md", Layout: config.LayoutFiles},
	}})
	if err == nil || !strings.Contains(err.Error(), "both match docs/a.md") {
		t.Fatalf("All = %v, want the overlap error", err)
	}
	if rows, _ := s.Entries(context.Background()); len(rows) != 0 {
		t.Errorf("rows written before the overlap error: %v", rows)
	}
}
