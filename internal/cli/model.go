// `kb model`: download and list the embedding models kb can run in-process.
package cli

import (
	"fmt"
	"io"

	"github.com/spf13/cobra"

	"github.com/nickstrad/kb/internal/config"
	"github.com/nickstrad/kb/internal/embed/local"
	"github.com/nickstrad/kb/internal/errs"
)

// newModelCmd builds `kb model pull` and `kb model list`.
func newModelCmd(d *deps, stdout, stderr io.Writer) *cobra.Command {
	cmd := &cobra.Command{
		Use:   "model",
		Short: "Download or list embedding models",
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
			Use:           "list",
			Short:         "List the supported embedding models and whether each is downloaded",
			Args:          cobra.NoArgs,
			SilenceUsage:  true,
			SilenceErrors: true,
			RunE: func(cmd *cobra.Command, args []string) error {
				return runModelList(d, stdout)
			},
		},
	)
	return cmd
}

// runModelPull downloads the model [embed] model names unless it is already cached. Outside a
// store it pulls the default model, so it works before a store exists. A failed download exits 2
// (embedder unavailable), like any other "the model cannot be used" failure.
func runModelPull(cmd *cobra.Command, d *deps, stdout, stderr io.Writer) error {
	ecfg := config.Default().Embed
	if _, cfg, err := d.workspaceConfig(cmd); err == nil {
		ecfg = cfg.Embed
	} else if errs.KindOf(err) != errs.NotFound {
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
		return usageErr("%s", err)
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
		return usageErr("%s", err)
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
