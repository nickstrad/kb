// Package reindex rebuilds the SQLite index from the knowledge repository's Markdown files. It
// backs `kb reindex`, and `kb add`/`kb edit` use One to reindex the entry they just wrote.
//
// Reindexing an entry is a three-way comparison (chunking rule 6): an entry whose body_hash is
// unchanged is skipped; otherwise every chunk is recomputed, and a chunk whose text_hash already
// exists for that entry reuses its stored embedding. Batching (Options.BatchSize texts per
// embedder call) is per entry, because the embedding-reuse lookup is per entry and each entry
// commits as soon as its own chunks are ready.
//
// Each entry is written with its tags, aliases and outgoing links (edges, PLAN 5.2): the
// relative Markdown links and [[wikilinks]] of every indexed file (Links) plus the front-matter
// related, supersedes and depends_on lists. Both All and One end with Store.ResolveEdges, which
// points every edge at the entry it names now.
//
// A bad entry (invalid front matter, an unreadable file, a broken docs/*.md fence) is warned about
// on Stderr and counted in Summary.Failed; the run continues. An embedder or store error aborts
// the run and is returned as an *errs.Error that keeps the inner kind (Unavailable for a missing
// model) and names the entry.
package reindex

import (
	"context"
	"errors"
	"fmt"
	"io"
	"slices"

	"github.com/nickstrad/kb/internal/assert"
	"github.com/nickstrad/kb/internal/chunk"
	"github.com/nickstrad/kb/internal/config"
	"github.com/nickstrad/kb/internal/embed"
	"github.com/nickstrad/kb/internal/entry"
	"github.com/nickstrad/kb/internal/errs"
	"github.com/nickstrad/kb/internal/links"
	"github.com/nickstrad/kb/internal/store"
)

// defaultBatchSize is used when Options.BatchSize is not positive: 16 texts per embedder call.
const defaultBatchSize = 16

// Options configures a reindex run. Store must already have EnsureEmbedMeta called on it for
// Embedder (the caller decides what to do about a mismatch; see cli/reindex.go). Force skips the
// body_hash comparison, which the CLI sets after ResetIndex for `kb reindex --all` on a mismatch.
// Sources is the store's [[sources]] list, which All discovers; One does not read it. Discover is
// how All lists the entries on disk; nil means entry.Discover (a test seam: All calls it twice,
// see All).
type Options struct {
	Root      string
	Sources   []config.Source
	Store     store.Index
	Embedder  embed.Embedder
	BatchSize int
	Stdout    io.Writer
	Stderr    io.Writer
	Force     bool
	Discover  func(root string, sources []config.Source) ([]entry.Entry, []entry.Skipped, error)
}

// Summary tallies one reindex run for the one-line report printed at the end and for tests.
// Scanned counts every entry attempted (Reindexed + Skipped + Failed); Removed counts DB entries
// deleted because their file is no longer on disk (All only — One never removes anything, since it
// only ever sees an entry that Resolve found on disk).
type Summary struct {
	Scanned    int
	Reindexed  int
	Skipped    int
	Failed     int
	Chunks     int
	EmbedCalls int
	Removed    int
}

// String renders the one-line summary: "entries: N scanned, M reindexed,
// K skipped; chunks: X; embed calls: Y", with ", F failed" and ", R removed" appended to the
// entries clause only when non-zero.
func (s Summary) String() string {
	entries := fmt.Sprintf("entries: %d scanned, %d reindexed, %d skipped", s.Scanned, s.Reindexed, s.Skipped)
	if s.Failed > 0 {
		entries += fmt.Sprintf(", %d failed", s.Failed)
	}
	if s.Removed > 0 {
		entries += fmt.Sprintf(", %d removed", s.Removed)
	}
	return fmt.Sprintf("%s; chunks: %d; embed calls: %d", entries, s.Chunks, s.EmbedCalls)
}

