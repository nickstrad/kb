// `kb config`: print the effective configuration, or where it is read from.
package cli

import (
	"encoding/json"
	"fmt"
	"io"

	"github.com/spf13/cobra"

	"github.com/nickstrad/kb/internal/config"
)

// newConfigCmd builds `kb config [--path | --json] [--set key=value]...`.
func newConfigCmd(d *deps, stdout, stderr io.Writer) *cobra.Command {
	var (
		pathOnly bool
		asJSON   bool
		sets     []string
	)
	cmd := &cobra.Command{
		Use:   "config",
		Short: "Show the effective configuration of the store",
		Long: "Prints the settings kb uses for this store as TOML: the defaults, overridden by the\n" +
			"store's config file. Every key is\n" +
			"shown, so the output is also a complete starting point for a config file.\n" +
			"--path prints where the file is read from (it may not exist yet: then every setting\n" +
			"is its default). --json prints the [embed] and [search] settings in canonical JSON\n" +
			"with their settings_hash, the fingerprint of the settings that decide search results.\n" +
			"--set key=value (repeatable) shows the result of overriding a key, without writing it.",
		Example: "  kb config | grep rrf_k\n" +
			"  kb config --path\n" +
			"  kb config --set search.bm25.title=4 --json",
		Args:          cobra.NoArgs,
		SilenceUsage:  true,
		SilenceErrors: true,
		RunE: func(cmd *cobra.Command, args []string) error {
			return runConfig(cmd, d, stdout, pathOnly, asJSON, sets)
		},
	}
	cmd.Flags().BoolVar(&pathOnly, "path", false, "print the path of the config file instead of its contents")
	cmd.Flags().BoolVar(&asJSON, "json", false, "print the [embed] and [search] settings as canonical JSON with settings_hash")
	cmd.Flags().StringArrayVar(&sets, "set", nil, "override a key for this run: section.key=value (repeatable)")
	cmd.MarkFlagsMutuallyExclusive("path", "json")
	cmd.MarkFlagsMutuallyExclusive("path", "set")
	return cmd
}

func runConfig(cmd *cobra.Command, d *deps, stdout io.Writer, pathOnly, asJSON bool, sets []string) error {
	ws, err := d.workspace(cmd)
	if err != nil {
		return err
	}
	if pathOnly {
		fmt.Fprintln(stdout, ws.ConfigPath)
		return nil
	}
	cfg, err := d.loadConfig(cmd, ws)
	if err != nil {
		return err
	}
	for _, s := range sets {
		if cfg, err = config.Apply(cfg, s); err != nil {
			return err
		}
	}
	// loadConfig already checked the model of the file, so this only fires for a --set
	// value. A second source layer (named stores) must check its own source here instead.
	if err := cfg.CheckModel("--set", knownModel); err != nil {
		return err
	}
	if asJSON {
		data, hash, err := cfg.EffectiveJSON()
		if err != nil {
			return err
		}
		enc := json.NewEncoder(stdout)
		enc.SetIndent("", "  ")
		return enc.Encode(struct {
			Settings     json.RawMessage `json:"settings"`
			SettingsHash string          `json:"settings_hash"`
		}{data, hash})
	}
	out, err := config.Save(cfg)
	if err != nil {
		return err
	}
	_, err = stdout.Write(out)
	return err
}
