// `kb index`: explicitly export an optional index.md snapshot from the entries on disk.
package cli

import (
	"fmt"
	"io"

	"github.com/spf13/cobra"

	"github.com/nickstrad/kb/internal/config"
	"github.com/nickstrad/kb/internal/index"
)

// newIndexCmd builds `kb index`.
func newIndexCmd(d *deps, stdout, stderr io.Writer) *cobra.Command {
	return &cobra.Command{
		Use:           "index",
		Short:         "Export an optional index.md snapshot",
		Args:          cobra.NoArgs,
		SilenceUsage:  true,
		SilenceErrors: true,
		RunE: func(cmd *cobra.Command, args []string) error {
			rr, err := d.resolveWorkspace(cmd)
			if err != nil {
				return err
			}
			ws := rr.ws
			cfg, err := d.loadConfig(cmd, ws)
			if err != nil {
				return err
			}
			if def := rr.lf.DefaultStore; def != ws.Store {
				// index.md is one file at the location root: written for another store it would
				// replace the default store's snapshot (W7 pass).
				return usageErr("index.md at %s describes the default store (%q); kb index does not run for store %q", ws.Root, def, ws.Store)
			}
			return runIndex(stdout, stderr, ws.Root, cfg.Sources)
		},
	}
}

// runIndex discovers every entry, warns on stderr about any whose front matter is broken (they
// are left out of the table, per discoverGoodEntries), writes index.md from the rest, and reports
// how many entries it wrote. A broken entry does not stop index.md from being written from the
// good ones, but it does make the command exit non-zero so scripts can detect the problem.
func runIndex(stdout, stderr io.Writer, root string, sources []config.Source) error {
	good, broken, err := discoverGoodEntries(root, sources, stderr)
	if err != nil {
		return err
	}
	if err := index.Write(root, good, index.Today()); err != nil {
		return usageErr("%s", err)
	}
	owned := 0 // in-place entries are not listed in index.md (index.Generate)
	for _, e := range good {
		if !e.InPlace() {
			owned++
		}
	}
	fmt.Fprintf(stdout, "index.md regenerated (%d entries)\n", owned)
	if broken > 0 {
		return usageErr("%d of %d entries had errors; see warnings above", broken, broken+len(good))
	}
	return nil
}
