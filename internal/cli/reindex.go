// `kb reindex`: rebuild the index from the files, skipping entries whose content hash is
// unchanged.
package cli

import (
	"context"
	"errors"
	"fmt"
	"io"

	"github.com/spf13/cobra"

	"github.com/nickstrad/kb/internal/embed"
	"github.com/nickstrad/kb/internal/entry"
	"github.com/nickstrad/kb/internal/errs"
	"github.com/nickstrad/kb/internal/reindex"
	"github.com/nickstrad/kb/internal/store"
)

// newReindexCmd builds `kb reindex [<entry>|--all]`.
func newReindexCmd(d *deps, stdout, stderr io.Writer) *cobra.Command {
	var all bool
	cmd := &cobra.Command{
		Use:   "reindex [entry]",
		Short: "Rebuild the index from the files on disk",
		Long: "Reindexes one entry, or every entry with --all, skipping any entry whose content\n" +
			"hash has not changed since the last index. `kb reindex --all` also drops any\n" +
			"database entry whose file no longer exists on disk.",
		Args:          cobra.MaximumNArgs(1),
		SilenceUsage:  true,
		SilenceErrors: true,
		RunE: func(cmd *cobra.Command, args []string) error {
			switch {
			case all && len(args) > 0:
				return usageErr("pass an <entry> or --all, not both")
			case !all && len(args) == 0:
				return usageErr("pass an <entry> or --all")
			}
			return runReindex(cmd, d, stdout, stderr, all, args)
		},
	}
	cmd.Flags().BoolVar(&all, "all", false, "reindex every entry in the repository")
	return cmd
}

// runReindex opens the store, resolves the embedder, decides Force per the embed_meta check, runs
// reindex.All or reindex.One, and prints the summary.
func runReindex(cmd *cobra.Command, d *deps, stdout, stderr io.Writer, all bool, args []string) error {
	sum, err := reindexRun(cmd, d, stdout, stderr, all, args)
	if err != nil {
		// SIGINT/SIGTERM cancel ctx (main's NotifyContext), possibly before anything ran. Every
		// entry commits on its own, so what finished is indexed and the rest is still stale.
		if errors.Is(err, context.Canceled) || errs.KindOf(err) == errs.Interrupted {
			return interruptedErr(err, sum)
		}
		return err
	}
	fmt.Fprintln(stdout, sum.String())
	if sum.Failed > 0 {
		return usageErr("%d of %d entries failed; see warnings above", sum.Failed, sum.Scanned)
	}
	return nil
}

func reindexRun(cmd *cobra.Command, d *deps, stdout, stderr io.Writer, all bool, args []string) (reindex.Summary, error) {
	ctx := cmd.Context()
	ws, cfg, err := d.workspaceConfig(cmd)
	if err != nil {
		return reindex.Summary{}, err
	}
	root := ws.Root
	st, err := d.openStoreForWrite(ctx, ws)
	if err != nil {
		return reindex.Summary{}, err
	}
	defer st.Close()

	// The default per-call budget already gives a 16-text batch far more time than it takes.
	embedder, err := d.newEmbedder(cmd, cfg.Embed)
	if err != nil {
		return reindex.Summary{}, err
	}
	force, err := ensureEmbedMetaForReindex(ctx, st, embedder, all, stderr)
	if err != nil {
		return reindex.Summary{}, err
	}
	opts := reindex.Options{
		Root:      root,
		Sources:   cfg.Sources,
		Store:     st,
		Embedder:  embedder,
		BatchSize: cfg.Embed.Batch,
		Stdout:    stdout,
		Stderr:    stderr,
		Force:     force,
	}
	var sum reindex.Summary
	if all {
		sum, err = reindex.All(ctx, opts)
	} else {
		e, rerr := entry.Resolve(root, cfg.Sources, args[0])
		if rerr != nil {
			return reindex.Summary{}, rerr
		}
		sum, err = reindex.One(ctx, opts, e)
	}
	// "embed calls" counts calls that reached the model, not those the shared cache answered.
	sum.EmbedCalls = modelCalls(embedder, sum.EmbedCalls)
	return sum, err
}

// interruptedErr is the message for a reindex cut short by SIGINT or SIGTERM: a known edge case,
// so it exits 1 (report's interrupted rule), naming what was kept.
func interruptedErr(err error, sum reindex.Summary) error {
	return errs.Wrap(err, errs.Interrupted, "cli.reindex",
		"interrupted (%d entries reindexed before it); finished entries are saved, rerun kb reindex to finish", sum.Reindexed)
}

// ensureEmbedMetaForReindex checks the stored embed_meta against embedder. On a fresh database it
// records it and returns force=false. On a mismatch with all=true it prints the mismatch to
// stderr, Pings the embedder (if it is a pinger), wipes the derived index with ResetIndex, records
// the new embed_meta and returns force=true, so every entry is re-embedded. The Ping comes first
// because ResetIndex is destructive: an unusable embedder must fail (exit 2) while the old index
// is intact. With all=false a mismatch is returned as is (a Conflict): reindexing one entry would
// leave the rest under the old model.
func ensureEmbedMetaForReindex(ctx context.Context, st store.Index, embedder embed.Embedder, all bool, stderr io.Writer) (force bool, err error) {
	want := store.EmbedMeta{Model: embedder.Model(), Dim: embedder.Dim()}
	err = st.EnsureEmbedMeta(ctx, want)
	if err == nil {
		return false, nil
	}
	var mismatch *store.EmbedMetaMismatchError
	if !errors.As(err, &mismatch) || !all {
		return false, err
	}
	fmt.Fprintln(stderr, mismatch.Error())

	if p, ok := embedder.(pinger); ok {
		if pingErr := p.Ping(ctx); pingErr != nil {
			// A Ctrl-C during the ping is not an embedder failure: return the cancel as is, so it
			// is not classified Unavailable. report's interrupted rule (exit 1) reaches it either way.
			if errors.Is(pingErr, context.Canceled) {
				return false, pingErr
			}
			return false, embedderErr("refusing to reset the index, the embedder is not usable: %v", pingErr)
		}
	}
	if err := st.ResetIndex(ctx); err != nil {
		return false, err
	}
	if err := st.EnsureEmbedMeta(ctx, want); err != nil {
		return false, err
	}
	return true, nil
}

// pinger is an embedder that can check, before any destructive work, that it is reachable and
// usable. Ping is optional, so it is not part of embed.Embedder.
type pinger interface {
	Ping(ctx context.Context) error
}
