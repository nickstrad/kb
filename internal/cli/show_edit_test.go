// Tests for kb show, kb list --tag, kb edit and kb rm. Every test builds its own temporary
// repository root and, where a reindex runs, uses the fake embedder.
package cli

import (
	"bytes"
	"context"
	"github.com/nickstrad/kb/internal/config"
	"github.com/nickstrad/kb/internal/errs"
	"github.com/nickstrad/kb/internal/store/sqlite"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/nickstrad/kb/internal/embed"
	"github.com/nickstrad/kb/internal/embed/fake"
	"github.com/nickstrad/kb/internal/entry"
	"github.com/nickstrad/kb/internal/reindex"
	"github.com/nickstrad/kb/internal/store"
)

const fooMD = `---
title: Foo Thing
summary: A test entry about foo.
tags: [alpha, bash]
updated: 2026-09-01
---

Intro text about foo.

## Details

Some details here.
`

const barReadmeMD = `---
title: Bar Dir
summary: A test directory entry.
tags: [alpha]
updated: 2026-09-01
---

Bar intro.

## More

More bar text.
`

const barExtraMD = `## Extra doc

A second file for the bar/ directory entry, so it has more than one indexed file.
`

// setupRepo builds a temp repo root with one file entry (foo.md) and one directory entry
// (bar/README.md).
func setupRepo(t *testing.T) string {
	t.Helper()
	root := t.TempDir()
	if err := os.MkdirAll(filepath.Join(root, "data"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, "data", "foo.md"), []byte(fooMD), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Join(root, "data", "bar"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, "data", "bar", "README.md"), []byte(barReadmeMD), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Join(root, "data", "bar", "docs"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, "data", "bar", "docs", "extra.md"), []byte(barExtraMD), 0o644); err != nil {
		t.Fatal(err)
	}
	return root
}

// withFakeEmbedder substitutes newEmbedder with a fake.Embedder for the duration of one test, so
// kb edit's reindex never loads a real model.
func withFakeEmbedder(t *testing.T) {
	t.Helper()
	orig := testDeps.embedder
	testDeps.embedder = func(config.Embed) (embed.Embedder, error) { return fake.New("test-model", testDim768), nil }
	t.Cleanup(func() { testDeps.embedder = orig })
}

// indexEntry reindexes one entry with the fake embedder directly, so tests can start from an
// "already indexed" repository without going through kb reindex's CLI wiring.
func indexEntry(t *testing.T, ctx context.Context, root, arg string) {
	t.Helper()
	if err := ensureKBDir(root); err != nil {
		t.Fatal(err)
	}
	st, err := sqlite.Open(context.Background(), testDBPath(root), sqlite.Options{})
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()

	e, err := entry.Resolve(root, arg)
	if err != nil {
		t.Fatal(err)
	}
	embedder := fake.New("test-model", testDim768)
	if err := st.EnsureEmbedMeta(context.Background(), store.EmbedMeta{Model: embedder.Model(), Dim: embedder.Dim()}); err != nil {
		t.Fatal(err)
	}
	opts := reindex.Options{Root: root, Store: st, Embedder: embedder, Stdout: io.Discard, Stderr: io.Discard}
	if _, err := reindex.One(ctx, opts, e); err != nil {
		t.Fatal(err)
	}
}

func TestShowPrintsFileVerbatim(t *testing.T) {
	root := setupRepo(t)
	var out, errBuf bytes.Buffer

	if err := runShow(context.Background(), testDeps, &out, &errBuf, testWS(root), config.Default(), "foo.md", false, true, false); err != nil {
		t.Fatalf("runShow: %v", err)
	}
	if out.String() != fooMD {
		t.Errorf("show should print the file verbatim; got:\n%s", out.String())
	}
}

func TestShowMultiFileEntryHasHeaders(t *testing.T) {
	root := setupRepo(t)
	var out, errBuf bytes.Buffer

	if err := runShow(context.Background(), testDeps, &out, &errBuf, testWS(root), config.Default(), "bar", false, true, false); err != nil {
		t.Fatalf("runShow: %v", err)
	}
	got := out.String()
	if !strings.Contains(got, "==> data/bar/README.md <==") {
		t.Errorf("expected a header for bar/README.md, got:\n%s", got)
	}
	if !strings.Contains(got, "==> data/bar/docs/extra.md <==") {
		t.Errorf("expected a header for bar/docs/extra.md, got:\n%s", got)
	}
	if !strings.Contains(got, "Extra doc") {
		t.Errorf("expected the docs/extra.md content to be printed, got:\n%s", got)
	}
}

func TestShowSingleFileEntryHasNoHeader(t *testing.T) {
	root := setupRepo(t)
	var out, errBuf bytes.Buffer

	if err := runShow(context.Background(), testDeps, &out, &errBuf, testWS(root), config.Default(), "foo.md", false, true, false); err != nil {
		t.Fatalf("runShow: %v", err)
	}
	if strings.Contains(out.String(), "==>") {
		t.Errorf("a single-file entry should print no ==> header, got:\n%s", out.String())
	}
}

