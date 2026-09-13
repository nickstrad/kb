package cli

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"knowledge/kb/internal/embed"
	"knowledge/kb/internal/embed/ollama"
	"knowledge/kb/internal/store"
)

type unavailableAddEmbedder struct{}

func (unavailableAddEmbedder) Model() string { return "fake-add-test" }
func (unavailableAddEmbedder) Dim() int      { return store.VecDim }
func (unavailableAddEmbedder) Embed(context.Context, []string) ([][]float32, error) {
	return nil, ollama.ErrUnavailable
}

func TestAddRejectsMissingFrontMatterBeforeCopy(t *testing.T) {
	for _, dir := range []bool{false, true} {
		t.Run(map[bool]string{false: "file", true: "dir"}[dir], func(t *testing.T) {
			root := newTempRoot(t)
			src := filepath.Join(t.TempDir(), "missing.md")
			args := []string{"add", src}
			dest := filepath.Join(root, "missing.md")
			if dir {
				src = filepath.Join(t.TempDir(), "missing")
				args = []string{"add", "--dir", src}
				dest = filepath.Join(root, "missing")
				writeFile(t, filepath.Join(src, "README.md"), "# Missing front matter\n")
			} else {
				writeFile(t, src, "# Missing front matter\n")
			}
			out, errOut, code := run(t, args...)
			if code != 1 || strings.Contains(out, "added") || !strings.Contains(errOut, "title and summary") {
				t.Fatalf("%d %q %q", code, out, errOut)
			}
			if _, err := os.Stat(dest); !os.IsNotExist(err) {
				t.Fatalf("destination exists: %v", err)
			}
			if _, err := os.Stat(filepath.Join(root, ".kb")); !os.IsNotExist(err) {
				t.Fatalf("validation created database: %v", err)
			}
		})
	}
}

func TestAddRollsBackCopiedFilesOnFailure(t *testing.T) {
	for _, dir := range []bool{false, true} {
		for _, inPlace := range []bool{false, true} {
			t.Run(map[bool]string{false: "file", true: "dir"}[dir]+map[bool]string{false: "-copy", true: "-inplace"}[inPlace], func(t *testing.T) {
				useFakeEmbedder(t)
				newEmbedder = func() embed.Embedder { return unavailableAddEmbedder{} }
				root := newTempRoot(t)
				sourceRoot := t.TempDir()
				if inPlace {
					sourceRoot = root
				}
				name := "outage.md"
				if dir {
					name = "outage"
				}
				src := filepath.Join(sourceRoot, name)
				front := src
				args := []string{"add", src}
				rel := name
				if dir {
					front = filepath.Join(src, "README.md")
					args = []string{"add", "--dir", src}
					rel += "/README.md"
				}
				writeFile(t, front, validFrontMatter("Outage", "Failure must preserve the source."))
				out, errOut, code := run(t, args...)
				if code != 2 || strings.Contains(out, "added") {
					t.Fatalf("%d %q %q", code, out, errOut)
				}
				if _, err := os.Stat(front); err != nil {
					t.Fatalf("source removed: %v", err)
				}
				if !inPlace {
					if _, err := os.Stat(filepath.Join(root, name)); !os.IsNotExist(err) {
						t.Fatalf("copy remains: %v", err)
					}
					if !strings.Contains(errOut, "removed copied") {
						t.Fatalf("missing cleanup notice: %q", errOut)
					}
				}
				st := openTestStore(t, root)
				row, err := st.GetEntryByPath(rel)
				if err != nil || row != nil {
					t.Fatalf("indexed failed add: %v %v", row, err)
				}
			})
		}
	}
}

