package reindex

import (
	"bytes"
	"context"
	"io"
	"math"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"knowledge/kb/internal/embed"
	"knowledge/kb/internal/embed/fake"
	"knowledge/kb/internal/entry"
	"knowledge/kb/internal/store"
)

// testDim matches the vec0 column declared in schema.sql (FLOAT[768]).
const testDim = 768

// sourceRepo is the miniature repository already checked in for the chunk package's golden
// tests: a plain file entry (tmux-daemons-die-at-logout.md) and a directory entry (droplet/,
// README.md plus a non-Markdown scripts/ directory that must never be indexed). This test copies
// it into a throwaway directory so it can edit and delete files without touching the checked-in
// fixture.
const sourceRepo = "../chunk/testdata/repo/data"

// countingEmbedder wraps another Embedder and counts how many times Embed is called (as opposed
// to how many texts were embedded across those calls), as an independent check on
// Summary.EmbedCalls, which reindex itself also counts.
type countingEmbedder struct {
	embed.Embedder
	calls int
}

func (c *countingEmbedder) Embed(ctx context.Context, texts []string) ([][]float32, error) {
	c.calls++
	return c.Embedder.Embed(ctx, texts)
}

// newTestRepo copies tmux-daemons-die-at-logout.md and droplet/ into a fresh temp directory and
// returns its path.
func newTestRepo(t *testing.T) string {
	t.Helper()
	root := t.TempDir()
	copyFile(t, filepath.Join(sourceRepo, "tmux-daemons-die-at-logout.md"), filepath.Join(root, "data", "tmux-daemons-die-at-logout.md"))
	copyTree(t, filepath.Join(sourceRepo, "droplet"), filepath.Join(root, "data", "droplet"))
	return root
}

func copyFile(t *testing.T, src, dst string) {
	t.Helper()
	data, err := os.ReadFile(src)
	if err != nil {
		t.Fatalf("read %s: %v", src, err)
	}
	if err := os.MkdirAll(filepath.Dir(dst), 0o755); err != nil {
		t.Fatalf("mkdir %s: %v", filepath.Dir(dst), err)
	}
	if err := os.WriteFile(dst, data, 0o644); err != nil {
		t.Fatalf("write %s: %v", dst, err)
	}
}

func copyTree(t *testing.T, src, dst string) {
	t.Helper()
	entries, err := os.ReadDir(src)
	if err != nil {
		t.Fatalf("read dir %s: %v", src, err)
	}
	if err := os.MkdirAll(dst, 0o755); err != nil {
		t.Fatalf("mkdir %s: %v", dst, err)
	}
	for _, e := range entries {
		s := filepath.Join(src, e.Name())
		d := filepath.Join(dst, e.Name())
		if e.IsDir() {
			copyTree(t, s, d)
			continue
		}
		copyFile(t, s, d)
	}
}

// newTestStore opens a temp database and records embed_meta for emb.
func newTestStore(t *testing.T, emb embed.Embedder) *store.Store {
	t.Helper()
	s, err := store.Open(filepath.Join(t.TempDir(), "kb.sqlite"))
	if err != nil {
		t.Fatalf("store.Open: %v", err)
	}
	t.Cleanup(func() { s.Close() })
	if err := s.EnsureEmbedMeta(emb.Model(), emb.Dim()); err != nil {
		t.Fatalf("EnsureEmbedMeta: %v", err)
	}
	return s
}

// ceilDiv is the "roughly ceil(chunks/16)" arithmetic the acceptance criteria ask for.
func ceilDiv(a, b int) int {
	return int(math.Ceil(float64(a) / float64(b)))
}