func TestShowChunksUnindexedFallsBackToSplit(t *testing.T) {
	root := setupRepo(t)
	var out, errBuf bytes.Buffer

	if err := runShow(context.Background(), testDeps, &out, &errBuf, testWS(root), config.Default(), "foo.md", true, true, false); err != nil {
		t.Fatalf("runShow --chunks: %v", err)
	}
	if !strings.Contains(errBuf.String(), "not indexed") {
		t.Errorf("expected the not-indexed note on stderr, got %q", errBuf.String())
	}
	if !strings.Contains(out.String(), "--- chunk 0 [data/foo.md] (summary)") {
		t.Errorf("expected a computed summary chunk, got:\n%s", out.String())
	}
	if !strings.Contains(out.String(), "Details") {
		t.Errorf("expected a computed section chunk for ## Details, got:\n%s", out.String())
	}
}

func TestShowChunksIndexedReadsFromDB(t *testing.T) {
	root := setupRepo(t)
	ctx := context.Background()
	indexEntry(t, ctx, root, "foo.md")

	var out, errBuf bytes.Buffer
	if err := runShow(context.Background(), testDeps, &out, &errBuf, testWS(root), config.Default(), "foo.md", true, true, false); err != nil {
		t.Fatalf("runShow --chunks: %v", err)
	}
	if strings.Contains(errBuf.String(), "not indexed") {
		t.Errorf("entry is indexed; should not print the fallback note, got %q", errBuf.String())
	}
	if !strings.Contains(out.String(), "--- chunk 0 [data/foo.md] (summary)") {
		t.Errorf("expected the stored summary chunk, got:\n%s", out.String())
	}
}

func TestShowUnknownEntryExitsUsage(t *testing.T) {
	root := setupRepo(t)
	var out, errBuf bytes.Buffer

	err := runShow(context.Background(), testDeps, &out, &errBuf, testWS(root), config.Default(), "nope.md", false, true, false)
	if err == nil {
		t.Fatal("expected an error for an unknown entry")
	}
	if exitCode(errs.KindOf(err)) != ExitUsage {
		t.Errorf("expected ExitUsage, got %v (kind %v)", err, errs.KindOf(err))
	}
}

func TestListFiltersByTag(t *testing.T) {
	root := setupRepo(t)
	var out bytes.Buffer

	if err := runList(&out, root, "bash"); err != nil {
		t.Fatalf("runList: %v", err)
	}
	got := out.String()
	if !strings.Contains(got, "foo.md") {
		t.Errorf("expected foo.md (tagged bash) in output:\n%s", got)
	}
	if strings.Contains(got, "bar/README.md") {
		t.Errorf("bar/README.md is not tagged bash and should be filtered out:\n%s", got)
	}
}

func TestListShowsBrokenEntryAsError(t *testing.T) {
	root := setupRepo(t)
	// Front matter with no summary: entry.ParseFrontMatter reports this as a *ValidationError,
	// which Discover/loadMeta turns into Entry.Err rather than a zero-valued Meta.
	brokenMD := "---\ntitle: Broken Entry\n---\n\nbody text\n"
	if err := os.WriteFile(filepath.Join(root, "data", "broken.md"), []byte(brokenMD), 0o644); err != nil {
		t.Fatal(err)
	}

	var out bytes.Buffer
	if err := runList(&out, root, ""); err != nil {
		t.Fatalf("runList: %v", err)
	}
	got := out.String()
	if !strings.Contains(got, "broken.md") || !strings.Contains(got, "ERROR:") {
		t.Errorf("expected broken.md listed with an ERROR summary, got:\n%s", got)
	}
}

func TestRmFileRemovesFileAndDBRow(t *testing.T) {
	root := setupRepo(t)
	ctx := context.Background()
	indexEntry(t, ctx, root, "foo.md")

	var out, errBuf bytes.Buffer
	if err := runRm(ctx, testDeps, &out, &errBuf, testWS(root), "foo.md", false); err != nil {
		t.Fatalf("runRm: %v", err)
	}
	if !strings.Contains(out.String(), "removed data/foo.md") {
		t.Errorf("expected a removed-foo.md report, got %q", out.String())
	}
	if _, err := os.Stat(filepath.Join(root, "data", "foo.md")); !os.IsNotExist(err) {
		t.Errorf("foo.md should no longer exist on disk, stat err = %v", err)
	}

	st, err := sqlite.Open(context.Background(), testDBPath(root), sqlite.Options{})
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	row, err := st.Entry(context.Background(), "data/foo.md")
	if err != nil {
		t.Fatal(err)
	}
	if row != nil {
		t.Errorf("expected the entries row for foo.md to be gone, got %+v", row)
	}
}