func TestAddDirectoryCopyPolicy(t *testing.T) {
	useFakeEmbedder(t)
	root := newTempRoot(t)
	src := filepath.Join(t.TempDir(), "copy-policy")
	writeFile(t, filepath.Join(src, "README.md"), validFrontMatter("Copy policy", "Only visible text companions are copied."))
	for _, name := range []string{".git/config", "nested/.private", "node_modules/module/index.js"} {
		writeFile(t, filepath.Join(src, name), "password=not-a-real-secret\n")
	}
	script := filepath.Join(src, "scripts/run.sh")
	writeFile(t, script, "#!/bin/sh\necho hi\n")
	if err := os.Chmod(script, 0751); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink("/does/not/exist", filepath.Join(src, "dangling")); err != nil {
		t.Fatal(err)
	}
	out, errOut, code := run(t, "add", "--dir", src)
	if code != 0 {
		t.Fatalf("%d %q %q", code, out, errOut)
	}
	if strings.Count(strings.TrimSpace(out), "\n") != 0 {
		t.Fatalf("not one output line: %q", out)
	}
	for _, name := range []string{".git", "nested/.private", "node_modules", "dangling"} {
		if _, err := os.Lstat(filepath.Join(root, "copy-policy", name)); !os.IsNotExist(err) {
			t.Fatalf("unexpected copied %s: %v", name, err)
		}
	}
	info, err := os.Stat(filepath.Join(root, "copy-policy/scripts/run.sh"))
	if err != nil {
		t.Fatal(err)
	}
	if info.Mode().Perm() != 0755 {
		t.Fatalf("mode=%o", info.Mode().Perm())
	}
}

func TestAddRejectsBinaryCompanionBeforeCopy(t *testing.T) {
	useFakeEmbedder(t)
	root := newTempRoot(t)
	src := filepath.Join(t.TempDir(), "binary-topic")
	writeFile(t, filepath.Join(src, "README.md"), validFrontMatter("Binary", "Reject a binary companion."))
	writeFile(t, filepath.Join(src, "blob"), "data\x00binary")
	_, errOut, code := run(t, "add", "--dir", src)
	if code != 1 || !strings.Contains(errOut, "non-text file") {
		t.Fatalf("%d %q", code, errOut)
	}
	if _, err := os.Stat(filepath.Join(root, "binary-topic")); !os.IsNotExist(err) {
		t.Fatalf("copy exists: %v", err)
	}
}

func TestAddDestinationAdvice(t *testing.T) {
	useFakeEmbedder(t)
	root := newTempRoot(t)
	src := filepath.Join(t.TempDir(), "duplicate.md")
	dest := filepath.Join(root, "duplicate.md")
	body := validFrontMatter("Duplicate", "Explains recovery.")
	writeFile(t, src, body)
	writeFile(t, dest, body)
	_, errOut, code := run(t, "add", src)
	if code != 1 || !strings.Contains(errOut, "use kb reindex duplicate.md") {
		t.Fatalf("%d %q", code, errOut)
	}
	if _, errOut, code = run(t, "add", dest); code != 0 {
		t.Fatalf("%d %q", code, errOut)
	}
	_, errOut, code = run(t, "add", src)
	if code != 1 || !strings.Contains(errOut, "use kb edit duplicate.md") {
		t.Fatalf("%d %q", code, errOut)
	}
	if err := os.Remove(dest); err != nil {
		t.Fatal(err)
	}
	_, errOut, code = run(t, "add", src)
	if code != 1 || !strings.Contains(errOut, "run kb reindex --all") {
		t.Fatalf("%d %q", code, errOut)
	}
}

func TestAddSecretPatternsAllowLocationProse(t *testing.T) {
	for _, text := range []string{"password: stored elsewhere", "API key: stored in a protected file", "xoxb-"} {
		if hits := scanSecrets([]byte(text)); len(hits) > 0 {
			t.Errorf("rejected prose %q", text)
		}
	}
	for _, text := range []string{"password = example", "TOKEN=example", "api-key=example", "xoxb-abcdefghijk"} {
		if hits := scanSecrets([]byte(text)); len(hits) == 0 {
			t.Errorf("missed pattern %q", text)
		}
	}
}

func TestAddRollbackAfterIndexWriteFailure(t *testing.T) {
	useFakeEmbedder(t)
	root := newTempRoot(t)
	src := filepath.Join(t.TempDir(), "index-failure.md")
	writeFile(t, src, validFrontMatter("Index failure", "Clean up the copy and database."))
	if err := os.Mkdir(filepath.Join(root, "index.md"), 0755); err != nil {
		t.Fatal(err)
	}
	out, errOut, code := run(t, "add", src)
	if code != 1 || strings.Contains(out, "added") {
		t.Fatalf("%d %q %q", code, out, errOut)
	}
	if _, err := os.Stat(filepath.Join(root, "index-failure.md")); !os.IsNotExist(err) {
		t.Fatalf("copy remains: %v", err)
	}
	st := openTestStore(t, root)
	row, err := st.GetEntryByPath("index-failure.md")
	if row != nil || err != nil {
		t.Fatalf("row=%v err=%v", row, err)
	}
}
