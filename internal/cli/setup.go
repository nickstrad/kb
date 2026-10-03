// Shared helpers for constructing the embedder, opening the database, and validating entries.
package cli

import (
	"fmt"
	"io"
	"os"

	"github.com/spf13/cobra"

	"knowledge/kb/internal/embed"
	"knowledge/kb/internal/embed/provider"
	"knowledge/kb/internal/entry"
	"knowledge/kb/internal/store"
)

// newEmbedder builds the configured embedder (see internal/embed/provider) from cmd's --embedder
// and --embed-model flags and the environment. It is a variable rather than a plain function so
// tests can substitute a fake embedder without touching a real embedding service.
var newEmbedder = func(cmd *cobra.Command) (embed.Embedder, error) {
	e, _, err := provider.New(embedderOptions(cmd))
	return e, err
}

// embedderOptions reads the root command's persistent --embedder and --embed-model flags.
func embedderOptions(cmd *cobra.Command) provider.Options {
	var o provider.Options
	if cmd == nil {
		return o
	}
	o.Embedder, _ = cmd.Flags().GetString("embedder")
	o.Model, _ = cmd.Flags().GetString("embed-model")
	return o
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
// The optional `kb index` export uses this to warn about broken entries and leave them
// out of the generated table.
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
