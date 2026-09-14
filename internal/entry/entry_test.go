package entry

import (
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"testing"
)

// realRepoRoot is the actual knowledge repository this module lives in:
// kb/internal/entry -> kb/internal -> kb -> the repo root.
const realRepoRoot = "../../.."

// TestParseFrontMatter covers the shapes the repository actually uses: an inline tag list, a block
// tag list, and a file with no front matter at all.
func TestParseFrontMatter(t *testing.T) {
	src := []byte("---\ntitle: Docker networking on this droplet\nsummary: One line.\ntags: [droplet, docker]\nupdated: 2026-09-11\nverified: 2026-09-11 on Ubuntu 24.04.4\n---\n\n# Body\n")
	fm, body, err := ParseFrontMatter(src)
	if err != nil {
		t.Fatalf("ParseFrontMatter: %v", err)
	}
	if fm.Title != "Docker networking on this droplet" || fm.Summary != "One line." {
		t.Errorf("title/summary = %q / %q", fm.Title, fm.Summary)
	}
	if len(fm.Tags) != 2 || fm.Tags[0] != "droplet" || fm.Tags[1] != "docker" {
		t.Errorf("tags = %v", fm.Tags)
	}
	if fm.Updated != "2026-09-11" || fm.Verified != "2026-09-11 on Ubuntu 24.04.4" {
		t.Errorf("updated/verified = %q / %q", fm.Updated, fm.Verified)
	}
	if got := string(body); got != "\n# Body\n" {
		t.Errorf("body = %q", got)
	}

	block := []byte("---\ntitle: T\nsummary: S\ntags:\n  - a\n  - b\n---\nbody\n")
	fm, _, err = ParseFrontMatter(block)
	if err != nil {
		t.Fatalf("ParseFrontMatter (block list): %v", err)
	}
	if len(fm.Tags) != 2 || fm.Tags[0] != "a" || fm.Tags[1] != "b" {
		t.Errorf("block tags = %v", fm.Tags)
	}

	plain := []byte("# No front matter\n")
	fm, body, err = ParseFrontMatter(plain)
	if err != nil {
		t.Fatalf("ParseFrontMatter (none): %v", err)
	}
	if fm.Title != "" || string(body) != string(plain) {
		t.Errorf("expected empty front matter and the whole source back, got %q / %q", fm.Title, body)
	}
}

