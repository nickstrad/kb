package cli

import (
	"bytes"
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"knowledge/kb/internal/embed"
	"knowledge/kb/internal/embed/fake"
	"knowledge/kb/internal/store"
)

// useFakeEmbedder swaps the package-level newEmbedder hook (internal/cli/setup.go) for a
// deterministic fake, restoring it after the test. Its dimension must equal store.VecDim, the
// fixed width of the schema's chunks_vec column.
func useFakeEmbedder(t *testing.T) {
	t.Helper()
	orig := newEmbedder
	newEmbedder = func() embed.Embedder { return fake.New("fake-add-test", store.VecDim) }
	t.Cleanup(func() { newEmbedder = orig })
}

// newTempRoot creates a fresh repository root and points KB_ROOT at it for the duration of the
// test.
func newTempRoot(t *testing.T) string {
	t.Helper()
	root := t.TempDir()
	t.Setenv("KB_ROOT", root)
	return root
}

// writeFile writes content to path, creating parent directories as needed.
func writeFile(t *testing.T, path, content string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatalf("mkdir %s: %v", filepath.Dir(path), err)
	}
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatalf("write %s: %v", path, err)
	}
}

// run invokes the whole kb command tree once, the way main() does, and captures its output and
// exit code.
func run(t *testing.T, args ...string) (stdout, stderr string, code int) {
	t.Helper()
	var out, errBuf bytes.Buffer
	code = Execute(context.Background(), args, &out, &errBuf)
	return out.String(), errBuf.String(), code
}

// validFrontMatter is a minimal but valid entry body: title, summary, tags, updated, and one
// non-empty "##" section, per plan.md's chunking rules.
func validFrontMatter(title, summary string) string {
	return "---\n" +
		"title: " + title + "\n" +
		"summary: " + summary + "\n" +
		"tags: [test]\n" +
		"updated: 2026-09-13\n" +
		"---\n\n" +
		"## Section\n\nSome body text for the chunker to index.\n"
}

// openTestStore opens the database kb add just wrote to, for assertions.
func openTestStore(t *testing.T, root string) *store.Store {
	t.Helper()
	st, err := store.Open(DBPath(root))
	if err != nil {
		t.Fatalf("store.Open: %v", err)
	}
	t.Cleanup(func() { st.Close() })
	return st
}

func TestAddFileOutsideRootIsCopiedAndIndexed(t *testing.T) {
	useFakeEmbedder(t)
	root := newTempRoot(t)
	scratch := t.TempDir()
	src := filepath.Join(scratch, "kb-add-outside-test.md")
	writeFile(t, src, validFrontMatter("KB add outside test", "Covers add of a file from outside the repo root."))

	out, errOut, code := run(t, "add", src)
	if code != ExitOK {
		t.Fatalf("exit = %d, want %d; stdout=%q stderr=%q", code, ExitOK, out, errOut)
	}
	if !strings.Contains(out, "added data/kb-add-outside-test.md") {
		t.Errorf("stdout = %q, want an \"added ...\" line", out)
	}
	if _, err := os.Stat(filepath.Join(root, "data", "kb-add-outside-test.md")); err != nil {
		t.Fatalf("file was not copied into the repo: %v", err)
	}

	st := openTestStore(t, root)
	e, err := st.GetEntryByPath("data/kb-add-outside-test.md")
	if err != nil {
		t.Fatalf("GetEntryByPath: %v", err)
	}
	if e == nil {
		t.Fatal("entry was not indexed")
	}
	chunks, err := st.ListChunks(e.ID)
	if err != nil {
		t.Fatalf("ListChunks: %v", err)
	}
	if len(chunks) == 0 {
		t.Error("want > 0 chunks, got 0")
	}
}

func TestAddBadNameIsRejected(t *testing.T) {
	useFakeEmbedder(t)
	root := newTempRoot(t)
	scratch := t.TempDir()
	src := filepath.Join(scratch, "Bad_Name.md")
	writeFile(t, src, validFrontMatter("Bad name test", "Should be rejected before anything is read or copied."))

	_, errOut, code := run(t, "add", src)
	if code != ExitUsage {
		t.Fatalf("exit = %d, want %d; stderr=%q", code, ExitUsage, errOut)
	}
	if !strings.Contains(errOut, "lowercase kebab-case") {
		t.Errorf("stderr = %q, want it to name the naming rule", errOut)
	}
	if _, err := os.Stat(filepath.Join(root, "data", "Bad_Name.md")); !os.IsNotExist(err) {
		t.Errorf("bad-name file should not have been copied (stat err = %v)", err)
	}
}

func TestAddMissingSummaryIsRejected(t *testing.T) {
	useFakeEmbedder(t)
	root := newTempRoot(t)
	scratch := t.TempDir()
	src := filepath.Join(scratch, "no-summary.md")
	writeFile(t, src, "---\ntitle: No summary\ntags: [test]\n---\n\n## Section\n\nbody\n")

	_, errOut, code := run(t, "add", src)
	if code != ExitUsage {
		t.Fatalf("exit = %d, want %d; stderr=%q", code, ExitUsage, errOut)
	}
	if !strings.Contains(errOut, "summary") {
		t.Errorf("stderr = %q, want it to mention summary", errOut)
	}
	if _, err := os.Stat(filepath.Join(root, "data", "no-summary.md")); !os.IsNotExist(err) {
		t.Errorf("file should not have been copied (stat err = %v)", err)
	}
}

