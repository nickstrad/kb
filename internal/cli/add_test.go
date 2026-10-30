package cli

import (
	"bytes"
	"context"
	"errors"
	"github.com/nickstrad/kb/internal/store/sqlite"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/nickstrad/kb/internal/config"
	"github.com/nickstrad/kb/internal/embed"
	"github.com/nickstrad/kb/internal/embed/fake"
	"github.com/nickstrad/kb/internal/entry"
	"github.com/nickstrad/kb/internal/errs"
	"github.com/nickstrad/kb/internal/workspace"
)

// useFakeEmbedder swaps the package-level newEmbedder hook (internal/cli/setup.go) for a
// deterministic fake, restoring it after the test.
func useFakeEmbedder(t *testing.T) {
	t.Helper()
	orig := depsFor(t).embedder
	depsFor(t).embedder = func(config.Embed) (embed.Embedder, error) { return fake.New("fake-add-test", 16), nil }
	t.Cleanup(func() { depsFor(t).embedder = orig })
}

// testWS is the workspace of a store root, for tests that call a run function directly.
func testWS(root string) *workspace.Workspace {
	ws, err := workspace.NewLocation(root, "").StoreAt("default")
	if err != nil {
		panic(err)
	}
	return ws
}

// testDBPath is the database file of a store root.
func testDBPath(root string) string { return testWS(root).DBPath }

// ensureKBDir creates the default store's directory <root>/.kb/stores/default.
func ensureKBDir(root string) error { return os.MkdirAll(testWS(root).StoreDir, 0o755) }

// dataSourceTOML is the [[sources]] table kb init writes for the default store: the entries
// directory data/.
const dataSourceTOML = "\n[[sources]]\npath = \"data\"\nlayout = \"entries\"\n"

// writeConfig writes the default store's config.toml, replacing the one newTempRoot made. A body
// with no [[sources]] gets the default store's data/ source appended, as kb init writes it.
func writeConfig(t *testing.T, root, body string) {
	t.Helper()
	if !strings.Contains(body, "[[sources]]") {
		body += dataSourceTOML
	}
	writeFile(t, testWS(root).ConfigPath, body)
}

// useNoneModel makes root an FTS-only store: [embed] model = "none".
func useNoneModel(t *testing.T, root string) {
	t.Helper()
	writeConfig(t, root, "[embed]\nmodel = \"none\"\n")
}

// newTempRoot creates a fresh location root (.kb/kb.toml and a default store config with only the
// data/ source, no database yet), makes it the working directory for the test and returns it with symlinks
// resolved, the form root discovery reports. It moves the working directory root discovery sees, not the process.
func newTempRoot(t *testing.T) string {
	t.Helper()
	return useRoot(t, t.TempDir())
}

// useRoot makes dir the location the CLI discovers for the test: it adds .kb/kb.toml (default
// store "default") and an empty stores/default/config.toml unless the directory already has a
// kb.toml, then chdirs there. It returns dir with symlinks resolved.
func useRoot(t *testing.T, dir string) string {
	t.Helper()
	real, err := filepath.EvalSymlinks(dir)
	if err != nil {
		t.Fatalf("resolve %s: %v", dir, err)
	}
	if _, err := os.Stat(filepath.Join(real, ".kb", "kb.toml")); err != nil {
		makeLocation(t, real, "default")
	}
	chdir(t, real)
	return real
}

// makeLocation writes root/.kb/kb.toml with default_store def and an empty config for that store.
func makeLocation(t *testing.T, root, def string) {
	t.Helper()
	writeFile(t, filepath.Join(root, ".kb", "kb.toml"), "version = 1\ndefault_store = \""+def+"\"\n")
	addStore(t, root, def)
}