// TestParseFrontMatterTolerance exercises the specific shapes plan.md and real entries use that a
// naive line-based parser could trip on: values containing an em dash and further colons after
// the key, quoted strings, and CRLF line endings.
func TestParseFrontMatterTolerance(t *testing.T) {
	t.Run("dash and colon in value", func(t *testing.T) {
		src := []byte("---\ntitle: T\nsummary: S\nverified: 2026-09-11 — every value below read live from the box\n---\nbody\n")
		fm, _, err := ParseFrontMatter(src)
		if err != nil {
			t.Fatalf("ParseFrontMatter: %v", err)
		}
		want := "2026-09-11 — every value below read live from the box"
		if fm.Verified != want {
			t.Errorf("verified = %q, want %q", fm.Verified, want)
		}
	})

	t.Run("colon inside the value itself", func(t *testing.T) {
		src := []byte("---\ntitle: T\nsummary: Reaches 127.0.0.1:11434 and uses `search_document: ` prefixes.\n---\nbody\n")
		fm, _, err := ParseFrontMatter(src)
		if err != nil {
			t.Fatalf("ParseFrontMatter: %v", err)
		}
		want := "Reaches 127.0.0.1:11434 and uses `search_document: ` prefixes."
		if fm.Summary != want {
			t.Errorf("summary = %q, want %q", fm.Summary, want)
		}
	})

	t.Run("quoted strings", func(t *testing.T) {
		src := []byte(`---
title: "A title: with a colon"
summary: 'Single-quoted summary.'
---
body
`)
		fm, _, err := ParseFrontMatter(src)
		if err != nil {
			t.Fatalf("ParseFrontMatter: %v", err)
		}
		if fm.Title != "A title: with a colon" {
			t.Errorf("title = %q", fm.Title)
		}
		if fm.Summary != "Single-quoted summary." {
			t.Errorf("summary = %q", fm.Summary)
		}
	})

	t.Run("CRLF", func(t *testing.T) {
		src := []byte("---\r\ntitle: T\r\nsummary: S\r\ntags: [a, b]\r\n---\r\nbody\r\n")
		fm, body, err := ParseFrontMatter(src)
		if err != nil {
			t.Fatalf("ParseFrontMatter: %v", err)
		}
		if fm.Title != "T" || fm.Summary != "S" || len(fm.Tags) != 2 {
			t.Errorf("fm = %+v", fm)
		}
		if !strings.Contains(string(body), "body") {
			t.Errorf("body = %q", body)
		}
	})

	t.Run("BOM", func(t *testing.T) {
		src := append([]byte("\ufeff"), []byte("---\ntitle: T\nsummary: S\n---\nbody\n")...)
		fm, _, err := ParseFrontMatter(src)
		if err != nil {
			t.Fatalf("ParseFrontMatter: %v", err)
		}
		if fm.Title != "T" {
			t.Errorf("title = %q", fm.Title)
		}
	})

	t.Run("BOM with no front matter", func(t *testing.T) {
		// A file with a BOM but no "---" fence at all: the BOM must still be stripped from the
		// body that comes back, not just from the (unused) front-matter detection.
		plain := []byte("# No front matter\n")
		src := append([]byte("\ufeff"), plain...)
		fm, body, err := ParseFrontMatter(src)
		if err != nil {
			t.Fatalf("ParseFrontMatter: %v", err)
		}
		if fm.Title != "" {
			t.Errorf("title = %q, want empty", fm.Title)
		}
		if string(body) != string(plain) {
			t.Errorf("body = %q, want %q (BOM leaked into the body)", body, plain)
		}
		if strings.Contains(string(body), "\ufeff") {
			t.Error("body still contains the BOM")
		}
	})
}

// TestParseFrontMatterValidation checks that a missing title or summary, or a summary spanning
// more than one line, produces a *ValidationError that errors.As can find, while still returning
// the correctly parsed fm and stripped body.
func TestParseFrontMatterValidation(t *testing.T) {
	cases := []struct {
		name string
		src  string
	}{
		{"missing title", "---\nsummary: S\n---\nbody\n"},
		{"missing summary", "---\ntitle: T\n---\nbody\n"},
		{"blank title", "---\ntitle: \"  \"\nsummary: S\n---\nbody\n"},
		{"summary block scalar |", "---\ntitle: T\nsummary: |\n  line one\n  line two\n---\nbody\n"},
		{"summary block scalar >-", "---\ntitle: T\nsummary: >-\n  line one\n  line two\n---\nbody\n"},
		{"summary block scalar |2", "---\ntitle: T\nsummary: |2\n  line one\n---\nbody\n"},
		{"title block scalar >", "---\ntitle: >\n  line one\n  line two\nsummary: S\n---\nbody\n"},
		{"summary empty value with continuation", "---\ntitle: T\nsummary:\n  line one\n---\nbody\n"},
		{"summary plain-scalar continuation", "---\ntitle: T\nsummary: This is a summary\n  that continues on the next line.\n---\nbody\n"},
		{"title plain-scalar continuation", "---\ntitle: This is a title\n  that continues.\nsummary: S\n---\nbody\n"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			_, body, err := ParseFrontMatter([]byte(c.src))
			if err == nil {
				t.Fatal("expected a validation error")
			}
			var ve *ValidationError
			if !errors.As(err, &ve) {
				t.Fatalf("error is %T, not *ValidationError: %v", err, err)
			}
			if !strings.Contains(string(body), "body") {
				t.Errorf("body should still be stripped despite the validation error, got %q", body)
			}
		})
	}
}

