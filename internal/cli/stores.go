// `kb stores [--json]`: the stores of the current location, the default one marked, with each
// store's model, entry count and database size, then the location's aliases.
//
// It reads each store's config and, when the database exists, opens it read-only to count the
// entries. A store whose config or database cannot be read is still listed, with the problem in
// its row, so one broken store never hides the others.
package cli

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"sort"
	"text/tabwriter"

	"github.com/spf13/cobra"

	"github.com/nickstrad/kb/internal/config"
	"github.com/nickstrad/kb/internal/workspace"
)

// storeRow is one store in `kb stores`. The JSON shape is a stable contract.
type storeRow struct {
	Name    string `json:"name"`
	Default bool   `json:"default"`
	Model   string `json:"model"`             // the configured [embed] model, "" when the config is unreadable
	Entries *int   `json:"entries"`           // nil (null) when not indexed or unreadable
	DBBytes *int64 `json:"db_bytes"`          // nil (null) when there is no database yet
	Indexed bool   `json:"indexed"`           // a database exists
	Problem string `json:"problem,omitempty"` // why a column is missing
}

// storesJSON is the --json document.
type storesJSON struct {
	Location     string            `json:"location"`
	DefaultStore string            `json:"default_store"`
	Stores       []storeRow        `json:"stores"`
	Skipped      []string          `json:"skipped_dirs,omitempty"` // directories under stores/ with an invalid name
	Aliases      map[string]string `json:"aliases"`
}

// newStoresCmd builds `kb stores`.
func newStoresCmd(d *deps, stdout, stderr io.Writer) *cobra.Command {
	var asJSON bool
	cmd := &cobra.Command{
		Use:   "stores",
		Short: "List the stores of the current location",
		Long: "kb stores lists the named stores of the current location: the default one (marked *),\n" +
			"its model, how many entries are indexed and the database size, or \"not indexed\".\n" +
			"Aliases from the location's kb.toml follow. Create a store with kb init --name NAME.",
		Args:          cobra.NoArgs,
		SilenceUsage:  true,
		SilenceErrors: true,
		RunE: func(cmd *cobra.Command, args []string) error {
			loc, lf, _, how, err := d.location(cmd)
			if err != nil {
				return err
			}
			doc, err := collectStores(cmd.Context(), d, loc, lf)
			if err != nil {
				return err
			}
			if debugFlag(cmd) {
				fmt.Fprintf(stderr, "%s: location %s (%s)\n", cmd.CommandPath(), loc.Root, how)
			}
			if asJSON {
				enc := json.NewEncoder(stdout)
				enc.SetIndent("", "  ")
				return enc.Encode(doc)
			}
			return writeStoresText(stdout, doc)
		},
	}
	cmd.Flags().BoolVar(&asJSON, "json", false, "machine-readable output")
	return cmd
}

// collectStores gathers one row per store, sorted by name.
func collectStores(ctx context.Context, d *deps, loc *workspace.Location, lf config.Location) (storesJSON, error) {
	names, skipped, err := loc.Stores()
	if err != nil {
		return storesJSON{}, err
	}
	doc := storesJSON{Location: loc.Root, DefaultStore: lf.DefaultStore, Stores: []storeRow{}, Skipped: skipped, Aliases: lf.Aliases}
	for _, name := range names {
		ws, err := loc.StoreAt(name)
		if err != nil { // unreachable: Stores returns valid names only
			return storesJSON{}, err
		}
		row := storeRow{Name: name, Default: name == lf.DefaultStore}
		cfg, _, cerr := config.Read(ws.ConfigPath)
		if cerr != nil {
			row.Problem = cerr.Error()
		} else {
			row.Model = cfg.Embed.Model
		}
		if fi, serr := os.Stat(ws.DBPath); serr == nil {
			row.Indexed = true
			size := fi.Size()
			row.DBBytes = &size
			if n, perr := countEntries(ctx, d, ws.DBPath); perr != nil {
				if row.Problem == "" {
					row.Problem = perr.Error()
				}
			} else {
				row.Entries = &n
			}
		}
		doc.Stores = append(doc.Stores, row)
	}
	sort.Slice(doc.Stores, func(i, j int) bool { return doc.Stores[i].Name < doc.Stores[j].Name })
	return doc, nil
}

// countEntries opens the database read-only and counts its entries.
func countEntries(ctx context.Context, d *deps, dbPath string) (int, error) {
	st, err := d.openStore(ctx, dbPath, true)
	if err != nil {
		return 0, err
	}
	defer st.Close()
	rows, err := st.Entries(ctx)
	if err != nil {
		return 0, err
	}
	return len(rows), nil
}

func writeStoresText(w io.Writer, doc storesJSON) error {
	tw := tabwriter.NewWriter(w, 0, 2, 2, ' ', 0)
	fmt.Fprintln(tw, "STORE\tMODEL\tENTRIES\tDATABASE")
	for _, r := range doc.Stores {
		name := r.Name
		if r.Default {
			name += " *"
		}
		model, entries, db := dash(r.Model), "-", "not indexed"
		if r.Entries != nil {
			entries = fmt.Sprint(*r.Entries)
		}
		if r.DBBytes != nil {
			db = humanBytes(*r.DBBytes)
		}
		line := fmt.Sprintf("%s\t%s\t%s\t%s", name, model, entries, db)
		if r.Problem != "" {
			line += "\t" + "problem: " + r.Problem
		}
		fmt.Fprintln(tw, line)
	}
	if err := tw.Flush(); err != nil {
		return err
	}
	if _, has := storeNamed(doc, doc.DefaultStore); !has {
		fmt.Fprintf(w, "\nwarning: default_store %q in %s names no store\n", doc.DefaultStore, doc.Location+"/.kb/kb.toml")
	}
	for _, s := range doc.Skipped {
		fmt.Fprintf(w, "warning: ignoring %s/.kb/stores/%s: not a valid store name\n", doc.Location, s)
	}
	if len(doc.Aliases) > 0 {
		keys := make([]string, 0, len(doc.Aliases))
		for k := range doc.Aliases {
			keys = append(keys, k)
		}
		sort.Strings(keys)
		fmt.Fprintln(w, "\naliases:")
		at := tabwriter.NewWriter(w, 0, 2, 2, ' ', 0)
		for _, k := range keys {
			fmt.Fprintf(at, "  %s\t%s\n", k, doc.Aliases[k])
		}
		return at.Flush()
	}
	return nil
}

func storeNamed(doc storesJSON, name string) (storeRow, bool) {
	for _, r := range doc.Stores {
		if r.Name == name {
			return r, true
		}
	}
	return storeRow{}, false
}

// humanBytes renders a size with a binary unit: 512 B, 1.5 KiB, 3.0 MiB.
func humanBytes(n int64) string {
	const unit = 1024
	if n < unit {
		return fmt.Sprintf("%d B", n)
	}
	div, exp := int64(unit), 0
	for m := n / unit; m >= unit; m /= unit {
		div *= unit
		exp++
	}
	return fmt.Sprintf("%.1f %ciB", float64(n)/float64(div), "KMGTPE"[exp])
}
