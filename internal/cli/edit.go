// `kb edit`: open an entry in $EDITOR, then reindex it.
package cli

import (
	"context"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"

	"github.com/spf13/cobra"

	"github.com/nickstrad/kb/internal/config"
	"github.com/nickstrad/kb/internal/entry"
	"github.com/nickstrad/kb/internal/reindex"
	"github.com/nickstrad/kb/internal/workspace"
)

// newEditCmd builds `kb edit <entry>`.
func newEditCmd(d *deps, stdout, stderr io.Writer) *cobra.Command {
	return &cobra.Command{
		Use:           "edit <entry>",
		Short:         "Edit an entry in $EDITOR and reindex it",
		Args:          cobra.ExactArgs(1),
		SilenceUsage:  true,
		SilenceErrors: true,
		RunE: func(cmd *cobra.Command, args []string) error {
			ws, cfg, err := d.workspaceConfig(cmd)
			if err != nil {
				return err
			}
			return runEdit(cmd.Context(), cmd, d, stdout, stderr, ws, cfg, args[0])
		},
	}
}

// runEdit resolves the entry, opens $EDITOR (default "vi") on its front-door file, and — only if
// the editor exits 0 and the entry's content actually changed — reindexes it via reindex.One.
// Front matter that is now invalid is reported and left un-reindexed; the database is never
// touched unless the reindex itself runs.
func runEdit(ctx context.Context, cmd *cobra.Command, d *deps, stdout, stderr io.Writer, ws *workspace.Workspace, cfg config.Config, arg string) error {
	root := ws.Root
	e, err := entry.Resolve(root, arg)
	if err != nil {
		return usageErr("%s", err)
	}

	before, err := entry.Load(root, e)
	if err != nil {
		return usageErr("%s", err)
	}
	beforeHash := entry.BodyHash(before, e.Files)

	// Resolve the embedder before the editor runs, so a configuration mistake is reported before
	// the file changes rather than leaving an edited, unindexed entry behind.
	embedder, err := d.newEmbedder(cmd, cfg.Embed)
	if err != nil {
		return err
	}

	if err := d.editor(ctx, cfg.Edit.Editor, filepath.Join(root, filepath.FromSlash(e.Path))); err != nil {
		return usageErr("%s", err)
	}

	// Re-resolve rather than reuse e: the editor may have changed the front matter, and Discover
	// is what (re)computes Meta and Err from the file as it now stands.
	e2, err := entry.Resolve(root, arg)
	if err != nil {
		return usageErr("%s", err)
	}
	if e2.Err != nil {
		return usageErr("%s", e2.Err)
	}

	after, err := entry.Load(root, e2)
	if err != nil {
		return usageErr("%s", err)
	}
	afterHash := entry.BodyHash(after, e2.Files)

	if afterHash == beforeHash {
		fmt.Fprintln(stdout, "unchanged")
		return nil
	}

	st, err := d.openStoreForWrite(ctx, ws)
	if err != nil {
		return err
	}
	defer st.Close()

	// all=false: a single `kb edit` cannot recover from an embed_meta mismatch; the usage error
	// tells the user to run `kb reindex --all`.
	force, err := ensureEmbedMetaForReindex(ctx, st, embedder, false, stderr)
	if err != nil {
		return err
	}

	opts := reindex.Options{
		Root:      root,
		Store:     st,
		Embedder:  embedder,
		BatchSize: cfg.Embed.Batch,
		Stdout:    stdout,
		Stderr:    stderr,
		Force:     force,
	}
	sum, err := reindex.One(ctx, opts, e2)
	if err != nil {
		return err
	}
	fmt.Fprintln(stdout, sum.String())

	if sum.Failed > 0 {
		return usageErr("reindex of %s failed; see warnings above", e2.Path)
	}
	return nil
}

// chooseEditor picks the editor command: the configured one, else $EDITOR, else vi.
func chooseEditor(configured, env string) string {
	switch {
	case configured != "":
		return configured
	case env != "":
		return env
	}
	return "vi"
}

// runEditor is deps.editor in production: the [edit] editor command, else $EDITOR, else "vi",
// on abs, attached to the terminal, run through `sh -c '<editor> "$@"' sh <path>` so "vi",
// "code --wait" and shell one-liners all work and the path stays one correctly quoted argument.
// $EDITOR is the only environment variable kb reads besides root discovery's XDG_DATA_HOME.
func runEditor(ctx context.Context, configured, abs string) error {
	editor := chooseEditor(configured, os.Getenv("EDITOR"))

	cmd := exec.CommandContext(ctx, "sh", "-c", editor+` "$@"`, "sh", abs)
	cmd.Stdin = os.Stdin
	cmd.Stdout = os.Stdout
	cmd.Stderr = os.Stderr
	if err := cmd.Run(); err != nil {
		return fmt.Errorf("editor (%s) failed: %w", editor, err)
	}
	return nil
}
