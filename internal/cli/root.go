// Package cli is the kb command tree: one file per command, plus this file for the shared parts —
// the root command, root/database path resolution, the exit codes and the error reporting.
//
// The tree is rebuilt from scratch on every Execute call, so no command state survives a run and
// tests can invoke the CLI repeatedly against different roots. The only package-level variable is
// Version, which the build stamps once and nothing mutates afterwards.
//
// Exit codes (plan.md, "CLI"): 0 success, ExitUsage for a usage or validation failure,
// ExitEmbedder when the embedder is unavailable.
package cli

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"

	"github.com/spf13/cobra"
)

// Exit codes. ExitOK is not returned through an error, it is the zero value of Execute.
const (
	ExitOK       = 0
	ExitUsage    = 1
	ExitEmbedder = 2
)

// DefaultRoot is the knowledge repository kb works on when KB_ROOT is unset.
const DefaultRoot = "/root/Raw/knowledge"

// Version is printed by `kb version`; the install script stamps it with the repo's short hash.
var Version = "dev"

// exitError carries an explicit exit code for one failure.
type exitError struct {
	code int
	msg  string
}

func (e *exitError) Error() string { return e.msg }

// usageErr is a usage or validation failure: ExitUsage.
func usageErr(format string, args ...any) error {
	return &exitError{code: ExitUsage, msg: fmt.Sprintf(format, args...)}
}

// embedderErr is an "embedder unavailable" failure: ExitEmbedder.
func embedderErr(format string, args ...any) error {
	return &exitError{code: ExitEmbedder, msg: fmt.Sprintf(format, args...)}
}

// notImplemented is what every command this skeleton has not filled in yet returns. It exits 1,
// like a usage error, so scripts cannot mistake it for success.
func notImplemented(command string) error {
	return &exitError{code: ExitUsage, msg: fmt.Sprintf("kb %s: not implemented yet", command)}
}

// report prints one failure to stderr and returns its exit code.
func report(stderr io.Writer, err error) int {
	var ee *exitError
	if errors.As(err, &ee) {
		fmt.Fprintf(stderr, "%s\n", ee.msg)
		return ee.code
	}
	fmt.Fprintf(stderr, "kb: %s\n", err.Error())
	return ExitUsage
}

// Root returns the knowledge repository root: $KB_ROOT when it is set and non-empty, else
// DefaultRoot.
func Root() string {
	if r := os.Getenv("KB_ROOT"); r != "" {
		return r
	}
	return DefaultRoot
}

// DBPath is the database file for a repo root: <root>/.kb/kb.sqlite. It only computes the path;
// a command that is about to create the database calls EnsureDBDir first.
func DBPath(root string) string {
	return filepath.Join(root, ".kb", "kb.sqlite")
}

// EnsureDBDir creates <root>/.kb so the database can be opened for writing. Read-only callers do
// not need it: a missing directory means there is no database yet, which is worth reporting rather
// than papering over.
func EnsureDBDir(root string) error {
	dir := filepath.Join(root, ".kb")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return usageErr("create %s: %s", dir, err)
	}
	return nil
}

// Caller resolves the --caller value: the flag, else $KB_CALLER, else "unknown".
func Caller(flag string) string {
	if flag != "" {
		return flag
	}
	if c := os.Getenv("KB_CALLER"); c != "" {
		return c
	}
	return "unknown"
}

// Execute runs one kb command line and returns its exit code. args excludes the program name. ctx
// carries the process's cancellation (main wires it to SIGINT and SIGTERM) and reaches every
// handler through cmd.Context().
func Execute(ctx context.Context, args []string, stdout, stderr io.Writer) int {
	cmd := newRootCmd(stdout, stderr)
	cmd.SetArgs(args)
	if err := cmd.ExecuteContext(ctx); err != nil {
		return report(stderr, err)
	}
	return ExitOK
}

// newRootCmd builds the whole command tree for one invocation.
func newRootCmd(stdout, stderr io.Writer) *cobra.Command {
	cmd := &cobra.Command{
		Use:   "kb",
		Short: "Search and maintain the knowledge repository",
		Long: "kb indexes knowledge entries under data/ in $KB_ROOT (default " + DefaultRoot + ")\n" +
			"into .kb/kb.sqlite and searches it with FTS5 and sqlite-vec at once.",
		SilenceUsage:  true,
		SilenceErrors: true,
	}
	cmd.SetOut(stdout)
	cmd.SetErr(stderr)
	// Flag parse failures are usage errors; wrap them so the exit code is ours.
	cmd.SetFlagErrorFunc(func(_ *cobra.Command, err error) error { return usageErr("kb: %s", err.Error()) })

	cmd.AddCommand(
		newAddCmd(stdout, stderr),
		newEditCmd(stdout, stderr),
		newRmCmd(stdout, stderr),
		newShowCmd(stdout, stderr),
		newListCmd(stdout, stderr),
		newSearchCmd(stdout, stderr),
		newReindexCmd(stdout, stderr),
		newIndexCmd(stdout, stderr),
		newDoctorCmd(stdout, stderr),
		newFeedbackCmd(stdout, stderr),
		newStatsCmd(stdout, stderr),
		newVersionCmd(stdout),
	)
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
