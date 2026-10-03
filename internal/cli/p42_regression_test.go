package cli

import (
	"bytes"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestShowChunksReportsDatabaseStatFailure(t *testing.T) {
	root := newTempRoot(t)
	writeFile(t, filepath.Join(root, "data", "show-topic.md"), validFrontMatter("Show topic", "Stat errors must surface."))
	if err := os.Mkdir(filepath.Join(root, ".kb"), 0755); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink("kb.sqlite", DBPath(root)); err != nil {
		t.Fatal(err)
	}
	out, errOut, code := run(t, "show", "show-topic.md", "--chunks")
	if code != 1 || strings.Contains(errOut, "not indexed") || !strings.Contains(errOut, "kb show:") {
		t.Fatalf("%d %q %q", code, out, errOut)
	}
}

func TestPrintChunkCountsUnicodeCharacters(t *testing.T) {
	var b bytes.Buffer
	printChunk(&b, 0, "a.md", "", "é—界", "0123456789abcdef")
	if !strings.Contains(b.String(), "(3 chars, 0123456789ab)") {
		t.Fatal(b.String())
	}
}

func TestRmPreservesSourceWhenDatabaseCannotOpen(t *testing.T) {
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
			writeFile(t, DBPath(root), "corrupt sqlite database")
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
	useFakeEmbedder(t)
	root := newTempRoot(t)
	writeFile(t, filepath.Join(root, "data", "edit-topic.md"), validFrontMatter("Edit topic", "Original summary."))
	if _, errOut, code := run(t, "reindex", "--all"); code != 0 {
		t.Fatalf("%d %q", code, errOut)
	}
	st := openTestStore(t, root)
	if _, err := st.DB().Exec("UPDATE embed_meta SET model='other'"); err != nil {
		t.Fatal(err)
	}
	t.Setenv("EDITOR", "sed -i 's/Original summary/Changed summary/'")
	_, errOut, code := run(t, "edit", "edit-topic.md")
	if code != 1 || !strings.HasPrefix(errOut, "kb edit:") || strings.HasPrefix(errOut, "kb reindex:") {
		t.Fatalf("%d %q", code, errOut)
	}
}
