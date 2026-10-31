// `kb migrate status|up|verify [--all] [--dry-run] [--no-backup] [--json]`: upgrade the durable
// files a kb release may change (docs/design/migrations.md, decision D10).
//
// The versioned files are the store's index database, the store's config.toml and the location's
// kb.toml. Every other command refuses an older one and says "run kb migrate up"; only `up`
// changes them, after it has copied each to <file>.bak-v<N>-<time> under an advisory lock
// (internal/migrate). The embedding cache is disposable and migrates on open, so it is shown by
// `status` and never touched here.
//
// Scope: the location file plus the resolved store (--store NAME, else kb.toml's default_store),
// or with --all the location file plus every store of the location, and the global location
// when it is another one. A file that does not exist yet (a store never indexed) is "absent" and
// skipped. The location is resolved without reading kb.toml through the strict reader, so an older
// kb.toml does not stop `kb migrate` from upgrading it.
package cli

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"text/tabwriter"

	"github.com/BurntSushi/toml"
	"github.com/spf13/cobra"

	"github.com/nickstrad/kb/internal/config"
	"github.com/nickstrad/kb/internal/embed/cache"
	"github.com/nickstrad/kb/internal/errs"
	"github.com/nickstrad/kb/internal/migrate"
	"github.com/nickstrad/kb/internal/workspace"
)

// migrateFlags are the flags the three subcommands share.
type migrateFlags struct {
	all, dryRun, noBackup, asJSON bool
}

// newMigrateCmd builds `kb migrate` with its three subcommands.
func newMigrateCmd(d *deps, stdout, stderr io.Writer) *cobra.Command {
	cmd := &cobra.Command{
		Use:   "migrate",
		Short: "Upgrade the index, config and location files after a kb upgrade",
		Long: "kb migrate upgrades the files kb keeps on disk when a new kb version changes their format:\n" +
			"each store's index database (which holds the search log) and config.toml, and the\n" +
			"location's kb.toml. Other commands refuse an older file with \"run kb migrate up\"; a file\n" +
			"written by a newer kb is refused everywhere, and nothing here changes it.\n\n" +
			"  kb migrate status   show each file's version and the pending steps (exit 1 if any are pending)\n" +
			"  kb migrate up       back each file up to <file>.bak-v<N>-<time> (the newest 3 are kept), then upgrade it\n" +
			"  kb migrate verify   check integrity and that each file matches its schema (exit 1 on drift)\n\n" +
			"By default it works on the location file and the current store (--store NAME picks another);\n" +
			"--all covers every store of the location and the global location. Some steps clear derived\n" +
			"data; up ends with \"needs: kb reindex\" when one did. The embedding cache is never migrated\n" +
			"here: it is rebuilt automatically.",
		Example: "  kb migrate status\n  kb migrate up --dry-run\n  kb migrate up --all\n" +
			"  for d in ~/work/*/; do (cd \"$d\" && kb migrate up); done",
		Args:          cobra.NoArgs,
		SilenceUsage:  true,
		SilenceErrors: true,
	}
	var f migrateFlags
	addAll := func(c *cobra.Command) {
		c.Flags().BoolVar(&f.all, "all", false, "every store of the location, and the global location")
	}
	status := &cobra.Command{
		Use: "status", Short: "Show each file's version and pending steps", Args: cobra.NoArgs,
		SilenceUsage: true, SilenceErrors: true,
		RunE: func(cmd *cobra.Command, args []string) error { return runMigrateStatus(cmd, d, stdout, f) },
	}
	status.Flags().BoolVar(&f.asJSON, "json", false, "machine-readable output")
	up := &cobra.Command{
		Use: "up", Short: "Back up and upgrade every file with pending steps", Args: cobra.NoArgs,
		SilenceUsage: true, SilenceErrors: true,
		RunE: func(cmd *cobra.Command, args []string) error { return runMigrateUp(cmd, d, stdout, f) },
	}
	up.Flags().BoolVar(&f.dryRun, "dry-run", false, "print the plan and the backups it would write; change nothing")
	up.Flags().BoolVar(&f.noBackup, "no-backup", false, "skip the backup (the files are not recoverable if a step goes wrong)")
	verify := &cobra.Command{
		Use: "verify", Short: "Check each file against its schema", Args: cobra.NoArgs,
		SilenceUsage: true, SilenceErrors: true,
		RunE: func(cmd *cobra.Command, args []string) error { return runMigrateVerify(cmd, d, stdout, f) },
	}
	for _, c := range []*cobra.Command{status, up, verify} {
		addAll(c)
	}
	cmd.AddCommand(status, up, verify)
	return cmd
}