// TestParseFrontMatterOverRealRepo runs ParseFrontMatter over the front door of every entry in
// the actual knowledge repository this module lives in, and over every docs/*.md file, and
// requires it to parse without a structural error. This is the tolerance test the deliverable
// calls for: if any real entry's front matter shape trips the parser, this fails.
func TestParseFrontMatterOverRealRepo(t *testing.T) {
	root, err := filepath.Abs(realRepoRoot)
	if err != nil {
		t.Fatalf("resolve real repo root: %v", err)
	}
	if _, err := os.Stat(filepath.Join(root, "AGENTS.md")); err != nil {
		t.Skipf("real repo root not found at %s (expected when the module moves): %v", root, err)
	}

	entries, err := Discover(root)
	if err != nil {
		t.Fatalf("Discover(%s): %v", root, err)
	}
	if len(entries) == 0 {
		t.Fatal("Discover found no entries in the real repo")
	}

	for _, e := range entries {
		for _, f := range e.Files {
			src, err := os.ReadFile(filepath.Join(root, filepath.FromSlash(f)))
			if err != nil {
				t.Fatalf("read %s: %v", f, err)
			}
			_, _, err = ParseFrontMatter(src)
			if err != nil {
				var ve *ValidationError
				if errors.As(err, &ve) {
					t.Errorf("%s: front matter present but incomplete: %v", f, err)
					continue
				}
				t.Errorf("%s: ParseFrontMatter could not parse this file's front matter at all: %v", f, err)
			}
		}
		if e.Err != nil {
			t.Errorf("entry %s: Err = %v (every real entry should currently have valid front matter)", e.Path, e.Err)
		}
	}
}

