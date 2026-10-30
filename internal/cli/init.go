// `kb init [--name NAME] [--model ID|none] [--source GLOB]... [--force]`: create a location and a
// named store in it. It is the only command that creates either.
//
// Steps, in order, each idempotent:
//  1. Resolve the target location: the working directory itself (not an ancestor), --root, or
//     --global. Creating a location at $HOME or / needs --force (DEFAULTS #121): a stray location
//     there would capture every directory below it.
//  2. Create .kb/kb.toml (default_store = this store) and .kb/.gitignore when there is no
//     location yet. An existing location is read, never rewritten: adding a second store leaves
//     default_store alone.
//  3. Create .kb/stores/NAME/config.toml with explicit [[sources]] (PLAN 1.5): the default store
//     reads the entries directory data/, a named store data-NAME/ (both created now), and
//     --source GLOB (repeatable) replaces that with in-place files sources and creates no
//     directory. A store that already exists is "already initialised": nothing is written, and
//     step 4 runs again with the store's own [embed], so a rerun finishes an init that exited 2.
//  4. Create the database at the current schema and record the embedder in embed_meta, then load
//     the model (downloading it when missing, with progress on stderr). A model that cannot be
//     loaded exits 2 and leaves the store created: kb model pull, or kb init again, finishes it.
package cli

import (
	"context"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"strings"

	"github.com/spf13/cobra"

	"github.com/nickstrad/kb/internal/config"
	"github.com/nickstrad/kb/internal/embed/local"
	"github.com/nickstrad/kb/internal/entry"
	"github.com/nickstrad/kb/internal/errs"
	"github.com/nickstrad/kb/internal/store"
	"github.com/nickstrad/kb/internal/workspace"
)

// gitignoreBody is .kb/.gitignore: the derived files stay out of git, the configs can be committed.
// migrate.lock is the advisory lock kb migrate leaves beside each artefact (internal/migrate).
const gitignoreBody = "# Written by kb init. Databases, baselines, backups, the incident log and migration locks are derived.\n" +
	"stores/*/kb.sqlite*\nstores/*/eval/\n*.bak-v*\nkb.log\nmigrate.lock\n"

// newInitCmd builds `kb init`.
func newInitCmd(d *deps, stdout, stderr io.Writer) *cobra.Command {
	var (
		name    string
		model   string
		sources []string
		force   bool
	)
	cmd := &cobra.Command{
		Use:   "init",
		Short: "Create a location and a named store",
		Long: "kb init creates the location (.kb/kb.toml) in the current directory (or --root DIR, or the\n" +
			"global location with --global) if there is none, then a store in it (default name \"default\",\n" +
			"or --name NAME): its config.toml, its database and its entries directory (data/ for the\n" +
			"default store, data-NAME/ for a named one). With --source GLOB (repeatable) the store\n" +
			"indexes the Markdown files the globs match where they are, instead. The first store of a\n" +
			"location becomes its default_store; a later kb init --name adr adds a store and leaves it alone.\n" +
			"Running it again on an existing store changes nothing. --model picks the embedding model\n" +
			"(kb model list) or none for full-text search only, and the model is downloaded now.\n" +
			"A location at $HOME or / is refused unless --force.",
		Args:          cobra.NoArgs,
		SilenceUsage:  true,
		SilenceErrors: true,
		RunE: func(cmd *cobra.Command, args []string) error {
			if s, _ := cmd.Flags().GetString("store"); s != "" {
				return usageErr("kb init names the store with --name, not --store")
			}
			return runInit(cmd, d, stdout, stderr, initOptions{name: name, model: model, sources: sources, force: force})
		},
	}
	cmd.Flags().StringVar(&name, "name", config.DefaultStoreName, "the store's name: 1-32 characters from a-z, 0-9, _ and -")
	cmd.Flags().StringVar(&model, "model", "", "embedding model id, or none for full-text search only (default "+local.DefaultModel+")")
	cmd.Flags().StringArrayVar(&sources, "source", nil, "index the *.md files this glob matches in place, relative to the location root (repeatable; replaces the entries directory)")
	cmd.Flags().BoolVar(&force, "force", false, "allow creating a location at $HOME or /")
	return cmd
}

type initOptions struct {
	name, model string
	sources     []string // --source globs; none = the store's entries directory
	force       bool
}

