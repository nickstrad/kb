// setup.go holds the small helpers shared across the mutating commands: constructing the
// embedder, opening the database for writing or only when it already exists, and regenerating
// index.md after a successful mutation.
//
// This file used to be split into setup_p42.go (newEmbedder, openStoreForWrite,
// openStoreIfExists, from P4.2) and this file (regenerateIndex, from P4.4); P4.4 folded the two
// together since every mutating command needs both halves.
package cli

import (
	"fmt"
	"io"
	"os"

	"knowledge/kb/internal/embed"
	"knowledge/kb/internal/embed/ollama"
	"knowledge/kb/internal/entry"
	"knowledge/kb/internal/index"
	"knowledge/kb/internal/store"
)

// newEmbedder builds the embedder used to reindex a changed entry. It is a variable rather than a
// plain function so tests can substitute a fake embedder without touching the real Ollama service.
var newEmbedder = func() embed.Embedder {
	return ollama.New("", "", 0)
}

// openStoreForWrite opens the store at root, creating .kb/ and the database file first if
// necessary.
func openStoreForWrite(root string) (*store.Store, error) {
	if err := EnsureDBDir(root); err != nil {
		return nil, err
	}
	return store.Open(DBPath(root))
}

// openStoreIfExists opens the database only when it already exists on disk. ok is false, with a
// nil Store and nil error, when there is no database yet: callers (kb rm) treat "nothing indexed
// yet" as "nothing to do to the database", not as an error.
func openStoreIfExists(root string) (st *store.Store, ok bool, err error) {
	if _, statErr := os.Stat(DBPath(root)); statErr != nil {
		if os.IsNotExist(statErr) {
			return nil, false, nil
		}
		return nil, false, statErr
	}
	st, err = store.Open(DBPath(root))
	if err != nil {
		return nil, false, err
	}
	return st, true, nil
}

// discoverGoodEntries runs entry.Discover(root) and splits the result: good holds every entry
// whose front matter parsed and validated (Err == nil), and broken counts the rest, each printed
// as a "warning: ..." line to stderr as it is skipped. entry.Err already names its own path (see
// the entry package doc), so nothing else is added to the line.
//
// This is the one place that decides which entries index.md is generated from: both `kb index`
// (index.go) and regenerateIndex below call it, so a broken entry is always warned about and
// always left out of the table, never silently dropped or silently kept.
func discoverGoodEntries(root string, stderr io.Writer) (good []entry.Entry, broken int, err error) {
	entries, err := entry.Discover(root)
	if err != nil {
		return nil, 0, err
	}
	good = make([]entry.Entry, 0, len(entries))
	for _, e := range entries {
		if e.Err != nil {
			fmt.Fprintf(stderr, "warning: %v\n", e.Err)
			broken++
			continue
		}
		good = append(good, e)
	}
	return good, broken, nil
}

// regenerateIndex rewrites index.md from the entries currently on disk. It is the index.md-write
// path shared by `kb index` (which additionally reports a count and a non-zero exit when an
// entry was broken) and the hooks in add.go, edit.go, rm.go and reindex.go: each of those calls
// this purely for effect after a successful mutation, and only reports an error when Discover or
// the write itself fails. A broken *other* entry is warned about (via discoverGoodEntries) but is
// never this command's own failure to report — the mutation it just performed already succeeded
// and its on-disk and database work stays regardless of whether index.md could be rewritten.
func regenerateIndex(root string, stderr io.Writer) error {
	good, _, err := discoverGoodEntries(root, stderr)
	if err != nil {
		return fmt.Errorf("regenerate index.md: %w", err)
	}
	if err := index.Write(root, good, index.Today()); err != nil {
		return fmt.Errorf("regenerate index.md: %w", err)
	}
	return nil
}
