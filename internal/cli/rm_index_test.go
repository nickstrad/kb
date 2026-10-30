package cli

import (
	"context"
	"path/filepath"
	"testing"
	"time"
)

// TestRmReresolvesLinks (Q105): removing the entry a [[wikilink]] resolved to re-points the link at
// the next match (an alias here) instead of leaving it dangling until the next reindex.
func TestRmReresolvesLinks(t *testing.T) {
	t.Parallel()
	useFakeEmbedder(t)
	root := newTempRoot(t)
	writeFile(t, filepath.Join(root, "data", "src.md"), validFrontMatter("Src", "Links to note.")+"\nSee [[note]].\n")
	writeFile(t, filepath.Join(root, "data", "note.md"), validFrontMatter("Note", "The basename match."))
	writeFile(t, filepath.Join(root, "data", "other.md"),
		"---\ntitle: Other\nsummary: The alias match.\ntags: [test]\naliases: [note]\nupdated: 2026-09-13\n---\n\n## S\n\nBody text here.\n")
	if out, errOut, code := run(t, "reindex", "--all"); code != 0 {
		t.Fatalf("reindex: %d %q %q", code, out, errOut)
	}
	target := func() string {
		t.Helper()
		edges, err := openTestStore(t, root).Edges(context.Background())
		if err != nil {
			t.Fatal(err)
		}
		for _, e := range edges {
			if e.SrcPath == "data/src.md" && e.DstPath == "note" {
				return e.DstEntryPath
			}
		}
		t.Fatalf("no wikilink edge from data/src.md in %v", edges)
		return ""
	}
	if got := target(); got != "data/note.md" {
		t.Fatalf("before rm the link resolves to %q, want data/note.md", got)
	}
	if out, errOut, code := run(t, "rm", "data/note.md"); code != 0 {
		t.Fatalf("rm: %d %q %q", code, out, errOut)
	}
	if got := target(); got != "data/other.md" {
		t.Errorf("after rm the link resolves to %q, want data/other.md (the alias match)", got)
	}
}

// TestIndexLastReviewedUsesInjectedClock (Q96): the "Last reviewed" date is the clock's, so a
// pinned clock pins index.md.
func TestIndexLastReviewedUsesInjectedClock(t *testing.T) {
	t.Parallel()
	root := newTempRoot(t)
	writeFile(t, filepath.Join(root, "data", "a.md"), validFrontMatter("A", "An entry."))
	orig := depsFor(t).now
	t.Cleanup(func() { depsFor(t).now = orig })
	depsFor(t).now = func() time.Time { return time.Date(2031, 2, 3, 23, 0, 0, 0, time.UTC) }
	if out, errOut, code := run(t, "index"); code != 0 {
		t.Fatalf("index: %d %q %q", code, out, errOut)
	}
	assertIndexContains(t, root, "Last reviewed: 2031-02-03", true)
}