// TestReindexLifecycle drives reindex.All through the scenarios P2.4 must cover: a clean first
// pass, a fully-skipped second pass, an edit that reindexes and re-embeds only the changed
// entry/chunk, a broken entry that is warned about rather than aborting the run, and a deleted
// entry that is removed from the database.
func TestReindexLifecycle(t *testing.T) {
	root := newTestRepo(t)
	base := fake.New("fake-embed", testDim)
	emb := &countingEmbedder{Embedder: base}
	s := newTestStore(t, base)
	ctx := context.Background()

	newOpts := func() Options {
		return Options{
			Root:      root,
			Store:     s,
			Embedder:  emb,
			BatchSize: 16,
			Stdout:    &bytes.Buffer{},
			Stderr:    &bytes.Buffer{},
		}
	}

	// --- Pass 1: everything is new. -----------------------------------------------------
	sum1, err := All(ctx, newOpts())
	if err != nil {
		t.Fatalf("pass 1: All: %v", err)
	}
	if sum1.Scanned != 2 {
		t.Fatalf("pass 1: Scanned = %d, want 2", sum1.Scanned)
	}
	if sum1.Reindexed != sum1.Scanned {
		t.Errorf("pass 1: Reindexed = %d, want %d (Scanned)", sum1.Reindexed, sum1.Scanned)
	}
	if sum1.Skipped != 0 || sum1.Failed != 0 {
		t.Errorf("pass 1: Skipped=%d Failed=%d, want 0, 0", sum1.Skipped, sum1.Failed)
	}
	if sum1.Chunks <= 0 {
		t.Errorf("pass 1: Chunks = %d, want > 0", sum1.Chunks)
	}
	// Batching is per entry (an entry's own existing-embeddings lookup only makes sense scoped
	// to itself), not across the whole run, so with two entries that each have well under 16
	// chunks (6 and 8: internal/chunk/testdata/golden/{tmux-daemons-die-at-logout,droplet}.json)
	// the exact count is one call per entry, not ceil(total chunks/16).
	wantCalls := sum1.Reindexed
	if sum1.EmbedCalls != wantCalls {
		t.Errorf("pass 1: EmbedCalls = %d, want %d (one batch per entry, each under 16 chunks)", sum1.EmbedCalls, wantCalls)
	}
	if got := ceilDiv(sum1.Chunks, 16); got != 1 {
		t.Fatalf("test fixture assumption broken: ceil(%d/16) = %d, want 1 (rewrite the EmbedCalls check above if the fixture grew)", sum1.Chunks, got)
	}
	if emb.calls != sum1.EmbedCalls {
		t.Errorf("pass 1: embedder saw %d calls, Summary reports %d", emb.calls, sum1.EmbedCalls)
	}
	if n := countRows(t, s, "SELECT count(*) FROM chunks_vec"); n != sum1.Chunks {
		t.Errorf("pass 1: chunks_vec has %d rows, want %d (chunks)", n, sum1.Chunks)
	}

	// --- Pass 2: nothing changed, everything is skipped. ---------------------------------
	emb.calls = 0
	sum2, err := All(ctx, newOpts())
	if err != nil {
		t.Fatalf("pass 2: All: %v", err)
	}
	if sum2.Skipped != sum2.Scanned || sum2.Scanned != 2 {
		t.Errorf("pass 2: Skipped=%d Scanned=%d, want both 2", sum2.Skipped, sum2.Scanned)
	}
	if sum2.Reindexed != 0 || sum2.Failed != 0 {
		t.Errorf("pass 2: Reindexed=%d Failed=%d, want 0, 0", sum2.Reindexed, sum2.Failed)
	}
	if sum2.EmbedCalls != 0 || emb.calls != 0 {
		t.Errorf("pass 2: EmbedCalls=%d embedder calls=%d, want 0, 0", sum2.EmbedCalls, emb.calls)
	}

	// --- Pass 3: edit one section of tmux-daemons-die-at-logout.md; only that entry should
	// be reindexed, and only the changed chunk should need a fresh embedding. -------------
	tmuxPath := filepath.Join(root, "data", "tmux-daemons-die-at-logout.md")
	appendToSection(t, tmuxPath, "## Edge cases",
		"- Added by TestReindexLifecycle to prove a targeted edit reindexes only one chunk.")

	emb.calls = 0
	sum3, err := All(ctx, newOpts())
	if err != nil {
		t.Fatalf("pass 3: All: %v", err)
	}
	if sum3.Reindexed != 1 {
		t.Fatalf("pass 3: Reindexed = %d, want 1 (only the edited entry)", sum3.Reindexed)
	}
	if sum3.Skipped != 1 {
		t.Errorf("pass 3: Skipped = %d, want 1 (droplet unchanged)", sum3.Skipped)
	}
	if sum3.EmbedCalls != 1 || emb.calls != 1 {
		t.Errorf("pass 3: EmbedCalls=%d embedder calls=%d, want 1, 1 (one changed chunk, one batch)", sum3.EmbedCalls, emb.calls)
	}

	// --- Pass 4: add a file entry with no summary; it is warned about and counted Failed,
	// but the run does not abort. -----------------------------------------------------------
	brokenPath := filepath.Join(root, "data", "broken.md")
	broken := "---\ntitle: Missing its summary\n---\n\n# Broken\n\nThis entry has no summary field.\n"
	if err := os.WriteFile(brokenPath, []byte(broken), 0o644); err != nil {
		t.Fatalf("write broken.md: %v", err)
	}

	opts4 := newOpts()
	stderr4 := &bytes.Buffer{}
	opts4.Stderr = stderr4
	sum4, err := All(ctx, opts4)
	if err != nil {
		t.Fatalf("pass 4: All returned an error for a bad entry, want nil (warn-and-skip): %v", err)
	}
	if sum4.Failed != 1 {
		t.Errorf("pass 4: Failed = %d, want 1", sum4.Failed)
	}
	if sum4.Scanned != 3 {
		t.Errorf("pass 4: Scanned = %d, want 3", sum4.Scanned)
	}
	if sum4.Reindexed != 0 || sum4.Skipped != 2 {
		t.Errorf("pass 4: Reindexed=%d Skipped=%d, want 0, 2", sum4.Reindexed, sum4.Skipped)
	}
	if !strings.Contains(stderr4.String(), "broken.md") {
		t.Errorf("pass 4: stderr = %q, want a warning mentioning broken.md", stderr4.String())
	}

	// --- Pass 5: delete the droplet/ directory entry; its DB rows must be removed. --------
	if err := os.RemoveAll(filepath.Join(root, "data", "droplet")); err != nil {
		t.Fatalf("remove droplet/: %v", err)
	}
	sum5, err := All(ctx, newOpts())
	if err != nil {
		t.Fatalf("pass 5: All: %v", err)
	}
	if sum5.Removed != 1 {
		t.Errorf("pass 5: Removed = %d, want 1", sum5.Removed)
	}
	if sum5.Scanned != 2 { // tmux-daemons-die-at-logout.md, broken.md
		t.Errorf("pass 5: Scanned = %d, want 2", sum5.Scanned)
	}
	if got, err := s.GetEntryByPath("data/droplet/README.md"); err != nil {
		t.Fatalf("GetEntryByPath(droplet/README.md): %v", err)
	} else if got != nil {
		t.Errorf("droplet/README.md is still in the database after its file was deleted")
	}

	// --- sqlite3-level invariant: every chunk has exactly one vector. --------------------
	nChunks := countRows(t, s, "SELECT count(*) FROM chunks")
	nVecs := countRows(t, s, "SELECT count(*) FROM chunks_vec")
	if nChunks != nVecs {
		t.Errorf("chunks = %d, chunks_vec = %d, want equal", nChunks, nVecs)
	}
}