// addStore adds a store called name to the location at root, with a config that holds only the
// data/ entries source (every test store shares data/ unless it says otherwise). An existing
// config is kept.
func addStore(t *testing.T, root, name string) {
	t.Helper()
	cfg := filepath.Join(root, ".kb", "stores", name, "config.toml")
	if _, err := os.Stat(cfg); err != nil {
		writeFile(t, cfg, dataSourceTOML)
	}
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
	code = execute(context.Background(), depsFor(t), args, &out, &errBuf)
	return out.String(), errBuf.String(), code
}

// runErr is run that also returns the error the command ended with (nil on success), so a
// decision test asserts its kind and typed cause (wantKind, errors.As) instead of stderr wording.
// Keep one wording test per message family (report_test.go).
func runErr(t *testing.T, args ...string) (stdout, stderr string, code int, err error) {
	t.Helper()
	var out, errBuf bytes.Buffer
	code, err = executeErr(context.Background(), depsFor(t), args, &out, &errBuf)
	return out.String(), errBuf.String(), code, err
}

// wantKind fails unless err is of kind k.
func wantKind(t *testing.T, err error, k errs.Kind) {
	t.Helper()
	if got := errs.KindOf(err); err == nil || got != k {
		t.Fatalf("error kind = %v (err %v), want %v", got, err, k)
	}
}

// validFrontMatter is a minimal but valid entry body: title, summary, tags, updated, and one
// non-empty "##" section, which the chunker requires.
func validFrontMatter(title, summary string) string {
	return "---\n" +
		"title: " + title + "\n" +
		"summary: " + summary + "\n" +
		"tags: [test]\n" +
		"updated: 2026-09-13\n" +
		"---\n\n" +
		"## Section\n\nSome body text for the chunker to index.\n"
}

// useFakeEditor replaces deps.editor with a Go function that replaces from with to in the file
// it is given, so edit tests need no shell or sed (TESTING.md §7).
func useFakeEditor(t *testing.T, from, to string) {
	t.Helper()
	orig := depsFor(t).editor
	depsFor(t).editor = func(_ context.Context, _, path string) error {
		b, err := os.ReadFile(path)
		if err != nil {
			return err
		}
		return os.WriteFile(path, []byte(strings.ReplaceAll(string(b), from, to)), 0o644)
	}
	t.Cleanup(func() { depsFor(t).editor = orig })
}

// openTestStore opens the database kb add just wrote to, for assertions.
func openTestStore(t *testing.T, root string) *sqlite.Store {
	t.Helper()
	st, err := sqlite.Open(context.Background(), testDBPath(root), sqlite.Options{})
	if err != nil {
		t.Fatalf("store.Open: %v", err)
	}
	t.Cleanup(func() { st.Close() })
	return st
}

func TestAddFileOutsideRootIsCopiedAndIndexed(t *testing.T) {
	t.Parallel()
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
	e, err := st.Entry(context.Background(), "data/kb-add-outside-test.md")
	if err != nil {
		t.Fatalf("GetEntryByPath: %v", err)
	}
	if e == nil {
		t.Fatal("entry was not indexed")
	}
	chunks, err := st.Chunks(context.Background(), e.ID)
	if err != nil {
		t.Fatalf("ListChunks: %v", err)
	}
	if len(chunks) == 0 {
		t.Error("want > 0 chunks, got 0")
	}
}

func TestAddBadNameIsRejected(t *testing.T) {
	t.Parallel()
	useFakeEmbedder(t)
	root := newTempRoot(t)
	scratch := t.TempDir()
	src := filepath.Join(scratch, "Bad_Name.md")
	writeFile(t, src, validFrontMatter("Bad name test", "Should be rejected before anything is read or copied."))

	_, errOut, code, err := runErr(t, "add", src)
	if code != ExitUsage {
		t.Fatalf("exit = %d, want %d; stderr=%q", code, ExitUsage, errOut)
	}
	wantKind(t, err, errs.Usage)
	if !strings.Contains(errOut, "lowercase kebab-case") { // the wording test of the naming rule
		t.Errorf("stderr = %q, want it to name the naming rule", errOut)
	}
	if _, err := os.Stat(filepath.Join(root, "data", "Bad_Name.md")); !os.IsNotExist(err) {
		t.Errorf("bad-name file should not have been copied (stat err = %v)", err)
	}
}