// migrateItem is one versioned file in scope.
type migrateItem struct {
	Scope  string // "location", "store NAME", or "cache"
	Where  string // the location root, to tell stores of several locations apart; "" for the cache
	Art    migrate.Artefact
	Path   string
	Absent bool // the file does not exist (a store that was never indexed)
	Info   bool // shown only: the embedding cache
}

// migrateScope resolves what the command works on.
func (d *deps) migrateScope(cmd *cobra.Command, all bool) ([]migrateItem, error) {
	root, _ := cmd.Flags().GetString("root")
	global, _ := cmd.Flags().GetBool("global")
	storeFlag, _ := cmd.Flags().GetString("store")
	env := d.workspaceEnv()
	ref, err := workspace.ParseRef(storeFlag)
	if err != nil {
		return nil, err
	}
	switch {
	case all && storeFlag != "":
		return nil, usageErr("--all and --store cannot be combined: --all covers every store")
	case ref.Global && root != "":
		return nil, usageErr("--store global cannot be combined with --root: global names the global location")
	case ref.Global:
		global = true
	case !all && !ref.IsZero():
		// NAME, NAME@LOC, PATH or an alias: resolved like every other command (resolveRef), but
		// reading kb.toml leniently, since an older location file is what this command upgrades.
		rr, err := d.resolveStoreFlag(cmd, true, rawLocation)
		if err != nil {
			return nil, err
		}
		return d.withCacheItem(append(locationItems(rr.ws.Location), d.storeItems(rr.ws)...)), nil
	}
	loc, err := workspace.ResolveLocation(root, global, env)
	if err != nil {
		return nil, err
	}
	if _, err := os.Stat(loc.FilePath); err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			return nil, errs.Wrap(err, errs.NotFound, "cli.migrate", "no kb location file at %s; run kb init", loc.FilePath)
		}
		return nil, usageErr("%v", err)
	}
	items := locationItems(loc)
	if all {
		names, _, err := loc.Stores()
		if err != nil {
			return nil, err
		}
		for _, n := range names {
			ws, err := loc.StoreAt(n)
			if err != nil {
				return nil, err
			}
			items = append(items, d.storeItems(ws)...)
		}
		// The global location, unless that is where we already are.
		if gl, err := workspace.ResolveLocation("", true, env); err == nil && gl.Root != loc.Root {
			if _, err := os.Stat(gl.FilePath); err == nil {
				items = append(items, locationItems(gl)...)
				gnames, _, err := gl.Stores()
				if err != nil {
					return nil, err
				}
				for _, n := range gnames {
					ws, err := gl.StoreAt(n)
					if err != nil {
						return nil, err
					}
					items = append(items, d.storeItems(ws)...)
				}
			}
		}
	} else { // no --store, or global: the location's default_store
		lf, err := rawLocation(loc.FilePath)
		if err != nil {
			return nil, err
		}
		ws, err := loc.Store(lf.DefaultStore)
		if err != nil {
			return nil, err
		}
		items = append(items, d.storeItems(ws)...)
	}
	return d.withCacheItem(items), nil
}

// withCacheItem appends the embedding cache, shown for information (it migrates itself on open).
func (d *deps) withCacheItem(items []migrateItem) []migrateItem {
	if d.embedCachePath != nil { // nil: the cache is off (tests)
		if p, err := d.embedCachePath(); err == nil {
			items = append(items, existing(migrateItem{Scope: "cache", Art: cache.NewArtefact(p), Path: p, Info: true}))
		}
	}
	return items
}

func locationItems(loc *workspace.Location) []migrateItem {
	return []migrateItem{existing(migrateItem{Scope: "location", Where: loc.Root, Art: config.NewLocationArtefact(loc.FilePath), Path: loc.FilePath})}
}

func (d *deps) storeItems(ws *workspace.Workspace) []migrateItem {
	scope := "store " + ws.Store
	return []migrateItem{
		existing(migrateItem{Scope: scope, Where: ws.Root, Art: config.NewArtefact(ws.ConfigPath), Path: ws.ConfigPath}),
		existing(migrateItem{Scope: scope, Where: ws.Root, Art: d.indexArtefact(ws.DBPath), Path: ws.DBPath}),
	}
}

// existing marks the item absent when its file is not there.
func existing(it migrateItem) migrateItem {
	if _, err := os.Stat(it.Path); errors.Is(err, fs.ErrNotExist) {
		it.Absent = true
	}
	return it
}