// TestResolveOne checks One against a freshly resolved entry, independent of All, including that
// it does not touch any entry other than the one it was given.
func TestResolveOne(t *testing.T) {
	root := newTestRepo(t)
	base := fake.New("fake-embed", testDim)
	s := newTestStore(t, base)
	ctx := context.Background()

	e, err := entry.Resolve(root, "tmux-daemons-die-at-logout.md")
	if err != nil {
		t.Fatalf("Resolve: %v", err)
	}

	opts := Options{
		Root: root, Store: s, Embedder: base, BatchSize: 16,
		Stdout: io.Discard, Stderr: io.Discard,
	}
	sum, err := One(ctx, opts, e)
	if err != nil {
		t.Fatalf("One: %v", err)
	}
	if sum.Reindexed != 1 || sum.Scanned != 1 {
		t.Errorf("One: Reindexed=%d Scanned=%d, want 1, 1", sum.Reindexed, sum.Scanned)
	}
	if got, err := s.GetEntryByPath("data/droplet/README.md"); err != nil {
		t.Fatalf("GetEntryByPath: %v", err)
	} else if got != nil {
		t.Error("One(tmux) must not index droplet/README.md, which was never discovered")
	}
}

// appendToSection inserts a line right after a given "## heading" line in a Markdown file, so a
// test can make a targeted, single-chunk edit without hand-writing a whole new file.
func appendToSection(t *testing.T, path, heading, line string) {
	t.Helper()
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read %s: %v", path, err)
	}
	lines := strings.Split(string(data), "\n")
	idx := -1
	for i, l := range lines {
		if strings.TrimSpace(l) == heading {
			idx = i
			break
		}
	}
	if idx < 0 {
		t.Fatalf("heading %q not found in %s", heading, path)
	}
	// Insert after the heading's blank line and first content line, i.e. append to the section
	// rather than right under the heading, so the edit lands inside the existing paragraph block.
	insertAt := idx + 1
	for insertAt < len(lines) && strings.TrimSpace(lines[insertAt]) != "" {
		insertAt++
	}
	if insertAt >= len(lines) {
		insertAt = len(lines)
	} else {
		insertAt++ // past the blank line, to the start of the section body
	}
	// Find end of this section's first paragraph/list block to append after it.
	end := insertAt
	for end < len(lines) && strings.TrimSpace(lines[end]) != "" {
		end++
	}
	out := make([]string, 0, len(lines)+1)
	out = append(out, lines[:end]...)
	out = append(out, line)
	out = append(out, lines[end:]...)
	if err := os.WriteFile(path, []byte(strings.Join(out, "\n")), 0o644); err != nil {
		t.Fatalf("write %s: %v", path, err)
	}
}