// All reindexes every entry Discover finds in o.Sources under o.Root, then deletes any DB entry
// whose path no longer exists on disk (Summary.Removed). It stops and returns an error the first
// time an entry fails for a reason other than its own content (an embedder or database failure);
// a bad entry's own content (missing front matter, unreadable file) never stops the run — see the
// package doc. A path Discover skipped (a symlink out of the root) is one warning on Stderr.
//
// A source directory that does not exist yet is an empty source, not an error, and a store with
// no sources has nothing to index (a warning says so): in both cases rows from earlier runs are
// pruned, since the index is derived from what is on disk.
//
// Pruning and concurrent writers: another kb process may add an entry after this run's Discover.
// That entry's row is then in Entries but not in the first discovery, and pruning on the
// first discovery alone would delete it. Every writer (kb add, kb edit) puts the file on disk
// before it commits the row, so a row that Entries returns had its file on disk before
// Entries ran. All therefore discovers again after Entries and prunes only the rows that
// neither discovery found: a row whose file exists at prune time is never removed.
func All(ctx context.Context, o Options) (Summary, error) {
	sum, err := all(ctx, o)
	if err != nil {
		return sum, boundary(err, "reindex.All")
	}
	return sum, nil
}

func all(ctx context.Context, o Options) (Summary, error) {
	if err := preconditions(ctx, o); err != nil {
		return Summary{}, err
	}
	o.Stdout = orDiscard(o.Stdout)
	o.Stderr = orDiscard(o.Stderr)
	var sum Summary

	if len(o.Sources) == 0 {
		fmt.Fprintln(o.Stderr, "warning: this store has no [[sources]] in its config: nothing to index")
	}
	discover := o.Discover
	if discover == nil {
		discover = entry.Discover
	}
	entries, skipped, err := discover(o.Root, o.Sources)
	if err != nil {
		return sum, err
	}
	for _, s := range skipped {
		fmt.Fprintf(o.Stderr, "warning: skipped %s\n", s)
	}

	for _, e := range entries {
		if err := reindexOne(ctx, o, e, &sum); err != nil {
			return sum, err
		}
	}

	rows, err := o.Store.Entries(ctx)
	if err != nil {
		return sum, fmt.Errorf("list existing entries: %w", err)
	}
	var stored []string
	for _, row := range rows {
		stored = append(stored, row.Path)
	}
	var now []entry.Entry
	if len(pruneSet(stored, entries, nil)) > 0 {
		// Re-check on disk now, after Entries: see the doc comment.
		if now, _, err = discover(o.Root, o.Sources); err != nil {
			return sum, err
		}
	}
	for _, path := range pruneSet(stored, entries, now) {
		if err := o.Store.DeleteEntry(ctx, path); err != nil {
			return sum, fmt.Errorf("remove %s: %w", path, err)
		}
		sum.Removed++
		fmt.Fprintf(o.Stdout, "removed %s (no longer on disk)\n", path)
	}
	if err := o.Store.ResolveEdges(ctx); err != nil {
		return sum, fmt.Errorf("resolve links: %w", err)
	}
	return sum, nil
}

// pruneSet is the rows to delete after a run: the stored paths that neither the first discovery
// nor the second (now, after Entries) found on disk, in stored order.
func pruneSet(stored []string, first, now []entry.Entry) []string {
	onDisk := make(map[string]bool, len(first)+len(now))
	for _, e := range first {
		onDisk[e.Path] = true
	}
	for _, e := range now {
		onDisk[e.Path] = true
	}
	var gone []string
	for _, p := range stored {
		if !onDisk[p] {
			gone = append(gone, p)
		}
	}
	return gone
}

// One reindexes a single entry, typically the result of entry.Resolve. It never rewrites or
// removes any other entry; it does re-resolve every edge (ResolveEdges), since a new or changed
// entry can be the target of another entry's link.
func One(ctx context.Context, o Options, e entry.Entry) (Summary, error) {
	if err := preconditions(ctx, o); err != nil {
		return Summary{}, err
	}
	o.Stdout = orDiscard(o.Stdout)
	o.Stderr = orDiscard(o.Stderr)
	var sum Summary
	if err := reindexOne(ctx, o, e, &sum); err != nil {
		return sum, boundary(err, "reindex.One")
	}
	if err := o.Store.ResolveEdges(ctx); err != nil {
		return sum, boundary(fmt.Errorf("resolve links: %w", err), "reindex.One")
	}
	return sum, nil
}