func TestAddSecretPatternIsRejected(t *testing.T) {
	useFakeEmbedder(t)
	root := newTempRoot(t)
	scratch := t.TempDir()
	src := filepath.Join(scratch, "has-secret.md")
	body := validFrontMatter("Has secret", "Contains a fake secret for the scanner to catch.") +
		"\npassword=example-not-real\n"
	writeFile(t, src, body)

	_, errOut, code := run(t, "add", src)
	if code != ExitUsage {
		t.Fatalf("exit = %d, want %d; stderr=%q", code, ExitUsage, errOut)
	}
	if !strings.Contains(errOut, "password") {
		t.Errorf("stderr = %q, want it to name the password pattern", errOut)
	}
	if _, err := os.Stat(filepath.Join(root, "data", "has-secret.md")); !os.IsNotExist(err) {
		t.Errorf("file with a secret should not have been copied (stat err = %v)", err)
	}
}

func TestAddDuplicateIsRejected(t *testing.T) {
	useFakeEmbedder(t)
	root := newTempRoot(t)
	// Hand-drafted directly in the repo root: valid, not yet in the DB.
	dest := filepath.Join(root, "data", "hand-drafted.md")
	writeFile(t, dest, validFrontMatter("Hand drafted", "Already sitting in the repo before kb add registers it."))

	if _, errOut, code := run(t, "add", dest); code != ExitOK {
		t.Fatalf("first add: exit = %d, want %d; stderr=%q", code, ExitOK, errOut)
	}

	_, errOut, code := run(t, "add", dest)
	if code != ExitUsage {
		t.Fatalf("second add: exit = %d, want %d; stderr=%q", code, ExitUsage, errOut)
	}
	if !strings.Contains(errOut, "already indexed") {
		t.Errorf("stderr = %q, want \"already indexed\"", errOut)
	}
}

func TestAddDirWithReadmeAndDocsIsIndexed(t *testing.T) {
	useFakeEmbedder(t)
	root := newTempRoot(t)
	scratch := t.TempDir()
	topic := filepath.Join(scratch, "sample-topic")
	writeFile(t, filepath.Join(topic, "README.md"),
		validFrontMatter("Sample topic", "A directory entry with a docs file and a script."))
	writeFile(t, filepath.Join(topic, "docs", "extra.md"),
		"# Extra\n\nMore detail. No front matter is required in a docs file.\n")
	writeFile(t, filepath.Join(topic, "scripts", "run.sh"),
		"#!/usr/bin/env bash\nset -euo pipefail\necho hi\n")

	out, errOut, code := run(t, "add", "--dir", topic)
	if code != ExitOK {
		t.Fatalf("exit = %d, want %d; stdout=%q stderr=%q", code, ExitOK, out, errOut)
	}
	if !strings.Contains(out, "added data/sample-topic/README.md") {
		t.Errorf("stdout = %q, want an \"added ...\" line", out)
	}

	st := openTestStore(t, root)
	e, err := st.GetEntryByPath("data/sample-topic/README.md")
	if err != nil {
		t.Fatalf("GetEntryByPath: %v", err)
	}
	if e == nil {
		t.Fatal("directory entry was not indexed")
	}
	if e.Kind != "dir" {
		t.Errorf("Kind = %q, want \"dir\"", e.Kind)
	}
	if _, err := os.Stat(filepath.Join(root, "data", "sample-topic", "scripts", "run.sh")); err != nil {
		t.Errorf("scripts/run.sh should have been copied into the repo: %v", err)
	}
	if _, err := os.Stat(filepath.Join(root, "data", "sample-topic", "docs", "extra.md")); err != nil {
		t.Errorf("docs/extra.md should have been copied into the repo: %v", err)
	}
}

func TestAddDirWithoutReadmeIsRejected(t *testing.T) {
	useFakeEmbedder(t)
	_ = newTempRoot(t)
	scratch := t.TempDir()
	topic := filepath.Join(scratch, "no-readme-topic")
	writeFile(t, filepath.Join(topic, "docs", "x.md"), "there is no README.md next to this file\n")

	_, errOut, code := run(t, "add", "--dir", topic)
	if code != ExitUsage {
		t.Fatalf("exit = %d, want %d; stderr=%q", code, ExitUsage, errOut)
	}
	if !strings.Contains(errOut, "README.md") {
		t.Errorf("stderr = %q, want it to mention README.md", errOut)
	}
}

func TestAddFileInsideRootSubdirectoryIsRejected(t *testing.T) {
	useFakeEmbedder(t)
	root := newTempRoot(t)
	nested := filepath.Join(root, "data", "sub", "nested.md")
	writeFile(t, nested, validFrontMatter("Nested", "Sits in a subdirectory of the repo root, which is not allowed."))

	_, errOut, code := run(t, "add", nested)
	if code != ExitUsage {
		t.Fatalf("exit = %d, want %d; stderr=%q", code, ExitUsage, errOut)
	}
	if !strings.Contains(errOut, "subdirectory") {
		t.Errorf("stderr = %q, want it to mention the subdirectory rule", errOut)
	}
}