// countRows runs a "SELECT count(*) FROM ..." query and returns the result.
func countRows(t *testing.T, s *store.Store, query string) int {
	t.Helper()
	var n int
	if err := s.DB().QueryRow(query).Scan(&n); err != nil {
		t.Fatalf("query %q: %v", query, err)
	}
	return n
}

// TestReindexChunkSplitFailureIsPerEntry covers the regression the reviewer found: a plain
// (non-*entry.ValidationError) error from chunk.Split — here, a directory entry whose front door
// (README.md) has perfectly good front matter but whose secondary docs/bad.md opens a front
// matter fence ("---") that never closes — must be warned about and counted as Failed like any
// other bad entry, not treated as a run-aborting error. Before the fix, only a
// *entry.ValidationError from chunk.Split was handled this way, so this case aborted All before
// it ever reached the second, healthy entry.
func TestReindexChunkSplitFailureIsPerEntry(t *testing.T) {
	root := t.TempDir()
	copyFile(t, filepath.Join(sourceRepo, "tmux-daemons-die-at-logout.md"), filepath.Join(root, "data", "tmux-daemons-die-at-logout.md"))

	readme := "---\ntitle: Broken directory entry\nsummary: Its docs/bad.md has an unclosed front matter fence.\n---\n\n" +
		"# Broken directory entry\n\nSee docs/bad.md.\n"
	if err := os.MkdirAll(filepath.Join(root, "data", "brokendir", "docs"), 0o755); err != nil {
		t.Fatalf("mkdir brokendir/docs: %v", err)
	}
	if err := os.WriteFile(filepath.Join(root, "data", "brokendir", "README.md"), []byte(readme), 0o644); err != nil {
		t.Fatalf("write brokendir/README.md: %v", err)
	}
	// "---" opens a front-matter block that is never closed by a matching "---" line: a plain
	// structural error from entry.ParseFrontMatter, not a *entry.ValidationError.
	bad := "---\ntitle: Bad doc\n\nThis front matter fence never closes.\n"
	if err := os.WriteFile(filepath.Join(root, "data", "brokendir", "docs", "bad.md"), []byte(bad), 0o644); err != nil {
		t.Fatalf("write brokendir/docs/bad.md: %v", err)
	}

	base := fake.New("fake-embed", testDim)
	s := newTestStore(t, base)
	ctx := context.Background()
	stderr := &bytes.Buffer{}

	sum, err := All(ctx, Options{
		Root: root, Store: s, Embedder: base, BatchSize: 16,
		Stdout: io.Discard, Stderr: stderr,
	})
	if err != nil {
		t.Fatalf("All returned an error for a chunk.Split failure, want nil (warn-and-skip): %v", err)
	}
	if sum.Scanned != 2 {
		t.Fatalf("Scanned = %d, want 2", sum.Scanned)
	}
	if sum.Failed != 1 {
		t.Errorf("Failed = %d, want 1 (brokendir/README.md)", sum.Failed)
	}
	if sum.Reindexed != 1 {
		t.Errorf("Reindexed = %d, want 1 (tmux-daemons-die-at-logout.md, unaffected)", sum.Reindexed)
	}
	if !strings.Contains(stderr.String(), "warning:") {
		t.Errorf("stderr = %q, want a warning line", stderr.String())
	}
	if got, err := s.GetEntryByPath("data/tmux-daemons-die-at-logout.md"); err != nil || got == nil {
		t.Errorf("the healthy entry must still be indexed: GetEntryByPath = %v, %v", got, err)
	}
	if got, err := s.GetEntryByPath("data/brokendir/README.md"); err != nil {
		t.Fatalf("GetEntryByPath(brokendir/README.md): %v", err)
	} else if got != nil {
		t.Error("brokendir/README.md must not be indexed: its docs/bad.md failed to chunk")
	}
}

