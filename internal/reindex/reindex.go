// Package reindex rebuilds the SQLite index from the knowledge repository's Markdown files: it is
// the implementation behind `kb reindex` (plan.md, P2.4) and is written so `kb add`/`kb edit`
// (P4.x) can reuse it to reindex the one entry they just wrote.
//
// Per plan.md chunking rule 6, reindexing an entry is a three-way comparison: an entry whose
// body_hash is unchanged is skipped entirely; otherwise every chunk is recomputed, but a chunk
// whose text_hash already exists for that entry reuses its stored embedding instead of paying for
// another call to the embedder. Batching (Options.BatchSize texts per embedder call) is scoped to
// one entry at a time, not to the whole run: the "reuse an unchanged chunk's embedding" lookup
// (store.ExistingEmbeddings) is itself scoped to one entry, so there is no benefit to batching
// across entries, and keeping the loop per-entry lets one entry's ReplaceEntryChunks commit as
// soon as its own chunks are ready instead of waiting on every other entry's embeddings too.
//
// A bad individual entry — front matter that fails to validate, a file that cannot be read, a
// secondary file (docs/*.md) with a structural problem such as an unclosed front-matter fence —
// is warned about on Stderr and counted in Summary.Failed; it does not stop the rest of the repo
// from being reindexed. An embedder failure is different: since it will keep failing for every
// subsequent entry too, it aborts the whole run and is returned as an error. This package never
// imports a concrete embedder implementation (e.g. internal/embed/ollama) — it only knows the
// embed.Embedder interface — so it has no way to distinguish "embedder unreachable" from any other
// embedder error by type. It simply returns the error from embed.Embedder.Embed wrapped with
// %w, which preserves the chain: a caller that constructed the embedder (kb reindex, in
// internal/cli/reindex.go) can still recognise the concrete sentinel (ollama.ErrUnavailable) with
// errors.Is on the error this package returns, and map that to exit code 2 per plan.md's CLI exit
// codes. Everything else maps to exit code 1.
package reindex

import (
	"context"
	"fmt"
	"io"

	"knowledge/kb/internal/chunk"
	"knowledge/kb/internal/embed"
	"knowledge/kb/internal/entry"
	"knowledge/kb/internal/store"
)

// defaultBatchSize is used when Options.BatchSize is not positive: 16 texts per embedder call, per
// plan.md ("embedded (batched, 16 texts per Ollama call, ...)").
const defaultBatchSize = 16

// Options configures a reindex run. Store must already have EnsureEmbedMeta called on it (the
// caller decides the model/dim and what to do about a mismatch — see cli/reindex.go); Force skips
// the body_hash comparison and reindexes every entry regardless, which the CLI sets after calling
// Store.ResetForReindex() for `kb reindex --all` on an embed_meta mismatch, since ResetForReindex
// has already erased the old body_hash anyway.
type Options struct {
	Root      string
	Store     *store.Store
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

// String renders the one-line summary from plan.md's P2.4 row: "entries: N scanned, M reindexed,
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
func All(ctx context.Context, o Options) (Summary, error) {
	o.Stdout = orDiscard(o.Stdout)
	o.Stderr = orDiscard(o.Stderr)
	var sum Summary

	entries, err := entry.Discover(o.Root)
	if err != nil {
		return sum, fmt.Errorf("reindex: %w", err)
	}

	onDisk := make(map[string]bool, len(entries))
	for _, e := range entries {
		onDisk[e.Path] = true
		if err := reindexOne(ctx, o, e, &sum); err != nil {
			return sum, err
		}
	}

	rows, err := o.Store.ListEntries()
	if err != nil {
		return sum, fmt.Errorf("reindex: list existing entries: %w", err)
	}
	for _, row := range rows {
		if onDisk[row.Path] {
			continue
		}
		if err := o.Store.DeleteEntry(ctx, row.Path); err != nil {
			return sum, fmt.Errorf("reindex: remove %s: %w", row.Path, err)
		}
		sum.Removed++
		fmt.Fprintf(o.Stdout, "removed %s (no longer on disk)\n", row.Path)
	}

	return sum, nil
}

// One reindexes a single entry, typically the result of entry.Resolve. It never touches any other
// entry and never removes anything from the database.
func One(ctx context.Context, o Options, e entry.Entry) (Summary, error) {
	o.Stdout = orDiscard(o.Stdout)
	o.Stderr = orDiscard(o.Stderr)
	var sum Summary
	if err := reindexOne(ctx, o, e, &sum); err != nil {
		return sum, err
	}
	return sum, nil
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

	existing, err := o.Store.GetEntryByPath(e.Path)
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
		reusable, err = o.Store.ExistingEmbeddings(ctx, existing.ID)
		if err != nil {
			return fmt.Errorf("reindex %s: %w", e.Path, err)
		}
	}

	inputs := make([]store.ChunkInput, len(chunks))
	var pendingIdx []int
	var pendingText []string
	for i, c := range chunks {
		inputs[i] = store.ChunkInput{
			Ord:        c.Ord,
			SourceFile: c.SourceFile,
			Heading:    c.Heading,
			Text:       c.Text,
			TextHash:   c.TextHash,
		}
		if vec, ok := reusable[c.TextHash]; ok {
			inputs[i].Embedding = vec
			continue
		}
		pendingIdx = append(pendingIdx, i)
		pendingText = append(pendingText, embed.DocPrefix+c.Text)
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
			return fmt.Errorf("reindex %s: embed: %w", e.Path, err)
		}
		sum.EmbedCalls++
		if len(vecs) != end-start {
			return fmt.Errorf("reindex %s: embedder returned %d vectors for %d texts", e.Path, len(vecs), end-start)
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
	if _, err := o.Store.ReplaceEntryChunks(ctx, entryInput, inputs); err != nil {
		return fmt.Errorf("reindex %s: %w", e.Path, err)
	}

	sum.Reindexed++
	sum.Chunks += len(chunks)
	fmt.Fprintf(o.Stdout, "reindexed %s (%d chunks, %d embedded)\n", e.Path, len(chunks), embedded)
	return nil
}
