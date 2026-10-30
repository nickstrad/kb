// Shared helpers for constructing the embedder, opening the database, and validating entries.
package cli

import (
	"context"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sync"

	"github.com/spf13/cobra"

	"github.com/nickstrad/kb/internal/config"
	"github.com/nickstrad/kb/internal/embed"
	"github.com/nickstrad/kb/internal/embed/cache"
	"github.com/nickstrad/kb/internal/embed/local"
	"github.com/nickstrad/kb/internal/entry"
	"github.com/nickstrad/kb/internal/errs"
	"github.com/nickstrad/kb/internal/store"
	"github.com/nickstrad/kb/internal/workspace"
)

// workspaceConfig resolves the store this command runs on and loads its configuration: the
// store's config file over the defaults. The model is checked against the supported table, so an unknown model fails here,
// naming the file and the key, before any work starts.
//
// The store exists (workspace resolution checked its config.toml), so the file is read as is.
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

// harnessModel builds an embedder for a model id outside the supported table; ok is false for
// an id it does not know. Only a harnessfake build sets it (harnessfake.go): nil in release builds.
var harnessModel func(id string) (e embed.Embedder, ok bool)

// knownModel is config.CheckModel's lookup into the supported embedding models.
func knownModel(id string) error {
	if harnessModel != nil {
		if _, ok := harnessModel(id); ok {
			return nil
		}
	}
	_, err := local.Lookup(id)
	return err
}

// newEmbedder builds the embedder [embed] describes, unless d.embedder replaces it. `kb reindex`
// and `kb add` download a missing model first (a one-line note on stderr); every other command
// only loads what is cached, so a search with no model falls back to FTS with a `kb model pull`
// hint. cmd may be nil when d.embedder is set.
func (d *deps) newEmbedder(cmd *cobra.Command, ecfg config.Embed) (embed.Embedder, error) {
	e, err := d.buildEmbedder(cmd, ecfg)
	if err != nil {
		return nil, err
	}
	return d.withEmbedCache(cmd, e), nil
}

// buildEmbedder is newEmbedder without the shared cache.
func (d *deps) buildEmbedder(cmd *cobra.Command, ecfg config.Embed) (embed.Embedder, error) {
	if d.embedder != nil {
		return d.embedder(ecfg)
	}
	if ecfg.Model == config.ModelNone {
		return embed.None{}, nil
	}
	if harnessModel != nil {
		if e, ok := harnessModel(ecfg.Model); ok {
			return e, nil
		}
	}
	spec, err := local.Lookup(ecfg.Model)
	if err != nil {
		return nil, usageErr("%v", err)
	}
	cache, err := d.modelCacheDir()
	if err != nil {
		return nil, err // already a Usage error naming the directory
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
// the index (and kb init) do, so search, show and doctor never wait on the network.
func downloadsModel(cmd *cobra.Command) bool {
	switch cmd.Name() {
	case "reindex", "add", "init":
		return true
	}
	return false
}

// openStoreForWrite opens the store's database, creating the file if it is not there yet (an
// initialised store that was never indexed). It never creates the location or the store: only
// kb init does.
func (d *deps) openStoreForWrite(ctx context.Context, ws *workspace.Workspace) (store.Store, error) {
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

// discoverGoodEntries runs entry.Discover(root, sources) and splits the result: good holds every entry
// whose front matter parsed and validated (Err == nil), and broken counts the rest, each printed
// as a "warning: ..." line to stderr as it is skipped. entry.Err already names its own path (see
// the entry package doc), so nothing else is added to the line.
//
// The optional `kb index` export uses this to warn about broken entries and leave them
// out of the generated table.
func discoverGoodEntries(root string, sources []config.Source, stderr io.Writer) (good []entry.Entry, broken int, err error) {
	entries, skipped, err := entry.Discover(root, sources)
	if err != nil {
		return nil, 0, err
	}
	warnSkipped(stderr, skipped)
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

// embedCacheFile is the shared embedding cache for a user cache base: <base>/kb/embed-cache.sqlite,
// the one place that path is spelled. "" base is an error: no user cache directory.
func embedCacheFile(base string) (string, error) {
	if base == "" {
		return "", errs.New(errs.Usage, "cli.embedCachePath", "cannot find the user cache directory for the embedding cache")
	}
	return filepath.Join(base, "kb", "embed-cache.sqlite"), nil
}

// embedCaches tracks the caches a command opened, so execute can warn about a cache that failed
// mid-run and close them once the command is done.
type embedCaches struct {
	mu   sync.Mutex
	open []openedCache
}

type openedCache struct {
	file *cache.Cache
	emb  *embed.CachedEmbedder
}

// cacheStatser is what both cached wrappers expose, so callers can read the counters.
type cacheStatser interface{ Stats() embed.CacheStats }

// cachedPinger is a cached embedder whose inner embedder can Ping: it keeps the Ping that
// add, reindex, init and doctor look for. A cached embedder over one with no Ping has none.
type cachedPinger struct {
	*embed.CachedEmbedder
	inner pinger
}

func (c cachedPinger) Ping(ctx context.Context) error { return c.inner.Ping(ctx) }

// withEmbedCache wraps e with the shared embedding cache, so a text embedded once, in any store or
// project, is never embedded again. It returns e itself, and the command goes on without a cache,
// when there is nothing to cache (model none), the command is doctor (whose timings must be
// real), no cache path is configured, or the cache cannot be opened (a warning says why: a
// damaged or newer file, an unwritable directory). The cache is closed by finishEmbedCaches.
func (d *deps) withEmbedCache(cmd *cobra.Command, e embed.Embedder) embed.Embedder {
	if d.embedCachePath == nil || d.caches == nil || embed.IsNone(e) || (cmd != nil && cmd.Name() == "doctor") {
		return e
	}
	stderr, ctx := io.Discard, context.Background()
	if cmd != nil {
		stderr = cmd.ErrOrStderr()
		if c := cmd.Context(); c != nil {
			ctx = c
		}
	}
	path, err := d.embedCachePath()
	var file *cache.Cache
	if err == nil {
		file, err = cache.Open(ctx, path)
	}
	if err != nil {
		fmt.Fprintf(stderr, "warning: embedding cache not used, embedding without it: %v (kb model cache --clear removes a damaged cache)\n", err)
		return e
	}
	ce := embed.Cached(e, file)
	d.caches.mu.Lock()
	d.caches.open = append(d.caches.open, openedCache{file: file, emb: ce})
	d.caches.mu.Unlock()
	if p, ok := e.(pinger); ok {
		return cachedPinger{CachedEmbedder: ce, inner: p}
	}
	return ce
}

// finishEmbedCaches runs after every command: it warns about a cache that went off mid-run (the
// results are right, they were just not cached) and closes the cache files.
func (d *deps) finishEmbedCaches(stderr io.Writer) {
	if d.caches == nil {
		return
	}
	d.caches.mu.Lock()
	open := d.caches.open
	d.caches.open = nil
	d.caches.mu.Unlock()
	for _, oc := range open {
		if err := oc.emb.Err(); err != nil {
			fmt.Fprintf(stderr, "warning: %v (kb model cache --clear removes a damaged cache)\n", err)
		}
		if err := oc.file.Close(); err != nil {
			fmt.Fprintf(stderr, "warning: %v\n", err)
		}
	}
}

// modelCalls is the number of calls that reached the model: the cached embedder's count, which
// excludes the calls the cache answered, else fallback (the caller's own count).
func modelCalls(e embed.Embedder, fallback int) int {
	if s, ok := e.(cacheStatser); ok {
		return s.Stats().ModelCalls
	}
	return fallback
}