func runInit(cmd *cobra.Command, d *deps, stdout, stderr io.Writer, o initOptions) error {
	if err := workspace.ValidName(o.name); err != nil {
		return err
	}
	// Build the store config first: a bad --model or --source fails before anything is written.
	srcs, err := initSources(o.name, o.sources)
	if err != nil {
		return err
	}
	cfgText, ecfg, err := storeConfigText(o.model, srcs)
	if err != nil {
		return err
	}

	root, _ := cmd.Flags().GetString("root")
	global, _ := cmd.Flags().GetBool("global")
	env := d.workspaceEnv()
	loc, err := workspace.TargetLocation(root, global, env)
	if err != nil {
		return err
	}

	newLocation, err := ensureLocation(loc, env, o)
	if err != nil {
		return err
	}
	ws, err := loc.StoreAt(o.name)
	if err != nil {
		return err
	}
	created, err := createStoreFiles(ws, cfgText, srcs)
	if err != nil {
		return err
	}
	if !created {
		fmt.Fprintf(stdout, "store %q is already initialised in %s\n", o.name, loc.Root)
		// Step 4 is idempotent and may not have finished last time (the model could not be
		// downloaded: exit 2), so run it again, with the store's own [embed], not the flags.
		cfg, _, err := config.Read(ws.ConfigPath)
		if err != nil {
			return err
		}
		return prepareIndex(cmd.Context(), cmd, d, stderr, ws, cfg.Embed)
	}
	if newLocation {
		fmt.Fprintf(stdout, "created location %s\n", loc.KBDir)
	}
	fmt.Fprintf(stdout, "created store %q in %s (model %s)\n", o.name, loc.Root, ecfg.Model)

	return prepareIndex(cmd.Context(), cmd, d, stderr, ws, ecfg)
}

// initSources returns the [[sources]] a new store starts with: the --source globs as files
// sources, else one entries directory, data/ for the default store and data-NAME/ for a named one
// (not data/NAME/, which the default store would read as one of its directory entries).
func initSources(name string, globs []string) ([]config.Source, error) {
	if len(globs) == 0 {
		dir := entry.DataDir
		if name != config.DefaultStoreName {
			dir = entry.DataDir + "-" + name
		}
		return []config.Source{{Path: dir, Layout: config.LayoutEntries, Exclude: []string{}}}, nil
	}
	srcs := make([]config.Source, len(globs))
	for i, g := range globs {
		srcs[i] = config.Source{Glob: g, Layout: config.LayoutFiles, Exclude: []string{}}
	}
	c := config.Default()
	c.Sources = srcs
	if err := c.Validate("--source"); err != nil {
		return nil, err
	}
	return srcs, nil
}

// storeConfigText renders the new store's config.toml for the chosen model ("" = the default) and
// sources, and checks that kb itself would read it back.
func storeConfigText(model string, srcs []config.Source) (text string, ecfg config.Embed, err error) {
	ecfg = config.Default().Embed
	if model != "" {
		ecfg.Model = model
	}
	text = "# kb store config. Every key you leave out has its default: kb config shows them all.\n" +
		"version = " + fmt.Sprint(config.SchemaVersion) + "\n\n" +
		"[embed]\n" +
		"# A supported model (kb model list), or \"none\" for full-text search only.\n" +
		"# Changing it on an existing index needs kb reindex --all.\n" +
		"model = " + quoteTOML(ecfg.Model) + "\n\n" +
		"# Sources: what this store indexes, relative to the location root. layout \"entries\" is a\n" +
		"# directory kb add writes to (front matter required); layout \"files\" indexes the *.md files\n" +
		"# a glob matches where they are (front matter optional; exclude = [...] leaves some out).\n" +
		"# A file may belong to one source only. No [[sources]] means nothing is indexed.\n"
	for _, src := range srcs {
		if src.Layout == config.LayoutEntries {
			text += "[[sources]]\npath = " + quoteTOML(src.Path) + "\nlayout = \"entries\"\n"
		} else {
			text += "[[sources]]\nglob = " + quoteTOML(src.Glob) + "\nlayout = \"files\"\n"
		}
	}
	cfg, err := config.Parse([]byte(text), "the new store config")
	if err != nil { // a --model value TOML cannot hold (a newline): Parse's error is already Usage
		return "", ecfg, err
	}
	if err := cfg.CheckModel("--model", knownModel); err != nil {
		return "", ecfg, err
	}
	return text, cfg.Embed, nil
}

