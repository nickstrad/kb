package chunk

import (
	"encoding/json"
	"flag"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"knowledge/kb/internal/entry"
)

// update regenerates the golden files under testdata/golden from the current Split output. Run
// with: go test -tags fts5 ./internal/chunk -run TestSplitGolden -update
var update = flag.Bool("update", false, "update golden files in testdata/golden")

// goldenRoot is the miniature repository copied from the real knowledge store for these tests:
// tmux-daemons-die-at-logout.md (a plain file entry), droplet/ (README.md + scripts/, which must
// never be chunked) and protobuf/ (README.md + docs/*.md + scripts/), keeping each entry's real
// tree shape.
const goldenRoot = "testdata/repo"

// TestSplitGolden runs Split over every entry in the miniature repo and compares the result to a
// checked-in golden file, plus a battery of assertions that do not depend on the golden content
// at all (see assertInvariants) so a bad golden update cannot hide a real regression.
func TestSplitGolden(t *testing.T) {
	entries, err := entry.Discover(goldenRoot)
	if err != nil {
		t.Fatalf("Discover(%s): %v", goldenRoot, err)
	}
	if len(entries) != 3 {
		var paths []string
		for _, e := range entries {
			paths = append(paths, e.Path)
		}
		t.Fatalf("Discover(%s) found %d entries, want 3: %v", goldenRoot, len(entries), paths)
	}

	for _, e := range entries {
		e := e
		if e.Err != nil {
			t.Fatalf("Discover(%s): entry %s: %v", goldenRoot, e.Path, e.Err)
		}
		t.Run(goldenName(e), func(t *testing.T) {
			files, err := entry.Load(goldenRoot, e)
			if err != nil {
				t.Fatalf("Load: %v", err)
			}
			chunks, err := Split(e, files)
			if err != nil {
				t.Fatalf("Split: %v", err)
			}

			assertInvariants(t, e, chunks)
			checkGolden(t, goldenName(e), chunks)
		})
	}
}

// goldenName is the <entry> golden-file stem: the directory name for a directory entry, or the
// file name without ".md" for a file entry.
func goldenName(e entry.Entry) string {
	if e.Kind == entry.KindDir {
		return e.Dir
	}
	return strings.TrimSuffix(e.Path, ".md")
}

// checkGolden compares chunks to testdata/golden/<name>.json, or (with -update) rewrites it.
func checkGolden(t *testing.T, name string, chunks []Chunk) {
	t.Helper()
	path := filepath.Join("testdata", "golden", name+".json")

	if *update {
		data, err := json.MarshalIndent(chunks, "", "  ")
		if err != nil {
			t.Fatalf("marshal golden: %v", err)
		}
		if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
			t.Fatalf("mkdir golden dir: %v", err)
		}
		if err := os.WriteFile(path, append(data, '\n'), 0o644); err != nil {
			t.Fatalf("write golden %s: %v", path, err)
		}
		return
	}

	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read golden %s: %v (run with -update to create it)", path, err)
	}
	var want []Chunk
	if err := json.Unmarshal(raw, &want); err != nil {
		t.Fatalf("unmarshal golden %s: %v", path, err)
	}
	if !reflect.DeepEqual(chunks, want) {
		got, _ := json.MarshalIndent(chunks, "", "  ")
		t.Errorf("Split output does not match %s.\ngot:\n%s", path, got)
	}
}

// assertInvariants checks properties Split must satisfy regardless of what the golden file says,
// so a stale or wrongly regenerated golden file cannot mask a real bug.
func assertInvariants(t *testing.T, e entry.Entry, chunks []Chunk) {
	t.Helper()
	if len(chunks) == 0 {
		t.Fatal("Split returned no chunks")
	}

	title := strings.TrimSpace(e.Meta.Title)
	summary := strings.TrimSpace(e.Meta.Summary)

	// Chunk 0: the summary chunk (rule 2).
	c0 := chunks[0]
	wantText := title + ": " + summary
	if c0.Ord != 0 || c0.Heading != "" || c0.SourceFile != e.Path || c0.Text != wantText {
		t.Errorf("chunk 0 = %+v, want text %q, heading \"\", source_file %q", c0, wantText, e.Path)
	}

	// Every heading actually used by a section chunk, so the "different section" check below
	// knows what a real heading line for this entry looks like.
	headings := map[string]bool{}
	for _, c := range chunks[1:] {
		headings[c.Heading] = true
	}

	sawReadmeChunk := false
	for i, c := range chunks {
		if c.TextHash != Hash(c.Text) {
			t.Errorf("chunk %d: TextHash %q does not match Hash(Text)", i, c.TextHash)
		}
		if c.Ord != i {
			t.Errorf("chunk %d: Ord = %d, want %d (Ord must be sequential)", i, c.Ord, i)
		}

		// scripts/ must never be chunked (P2.3 deliverable requirement).
		if strings.Contains(c.SourceFile, "/scripts/") || strings.HasSuffix(c.SourceFile, ".sh") {
			t.Errorf("chunk %d sourced from a script file: %s", i, c.SourceFile)
		}
		if strings.Contains(c.Text, "#!/bin/") || strings.Contains(c.Text, "#!/usr/bin/env") {
			t.Errorf("chunk %d text looks like it leaked script content: %.80q", i, c.Text)
		}

		if c.SourceFile == e.Path {
			sawReadmeChunk = true
		}
		if e.Kind == entry.KindDir && strings.HasPrefix(c.SourceFile, e.Dir+"/docs/") && !sawReadmeChunk {
			t.Errorf("chunk %d from %s (a docs/*.md file) appears before any README chunk", i, c.SourceFile)
		}

		if i == 0 {
			continue // the summary chunk carries no context line
		}

		context := title + " › " + c.Heading
		if c.Text != context && !strings.HasPrefix(c.Text, context+"\n") {
			t.Errorf("chunk %d (heading %q): text does not start with the context line %q: %.60q",
				i, c.Heading, context, c.Text)
		}

		if len(c.Text) > MaxChunkChars {
			body := strings.TrimPrefix(c.Text, context)
			body = strings.TrimPrefix(body, "\n")
			if strings.Contains(strings.TrimSpace(body), "\n\n") {
				t.Errorf("chunk %d (heading %q, %d chars) has a paragraph break and should have "+
					"been split at it; only a single unsplittable paragraph/code block may exceed "+
					"MaxChunkChars", i, c.Heading, len(c.Text))
			}
		}

		for h := range headings {
			if h == "" || h == c.Heading {
				continue
			}
			if strings.Contains(c.Text, "\n## "+h) {
				t.Errorf("chunk %d (heading %q) contains a literal heading line for a different "+
					"section (%q)", i, c.Heading, h)
			}
		}
	}
}