func TestRmDirRequiresYes(t *testing.T) {
	root := setupRepo(t)
	ctx := context.Background()
	indexEntry(t, ctx, root, "bar")

	var out, errBuf bytes.Buffer
	err := runRm(ctx, testDeps, &out, &errBuf, testWS(root), "bar", false)
	if err == nil {
		t.Fatal("expected rm of a directory entry without --yes to be refused")
	}
	if exitCode(errs.KindOf(err)) != ExitUsage {
		t.Errorf("expected ExitUsage, got %v (kind %v)", err, errs.KindOf(err))
	}
	if _, statErr := os.Stat(filepath.Join(root, "data", "bar")); statErr != nil {
		t.Errorf("bar/ should still exist after a refused rm: %v", statErr)
	}

	if err := runRm(ctx, testDeps, &out, &errBuf, testWS(root), "bar", true); err != nil {
		t.Fatalf("runRm --yes: %v", err)
	}
	if _, statErr := os.Stat(filepath.Join(root, "data", "bar")); !os.IsNotExist(statErr) {
		t.Errorf("bar/ should be removed after rm --yes")
	}
	if !strings.Contains(out.String(), "removed data/bar/ and") {
		t.Errorf("expected a 'removed bar/ and N files' report, got %q", out.String())
	}

	st, err := sqlite.Open(context.Background(), testDBPath(root), sqlite.Options{})
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	row, err := st.Entry(context.Background(), "data/bar/README.md")
	if err != nil {
		t.Fatal(err)
	}
	if row != nil {
		t.Errorf("expected the entries row for bar/README.md to be gone, got %+v", row)
	}
}

func TestEditReindexesOnChange(t *testing.T) {
	root := setupRepo(t)
	withFakeEmbedder(t)
	ctx := context.Background()
	indexEntry(t, ctx, root, "foo.md")

	// An editor that is a shell one-liner: appends a "## Added" section via printf.
	t.Setenv("EDITOR", `sh -c 'printf "\n## Added\n\nnew text\n" >> "$1"' --`)

	var out, errBuf bytes.Buffer
	if err := runEdit(ctx, nil, testDeps, &out, &errBuf, testWS(root), config.Default(), "foo.md"); err != nil {
		t.Fatalf("runEdit: %v (stderr=%s)", err, errBuf.String())
	}
	if strings.Contains(out.String(), "unchanged") {
		t.Errorf("expected a reindex summary, not 'unchanged', got %q", out.String())
	}

	st, err := sqlite.Open(context.Background(), testDBPath(root), sqlite.Options{})
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	row, err := st.Entry(context.Background(), "data/foo.md")
	if err != nil || row == nil {
		t.Fatalf("expected an entries row for foo.md, err=%v row=%v", err, row)
	}
	chunks, err := st.Chunks(context.Background(), row.ID)
	if err != nil {
		t.Fatal(err)
	}
	found := false
	for _, c := range chunks {
		if strings.Contains(c.Text, "new text") {
			found = true
		}
	}
	if !found {
		t.Errorf("expected a chunk containing the appended text, got %+v", chunks)
	}
}

func TestEditEditorFailureAbortsWithoutTouchingDB(t *testing.T) {
	root := setupRepo(t)
	withFakeEmbedder(t)
	ctx := context.Background()
	indexEntry(t, ctx, root, "foo.md")

	st0, err := sqlite.Open(context.Background(), testDBPath(root), sqlite.Options{})
	if err != nil {
		t.Fatal(err)
	}
	before, err := st0.Entry(context.Background(), "data/foo.md")
	st0.Close()
	if err != nil || before == nil {
		t.Fatalf("setup: expected an indexed foo.md, err=%v", err)
	}

	t.Setenv("EDITOR", "false")

	var out, errBuf bytes.Buffer
	err = runEdit(ctx, nil, testDeps, &out, &errBuf, testWS(root), config.Default(), "foo.md")
	if err == nil {
		t.Fatal("expected an error when $EDITOR exits non-zero")
	}
	if exitCode(errs.KindOf(err)) != ExitUsage {
		t.Errorf("expected ExitUsage, got %v (kind %v)", err, errs.KindOf(err))
	}

	content, err := os.ReadFile(filepath.Join(root, "data", "foo.md"))
	if err != nil {
		t.Fatal(err)
	}
	if string(content) != fooMD {
		t.Errorf("file should be untouched by a failing editor, got:\n%s", content)
	}

	st1, err := sqlite.Open(context.Background(), testDBPath(root), sqlite.Options{})
	if err != nil {
		t.Fatal(err)
	}
	defer st1.Close()
	after, err := st1.Entry(context.Background(), "data/foo.md")
	if err != nil {
		t.Fatal(err)
	}
	if after == nil || after.BodyHash != before.BodyHash {
		t.Errorf("DB row should be unchanged: before=%+v after=%+v", before, after)
	}
}

func TestEditNoopIsUnchanged(t *testing.T) {
	root := setupRepo(t)
	withFakeEmbedder(t)
	ctx := context.Background()
	indexEntry(t, ctx, root, "foo.md")

	t.Setenv("EDITOR", "true")

	var out, errBuf bytes.Buffer
	if err := runEdit(ctx, nil, testDeps, &out, &errBuf, testWS(root), config.Default(), "foo.md"); err != nil {
		t.Fatalf("runEdit: %v (stderr=%s)", err, errBuf.String())
	}
	if strings.TrimSpace(out.String()) != "unchanged" {
		t.Errorf(`expected "unchanged", got %q`, out.String())
	}
}