// quoteTOML renders s as a TOML basic string.
func quoteTOML(s string) string {
	return `"` + strings.NewReplacer(`\`, `\\`, `"`, `\"`).Replace(s) + `"`
}

// ensureLocation creates the location file and .gitignore when there is no location. An existing
// location is read (so a broken kb.toml is reported) and left alone. created says it was made now.
func ensureLocation(loc *workspace.Location, env workspace.Env, o initOptions) (created bool, err error) {
	if _, statErr := os.Stat(loc.FilePath); statErr == nil {
		if _, err := config.ReadLocation(loc.FilePath); err != nil {
			return false, err
		}
		return false, nil
	}
	if !o.force && isHomeOrRoot(loc.Root, env.Home) {
		return false, usageErr("refusing to create a kb location at %s: it would capture every directory below it. "+
			"Run kb init in a project directory, or use --force", loc.Root)
	}
	if err := os.MkdirAll(loc.KBDir, 0o755); err != nil {
		return false, usageErr("create %s: %v", loc.KBDir, err)
	}
	data, err := config.SaveLocation(config.DefaultLocation(o.name))
	if err != nil {
		return false, err
	}
	switch err := writeNew(loc.FilePath, data); {
	case errors.Is(err, fs.ErrExist):
		return false, nil // another kb init made it first: use it
	case err != nil:
		return false, usageErr("create %s: %v", loc.FilePath, err)
	}
	if err := writeNew(filepath.Join(loc.KBDir, ".gitignore"), []byte(gitignoreBody)); err != nil && !errors.Is(err, fs.ErrExist) {
		return true, usageErr("create %s/.gitignore: %v", loc.KBDir, err)
	}
	return true, nil
}

// isHomeOrRoot reports whether root is the filesystem root or the user's home directory (with
// symlinks resolved on both sides, so /var and /private/var compare equal).
func isHomeOrRoot(root, home string) bool {
	real := func(p string) string {
		if r, err := filepath.EvalSymlinks(p); err == nil {
			return r
		}
		return filepath.Clean(p)
	}
	r := real(root)
	return r == string(filepath.Separator) || (home != "" && r == real(home))
}

// createStoreFiles creates the store directory, its config.toml and its entries directories.
// created is false when the config already exists: the store was initialised before and nothing
// is touched.
func createStoreFiles(ws *workspace.Workspace, cfgText string, srcs []config.Source) (created bool, err error) {
	if fi, statErr := os.Stat(ws.ConfigPath); statErr == nil && fi.Mode().IsRegular() {
		return false, nil
	}
	if err := os.MkdirAll(ws.StoreDir, 0o755); err != nil {
		return false, usageErr("create %s: %v", ws.StoreDir, err)
	}
	switch err := writeNew(ws.ConfigPath, []byte(cfgText)); {
	case errors.Is(err, fs.ErrExist):
		return false, nil // a concurrent kb init won: already initialised
	case err != nil:
		return false, usageErr("create %s: %v", ws.ConfigPath, err)
	}
	for _, src := range srcs {
		if src.Layout != config.LayoutEntries {
			continue
		}
		dir := filepath.Join(ws.Root, filepath.FromSlash(src.Path))
		if err := os.MkdirAll(dir, 0o755); err != nil {
			return true, usageErr("create %s: %v", dir, err)
		}
	}
	return true, nil
}

// writeNew creates path with data and fails with fs.ErrExist when it is already there.
func writeNew(path string, data []byte) error {
	f, err := os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o644)
	if err != nil {
		return err
	}
	if _, err := f.Write(data); err != nil {
		// Best effort: the write error is the one to report, and a half-written file must not
		// pass for an initialised store, so it is removed whatever Close says.
		_ = f.Close()
		_ = os.Remove(path)
		return err
	}
	return f.Close()
}

// prepareIndex creates the database, records the embedder in embed_meta and loads the model so a
// download happens now, not at the first reindex. The embedder is built from ecfg (d.embedder
// replaces it in tests).
func prepareIndex(ctx context.Context, cmd *cobra.Command, d *deps, stderr io.Writer, ws *workspace.Workspace, ecfg config.Embed) error {
	embedder, err := d.newEmbedder(cmd, ecfg)
	if err != nil {
		return err
	}
	st, err := d.openStoreForWrite(ctx, ws)
	if err != nil {
		return err
	}
	defer st.Close()
	if err := st.EnsureEmbedMeta(ctx, store.EmbedMeta{Model: embedder.Model(), Dim: embedder.Dim()}); err != nil {
		return err
	}
	if p, ok := embedder.(pinger); ok {
		fmt.Fprintf(stderr, "kb init: loading model %s\n", embedder.Model())
		if err := p.Ping(ctx); err != nil {
			if errors.Is(err, context.Canceled) {
				return err
			}
			return errs.Wrap(err, errs.Unavailable, "cli.init",
				"the store is created, but the model could not be loaded: %v", err)
		}
	}
	return nil
}
