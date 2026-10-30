// Package cli is the kb command tree: one file per command, plus this file for the shared parts
// (the root command, root and database paths, deps) and report.go for errors and exit codes.
//
// The tree is rebuilt on every Execute call, so no command state survives a run and tests can
// invoke the CLI repeatedly. The only package-level variable is Version, stamped once at start.
// Everything a test may replace, including the working directory and home that root discovery
// reads, arrives through deps.
//
// Exit codes (the CLI contract): 0 success, ExitUsage (1) for a well-formed failure, ExitEmbedder
// (2) when the embedder is unavailable, ExitInternal (3) for a bug, which is logged as an
// incident.
package cli

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"

	"github.com/spf13/cobra"

	"github.com/nickstrad/kb/internal/config"
	"github.com/nickstrad/kb/internal/embed"
	"github.com/nickstrad/kb/internal/embed/local"
	"github.com/nickstrad/kb/internal/store"
	"github.com/nickstrad/kb/internal/store/sqlite"
	"github.com/nickstrad/kb/internal/workspace"
)

// Exit codes. ExitOK is not returned through an error, it is the zero value of Execute.
const (
	ExitOK       = 0
	ExitUsage    = 1
	ExitEmbedder = 2
	ExitInternal = 3
)

// deps is everything a command needs from the outside world that a test may replace.
type deps struct {
	// embedder, when set, replaces the embedder the [embed] config would build.
	embedder      func(config.Embed) (embed.Embedder, error)
	modelCacheDir func() (string, error)
	pullModel     func(ctx context.Context, spec local.Spec, cacheDir string) error
	openStore     func(ctx context.Context, path string, readOnly bool) (store.Store, error)
	// editor runs the configured editor command on path; configured is [edit] editor, "" when unset.
	editor func(ctx context.Context, configured, path string) error
	// userCacheDir is where the incident log goes when there is no <root>/.kb.
	userCacheDir func() (string, error)
	// workspaceEnv supplies the working directory, home, XDG_DATA_HOME and cache base that
	// workspace.Resolve reads.
	workspaceEnv func() workspace.Env
}

// productionDeps are the real implementations.
func productionDeps() *deps {
	return &deps{
		modelCacheDir: local.DefaultCacheDir,
		pullModel:     local.Pull,
		openStore:     openSQLite,
		editor:        runEditor,
		userCacheDir:  os.UserCacheDir,
		workspaceEnv:  workspace.OSEnv,
	}
}

// openSQLite is deps.openStore in production.
func openSQLite(ctx context.Context, path string, readOnly bool) (store.Store, error) {
	st, err := sqlite.Open(ctx, path, sqlite.Options{ReadOnly: readOnly})
	if err != nil {
		return nil, err // not `return sqlite.Open(...)`: a nil *Store would become a non-nil interface
	}
	return st, nil
}

// workspace resolves the store this command runs on from the root persistent flags (--root,
// --global), else by walking up from the working directory. The error is already well-formed.
// Under --debug it says on stderr which store was chosen and how.
func (d *deps) workspace(cmd *cobra.Command) (*workspace.Workspace, error) {
	// Get* fails only for an undefined flag; both are root persistent flags, so "" / false is safe.
	root, _ := cmd.Flags().GetString("root")
	global, _ := cmd.Flags().GetBool("global")
	env := d.workspaceEnv()
	ws, err := workspace.Resolve(root, global, env)
	if err == nil && debugFlag(cmd) {
		how := "discovered from " + env.Cwd
		switch {
		case root != "":
			how = "--root"
		case global:
			how = "--global"
		}
		fmt.Fprintf(cmd.ErrOrStderr(), "%s: store %s (%s)\n", cmd.CommandPath(), ws.Root, how)
	}
	return ws, err
}

// Version is printed by `kb version` and recorded with every logged search; main sets it from
// ResolveVersion.
var Version = "dev"

// Execute runs one kb command line and returns its exit code. args excludes the program name. ctx
// carries the process's cancellation (main wires it to SIGINT and SIGTERM) and reaches every
// handler through cmd.Context().
func Execute(ctx context.Context, args []string, stdout, stderr io.Writer) int {
	return execute(ctx, productionDeps(), args, stdout, stderr)
}

