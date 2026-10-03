// `kb list`: the entries on disk, with the columns the index cares about. This command reads only
// the files, never the database, so it works before the first reindex.
package cli

import (
	"fmt"
	"io"
	"strings"
	"text/tabwriter"

	"github.com/spf13/cobra"

	"github.com/nickstrad/kb/internal/entry"
)

// maxSummaryWidth keeps the table readable when a summary line is long; the full text is always
// in the file itself.
const maxSummaryWidth = 72

// newListCmd builds `kb list [--tag t]`.
func newListCmd(stdout, stderr io.Writer) *cobra.Command {
	var tag string
	cmd := &cobra.Command{
		Use:           "list",
		Short:         "List the entries: path, summary, updated, verified",
		Args:          cobra.NoArgs,
		SilenceUsage:  true,
		SilenceErrors: true,
		RunE: func(cmd *cobra.Command, args []string) error {
			return runList(stdout, Root(), tag)
		},
	}
	cmd.Flags().StringVar(&tag, "tag", "", "only entries carrying this tag")
	return cmd
}

// runList prints every discovered entry, optionally filtered by tag.
func runList(stdout io.Writer, root, tag string) error {
	entries, err := entry.Discover(root)
	if err != nil {
		return usageErr("kb list: %s", err.Error())
	}

	w := tabwriter.NewWriter(stdout, 0, 2, 2, ' ', 0)
	fmt.Fprintln(w, "PATH\tSUMMARY\tUPDATED\tVERIFIED")
	shown := 0
	for _, e := range entries {
		// A broken entry carries no tags to filter on; it is shown regardless of --tag so its
		// error is not silently hidden from a filtered view.
		if e.Err == nil && tag != "" && !e.HasTag(tag) {
			continue
		}
		shown++
		summary := e.Meta.Summary
		if e.Err != nil {
			summary = "ERROR: " + e.Err.Error()
		}
		fmt.Fprintf(w, "%s\t%s\t%s\t%s\n",
			e.Path,
			truncate(summary, maxSummaryWidth),
			dash(e.Meta.Updated),
			dash(e.Meta.Verified))
	}
	if err := w.Flush(); err != nil {
		return err
	}
	fmt.Fprintf(stdout, "\n%d entries\n", shown)
	return nil
}

// truncate shortens s to at most width runes, marking the cut with an ellipsis.
func truncate(s string, width int) string {
	s = strings.TrimSpace(s)
	if s == "" {
		return "-"
	}
	runes := []rune(s)
	if len(runes) <= width {
		return s
	}
	return strings.TrimSpace(string(runes[:width-1])) + "…"
}

// dash renders an empty optional field as "-" so the columns stay readable.
func dash(s string) string {
	if strings.TrimSpace(s) == "" {
		return "-"
	}
	return s
}
