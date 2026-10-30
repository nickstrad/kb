// `kb rm`: remove an entry's files, drop its database rows.
package cli

import (
	"context"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"

	"github.com/spf13/cobra"

	"github.com/nickstrad/kb/internal/config"
	"github.com/nickstrad/kb/internal/entry"
	"github.com/nickstrad/kb/internal/errs"
	"github.com/nickstrad/kb/internal/workspace"
)

// newRmCmd builds `kb rm <entry> [--yes]`.
func newRmCmd(d *deps, stdout, stderr io.Writer) *cobra.Command {
	var yes bool
	cmd := &cobra.Command{
		Use:   "rm <entry>",
		Short: "Remove an entry and its rows",
		Long: "Removes a file entry outright. A directory entry removes the whole directory and\n" +
			"everything in it, so it additionally requires --yes. kb rm only removes entries kb owns\n" +
			"(an entries source such as data/): an in-place file stays, delete it yourself and run\n" +
			"kb reindex --all.",
		Args:          cobra.ExactArgs(1),
		SilenceUsage:  true,
		SilenceErrors: true,
		RunE: func(cmd *cobra.Command, args []string) error {
			ws, cfg, err := d.workspaceConfig(cmd)
			if err != nil {
				return err
			}
			return runRm(cmd.Context(), d, stdout, stderr, ws, cfg.Sources, args[0], yes)
		},
	}
	cmd.Flags().BoolVarP(&yes, "yes", "y", false, "confirm removing a directory entry and everything in it")
	return cmd
}

// runRm resolves the entry, removes its file(s) from disk, drops its row from the database (if one
// exists), and reports what was removed. An in-place entry (files layout) is refused before
// anything is touched: kb indexes that file but does not own it.
func runRm(ctx context.Context, d *deps, stdout, stderr io.Writer, ws *workspace.Workspace, sources []config.Source, arg string, yes bool) error {
	root := ws.Root
	e, err := entry.Resolve(root, sources, arg)
	if err != nil {
		return err
	}
	if e.InPlace() {
		return usageErr("%s is not owned by kb (an in-place source); delete the file and run kb reindex --all", e.Path)
	}

	st, ok, err := d.openStoreIfExists(ctx, ws)
	if err != nil {
		return err
	}
	if ok {
		defer st.Close()
	}

	var removed string
	switch e.Kind {
	case entry.KindFile:
		target := filepath.Join(root, filepath.FromSlash(e.Path))
		if err := os.Remove(target); err != nil {
			return usageErr("%s", err)
		}
		removed = fmt.Sprintf("removed %s", e.Path)

	case entry.KindDir:
		if !yes {
			return usageErr("%s/ is a directory entry; pass --yes to remove %s/ and everything in it", e.Dir, e.Dir)
		}
		target := filepath.Join(root, e.Dir)
		n, err := countFiles(target)
		if err != nil {
			return usageErr("%s", err)
		}
		if err := os.RemoveAll(target); err != nil {
			return usageErr("%s", err)
		}
		removed = fmt.Sprintf("removed %s/ and %d files", e.Dir, n)

	default:
		return errs.New(errs.Internal, "cli.rm", "%s: unknown entry kind %q", e.Path, e.Kind)
	}

	if ok {
		if err := st.DeleteEntry(ctx, e.Path); err != nil {
			return err
		}
		// Inbound edges went NULL; another entry may match them now (a basename tie, a README fallback).
		if err := st.ResolveEdges(ctx); err != nil {
			fmt.Fprintf(stderr, "warning: could not re-resolve links after removing %s: %v\n", e.Path, err)
		}
	}

	fmt.Fprintln(stdout, removed)

	return nil
}

// countFiles counts every non-directory entry under dir, for the "and N files" part of kb rm's
// report when removing a directory entry.
func countFiles(dir string) (int, error) {
	n := 0
	err := filepath.WalkDir(dir, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if !d.IsDir() {
			n++
		}
		return nil
	})
	if err != nil {
		return 0, fmt.Errorf("count files in %s: %w", dir, err)
	}
	return n, nil
}