// rawLocation is the locationReader of kb migrate: default_store and [aliases] of a kb.toml,
// read without the version check config.ReadLocation makes (an older location file must not stop
// the command that upgrades it). No kb.toml is NotFound, as the Resolver expects.
func rawLocation(path string) (config.Location, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			return config.Location{}, errs.Wrap(err, errs.NotFound, "cli.migrate", "no kb location file at %s; run kb init", path).With("path", path)
		}
		return config.Location{}, usageErr("cannot read %s: %v", path, err)
	}
	var head struct {
		DefaultStore string            `toml:"default_store"`
		Aliases      map[string]string `toml:"aliases"`
	}
	if _, err := toml.Decode(string(data), &head); err != nil {
		return config.Location{}, usageErr("%s: %v", path, err)
	}
	if head.DefaultStore == "" {
		head.DefaultStore = config.DefaultStoreName
	}
	return config.Location{DefaultStore: head.DefaultStore, Aliases: head.Aliases}, nil
}

// migrateRow is one file in the status output; the JSON shape is a stable contract.
type migrateRow struct {
	Scope    string        `json:"scope"`
	Location string        `json:"location,omitempty"`
	Artefact string        `json:"artefact"`
	Path     string        `json:"path"`
	Current  int           `json:"current"`
	Target   int           `json:"target"`
	State    string        `json:"state"` // current, pending, newer, absent
	Auto     bool          `json:"auto,omitempty"`
	Pending  []migrateStep `json:"pending,omitempty"`
	// Partials are unfinished backups an interrupted migration left beside the file (Q65).
	Partials []string `json:"partials,omitempty"`
}

type migrateStep struct {
	ID     string `json:"id"`
	Desc   string `json:"desc"`
	Effect string `json:"effect"`
}

func stepsOf(infos []migrate.Info) []migrateStep {
	out := make([]migrateStep, len(infos))
	for i, in := range infos {
		out[i] = migrateStep{in.ID, in.Desc, in.Effect.String()}
	}
	return out
}

// statusRows reads every item's status. The artefact name comes from the adapter's Status, so
// an absent file still gets its name from the item.
func statusRows(ctx context.Context, items []migrateItem) ([]migrateRow, error) {
	rows := make([]migrateRow, 0, len(items))
	for _, it := range items {
		st, err := it.Art.Status(ctx)
		if err != nil {
			return nil, err
		}
		row := migrateRow{Scope: it.Scope, Location: it.Where, Artefact: st.Artefact, Path: it.Path, Current: st.Current, Target: st.Target, Auto: it.Info}
		row.Partials = migrate.Partials(it.Path)
		switch {
		case it.Absent:
			row.State, row.Pending = "absent", nil
		case st.Newer():
			row.State = "newer"
		case len(st.Pending) > 0:
			row.State, row.Pending = "pending", stepsOf(st.Pending)
		default:
			row.State = "current"
		}
		rows = append(rows, row)
	}
	return rows, nil
}

// needsAttention reports whether a row makes `kb migrate status` exit 1. The cache never does.
func (r migrateRow) needsAttention() bool {
	return !r.Auto && (r.State == "pending" || r.State == "newer")
}

func runMigrateStatus(cmd *cobra.Command, d *deps, stdout io.Writer, f migrateFlags) error {
	items, err := d.migrateScope(cmd, f.all)
	if err != nil {
		return err
	}
	rows, err := statusRows(cmd.Context(), items)
	if err != nil {
		return err
	}
	attention := false
	for _, r := range rows {
		attention = attention || r.needsAttention()
	}
	if f.asJSON {
		enc := json.NewEncoder(stdout)
		enc.SetIndent("", "  ")
		if err := enc.Encode(struct {
			Artefacts []migrateRow `json:"artefacts"`
		}{rows}); err != nil {
			return err
		}
	} else if err := writeMigrateStatus(stdout, rows); err != nil {
		return err
	}
	if attention {
		return exitSilently(ExitUsage)
	}
	return nil
}

func versionText(r migrateRow) string {
	switch r.State {
	case "absent":
		return "-"
	case "pending":
		return fmt.Sprintf("v%d -> v%d", r.Current, r.Target)
	}
	return fmt.Sprintf("v%d", r.Current)
}

func stateText(r migrateRow) string {
	switch r.State {
	case "pending":
		return fmt.Sprintf("%d pending: run kb migrate up", len(r.Pending))
	case "newer":
		return "written by a newer kb (v" + fmt.Sprint(r.Current) + " > v" + fmt.Sprint(r.Target) + "): upgrade kb"
	case "absent":
		return "not created yet"
	}
	if r.Auto {
		return "current (migrates automatically)"
	}
	return "current"
}

