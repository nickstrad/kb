// `kb index`: explicitly export an optional index.md snapshot from the entries on disk.
package cli

import (
	"fmt"
	"io"

	"github.com/spf13/cobra"

	"knowledge/kb/internal/index"
)

// newIndexCmd builds `kb index`.
func newIndexCmd(stdout, stderr io.Writer) *cobra.Command {
	return &cobra.Command{
		Use:           "index",
		Short:         "Export an optional index.md snapshot",
		Args:          cobra.NoArgs,
		SilenceUsage:  true,
		SilenceErrors: true,
		RunE: func(cmd *cobra.Command, args []string) error {
			return runIndex(stdout, stderr, Root())
		},
	}
}

// runIndex discovers every entry, warns on stderr about any whose front matter is broken (they
// are left out of the table, per discoverGoodEntries), writes index.md from the rest, and reports
// how many entries it wrote. A broken entry does not stop index.md from being written from the
// good ones, but it does make the command exit non-zero so scripts can detect the problem.
func runIndex(stdout, stderr io.Writer, root string) error {
	good, broken, err := discoverGoodEntries(root, stderr)
	if err != nil {
		return usageErr("kb index: %s", err)
	}
	if err := index.Write(root, good, index.Today()); err != nil {
		return usageErr("kb index: %s", err)
	}
	fmt.Fprintf(stdout, "index.md regenerated (%d entries)\n", len(good))
	if broken > 0 {
		return usageErr("kb index: %d of %d entries had errors; see warnings above", broken, broken+len(good))
	}
	return nil
}