func TestAddMissingSummaryIsRejected(t *testing.T) {
	t.Parallel()
	useFakeEmbedder(t)
	root := newTempRoot(t)
	scratch := t.TempDir()
	src := filepath.Join(scratch, "no-summary.md")
	writeFile(t, src, "---\ntitle: No summary\ntags: [test]\n---\n\n## Section\n\nbody\n")

	_, errOut, code, err := runErr(t, "add", src)
	if code != ExitUsage {
		t.Fatalf("exit = %d, want %d; stderr=%q", code, ExitUsage, errOut)
	}
	wantKind(t, err, errs.Usage)
	var ve *entry.ValidationError
	if !errors.As(err, &ve) {
		t.Errorf("error %v does not carry an *entry.ValidationError", err)
	}
	if _, err := os.Stat(filepath.Join(root, "data", "no-summary.md")); !os.IsNotExist(err) {
		t.Errorf("file should not have been copied (stat err = %v)", err)
	}
}

func TestAddSecretPatternIsRejected(t *testing.T) {
	t.Parallel()
	useFakeEmbedder(t)
	root := newTempRoot(t)
	scratch := t.TempDir()
	src := filepath.Join(scratch, "has-secret.md")
	body := validFrontMatter("Has secret", "Contains a fake secret for the scanner to catch.") +
		"\npassword=example-not-real\n"
	writeFile(t, src, body)

	_, errOut, code, err := runErr(t, "add", src)
	if code != ExitUsage {
		t.Fatalf("exit = %d, want %d; stderr=%q", code, ExitUsage, errOut)
	}
	wantKind(t, err, errs.Usage)
	if !strings.Contains(errOut, "password") {
		t.Errorf("stderr = %q, want it to name the password pattern", errOut)
	}
	if strings.Count(errOut, "kb add:") != 1 {
		t.Errorf("stderr = %q, want exactly one \"kb add:\" prefix", errOut)
	}
	if _, err := os.Stat(filepath.Join(root, "data", "has-secret.md")); !os.IsNotExist(err) {
		t.Errorf("file with a secret should not have been copied (stat err = %v)", err)
	}
}

func TestAddDuplicateIsRejected(t *testing.T) {
	t.Parallel()
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
	t.Parallel()
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
	e, err := st.Entry(context.Background(), "data/sample-topic/README.md")
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
	t.Parallel()
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
	t.Parallel()
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

// unusableEmbedder is a fake embedder whose Ping fails, as a missing or broken model does.
type unusableEmbedder struct{ *fake.Embedder }

func (unusableEmbedder) Ping(context.Context) error { return errors.New("model not cached") }

// TestAddFileFailsBeforeCopyWhenEmbedderUnusable pins Q11: kb add checks the embedder before it
// writes anything, so a failed model load exits 2 with no copied file to roll back.
func TestAddFileFailsBeforeCopyWhenEmbedderUnusable(t *testing.T) {
	t.Parallel()
	orig := depsFor(t).embedder
	depsFor(t).embedder = func(config.Embed) (embed.Embedder, error) {
		return unusableEmbedder{fake.New("fake-add-test", 16)}, nil
	}
	t.Cleanup(func() { depsFor(t).embedder = orig })

	root := newTempRoot(t)
	src := filepath.Join(t.TempDir(), "kb-add-unusable-test.md")
	writeFile(t, src, validFrontMatter("KB add unusable test", "Covers add when the model cannot load."))

	_, errOut, code := run(t, "add", src)
	if code != ExitEmbedder {
		t.Fatalf("exit = %d, want %d; stderr=%q", code, ExitEmbedder, errOut)
	}
	if _, err := os.Stat(filepath.Join(root, "data", "kb-add-unusable-test.md")); !os.IsNotExist(err) {
		t.Fatalf("file was copied before the embedder check: stat err = %v", err)
	}
}
