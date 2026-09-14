// `kb edit`: open an entry in $EDITOR, then reindex it. P4.2 implements it.
package cli

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"

	"github.com/spf13/cobra"

	"knowledge/kb/internal/embed/ollama"
	"knowledge/kb/internal/entry"
	"knowledge/kb/internal/reindex"
)

// newEditCmd builds `kb edit <entry>`.
func newEditCmd(stdout, stderr io.Writer) *cobra.Command {
	return &cobra.Command{
		Use:           "edit <entry>",
		Short:         "Edit an entry in $EDITOR and reindex it",
		Args:          cobra.ExactArgs(1),
		SilenceUsage:  true,
		SilenceErrors: true,
		RunE: func(cmd *cobra.Command, args []string) error {
			return runEdit(cmd.Context(), stdout, stderr, Root(), args[0])
		},
	}
}

// runEdit resolves the entry, opens $EDITOR (default "vi") on its front-door file, and — only if
// the editor exits 0 and the entry's content actually changed — reindexes it via reindex.One.
// Front matter that is now invalid is reported and left un-reindexed; the database is never
// touched unless the reindex itself runs.
func runEdit(ctx context.Context, stdout, stderr io.Writer, root, arg string) error {
	e, err := entry.Resolve(root, arg)
	if err != nil {
		return usageErr("kb edit: %s", err)
	}

	before, err := entry.Load(root, e)
	if err != nil {
		return usageErr("kb edit: %s", err)
	}
	beforeHash := entry.BodyHash(before, e.Files)

	if err := runEditor(ctx, root, e.Path); err != nil {
		return usageErr("kb edit: %s", err)
	}

	// Re-resolve rather than reuse e: the editor may have changed the front matter, and Discover
	// is what (re)computes Meta and Err from the file as it now stands.
	e2, err := entry.Resolve(root, arg)
	if err != nil {
		return usageErr("kb edit: %s", err)
	}
	if e2.Err != nil {
		var ve *entry.ValidationError
		if errors.As(e2.Err, &ve) {
			return usageErr("kb edit: %s", ve.Error())
		}
		return usageErr("kb edit: %s", e2.Err)
	}

	after, err := entry.Load(root, e2)
	if err != nil {
		return usageErr("kb edit: %s", err)
	}
	afterHash := entry.BodyHash(after, e2.Files)

	if afterHash == beforeHash {
		fmt.Fprintln(stdout, "unchanged")
		return nil
	}

	st, err := openStoreForWrite(root)
	if err != nil {
		return usageErr("kb edit: %s", err)
	}
	defer st.Close()

	embedder := newEmbedder()
	// all=false: a single `kb edit` cannot recover from an embed_meta mismatch on its own (see
	// ensureEmbedMetaForReindex's doc in reindex.go) — that path already returns a usage error
	// telling the user to run `kb reindex --all`.
	force, err := ensureEmbedMetaForReindex(ctx, st, embedder, false, stderr)
	if err != nil {
		return usageErr("kb edit: %s", strings.TrimPrefix(err.Error(), "kb reindex: "))
	}

	opts := reindex.Options{
		Root:      root,
		Store:     st,
		Embedder:  embedder,
		BatchSize: reindexBatchSize,
		Stdout:    stdout,
		Stderr:    stderr,
		Force:     force,
	}
	sum, err := reindex.One(ctx, opts, e2)
	if err != nil {
		if errors.Is(err, ollama.ErrUnavailable) {
			return embedderErr("kb edit: %s", err)
		}
		return usageErr("kb edit: %s", err)
	}
	fmt.Fprintln(stdout, sum.String())

	if sum.Failed > 0 {
		return usageErr("kb edit: reindex of %s failed; see warnings above", e2.Path)
	}
	return nil
}

// runEditor runs $EDITOR (default "vi") on root/path, with stdin/stdout/stderr attached to the
// terminal and ctx as its cancellation.
//
// The editor value is handed to a shell rather than split on whitespace ourselves:
// `sh -c '<editor> "$@"' sh <path>` runs <editor> through /bin/sh, with <path> passed through
// "$@" as a single, correctly quoted argument no matter what it contains. This is what makes a
// bare command ("vi"), one with its own arguments ("code --wait"), and a whole shell one-liner all
// work the same way a real shell would run them.
func runEditor(ctx context.Context, root, path string) error {
	editor := os.Getenv("EDITOR")
	if editor == "" {
		editor = "vi"
	}
	abs := filepath.Join(root, filepath.FromSlash(path))

	cmd := exec.CommandContext(ctx, "sh", "-c", editor+` "$@"`, "sh", abs)
	cmd.Stdin = os.Stdin
	cmd.Stdout = os.Stdout
	cmd.Stderr = os.Stderr
	if err := cmd.Run(); err != nil {
		return fmt.Errorf("$EDITOR (%s) failed: %w", editor, err)
	}
	return nil
}
