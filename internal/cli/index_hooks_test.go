package cli

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/nickstrad/kb/internal/store/sqlite/sqlitetest"
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

func TestMutationCommandsLeaveOptionalIndexAlone(t *testing.T) {
	t.Parallel()
	for _, exported := range []bool{false, true} {
		t.Run(map[bool]string{false: "absent", true: "existing-snapshot"}[exported], func(t *testing.T) {
			useFakeEmbedder(t)
			root := newTempRoot(t)
			indexPath := filepath.Join(root, "index.md")
			snapshot := "existing snapshot\n"
			if exported {
				writeFile(t, indexPath, snapshot)
			}
			check := func() {
				t.Helper()
				got, err := os.ReadFile(indexPath)
				if exported {
					if err != nil || string(got) != snapshot {
						t.Fatalf("snapshot changed: %q %v", got, err)
					}
				} else if !os.IsNotExist(err) {
					t.Fatalf("index.md unexpectedly created: %v", err)
				}
			}
			invoke := func(args ...string) {
				t.Helper()
				if out, errOut, code := run(t, args...); code != 0 {
					t.Fatalf("%v: %d %q %q", args, code, out, errOut)
				}
				check()
			}
			src := filepath.Join(t.TempDir(), "index-hooks.md")
			writeFile(t, src, validFrontMatter("Index hooks", "Original summary."))
			invoke("add", src)
			useFakeEditor(t, "Original summary", "Edited summary")
			invoke("edit", "index-hooks.md")
			invoke("reindex", "index-hooks.md")
			invoke("reindex", "--all")
			invoke("rm", "index-hooks.md")
			topic := filepath.Join(t.TempDir(), "topic")
			writeFile(t, filepath.Join(topic, "README.md"), validFrontMatter("Topic", "Directory entry."))
			invoke("add", "--dir", topic)
			invoke("rm", "topic", "--yes")
		})
	}
}

func TestIndexCanBeReproducedWithoutDatabase(t *testing.T) {
	t.Parallel()
	root := newTempRoot(t)
	writeFile(t, filepath.Join(root, "data", "export.md"), validFrontMatter("Export", "Reproducible from source."))
	for range 2 {
		if out, errOut, code := run(t, "index"); code != 0 {
			t.Fatalf("%d %q %q", code, out, errOut)
		}
		assertIndexContains(t, root, "[data/export.md](data/export.md)", true)
		assertIndexContains(t, root, "Reproducible from source.", true)
		if _, err := os.Stat(testDBPath(root)); !os.IsNotExist(err) {
			t.Fatalf("export created a database: %v", err)
		}
		if err := os.Remove(filepath.Join(root, "index.md")); err != nil {
			t.Fatal(err)
		}
	}
}

func TestDoctorDetectsIndependentDrift(t *testing.T) {
	t.Parallel()
	for _, kind := range []string{"clean", "changed", "missing", "unindexed", "index", "invalid", "model", "dimension", "database"} {
		t.Run(kind, func(t *testing.T) {
			useFakeEmbedder(t)
			root := newTempRoot(t)
			path := filepath.Join(root, "data", "doctor-topic.md")
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
				writeFile(t, filepath.Join(root, "data", "unindexed.md"), validFrontMatter("New", "New entry."))
				want = "FAIL db-vs-files: stale"
			case "index":
				writeFile(t, filepath.Join(root, "index.md"), "hand edit\n")
				want = "warn index.md: optional snapshot is stale"
			case "invalid":
				writeFile(t, path, "---\ntitle: Missing summary\n---\nBody\n")
				want = "FAIL entry:"
			case "model", "dimension":
				st := openTestStore(t, root)
				if kind == "dimension" {
					sqlitetest.ForceEmbedDim(t, st, 4)
				} else {
					sqlitetest.ForceEmbedModel(t, st, "different")
				}
				want = "FAIL embed_meta:"
			case "database":
				if err := os.Remove(testDBPath(root)); err != nil {
					t.Fatal(err)
				}
				want = "warn database: not indexed: run kb reindex"
			}
			out, errOut, code := run(t, "doctor")
			if kind == "clean" {
				if code != 0 || !strings.Contains(out, "fts5 enabled") || !strings.Contains(out, "entries in sync") || !strings.Contains(out, "index.md: absent (optional export)") {
					t.Fatalf("%d %q %q", code, out, errOut)
				}
			} else if kind == "index" || kind == "database" { // warnings: exit 0
				if code != 0 || !strings.Contains(out, want) {
					t.Fatalf("want %q: %d %q %q", want, code, out, errOut)
				}
			} else if code != 1 || !strings.Contains(out, want) {
				t.Fatalf("want %q: %d %q %q", want, code, out, errOut)
			}
		})
	}
}

func TestIndexReportsInvalidEntryAndWritesValidRows(t *testing.T) {
	t.Parallel()
	root := newTempRoot(t)
	writeFile(t, filepath.Join(root, "data", "valid.md"), validFrontMatter("Valid", "Included."))
	writeFile(t, filepath.Join(root, "data", "invalid.md"), "---\ntitle: Invalid\n---\n")
	_, errOut, code := run(t, "index")
	if code != 1 || !strings.Contains(errOut, "summary") {
		t.Fatalf("%d %q", code, errOut)
	}
	assertIndexContains(t, root, "valid.md", true)
	assertIndexContains(t, root, "[invalid.md]", false)
}

// TestIndexNamedStoreDoesNotClobber: index.md is one file at the location root and describes the
// default store, so kb index for another store is refused and leaves it alone, and doctor run on
// another store does not judge it (W7 pass: a files-only store wrote "(0 entries)" over it).
func TestIndexNamedStoreDoesNotClobber(t *testing.T) {
	t.Parallel()
	root := newTempRoot(t)
	useNoneModel(t, root)
	writeFile(t, filepath.Join(root, "data", "a.md"), validFrontMatter("A", "a"))
	addStore(t, root, "notes")
	if _, errOut, code := run(t, "index"); code != 0 {
		t.Fatalf("index: %d %q", code, errOut)
	}
	before := readFileT(t, filepath.Join(root, "index.md"))
	_, errOut, code := run(t, "--store", "notes", "index")
	if code != ExitUsage || !strings.Contains(errOut, `describes the default store ("default")`) {
		t.Fatalf("index for notes: %d %q", code, errOut)
	}
	if readFileT(t, filepath.Join(root, "index.md")) != before {
		t.Error("kb --store notes index changed index.md")
	}
	writeFile(t, filepath.Join(root, "index.md"), before+"stale\n")
	if out, _, _ := run(t, "--store", "notes", "doctor"); !strings.Contains(out, `ok   index.md: not checked: it describes the default store "default"`) {
		t.Errorf("doctor on notes judged index.md:\n%s", out)
	}
	if out, _, _ := run(t, "doctor"); !strings.Contains(out, "warn index.md: optional snapshot is stale") {
		t.Errorf("doctor on the default store lost the stale check:\n%s", out)
	}
}
