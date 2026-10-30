package main

import (
	"bytes"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
)

func TestLinks(t *testing.T) {
	md := "- [a.sh](a.sh) — x\n- [tool](tool/README.md) — y\n- [dir](./dir/) — z\n" +
		"- [web](https://example.com/p) and [top](/abs) and [up](../PLAN.md) and [anchor](b.md#h)\n" +
		"- [titled](c.md \"title\")\n"
	got := links(md)
	want := []string{"a.sh", "tool", "dir", "b.md", "c.md"}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("links = %q, want %q", got, want)
	}
}

func TestDiff(t *testing.T) {
	tests := []struct {
		name                   string
		have, listed           []string
		wantMissing, wantExtra []string
	}{
		{"in sync", []string{"a", "b"}, []string{"b", "a"}, nil, nil},
		{"missing only", []string{"a", "b"}, []string{"a"}, []string{"b"}, nil},
		{"extra only", []string{"a"}, []string{"a", "z"}, nil, []string{"z"}},
		{"both, duplicates collapse", []string{"c", "a"}, []string{"z", "z", "a"}, []string{"c"}, []string{"z"}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			m, x := diff(tt.have, tt.listed)
			if !reflect.DeepEqual(m, tt.wantMissing) || !reflect.DeepEqual(x, tt.wantExtra) {
				t.Fatalf("diff = missing %q extra %q, want %q %q", m, x, tt.wantMissing, tt.wantExtra)
			}
		})
	}
}

func TestCheckSkipsIndexReadmeAndDotfiles(t *testing.T) {
	dir := t.TempDir()
	for _, f := range []string{"index.md", "README.md", ".gitignore", "a.md", "extra.md"} {
		if err := os.WriteFile(filepath.Join(dir, f), nil, 0o644); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.Mkdir(filepath.Join(dir, "tool"), 0o755); err != nil {
		t.Fatal(err)
	}
	index := "- [a](a.md)\n- [tool](tool/README.md)\n- [gone](gone.md)\n"
	if err := os.WriteFile(filepath.Join(dir, "index.md"), []byte(index), 0o644); err != nil {
		t.Fatal(err)
	}
	m, x, err := check(dir)
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(m, []string{"extra.md"}) || !reflect.DeepEqual(x, []string{"gone.md"}) {
		t.Fatalf("check = missing %q extra %q", m, x)
	}
}

func TestCheckMissingIndexIsError(t *testing.T) {
	if _, _, err := check(t.TempDir()); err == nil {
		t.Fatal("check without index.md returned no error")
	}
}

// TestRunChecksEveryIndexedFolder: run covers scripts, knowledge, leverage and testability, and its
// exit status says in sync (0), out of sync (1) or unreadable (2).
func TestRunChecksEveryIndexedFolder(t *testing.T) {
	root := t.TempDir()
	for _, name := range indexed {
		dir := filepath.Join(root, name)
		if err := os.MkdirAll(filepath.Join(dir, "archive"), 0o755); err != nil {
			t.Fatal(err)
		}
		for f, body := range map[string]string{"a.md": "", "index.md": "- [a](a.md)\n- [archive](archive/README.md)\n"} {
			if err := os.WriteFile(filepath.Join(dir, f), []byte(body), 0o644); err != nil {
				t.Fatal(err)
			}
		}
	}
	var out, errOut bytes.Buffer
	if code := run(root, &out, &errOut); code != 0 || !strings.Contains(out.String(), "indexes in sync") {
		t.Fatalf("in sync: code %d, stdout %q, stderr %q", code, out.String(), errOut.String())
	}
	for _, name := range indexed {
		t.Run(name, func(t *testing.T) {
			extra := filepath.Join(root, name, "new.md")
			if err := os.WriteFile(extra, nil, 0o644); err != nil {
				t.Fatal(err)
			}
			defer os.Remove(extra)
			var out bytes.Buffer
			want := filepath.Join(root, name) + "/index.md: missing entry for new.md"
			if code := run(root, &out, &bytes.Buffer{}); code != 1 || !strings.Contains(out.String(), want) {
				t.Fatalf("code %d, stdout %q; want 1 and %q", code, out.String(), want)
			}
		})
	}
	if err := os.Remove(filepath.Join(root, "testability", "index.md")); err != nil {
		t.Fatal(err)
	}
	if code := run(root, &bytes.Buffer{}, &bytes.Buffer{}); code != 2 {
		t.Fatalf("missing testability/index.md: code %d, want 2", code)
	}
}
