package cli

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func assertIndexContains(t *testing.T, root, text string, want bool) {
	t.Helper()
	b, err := os.ReadFile(filepath.Join(root, "index.md"))
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(b), text) != want {
		t.Fatalf("index contains %q=%v, want %v: %s", text, !want, want, b)
	}
}

func TestMutationCommandsRegenerateIndex(t *testing.T) {
	useFakeEmbedder(t)
	root := newTempRoot(t)
	src := filepath.Join(t.TempDir(), "index-hooks.md")
	writeFile(t, src, validFrontMatter("Index hooks", "Original summary."))
	if out, errOut, code := run(t, "add", src); code != 0 {
		t.Fatalf("add: %d %q %q", code, out, errOut)
	}
	assertIndexContains(t, root, "Original summary.", true)
	// $EDITOR changes both Markdown metadata and indexed text through the actual CLI.
	editor := filepath.Join(t.TempDir(), "edit.sh")
	writeFile(t, editor, "#!/bin/sh\nsed -i 's/Original summary/Edited summary/' \"$1\"\n")
	if err := os.Chmod(editor, 0755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("EDITOR", editor)
	if out, errOut, code := run(t, "edit", "index-hooks.md"); code != 0 {
		t.Fatalf("edit: %d %q %q", code, out, errOut)
	}
	assertIndexContains(t, root, "Edited summary.", true)
	assertIndexContains(t, root, "Original summary.", false)
	writeFile(t, filepath.Join(root, "index-hooks.md"), validFrontMatter("Index hooks", "Reindexed summary."))
	if out, errOut, code := run(t, "reindex", "index-hooks.md"); code != 0 {
		t.Fatalf("reindex: %d %q %q", code, out, errOut)
	}
	assertIndexContains(t, root, "Reindexed summary.", true)
	if out, errOut, code := run(t, "rm", "index-hooks.md"); code != 0 {
		t.Fatalf("rm: %d %q %q", code, out, errOut)
	}
	assertIndexContains(t, root, "index-hooks.md", false)
}

func TestDoctorDetectsIndependentDrift(t *testing.T) {
	for _, kind := range []string{"clean", "changed", "missing", "unindexed", "index", "invalid", "model", "dimension", "database"} {
		t.Run(kind, func(t *testing.T) {
			useFakeEmbedder(t)
			root := newTempRoot(t)
			path := filepath.Join(root, "doctor-topic.md")
			writeFile(t, path, validFrontMatter("Doctor topic", "A health check fixture."))
			if out, errOut, code := run(t, "reindex", "--all"); code != 0 {
				t.Fatalf("setup: %d %q %q", code, out, errOut)
			}
			want := ""
			switch kind {
			case "changed":
				writeFile(t, path, validFrontMatter("Changed", "Changed summary."))
				want = "FAIL db-vs-files: stale"
			case "missing":
				if err := os.Remove(path); err != nil {
					t.Fatal(err)
				}
				want = "FAIL db-vs-files: orphan"
			case "unindexed":
				writeFile(t, filepath.Join(root, "unindexed.md"), validFrontMatter("New", "New entry."))
				want = "FAIL db-vs-files: stale"
			case "index":
				writeFile(t, filepath.Join(root, "index.md"), "hand edit\n")
				want = "FAIL index.md: differs"
			case "invalid":
				writeFile(t, path, "---\ntitle: Missing summary\n---\nBody\n")
				want = "FAIL entry:"
			case "model", "dimension":
				st := openTestStore(t, root)
				sql := "UPDATE embed_meta SET model='different'"
				if kind == "dimension" {
					sql = "UPDATE embed_meta SET dim=4"
				}
				if _, err := st.DB().Exec(sql); err != nil {
					t.Fatal(err)
				}
				want = "FAIL embed_meta:"
			case "database":
				if err := os.Remove(DBPath(root)); err != nil {
					t.Fatal(err)
				}
				want = "FAIL database:"
			}
			out, errOut, code := run(t, "doctor")
			if kind == "clean" {
				if code != 0 || !strings.Contains(out, "fts5 enabled") || !strings.Contains(out, "entries in sync") || !strings.Contains(out, "index.md: up to date") {
					t.Fatalf("%d %q %q", code, out, errOut)
				}
			} else if code != 1 || !strings.Contains(out, want) {
				t.Fatalf("want %q: %d %q %q", want, code, out, errOut)
			}
		})
	}
}

func TestIndexReportsInvalidEntryAndWritesValidRows(t *testing.T) {
	root := newTempRoot(t)
	writeFile(t, filepath.Join(root, "valid.md"), validFrontMatter("Valid", "Included."))
	writeFile(t, filepath.Join(root, "invalid.md"), "---\ntitle: Invalid\n---\n")
	_, errOut, code := run(t, "index")
	if code != 1 || !strings.Contains(errOut, "summary") {
		t.Fatalf("%d %q", code, errOut)
	}
	assertIndexContains(t, root, "valid.md", true)
	assertIndexContains(t, root, "[invalid.md]", false)
}