// TestDiscover builds a miniature repository and checks the inclusion and exclusion rules.
func TestDiscover(t *testing.T) {
	root := t.TempDir()
	write := func(rel, content string) {
		t.Helper()
		path := filepath.Join(root, filepath.FromSlash(rel))
		if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	fm := "---\ntitle: T\nsummary: S\ntags: [x]\nupdated: 2026-09-13\n---\n"

	write("data/thing.md", fm)
	write("legacy.md", fm)            // sources outside data/ must not be discovered
	write("kb/README.md", fm)         // module docs are not corpus entries
	write("data/index.md", fm)        // reserved files are still excluded inside data/
	write("data/skill/README.md", fm) // reserved directories too
	write("index.md", fm)             // machinery, excluded
	write("AGENTS.md", fm)            // machinery, excluded
	write("plan.md", fm)              // machinery, excluded
	write("README.md", fm)            // machinery, excluded
	write("CLAUDE.md", fm)            // machinery, excluded
	write("data/droplet/README.md", fm)
	write("data/droplet/docs/b.md", "b")
	write("data/droplet/docs/a.md", "a")
	write("data/droplet/scripts/x.sh", "#!/bin/sh\n") // not indexed
	write("skill/README.md", fm)                      // excluded directory
	write(".kb/README.md", fm)                        // excluded directory
	write("data/nodocs/notes.md", fm)                 // directory without a README.md

	entries, err := Discover(root)
	if err != nil {
		t.Fatalf("Discover: %v", err)
	}
	var paths []string
	for _, e := range entries {
		paths = append(paths, e.Path)
	}
	want := []string{"data/droplet/README.md", "data/thing.md"}
	if len(paths) != len(want) {
		t.Fatalf("paths = %v, want %v", paths, want)
	}
	for i := range want {
		if paths[i] != want[i] {
			t.Fatalf("paths = %v, want %v", paths, want)
		}
	}

	dir := entries[0]
	if dir.Kind != KindDir || dir.Dir != "data/droplet" {
		t.Errorf("kind/dir = %q / %q", dir.Kind, dir.Dir)
	}
	if dir.Err != nil {
		t.Errorf("dir.Err = %v, want nil", dir.Err)
	}
	wantFiles := []string{"data/droplet/README.md", "data/droplet/docs/a.md", "data/droplet/docs/b.md"}
	if len(dir.Files) != len(wantFiles) {
		t.Fatalf("files = %v, want %v", dir.Files, wantFiles)
	}
	for i := range wantFiles {
		if dir.Files[i] != wantFiles[i] {
			t.Fatalf("files = %v, want %v", dir.Files, wantFiles)
		}
	}
	if entries[1].Kind != KindFile || !entries[1].HasTag("X") {
		t.Errorf("file entry = %+v", entries[1])
	}
}

// TestDiscoverToleratesOneBadEntry is the case the coordinator's P2.1 review flagged: Discover
// must not abort the whole scan because one entry's front matter is broken. Both entries come
// back; the bad one carries a *ValidationError in Err and a zero-valued Meta, and the good one is
// unaffected.
func TestDiscoverToleratesOneBadEntry(t *testing.T) {
	root := t.TempDir()
	write := func(rel, content string) {
		t.Helper()
		path := filepath.Join(root, filepath.FromSlash(rel))
		if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
			t.Fatal(err)
		}
	}

	write("data/good.md", "---\ntitle: Good\nsummary: This one parses fine.\n---\nbody\n")
	write("data/bad.md", "---\ntitle: Bad\n---\nbody\n") // missing summary

	entries, err := Discover(root)
	if err != nil {
		t.Fatalf("Discover: %v", err)
	}
	if len(entries) != 2 {
		t.Fatalf("got %d entries, want 2: %+v", len(entries), entries)
	}

	byPath := map[string]Entry{}
	for _, e := range entries {
		byPath[e.Path] = e
	}

	good, ok := byPath["data/good.md"]
	if !ok {
		t.Fatal("good.md missing from Discover results")
	}
	if good.Err != nil {
		t.Errorf("good.md: Err = %v, want nil", good.Err)
	}
	if good.Meta.Title != "Good" || good.Meta.Summary != "This one parses fine." {
		t.Errorf("good.md: Meta = %+v", good.Meta)
	}

	bad, ok := byPath["data/bad.md"]
	if !ok {
		t.Fatal("bad.md missing from Discover results")
	}
	if bad.Err == nil {
		t.Fatal("bad.md: Err = nil, want a *ValidationError")
	}
	var ve *ValidationError
	if !errors.As(bad.Err, &ve) {
		t.Fatalf("bad.md: Err is %T, not *ValidationError: %v", bad.Err, bad.Err)
	}
	if ve.Path != "data/bad.md" {
		t.Errorf("bad.md: ValidationError.Path = %q, want %q", ve.Path, "data/bad.md")
	}
	if bad.Meta.Title != "" || bad.Meta.Summary != "" || bad.Meta.Tags != nil {
		t.Errorf("bad.md: Meta = %+v, want the zero value since Err is set", bad.Meta)
	}
}

// TestDiscoverMatchesRealRepoListing computes, independently of Discover's own implementation,
// which paths under the real repo root ought to be entries, and checks that Discover agrees.
func TestDiscoverMatchesRealRepoListing(t *testing.T) {
	root, err := filepath.Abs(realRepoRoot)
	if err != nil {
		t.Fatalf("resolve real repo root: %v", err)
	}
	if _, err := os.Stat(filepath.Join(root, "AGENTS.md")); err != nil {
		t.Skipf("real repo root not found at %s: %v", root, err)
	}

	excludedFiles := map[string]bool{"AGENTS.md": true, "CLAUDE.md": true, "index.md": true, "plan.md": true, "README.md": true}
	excludedDirsHere := map[string]bool{"skill": true, ".kb": true, ".git": true}

	items, err := os.ReadDir(filepath.Join(root, "data"))
	if err != nil {
		t.Fatalf("ReadDir(%s): %v", root, err)
	}
	var want []string
	for _, item := range items {
		name := item.Name()
		if item.IsDir() {
			if excludedDirsHere[name] {
				continue
			}
			if info, err := os.Stat(filepath.Join(root, "data", name, "README.md")); err == nil && !info.IsDir() {
				want = append(want, "data/"+name+"/README.md")
			}
			continue
		}
		if strings.HasSuffix(name, ".md") && !excludedFiles[name] {
			want = append(want, "data/"+name)
		}
	}
	sort.Strings(want)

	entries, err := Discover(root)
	if err != nil {
		t.Fatalf("Discover(%s): %v", root, err)
	}
	var got []string
	for _, e := range entries {
		got = append(got, e.Path)
	}

	if len(got) != len(want) {
		t.Fatalf("Discover paths = %v\nwant (independently computed) = %v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("Discover paths = %v\nwant (independently computed) = %v", got, want)
		}
	}
}