// execute is Execute with explicit deps. It holds the codebase's only recover: any panic (an
// assert.Violation or a runtime error) becomes an incident and ExitInternal, never a crash with
// Go's exit 2, which would read as "embedder unavailable".
func execute(ctx context.Context, d *deps, args []string, stdout, stderr io.Writer) (code int) {
	cmd := newRootCmd(d, stdout, stderr)
	cmd.SetArgs(args)
	ran := cmd
	defer func() {
		if r := recover(); r != nil {
			code = reportPanic(stderr, d, ran, args, r)
		}
	}()
	ran, err := cmd.ExecuteContextC(ctx)
	if err == nil {
		return ExitOK
	}
	var re runError
	if !errors.As(err, &re) {
		// cobra failed before running the command: unknown command or flag, bad argument count.
		err = usageErr("%v", err)
	} else {
		err = re.err
	}
	return report(stderr, d, ran, args, err)
}

// runError marks an error returned by a command's RunE, as opposed to one cobra produced while
// parsing, which is always a usage error.
type runError struct{ err error }

func (e runError) Error() string { return e.err.Error() }
func (e runError) Unwrap() error { return e.err }

// markRunErrors wraps every RunE in the tree so execute can tell its errors from cobra's.
func markRunErrors(c *cobra.Command) {
	if run := c.RunE; run != nil {
		c.RunE = func(cmd *cobra.Command, args []string) error {
			if err := run(cmd, args); err != nil {
				return runError{err}
			}
			return nil
		}
	}
	for _, sub := range c.Commands() {
		markRunErrors(sub)
	}
}

// newRootCmd builds the whole command tree for one invocation.
func newRootCmd(d *deps, stdout, stderr io.Writer) *cobra.Command {
	cmd := &cobra.Command{
		Use:   "kb",
		Short: "Search and maintain the knowledge repository",
		Long: "kb is a knowledge store for agents and people: Markdown entries, indexed for hybrid\n" +
			"full-text and vector search. kb help is the reference for using it.\n\n" +
			"Store: kb works on the nearest directory at or above the current one that holds a kb\n" +
			"store. --root DIR names a store explicitly\n" +
			"and --global uses the machine-wide store ($XDG_DATA_HOME/kb, else ~/.local/share/kb);\n" +
			"add and reindex create the store there when it is missing. Entries live under data/;\n" +
			"the index and search log live in the store's database.\n\n" +
			"Use it in two steps:\n" +
			"  kb search \"<query>\" --caller claude   ranked hits, opening with \"search N · M hits\"\n" +
			"  kb show <path>                        read a hit's entry\n" +
			"kb feedback is optional: kb feedback N <rank> --useful (or --not-useful, or N --none)\n" +
			"records whether a search helped, which kb stats reports on.\n\n" +
			"Embeddings are computed in-process by a local model (default " + local.DefaultModel + "),\n" +
			"downloaded once per machine by kb model pull, or automatically by kb reindex and\n" +
			"kb add. Search never downloads: without the model it falls back to full-text search\n" +
			"(exit 2). The model is the [embed] model key of the store config (kb config --path; kb model list shows\n" +
			"the supported ids; \"none\" indexes and searches with full-text search only). Changing\n" +
			"it on an existing index needs kb reindex --all. kb config shows the effective settings.",
		SilenceUsage:  true,
		SilenceErrors: true,
	}
	cmd.SetOut(stdout)
	cmd.SetErr(stderr)
	cmd.PersistentFlags().String("root", "", "use the store in DIR instead of discovering one from the current directory")
	cmd.PersistentFlags().Bool("global", false, "use the machine-wide store ($XDG_DATA_HOME/kb, else ~/.local/share/kb)")
	cmd.MarkFlagsMutuallyExclusive("root", "global")
	cmd.PersistentFlags().Bool("debug", false, "print extra detail on stderr: the full error chain, and best-effort steps that failed quietly")

	cmd.AddCommand(
		newAddCmd(d, stdout, stderr),
		newEditCmd(d, stdout, stderr),
		newRmCmd(d, stdout, stderr),
		newShowCmd(d, stdout, stderr),
		newListCmd(d, stdout, stderr),
		newSearchCmd(d, stdout, stderr),
		newReindexCmd(d, stdout, stderr),
		newIndexCmd(d, stdout, stderr),
		newDoctorCmd(d, stdout, stderr),
		newFeedbackCmd(d, stdout, stderr),
		newStatsCmd(d, stdout, stderr),
		newVersionCmd(stdout),
		newModelCmd(d, stdout, stderr),
		newConfigCmd(d, stdout, stderr),
	)
	markRunErrors(cmd)
	return cmd
}

// newVersionCmd prints the version the binary was built from.
func newVersionCmd(stdout io.Writer) *cobra.Command {
	return &cobra.Command{
		Use:   "version",
		Short: "Print the kb version",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, args []string) error {
			fmt.Fprintf(stdout, "kb %s\n", Version)
			return nil
		},
	}
}
