package cli

import (
	"bytes"
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/nickstrad/kb/internal/entry"
	"github.com/nickstrad/kb/internal/store/sqlite/sqlitetest"
)

func TestShowChunksReportsDatabaseStatFailure(t *testing.T) {
	t.Parallel()
	root := newTempRoot(t)
	writeFile(t, filepath.Join(root, "data", "show-topic.md"), validFrontMatter("Show topic", "Stat errors must surface."))
	if err := os.Symlink("kb.sqlite", testDBPath(root)); err != nil {
		t.Fatal(err)
	}
	out, errOut, code := run(t, "show", "show-topic.md", "--chunks")
	if code != 1 || strings.Contains(errOut, "not indexed") || !strings.Contains(errOut, "kb show:") {
		t.Fatalf("%d %q %q", code, out, errOut)
	}
}

func TestPrintChunkCountsUnicodeCharacters(t *testing.T) {
	t.Parallel()
	var b bytes.Buffer
	printChunk(&b, 0, "a.md", "", "é—界", "0123456789abcdef")
	if !strings.Contains(b.String(), "(3 chars, 0123456789ab)") {
		t.Fatal(b.String())
	}
}

func TestRmPreservesSourceWhenDatabaseCannotOpen(t *testing.T) {
	t.Parallel()
	for _, dir := range []bool{false, true} {
		t.Run(map[bool]string{false: "file", true: "dir"}[dir], func(t *testing.T) {
			root := newTempRoot(t)
			name := "rm-topic.md"
			path := filepath.Join(root, "data", name)
			args := []string{"rm", name}
			if dir {
				name = "rm-topic"
				path = filepath.Join(root, "data", name, "README.md")
				args = []string{"rm", name, "--yes"}
			}
			writeFile(t, path, validFrontMatter("Rm topic", "Keep source on preflight failure."))
			writeFile(t, testDBPath(root), "corrupt sqlite database")
			out, errOut, code := run(t, args...)
			if code != 1 || strings.Contains(out, "removed") {
				t.Fatalf("%d %q %q", code, out, errOut)
			}
			if _, err := os.Stat(path); err != nil {
				t.Fatalf("source destroyed: %v", err)
			}
		})
	}
}

func TestEditModelMismatchUsesEditPrefix(t *testing.T) {
	t.Parallel()
	useFakeEmbedder(t)
	root := newTempRoot(t)
	writeFile(t, filepath.Join(root, "data", "edit-topic.md"), validFrontMatter("Edit topic", "Original summary."))
	if _, errOut, code := run(t, "reindex", "--all"); code != 0 {
		t.Fatalf("%d %q", code, errOut)
	}
	st := openTestStore(t, root)
	sqlitetest.ForceEmbedModel(t, st, "other")
	useFakeEditor(t, "Original summary", "Changed summary")
	_, errOut, code := run(t, "edit", "edit-topic.md")
	if code != 1 || !strings.HasPrefix(errOut, "kb edit:") || strings.HasPrefix(errOut, "kb reindex:") {
		t.Fatalf("%d %q", code, errOut)
	}
	if !strings.Contains(errOut, "kb reindex --all") || strings.Contains(errOut, "$EDITOR") {
		t.Fatalf("want the model-mismatch error, not an editor failure: %q", errOut)
	}
	b, err := os.ReadFile(filepath.Join(root, "data", "edit-topic.md"))
	if err != nil || !strings.Contains(string(b), "Changed summary") {
		t.Fatalf("editor did not change the file: %v %q", err, b)
	}
}

// TestEditValidationErrorKeepsChain: an edit that leaves the front matter invalid returns the
// entry's *ValidationError in the error chain (so --debug and errors.As see it), as a Usage error.
func TestEditValidationErrorKeepsChain(t *testing.T) {
	t.Parallel()
	useFakeEmbedder(t)
	root := newTempRoot(t)
	writeFile(t, filepath.Join(root, "data", "edit-bad.md"), validFrontMatter("Edit bad", "Original summary."))
	orig := depsFor(t).editor
	t.Cleanup(func() { depsFor(t).editor = orig })
	depsFor(t).editor = func(_ context.Context, _, path string) error {
		// No summary: ParseFrontMatter reports a *ValidationError.
		return os.WriteFile(path, []byte("---\ntitle: Edit bad\n---\n\nbody\n"), 0o644)
	}
	var out, errBuf bytes.Buffer
	err := runEdit(context.Background(), nil, depsFor(t), &out, &errBuf, testWS(root), testConfig(), "edit-bad.md")
	var ve *entry.ValidationError
	if !errors.As(err, &ve) {
		t.Fatalf("runEdit = %v, want the *entry.ValidationError in its chain", err)
	}
}
