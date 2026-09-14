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

func TestMutationCommandsLeaveOptionalIndexAlone(t *testing.T) {
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
			t.Setenv("EDITOR", "sed -i 's/Original summary/Edited summary/'")
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
	root := newTempRoot(t)
	writeFile(t, filepath.Join(root, "data", "export.md"), validFrontMatter("Export", "Reproducible from source."))
	for range 2 {
		if out, errOut, code := run(t, "index"); code != 0 {
			t.Fatalf("%d %q %q", code, out, errOut)
		}
		assertIndexContains(t, root, "[data/export.md](data/export.md)", true)
		assertIndexContains(t, root, "Reproducible from source.", true)
		if _, err := os.Stat(DBPath(root)); !os.IsNotExist(err) {
			t.Fatalf("export created a database: %v", err)
		}
		if err := os.Remove(filepath.Join(root, "index.md")); err != nil {
			t.Fatal(err)
		}
	}
}

func TestDoctorDetectsIndependentDrift(t *testing.T) {
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
				if code != 0 || !strings.Contains(out, "fts5 enabled") || !strings.Contains(out, "entries in sync") || !strings.Contains(out, "index.md: absent (optional export)") {
					t.Fatalf("%d %q %q", code, out, errOut)
				}
			} else if kind == "index" {
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
