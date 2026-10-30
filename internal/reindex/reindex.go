// Package reindex rebuilds the SQLite index from the knowledge repository's Markdown files. It
// backs `kb reindex`, and `kb add`/`kb edit` use One to reindex the entry they just wrote.
//
// Reindexing an entry is a three-way comparison (chunking rule 6): an entry whose body_hash is
// unchanged is skipped; otherwise every chunk is recomputed, and a chunk whose text_hash already
// exists for that entry reuses its stored embedding. Batching (Options.BatchSize texts per
// embedder call) is per entry, because the embedding-reuse lookup is per entry and each entry
// commits as soon as its own chunks are ready.
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
	"io/fs"
	"os"

	"github.com/nickstrad/kb/internal/assert"
	"github.com/nickstrad/kb/internal/chunk"
	"github.com/nickstrad/kb/internal/embed"
	"github.com/nickstrad/kb/internal/entry"
	"github.com/nickstrad/kb/internal/errs"
	"github.com/nickstrad/kb/internal/store"
)

// defaultBatchSize is used when Options.BatchSize is not positive: 16 texts per embedder call.
const defaultBatchSize = 16

// Options configures a reindex run. Store must already have EnsureEmbedMeta called on it for
// Embedder (the caller decides what to do about a mismatch; see cli/reindex.go). Force skips the
// body_hash comparison, which the CLI sets after ResetIndex for `kb reindex --all` on a mismatch.
type Options struct {
	Root      string
	Store     store.Index
	Embedder  embed.Embedder
	BatchSize int
	Stdout    io.Writer
	Stderr    io.Writer
	Force     bool
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

// All reindexes every entry Discover finds under o.Root, then deletes any DB entry whose path no
// longer exists on disk (Summary.Removed). It stops and returns an error the first time an entry
// fails for a reason other than its own content (an embedder or database failure); a bad entry's
// own content (missing front matter, unreadable file) never stops the run — see the package doc.
//
// A root with no data/ directory yet is an empty corpus, not an error: `kb add` creates data/ on
// demand, so a reindex on a fresh root (possibly racing that add) has simply nothing to scan.
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

	entries, err := discover(o.Root)
	if err != nil {
		return sum, err
	}

	onDisk := make(map[string]bool, len(entries))
	for _, e := range entries {
		onDisk[e.Path] = true
		if err := reindexOne(ctx, o, e, &sum); err != nil {
			return sum, err
		}
	}

	rows, err := o.Store.Entries(ctx)
	if err != nil {
		return sum, fmt.Errorf("list existing entries: %w", err)
	}
	var gone []string
	for _, row := range rows {
		if !onDisk[row.Path] {
			gone = append(gone, row.Path)
		}
	}
	if len(gone) == 0 {
		return sum, nil
	}
	// Re-check on disk now, after Entries: see the doc comment.
	now, err := discover(o.Root)
	if err != nil {
		return sum, err
	}
	for _, e := range now {
		onDisk[e.Path] = true
	}
	for _, path := range gone {
		if onDisk[path] {
			continue
		}
		if err := o.Store.DeleteEntry(ctx, path); err != nil {
			return sum, fmt.Errorf("remove %s: %w", path, err)
		}
		sum.Removed++
		fmt.Fprintf(o.Stdout, "removed %s (no longer on disk)\n", path)
	}

	return sum, nil
}

// discover is entry.Discover with a missing data/ directory read as an empty corpus (see All).
func discover(root string) ([]entry.Entry, error) {
	entries, err := entry.Discover(root)
	if errors.Is(err, fs.ErrNotExist) {
		if _, statErr := os.Stat(entry.DataRoot(root)); errors.Is(statErr, fs.ErrNotExist) {
			return nil, nil
		}
	}
	if err != nil {
		return nil, errs.Wrap(err, errs.Usage, "reindex.All", "could not read the entries under %s: %v", entry.DataRoot(root), err)
	}
	return entries, nil
}

// One reindexes a single entry, typically the result of entry.Resolve. It never touches any other
// entry and never removes anything from the database.
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
		Updated:  e.Meta.Updated,
		Verified: e.Meta.Verified,
		BodyHash: bodyHash,
	}
	if _, err := o.Store.ReplaceEntry(ctx, entryInput, inputs); err != nil {
		return fmt.Errorf("reindex %s: %w", e.Path, err)
	}

	sum.Reindexed++
	sum.Chunks += len(chunks)
	fmt.Fprintf(o.Stdout, "reindexed %s (%d chunks, %d embedded)\n", e.Path, len(chunks), embedded)
	return nil
}