// boundary makes a run-aborting error well-formed: it keeps the inner kind (an Unavailable model
// stays exit 2, a store driver failure stays Internal) and the inner message, which names the
// entry.
func boundary(err error, op string) error {
	return errs.Wrap(err, errs.KindOf(err), op, "%v", err)
}

// preconditions checks the run's inputs before any work. A nil Store or Embedder, or an embed_meta
// that was never recorded, is a caller bug (the CLI always runs EnsureEmbedMeta first): an
// assertion (TESTING.md §3.5). embed_meta that holds a different model is not a bug: another kb
// process, or an edited config, can change it between EnsureEmbedMeta and here, so it is a
// Conflict error that says to reindex --all. A read error is left for the run itself to report.
func preconditions(ctx context.Context, o Options) error {
	assert.Pre(o.Store != nil && o.Embedder != nil, "reindex: nil Store or Embedder")
	m, ok, err := o.Store.EmbedMeta(ctx)
	if err != nil {
		return nil
	}
	assert.Pre(ok, "reindex: embed_meta not recorded; call EnsureEmbedMeta first")
	want := store.EmbedMeta{Model: o.Embedder.Model(), Dim: o.Embedder.Dim()}
	if ok && m != want {
		mismatch := &store.EmbedMetaMismatchError{StoredModel: m.Model, StoredDim: m.Dim, WantModel: want.Model, WantDim: want.Dim}
		return errs.Wrap(mismatch, errs.Conflict, "reindex.preconditions", "%s", mismatch.Error())
	}
	return nil
}

// orDiscard returns w unchanged, or io.Discard when w is nil, so All and One never write through a
// nil Options.Stdout/Stderr.
func orDiscard(w io.Writer) io.Writer {
	if w == nil {
		return io.Discard
	}
	return w
}

