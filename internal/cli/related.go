// `kb related`: the entries linked to an entry, in both directions, up to three hops.
package cli

import (
	"encoding/json"
	"fmt"
	"io"
	"strings"

	"github.com/spf13/cobra"

	"github.com/nickstrad/kb/internal/entry"
	"github.com/nickstrad/kb/internal/errs"
	"github.com/nickstrad/kb/internal/store"
)

// newRelatedCmd builds `kb related <path|REF:path> [--depth 1..3] [--json]`.
func newRelatedCmd(d *deps, stdout, stderr io.Writer) *cobra.Command {
	var (
		depth  int
		asJSON bool
	)
	cmd := &cobra.Command{
		Use:   "related <entry>",
		Short: "List the entries linked to an entry, in both directions",
		Long: "Lists the links around an entry from the index: the entries it links to (out) and the\n" +
			"entries that link to it (in), with each link's rel (links for a body link; related,\n" +
			"supersedes or depends_on from the front matter). --depth 2 or 3 also follows the\n" +
			"links of the entries reached; a line then names the entry it was reached through.\n" +
			"A link to nothing that is indexed is listed as unresolved (see kb reindex).\n" +
			"REF:path reads an entry of another store. The index is read as the last kb reindex\n" +
			"left it.",
		Args:          cobra.ExactArgs(1),
		SilenceUsage:  true,
		SilenceErrors: true,
		RunE: func(cmd *cobra.Command, args []string) error {
			return runRelated(cmd, d, stdout, args[0], depth, asJSON)
		},
	}
	cmd.Flags().IntVar(&depth, "depth", 1, "how many hops to follow, 1..3")
	cmd.Flags().BoolVar(&asJSON, "json", false, "machine-readable output")
	return cmd
}

// runRelated resolves arg to an entry of its store, reads its links and prints them. A depth outside
// 1..3 is a Usage error; no index, or an entry that is not in it, is NotFound.
func runRelated(cmd *cobra.Command, d *deps, stdout io.Writer, arg string, depth int, asJSON bool) error {
	const op = "cli.related"
	if depth < 1 || depth > store.MaxRelatedDepth {
		return usageErr("invalid --depth %d (want 1 to %d)", depth, store.MaxRelatedDepth)
	}
	ctx := cmd.Context()
	ws, cfg, err := d.workspaceConfig(cmd)
	if err != nil {
		return err
	}
	tgt, arg, err := d.showTarget(cmd, ws, cfg, arg)
	if err != nil {
		return err
	}
	// An argument that names no file on disk (deleted since the last reindex, or not a path at
	// all) is looked up in the index as typed; the index answers NotFound if it is not there.
	path := arg
	if e, rerr := entry.Resolve(tgt.ws.Root, tgt.cfg.Sources, arg); rerr == nil {
		path = e.Path
	}
	st, ok, err := d.openStoreIfExists(ctx, tgt.ws)
	if err != nil {
		return err
	}
	if !ok {
		return errs.New(errs.NotFound, op, "no index at %s; run kb reindex --all", tgt.ws.DBPath)
	}
	defer st.Close()
	rows, err := st.Related(ctx, path, depth)
	if err != nil {
		return err
	}
	if asJSON {
		return printRelatedJSON(stdout, path, depth, rows)
	}
	printRelated(stdout, path, depth, rows)
	return nil
}

// jsonRelated is one edge of the --json output.
type jsonRelated struct {
	Depth     int    `json:"depth"`
	Direction string `json:"direction"`
	Rel       string `json:"rel"`
	Kind      string `json:"kind"`
	Via       string `json:"via"`
	Path      string `json:"path"`
	Target    string `json:"target"`
	Resolved  bool   `json:"resolved"`
}

func printRelatedJSON(w io.Writer, path string, depth int, rows []store.RelatedRow) error {
	edges := make([]jsonRelated, len(rows))
	for i, r := range rows {
		edges[i] = jsonRelated{r.Depth, r.Direction, r.Rel, r.Kind, r.Via, r.Path, r.Target, r.Path != ""}
	}
	data, err := json.MarshalIndent(struct {
		Path  string        `json:"path"`
		Depth int           `json:"depth"`
		Edges []jsonRelated `json:"edges"`
	}{path, depth, edges}, "", "  ")
	if err != nil {
		return errs.Wrap(err, errs.Internal, "cli.related", "encode links: %v", err)
	}
	fmt.Fprintln(w, string(data))
	return nil
}

// relatedFar is the entry on the far side of a row, or the link as written when it is dangling.
func relatedFar(r store.RelatedRow) string {
	if r.Path == "" {
		return r.Target
	}
	return r.Path
}

// printRelated writes one line per edge: direction arrow, rel, the entry on the far side (a
// dangling link shows its target as written and "(unresolved)"), and at depth 2 and 3 the entry
// it was reached through.
func printRelated(w io.Writer, path string, depth int, rows []store.RelatedRow) {
	fmt.Fprintf(w, "%s · %d %s\n", path, len(rows), plural(len(rows), "link", "links"))
	if len(rows) == 0 {
		fmt.Fprintln(w, "no links")
		return
	}
	relW, farW := 0, 0
	for _, r := range rows {
		relW, farW = max(relW, len(r.Rel)), max(farW, len(relatedFar(r)))
	}
	for _, r := range rows {
		arrow := "→"
		if r.Direction == store.DirIn {
			arrow = "←"
		}
		var notes []string
		if r.Path == "" {
			notes = append(notes, "(unresolved)")
		}
		if r.Via != path {
			notes = append(notes, "via "+r.Via)
		}
		far := relatedFar(r)
		if len(notes) > 0 { // pad only when a note follows: no trailing spaces
			far = fmt.Sprintf("%-*s  %s", farW, far, strings.Join(notes, " "))
		}
		fmt.Fprintf(w, "%s  %-*s  %s\n", arrow, relW, r.Rel, far)
	}
}