// writeMigrateStatus prints the status table, then each pending step under its file.
func writeMigrateStatus(w io.Writer, rows []migrateRow) error {
	tw := tabwriter.NewWriter(w, 0, 2, 2, ' ', 0)
	fmt.Fprintln(tw, "SCOPE\tFILE\tVERSION\tSTATE\tPATH")
	for _, r := range rows {
		fmt.Fprintf(tw, "%s\t%s\t%s\t%s\t%s\n", r.Scope, r.Artefact, versionText(r), stateText(r), r.Path)
	}
	if err := tw.Flush(); err != nil {
		return err
	}
	for _, r := range rows {
		for i, s := range r.Pending {
			if i == 0 {
				fmt.Fprintf(w, "\n%s %s:\n", r.Scope, r.Artefact)
			}
			note := ""
			if s.Effect != "none" {
				note = "  [needs kb reindex; effect " + s.Effect + "]"
			}
			fmt.Fprintf(w, "  %s  %s%s\n", s.ID, s.Desc, note)
		}
	}
	for _, r := range rows {
		for i, p := range r.Partials {
			if i == 0 {
				fmt.Fprintf(w, "\n%s %s: unfinished backups from an interrupted migration (not usable; the next kb migrate up deletes them, or delete them yourself):\n", r.Scope, r.Artefact)
			}
			fmt.Fprintf(w, "  %s\n", p)
		}
	}
	return nil
}

// runItems are the items `up` and `verify` act on: not the cache, not absent files.
func runItems(items []migrateItem) []migrateItem {
	var out []migrateItem
	for _, it := range items {
		if !it.Info && !it.Absent {
			out = append(out, it)
		}
	}
	return out
}

func runMigrateUp(cmd *cobra.Command, d *deps, stdout io.Writer, f migrateFlags) error {
	items, err := d.migrateScope(cmd, f.all)
	if err != nil {
		return err
	}
	items = runItems(items)
	arts := make([]migrate.Artefact, len(items))
	for i, it := range items {
		arts[i] = it.Art
	}
	rep, runErr := migrate.Run(cmd.Context(), arts, migrate.Options{DryRun: f.dryRun, NoBackup: f.noBackup})
	printMigrateReport(stdout, items, rep)
	return runErr
}

// printMigrateReport prints what Run did to each file, then the effects the user must act on.
func printMigrateReport(w io.Writer, items []migrateItem, rep migrate.Report) {
	byPath := map[string]migrateItem{}
	for _, it := range items {
		byPath[it.Path] = it
	}
	verb, done := "migrated", "applied"
	if rep.DryRun {
		verb, done = "would migrate", "would apply"
	}
	changed := 0
	var needs []string
	for _, o := range rep.Artefacts {
		steps := o.Applied
		if rep.DryRun {
			steps = o.Status.Pending
		}
		it := byPath[o.Status.Path]
		if len(steps) == 0 {
			continue
		}
		changed++
		fmt.Fprintf(w, "%s %s %s: v%d -> v%d (%s)\n", verb, it.Scope, o.Status.Artefact, o.Status.Current, o.Status.Target, o.Status.Path)
		switch {
		case o.Backup != "" && rep.DryRun:
			fmt.Fprintf(w, "  backup would be written to %s\n", o.Backup)
		case o.Backup != "":
			fmt.Fprintf(w, "  backup: %s\n", o.Backup)
		case o.Status.Current > 0:
			fmt.Fprintln(w, "  no backup (--no-backup)")
		}
		for _, s := range steps {
			fmt.Fprintf(w, "  %s %s: %s\n", done, s.ID, s.Desc)
			if s.Effect != migrate.EffectNone {
				needs = append(needs, fmt.Sprintf("%s: %s", it.Scope, s.ID))
			}
		}
		if (o.Status.Artefact == config.ConfigArtefactName || o.Status.Artefact == config.LocationArtefactName) && !rep.DryRun {
			fmt.Fprintln(w, "  comments in the file are not kept; the backup has the original text")
		}
		for _, warn := range o.Warnings {
			fmt.Fprintf(w, "  warning: %s\n", warn)
		}
	}
	if changed == 0 {
		fmt.Fprintln(w, "everything is up to date")
	}
	if len(needs) > 0 {
		sort.Strings(needs)
		fmt.Fprintf(w, "needs: kb reindex (%s)\n", strings.Join(needs, "; "))
	}
}

func runMigrateVerify(cmd *cobra.Command, d *deps, stdout io.Writer, f migrateFlags) error {
	items, err := d.migrateScope(cmd, f.all)
	if err != nil {
		return err
	}
	failed := 0
	for _, it := range runItems(items) {
		name := filepath.Base(it.Path)
		v, ok := it.Art.(migrate.Verifier)
		if !ok {
			continue
		}
		if err := v.Verify(cmd.Context()); err != nil {
			if errors.Is(err, context.Canceled) || errs.KindOf(err) == errs.Interrupted {
				return err
			}
			fmt.Fprintf(stdout, "FAIL %s %s: %v\n", it.Scope, name, err)
			failed++
			continue
		}
		fmt.Fprintf(stdout, "ok   %s %s\n", it.Scope, name)
	}
	if failed > 0 {
		return usageErr("%d file(s) failed verification", failed)
	}
	return nil
}
