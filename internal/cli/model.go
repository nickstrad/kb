// `kb model`: download and list the embedding models kb can run in-process.
package cli

import (
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"

	"github.com/spf13/cobra"

	"github.com/nickstrad/kb/internal/config"
	"github.com/nickstrad/kb/internal/embed/cache"
	"github.com/nickstrad/kb/internal/embed/local"
	"github.com/nickstrad/kb/internal/errs"
)

// newModelCmd builds `kb model pull`, `path`, `list` and `cache`.
func newModelCmd(d *deps, stdout, stderr io.Writer) *cobra.Command {
	cmd := &cobra.Command{
		Use:   "model",
		Short: "Download, locate or list embedding models",
		Long: "Embedding models run in-process and are kept in one cache per machine. kb reindex\n" +
			"and kb add download the configured model when it is missing; kb model pull does it\n" +
			"up front. kb search never downloads.",
		Args: cobra.NoArgs,
	}
	cmd.AddCommand(
		&cobra.Command{
			Use:           "pull",
			Short:         "Download the embedding model named by [embed] model in the config (default " + local.DefaultModel + ")",
			Args:          cobra.NoArgs,
			SilenceUsage:  true,
			SilenceErrors: true,
			RunE: func(cmd *cobra.Command, args []string) error {
				return runModelPull(cmd, d, stdout, stderr)
			},
		},
		&cobra.Command{
			Use:           "path",
			Short:         "Print the directory of the model named by [embed] model",
			Args:          cobra.NoArgs,
			SilenceUsage:  true,
			SilenceErrors: true,
			RunE: func(cmd *cobra.Command, args []string) error {
				return runModelPath(cmd, d, stdout)
			},
		},
		&cobra.Command{
			Use:           "list",
			Aliases:       []string{"ls"},
			Short:         "List the supported embedding models and whether each is downloaded",
			Args:          cobra.NoArgs,
			SilenceUsage:  true,
			SilenceErrors: true,
			RunE: func(cmd *cobra.Command, args []string) error {
				return runModelList(d, stdout)
			},
		},
		newModelCacheCmd(d, stdout),
	)
	return cmd
}

// newModelCacheCmd builds `kb model cache`.
func newModelCacheCmd(d *deps, stdout io.Writer) *cobra.Command {
	var stats, clear bool
	cmd := &cobra.Command{
		Use:   "cache [--stats | --clear]",
		Short: "Show or clear the embedding cache shared by every store on this machine",
		Long: "Embeddings are cached in one file per machine, keyed by model and text, so the same\n" +
			"text is never embedded twice, in any store or project. --stats (the default) lists the\n" +
			"entries per model. --clear deletes them; a cache that cannot be opened (damaged, or\n" +
			"written by a newer kb) is removed. A cache that is missing is not an error.",
		Args:          cobra.NoArgs,
		SilenceUsage:  true,
		SilenceErrors: true,
		RunE: func(cmd *cobra.Command, args []string) error {
			if stats && clear {
				return usageErr("--stats and --clear cannot be used together")
			}
			if clear {
				return runModelCacheClear(cmd, d, stdout)
			}
			return runModelCacheStats(cmd, d, stdout)
		},
	}
	cmd.Flags().BoolVar(&stats, "stats", false, "list the cached entries per model (the default)")
	cmd.Flags().BoolVar(&clear, "clear", false, "delete every cached embedding")
	return cmd
}

// embedCacheFileIfExists is the cache file's path and whether it exists. Neither flag creates it.
func embedCacheFileIfExists(d *deps) (path string, exists bool, err error) {
	if d.embedCachePath == nil {
		return "", false, usageErr("the embedding cache is not available")
	}
	path, err = d.embedCachePath()
	if err != nil {
		return "", false, err
	}
	if _, err := os.Stat(path); err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			return path, false, nil
		}
		return path, false, errs.Wrap(err, errs.Usage, "cli.modelCache", "cannot read the embedding cache %s", path).With("path", path)
	}
	return path, true, nil
}

func runModelCacheStats(cmd *cobra.Command, d *deps, stdout io.Writer) error {
	path, exists, err := embedCacheFileIfExists(d)
	if err != nil {
		return err
	}
	if !exists {
		fmt.Fprintf(stdout, "embedding cache %s: not created yet, nothing cached\n", path)
		return nil
	}
	ctx := cmd.Context()
	c, err := cache.Open(ctx, path)
	if err != nil {
		return errs.Wrap(err, errs.Conflict, "cli.modelCache", "%v; kb model cache --clear removes it", err).With("path", path)
	}
	defer c.Close()
	rows, err := c.Stats(ctx)
	if err != nil {
		return errs.Wrap(err, errs.Conflict, "cli.modelCache", "%v; kb model cache --clear removes it", err).With("path", path)
	}
	var total int64
	for _, r := range rows {
		total += r.Entries
	}
	var size int64
	if fi, err := os.Stat(path); err == nil {
		size = fi.Size()
	}
	fmt.Fprintf(stdout, "embedding cache %s: %d entries, %d bytes\n", path, total, size)
	for _, r := range rows {
		fmt.Fprintf(stdout, "%-40s %4d dims  %d entries\n", r.Model, r.Dim, r.Entries)
	}
	return nil
}

