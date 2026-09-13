// `kb rm`: remove an entry's files, drop its rows, regenerate index.md. P4.2 implements it.
package cli

import (
	"context"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"

	"github.com/spf13/cobra"

	"knowledge/kb/internal/entry"
)

// newRmCmd builds `kb rm <entry> [--yes]`.
func newRmCmd(stdout, stderr io.Writer) *cobra.Command {
	var yes bool
	cmd := &cobra.Command{
		Use:   "rm <entry>",
		Short: "Remove an entry and its rows",
		Long: "Removes a file entry outright. A directory entry removes the whole directory and\n" +
			"everything in it, so it additionally requires --yes.",
		Args:          cobra.ExactArgs(1),
		SilenceUsage:  true,
		SilenceErrors: true,
		RunE: func(cmd *cobra.Command, args []string) error {
			return runRm(cmd.Context(), stdout, stderr, Root(), args[0], yes)
		},
	}
	cmd.Flags().BoolVarP(&yes, "yes", "y", false, "confirm removing a directory entry and everything in it")
	return cmd
}

// runRm resolves the entry, removes its file(s) from disk, drops its row from the database (if one
// exists), and reports what was removed.
func runRm(ctx context.Context, stdout, stderr io.Writer, root, arg string, yes bool) error {
	e, err := entry.Resolve(root, arg)
	if err != nil {
		return usageErr("kb rm: %s", err)
	}

	st, ok, err := openStoreIfExists(root)
	if err != nil {
		return usageErr("kb rm: %s", err)
	}
	if ok {
		defer st.Close()
	}

	var removed string
	switch e.Kind {
	case entry.KindFile:
		target := filepath.Join(root, filepath.FromSlash(e.Path))
		if err := os.Remove(target); err != nil {
			return usageErr("kb rm: %s", err)
		}
		removed = fmt.Sprintf("removed %s", e.Path)

	case entry.KindDir:
		if !yes {
			return usageErr("kb rm: %s/ is a directory entry; pass --yes to remove %s/ and everything in it", e.Dir, e.Dir)
		}
		target := filepath.Join(root, e.Dir)
		n, err := countFiles(target)
		if err != nil {
			return usageErr("kb rm: %s", err)
		}
		if err := os.RemoveAll(target); err != nil {
			return usageErr("kb rm: %s", err)
		}
		removed = fmt.Sprintf("removed %s/ and %d files", e.Dir, n)

	default:
		return usageErr("kb rm: %s: unknown entry kind %q", e.Path, e.Kind)
	}

	if ok {
		if err := st.DeleteEntry(ctx, e.Path); err != nil {
			return usageErr("kb rm: %s", err)
		}
	}

	fmt.Fprintln(stdout, removed)

	if err := regenerateIndex(root, stderr); err != nil {
		return usageErr("kb rm: %s", err)
	}
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
