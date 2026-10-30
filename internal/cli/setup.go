// Shared helpers for constructing the embedder, opening the database, and validating entries.
package cli

import (
	"context"
	"fmt"
	"io"
	"os"

	"github.com/spf13/cobra"

	"github.com/nickstrad/kb/internal/embed"
	"github.com/nickstrad/kb/internal/embed/local"
	"github.com/nickstrad/kb/internal/entry"
	"github.com/nickstrad/kb/internal/store"
)

// Embedder names accepted by --embedder.
const (
	embedderLocal = "local"
	embedderNone  = "none"
)

// newEmbedder builds the embedder named by cmd's --embedder and --embed-model flags, unless
// d.embedder replaces it. `kb reindex` and `kb add` download a missing model first (a one-line
// note on stderr); every other command only loads what is cached, so a search with no model falls
// back to FTS with a `kb model pull` hint.
func (d *deps) newEmbedder(cmd *cobra.Command) (embed.Embedder, error) {
	if d.embedder != nil {
		return d.embedder(cmd)
	}
	name, spec, err := embedderChoice(cmd)
	if err != nil {
		return nil, err
	}
	if name == embedderNone {
		return embed.None{}, nil
	}
	cache, err := d.modelCacheDir()
	if err != nil {
		return nil, usageErr("%v", err)
	}
	e := local.New(spec, cache)
	if downloadsModel(cmd) && !e.Cached() {
		stderr := cmd.ErrOrStderr()
		fmt.Fprintf(stderr, "kb: downloading embedding model %s to %s (once per machine)\n", spec.ID, cache)
		if err := d.pullModel(cmd.Context(), spec, cache); err != nil {
			// Not fatal here: the embedder reports Unavailable when it is used, which the
			// commands turn into exit 2 before writing anything.
			fmt.Fprintf(stderr, "warning: %s\n", err)
		}
	}
	return e, nil
}

// downloadsModel reports whether cmd may download a missing model: only the commands that build
// the index do, so search, show and doctor never wait on the network.
func downloadsModel(cmd *cobra.Command) bool {
	switch cmd.Name() {
	case "reindex", "add":
		return true
	}
	return false
}

// embedderChoice reads and validates the root's persistent --embedder and --embed-model flags.
// An unknown name or model is an error listing what is supported.
func embedderChoice(cmd *cobra.Command) (name string, spec local.Spec, err error) {
	name, model := embedderLocal, local.DefaultModel
	if cmd != nil {
		if v, _ := cmd.Flags().GetString("embedder"); v != "" {
			name = v
		}
		if v, _ := cmd.Flags().GetString("embed-model"); v != "" {
			model = v
		}
	}
	switch name {
	case embedderNone:
		return name, local.Spec{}, nil
	case embedderLocal:
		spec, err = local.Lookup(model)
		if err != nil {
			return "", local.Spec{}, usageErr("%v", err)
		}
		return name, spec, nil
	}
	return "", local.Spec{}, usageErr("unknown embedder %q (want %s or %s)", name, embedderLocal, embedderNone)
}

// openStoreForWrite opens the store at root, creating .kb/ and the database file if necessary.
func (d *deps) openStoreForWrite(ctx context.Context, root string) (store.Store, error) {
	if err := EnsureDBDir(root); err != nil {
		return nil, err
	}
	return d.openStore(ctx, DBPath(root), false)
}

// openStoreIfExists opens the database only when it already exists. ok is false, with a nil Store
// and nil error, when there is none yet: kb rm and kb show treat that as "nothing indexed".
func (d *deps) openStoreIfExists(ctx context.Context, root string) (st store.Store, ok bool, err error) {
	if _, statErr := os.Stat(DBPath(root)); statErr != nil {
		if os.IsNotExist(statErr) {
			return nil, false, nil
		}
		return nil, false, usageErr("%v", statErr)
	}
	st, err = d.openStore(ctx, DBPath(root), false)
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