// reindexOne processes one entry into sum, per the package doc: a per-entry content problem is
// warned about and counted as Failed without returning an error; anything else that would keep
// failing for every other entry too (a database or embedder error) is returned so the caller
// aborts the run.
func reindexOne(ctx context.Context, o Options, e entry.Entry, sum *Summary) error {
	sum.Scanned++

	if e.Err != nil {
		// e.Err (a *entry.ValidationError or a wrapped read/parse error) already names e.Path in
		// its own Error() string, so printing e.Path again here would show it twice.
		fmt.Fprintf(o.Stderr, "warning: %v\n", e.Err)
		sum.Failed++
		return nil
	}

	files, err := entry.Load(o.Root, e)
	if err != nil {
		// entry.Load's error already names the specific file it failed on.
		fmt.Fprintf(o.Stderr, "warning: %v\n", err)
		sum.Failed++
		return nil
	}
	bodyHash := entry.BodyHash(files, e.Files)

	existing, err := o.Store.Entry(ctx, e.Path)
	if err != nil {
		return fmt.Errorf("reindex %s: %w", e.Path, err)
	}
	if existing != nil && !o.Force && existing.BodyHash == bodyHash {
		sum.Skipped++
		return nil
	}

	// Every failure chunk.Split can return is about this entry's own content — a missing
	// title/summary (*entry.ValidationError), or a secondary file (docs/*.md) with a structural
	// problem such as an unclosed front-matter fence — never about the database or the embedder,
	// so all of them are per-entry content failures: warn and move on to the next entry rather
	// than aborting the whole run. chunk.Split's errors already name e.Path (and, for a
	// secondary-file problem, that file too).
	chunks, err := chunk.Split(e, files)
	if err != nil {
		fmt.Fprintf(o.Stderr, "warning: %v\n", err)
		sum.Failed++
		return nil
	}

	var reusable map[string][]float32
	if existing != nil {
		reusable, err = o.Store.Embeddings(ctx, existing.ID)
		if err != nil {
			return fmt.Errorf("reindex %s: %w", e.Path, err)
		}
	}

	inputs := make([]store.ChunkInput, len(chunks))
	var pendingIdx []int
	var pendingText []string
	ftsOnly := embed.IsNone(o.Embedder)
	for i, c := range chunks {
		inputs[i] = store.ChunkInput{
			Ord:        c.Ord,
			SourceFile: c.SourceFile,
			Heading:    c.Heading,
			Text:       c.Text,
			TextHash:   c.TextHash,
		}
		if ftsOnly {
			continue
		}
		if vec, ok := reusable[c.TextHash]; ok {
			inputs[i].Embedding = vec
			continue
		}
		pendingIdx = append(pendingIdx, i)
		pendingText = append(pendingText, embed.DocText(o.Embedder, c.Text))
	}

	batchSize := o.BatchSize
	if batchSize <= 0 {
		batchSize = defaultBatchSize
	}
	embedded := 0
	for start := 0; start < len(pendingText); start += batchSize {
		end := start + batchSize
		if end > len(pendingText) {
			end = len(pendingText)
		}
		vecs, err := o.Embedder.Embed(ctx, pendingText[start:end])
		if err != nil {
			return errs.Wrap(err, errs.KindOf(err), "reindex.One", "reindex %s: embed: %v", e.Path, err).With("path", e.Path)
		}
		sum.EmbedCalls++
		if len(vecs) != end-start {
			return errs.New(errs.Conflict, "reindex.One", "reindex %s: embedder returned %d vectors for %d texts", e.Path, len(vecs), end-start).With("path", e.Path)
		}
		for j, vec := range vecs {
			inputs[pendingIdx[start+j]].Embedding = vec
			embedded++
		}
	}

	entryInput := store.EntryInput{
		Path:     e.Path,
		Kind:     e.Kind,
		Title:    e.Meta.Title,
		Summary:  e.Meta.Summary,
		Tags:     e.Meta.Tags,
		Aliases:  e.Meta.Aliases,
		Updated:  e.Meta.Updated,
		Verified: e.Meta.Verified,
		BodyHash: bodyHash,
		Edges:    Links(e, files),
	}
	if _, err := o.Store.ReplaceEntry(ctx, entryInput, inputs); err != nil {
		return fmt.Errorf("reindex %s: %w", e.Path, err)
	}

	sum.Reindexed++
	sum.Chunks += len(chunks)
	fmt.Fprintf(o.Stdout, "reindexed %s (%d chunks, %d embedded)\n", e.Path, len(chunks), embedded)
	return nil
}

// Links returns the outgoing edges of e, whose files (front matter included) are in files: the
// front door's related, supersedes and depends_on lists (paths resolve against the front door's
// own directory, so `related: [x.md]` in data/droplet/README.md names data/droplet/x.md), then
// the body links of every indexed file in e.Files order (each resolved against its own file's
// directory), deduplicated by (target, rel, kind) keeping the first (links.Dedupe). A path link to
// the entry itself (one of its own files, or its directory) is dropped; a wikilink is kept as
// written and resolved later. A file whose front matter is broken contributes no body links
// (chunk.Split has already refused such an entry).
func Links(e entry.Entry, files map[string][]byte) []store.EdgeInput {
	ls := links.FromFrontMatter(e.Path, e.Meta)
	for _, f := range e.Files {
		_, body, err := entry.ParseFrontMatter(files[f])
		var ve *entry.ValidationError
		if err != nil && !errors.As(err, &ve) {
			continue
		}
		ls = append(ls, links.Extract(f, body)...)
	}
	var out []store.EdgeInput
	for _, l := range links.Dedupe(ls) {
		kind := store.EdgePath
		if l.Kind == links.KindWikilink {
			kind = store.EdgeWikilink
		} else if l.Target == e.Dir || slices.Contains(e.Files, l.Target) {
			continue
		}
		out = append(out, store.EdgeInput{DstPath: l.Target, Rel: l.Rel, Kind: kind})
	}
	return out
}