// runModelCacheClear empties the cache. A file that cannot be opened is deleted with its WAL
// files: it is disposable, and clearing is the way out the damage messages promise.
func runModelCacheClear(cmd *cobra.Command, d *deps, stdout io.Writer) error {
	path, exists, err := embedCacheFileIfExists(d)
	if err != nil {
		return err
	}
	if !exists {
		fmt.Fprintf(stdout, "embedding cache %s: not created yet, nothing to clear\n", path)
		return nil
	}
	ctx := cmd.Context()
	c, openErr := cache.Open(ctx, path)
	if openErr == nil {
		defer c.Close()
		if err := c.Clear(ctx); err != nil {
			return errs.Wrap(err, errs.Conflict, "cli.modelCache", "%v", err).With("path", path)
		}
		fmt.Fprintf(stdout, "cleared the embedding cache %s\n", path)
		return nil
	}
	if ctxErr := ctx.Err(); ctxErr != nil {
		return ctxErr
	}
	for _, p := range []string{path, path + "-wal", path + "-shm"} {
		if err := os.Remove(p); err != nil && !errors.Is(err, fs.ErrNotExist) {
			return errs.Wrap(err, errs.Usage, "cli.modelCache", "cannot remove the unreadable embedding cache %s: %v", p, err).With("path", p)
		}
	}
	fmt.Fprintf(stdout, "removed the unreadable embedding cache %s (%v)\n", path, openErr)
	return nil
}

// configuredEmbed is the [embed] table of the store this command runs on, or the defaults outside
// any location, so the model commands work before kb init. Inside a location the store must
// resolve: a --store or default_store that names no store is NotFound, not a silent fallback to
// the default model.
func configuredEmbed(cmd *cobra.Command, d *deps) (config.Embed, error) {
	ecfg := config.Default().Embed
	if _, _, _, _, err := d.location(cmd); err != nil {
		if errs.KindOf(err) == errs.NotFound {
			return ecfg, nil
		}
		return ecfg, err
	}
	_, cfg, err := d.workspaceConfig(cmd)
	if err != nil {
		return ecfg, err
	}
	return cfg.Embed, nil
}

// runModelPath prints the directory the configured model is (or will be) cached in, even before
// it is downloaded, so scripts can find or clean it. "none" has no directory.
func runModelPath(cmd *cobra.Command, d *deps, stdout io.Writer) error {
	ecfg, err := configuredEmbed(cmd, d)
	if err != nil {
		return err
	}
	if ecfg.Model == config.ModelNone {
		return usageErr("embed.model is %q: this store uses full-text search only, so there is no model directory", config.ModelNone)
	}
	spec, err := local.Lookup(ecfg.Model)
	if err != nil {
		return usageErr("%s", err)
	}
	cache, err := d.modelCacheDir()
	if err != nil {
		return err
	}
	fmt.Fprintln(stdout, local.New(spec, cache).ModelDir())
	return nil
}

// runModelPull downloads the model [embed] model names unless it is already cached. Outside a
// store it pulls the default model, so it works before a store exists. A failed download exits 2
// (embedder unavailable), like any other "the model cannot be used" failure.
func runModelPull(cmd *cobra.Command, d *deps, stdout, stderr io.Writer) error {
	ecfg, err := configuredEmbed(cmd, d)
	if err != nil {
		return err
	}
	if ecfg.Model == config.ModelNone {
		return usageErr("embed.model is %q: this store uses full-text search only, so there is no model to download", config.ModelNone)
	}
	spec, err := local.Lookup(ecfg.Model)
	if err != nil {
		return usageErr("%s", err)
	}
	cache, err := d.modelCacheDir()
	if err != nil {
		return err
	}
	e := local.New(spec, cache)
	if e.Cached() {
		fmt.Fprintf(stdout, "model %s already downloaded in %s\n", spec.ID, e.ModelDir())
		return nil
	}
	fmt.Fprintf(stderr, "kb: downloading embedding model %s to %s\n", spec.ID, cache)
	if err := d.pullModel(cmd.Context(), spec, cache); err != nil {
		return embedderErr("%s", err)
	}
	fmt.Fprintf(stdout, "pulled %s (%d dims) to %s\n", spec.ID, spec.Dim, e.ModelDir())
	return nil
}

// runModelList prints one line per supported model: id, dims, and default/downloaded markers.
func runModelList(d *deps, stdout io.Writer) error {
	cache, err := d.modelCacheDir()
	if err != nil {
		return err
	}
	for _, id := range local.Supported() {
		spec, _ := local.Lookup(id) // id comes from Supported, so it is in the table
		state := "not downloaded"
		if local.New(spec, cache).Cached() {
			state = "downloaded"
		}
		def := ""
		if id == local.DefaultModel {
			def = " (default)"
		}
		fmt.Fprintf(stdout, "%-40s %4d dims  %s%s\n", id, spec.Dim, state, def)
	}
	return nil
}