// TestResolve covers every <entry> argument form from the CLI section of plan.md.
func TestResolve(t *testing.T) {
	root := t.TempDir()
	write := func(rel, content string) {
		t.Helper()
		path := filepath.Join(root, filepath.FromSlash(rel))
		if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	fm := "---\ntitle: T\nsummary: S\n---\nbody\n"
	write("data/foo.md", fm)
	write("data/droplet/README.md", fm)

	absRoot, err := filepath.Abs(root)
	if err != nil {
		t.Fatal(err)
	}

	cases := []struct {
		arg      string
		wantPath string
	}{
		{"foo.md", "data/foo.md"},
		{"data/foo.md", "data/foo.md"},
		{"droplet", "data/droplet/README.md"},
		{"droplet/README.md", "data/droplet/README.md"},
		{"data/droplet", "data/droplet/README.md"},
		{"data/droplet/", "data/droplet/README.md"},
		{"data/droplet/README.md", "data/droplet/README.md"},
		{filepath.Join(absRoot, "data/foo.md"), "data/foo.md"},
		{filepath.Join(absRoot, "data/droplet"), "data/droplet/README.md"},
	}
	for _, c := range cases {
		t.Run(c.arg, func(t *testing.T) {
			e, err := Resolve(root, c.arg)
			if err != nil {
				t.Fatalf("Resolve(%q): %v", c.arg, err)
			}
			if e.Path != c.wantPath {
				t.Errorf("Resolve(%q).Path = %q, want %q", c.arg, e.Path, c.wantPath)
			}
		})
	}

	badCases := []string{"nope.md", "nodir", "", "/etc/passwd", filepath.Join(root, "foo.md"), "../foo.md", "data/../foo.md"}
	for _, arg := range badCases {
		t.Run("bad:"+arg, func(t *testing.T) {
			if _, err := Resolve(root, arg); err == nil {
				t.Errorf("Resolve(%q): expected an error", arg)
			}
		})
	}
}

// TestLoadAndBodyHash checks Load reads exactly the entry's files and BodyHash matches a
// concatenation computed independently.
func TestLoadAndBodyHash(t *testing.T) {
	root := t.TempDir()
	write := func(rel, content string) {
		t.Helper()
		path := filepath.Join(root, filepath.FromSlash(rel))
		if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	write("data/droplet/README.md", "---\ntitle: T\nsummary: S\n---\nREADME body\n")
	write("data/droplet/docs/a.md", "a content\n")
	write("data/droplet/docs/b.md", "b content\n")

	entries, err := Discover(root)
	if err != nil {
		t.Fatalf("Discover: %v", err)
	}
	if len(entries) != 1 {
		t.Fatalf("got %d entries, want 1", len(entries))
	}
	e := entries[0]

	files, err := Load(root, e)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if len(files) != 3 {
		t.Fatalf("Load returned %d files, want 3: %v", len(files), files)
	}
	if string(files["data/droplet/docs/a.md"]) != "a content\n" {
		t.Errorf("data/droplet/docs/a.md = %q", files["data/droplet/docs/a.md"])
	}

	got := BodyHash(files, e.Files)
	h := sha256.New()
	for _, f := range e.Files {
		h.Write(files[f])
	}
	want := hex.EncodeToString(h.Sum(nil))
	if got != want {
		t.Errorf("BodyHash = %q, want %q", got, want)
	}

	// Order matters: hashing the same files in a different order must give a different hash.
	reversed := []string{e.Files[2], e.Files[1], e.Files[0]}
	if BodyHash(files, reversed) == got {
		t.Error("BodyHash should depend on the order files are concatenated in")
	}
}
