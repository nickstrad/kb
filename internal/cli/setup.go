// Shared helpers for constructing the embedder, opening the database, and validating entries.
package cli

import (
	"context"
	"fmt"
	"io"
	"os"

	"github.com/spf13/cobra"

	"github.com/nickstrad/kb/internal/config"
	"github.com/nickstrad/kb/internal/embed"
	"github.com/nickstrad/kb/internal/embed/local"
	"github.com/nickstrad/kb/internal/entry"
	"github.com/nickstrad/kb/internal/store"
	"github.com/nickstrad/kb/internal/workspace"
)

// workspaceConfig resolves the store this command runs on and loads its configuration: the
// store's config file over the defaults. The model is checked against the supported table, so an unknown model fails here,
// naming the file and the key, before any work starts.
//
// A store with no config.toml (add/reindex on a fresh --root, until kb init) gets the defaults.
// The model recorded in the index plays no part: the config is the one source, and a mismatch
// with the index is reported with the fix (kb reindex --all).
func (d *deps) workspaceConfig(cmd *cobra.Command) (*workspace.Workspace, config.Config, error) {
	ws, err := d.workspace(cmd)
	if err != nil {
		return nil, config.Config{}, err
	}
	cfg, err := d.loadConfig(cmd, ws)
	if err != nil {
		return nil, config.Config{}, err
	}
	return ws, cfg, nil
}

// loadConfig is workspaceConfig for an already resolved workspace. It is the only place the
// config file is read.
func (d *deps) loadConfig(cmd *cobra.Command, ws *workspace.Workspace) (config.Config, error) {
	cfg, _, err := config.Read(ws.ConfigPath)
	if err != nil {
		return config.Config{}, err
	}
	if err := cfg.CheckModel(ws.ConfigPath, knownModel); err != nil {
		return config.Config{}, err
	}
	return cfg, nil
}

// knownModel is config.CheckModel's lookup into the supported embedding models.
func knownModel(id string) error {
	_, err := local.Lookup(id)
	return err
}

// newEmbedder builds the embedder [embed] describes, unless d.embedder replaces it. `kb reindex`
// and `kb add` download a missing model first (a one-line note on stderr); every other command
// only loads what is cached, so a search with no model falls back to FTS with a `kb model pull`
// hint. cmd may be nil when d.embedder is set.
func (d *deps) newEmbedder(cmd *cobra.Command, ecfg config.Embed) (embed.Embedder, error) {
	if d.embedder != nil {
		return d.embedder(ecfg)
	}
	if ecfg.Model == config.ModelNone {
		return embed.None{}, nil
	}
	spec, err := local.Lookup(ecfg.Model)
	if err != nil {
		return nil, usageErr("%v", err)
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

// openStoreForWrite opens the store's database, creating the store directory and the file if necessary.
func (d *deps) openStoreForWrite(ctx context.Context, ws *workspace.Workspace) (store.Store, error) {
	if err := os.MkdirAll(ws.KBDir, 0o755); err != nil {
		return nil, usageErr("create %s: %v", ws.KBDir, err)
	}
	return d.openStore(ctx, ws.DBPath, false)
}

// openStoreIfExists opens the database only when it already exists. ok is false, with a nil Store
// and nil error, when there is none yet: kb rm and kb show treat that as "nothing indexed".
func (d *deps) openStoreIfExists(ctx context.Context, ws *workspace.Workspace) (st store.Store, ok bool, err error) {
	if _, statErr := os.Stat(ws.DBPath); statErr != nil {
		if os.IsNotExist(statErr) {
			return nil, false, nil
		}
		return nil, false, usageErr("%v", statErr)
	}
	st, err = d.openStore(ctx, ws.DBPath, false)
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