// TestSummaryStringFormat pins the exact one-line summary format from plan.md's P2.4 row,
// including the optional ", N failed" / ", N removed" clauses.
func TestSummaryStringFormat(t *testing.T) {
	cases := []struct {
		sum  Summary
		want string
	}{
		{
			sum:  Summary{Scanned: 3, Reindexed: 2, Skipped: 1, Chunks: 14, EmbedCalls: 2},
			want: "entries: 3 scanned, 2 reindexed, 1 skipped; chunks: 14; embed calls: 2",
		},
		{
			sum:  Summary{Scanned: 3, Reindexed: 1, Skipped: 1, Failed: 1, Chunks: 6, EmbedCalls: 1},
			want: "entries: 3 scanned, 1 reindexed, 1 skipped, 1 failed; chunks: 6; embed calls: 1",
		},
		{
			sum:  Summary{Scanned: 2, Reindexed: 0, Skipped: 2, Removed: 1},
			want: "entries: 2 scanned, 0 reindexed, 2 skipped, 1 removed; chunks: 0; embed calls: 0",
		},
		{
			sum:  Summary{Scanned: 4, Reindexed: 1, Skipped: 1, Failed: 1, Removed: 1, Chunks: 3, EmbedCalls: 1},
			want: "entries: 4 scanned, 1 reindexed, 1 skipped, 1 failed, 1 removed; chunks: 3; embed calls: 1",
		},
	}
	for _, c := range cases {
		if got := c.sum.String(); got != c.want {
			t.Errorf("Summary%+v.String() = %q, want %q", c.sum, got, c.want)
		}
	}
}

// TestReindexNeverStoresDocPrefix confirms embed.DocPrefix is prepended only to the text handed to
// the embedder, never to what lands in chunks.text: a search over stored text should see the
// user's Markdown, not the embedding-prefix implementation detail.
func TestReindexNeverStoresDocPrefix(t *testing.T) {
	root := newTestRepo(t)
	base := fake.New("fake-embed", testDim)
	s := newTestStore(t, base)
	ctx := context.Background()

	if _, err := All(ctx, Options{
		Root: root, Store: s, Embedder: base, BatchSize: 16,
		Stdout: io.Discard, Stderr: io.Discard,
	}); err != nil {
		t.Fatalf("All: %v", err)
	}

	entries, err := s.ListEntries()
	if err != nil {
		t.Fatalf("ListEntries: %v", err)
	}
	if len(entries) == 0 {
		t.Fatal("no entries were indexed")
	}
	for _, e := range entries {
		chunks, err := s.ListChunks(e.ID)
		if err != nil {
			t.Fatalf("ListChunks(%s): %v", e.Path, err)
		}
		for _, c := range chunks {
			if strings.HasPrefix(c.Text, embed.DocPrefix) {
				t.Errorf("%s chunk %d: stored text starts with embed.DocPrefix %q: %q", e.Path, c.Ord, embed.DocPrefix, c.Text)
			}
		}
	}
}
