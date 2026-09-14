// `kb reindex`: rebuild the index from the files, skipping entries whose content hash is
// unchanged. P2.4 implements it.
package cli

import (
	"context"
	"errors"
	"fmt"
	"io"

	"github.com/spf13/cobra"

	"knowledge/kb/internal/embed"
	"knowledge/kb/internal/embed/ollama"
	"knowledge/kb/internal/entry"
	"knowledge/kb/internal/reindex"
	"knowledge/kb/internal/store"
)

// reindexBatchSize is the number of chunk texts sent to the embedder per call (plan.md: "batched,
// 16 texts per Ollama call").
const reindexBatchSize = 16

// newReindexCmd builds `kb reindex [<entry>|--all]`.
func newReindexCmd(stdout, stderr io.Writer) *cobra.Command {
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
				return usageErr("kb reindex: pass an <entry> or --all, not both")
			case !all && len(args) == 0:
				return usageErr("kb reindex: pass an <entry> or --all")
			}
			return runReindex(cmd, stdout, stderr, all, args)
		},
	}
	cmd.Flags().BoolVar(&all, "all", false, "reindex every entry in the repository")
	return cmd
}

// runReindex opens the store, resolves the embedder, decides Force per the embed_meta check, runs
// reindex.All or reindex.One, and prints the summary.
func runReindex(cmd *cobra.Command, stdout, stderr io.Writer, all bool, args []string) error {
	root := Root()
	if err := EnsureDBDir(root); err != nil {
		return err
	}
	st, err := store.Open(DBPath(root))
	if err != nil {
		return usageErr("kb reindex: %s", err)
	}
	defer st.Close()

	// The default per-call budget (BaseTimeout 30s + PerTextTimeout*n) already gives a 16-text
	// batch 190s (30s + 10s*16), well above the 16-39s this box has measured for one such batch,
	// so it is left at New's defaults rather than raised.
	embedder := newEmbedder()
	ctx := cmd.Context()

	force, err := ensureEmbedMetaForReindex(ctx, st, embedder, all, stderr)
	if err != nil {
		return err
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
	var sum reindex.Summary
	if all {
		sum, err = reindex.All(ctx, opts)
	} else {
		var e entry.Entry
		e, err = entry.Resolve(root, args[0])
		if err != nil {
			return usageErr("kb reindex: %s", err)
		}
		sum, err = reindex.One(ctx, opts, e)
	}
	if err != nil {
		// reindex never imports the ollama package (see internal/reindex's doc); it just
		// propagates the embedder's error wrapped with %w, so the sentinel this CLI package
		// already knows about (because it built the embedder) is still reachable here.
		if errors.Is(err, ollama.ErrUnavailable) {
			return embedderErr("kb reindex: %s", err)
		}
		return usageErr("kb reindex: %s", err)
	}

	fmt.Fprintln(stdout, sum.String())

	if sum.Failed > 0 {
		return usageErr("kb reindex: %d of %d entries failed; see warnings above", sum.Failed, sum.Scanned)
	}
	return nil
}

// ensureEmbedMetaForReindex checks the stored embed_meta against embedder. On a fresh database it
// records it and returns Force=false. On a mismatch, only `kb reindex --all` may recover: it
// prints the mismatch error to stderr (its message already tells the user to run
// `kb reindex --all`, per store.EmbedMetaMismatchError), confirms the embedder actually works with
// a Ping BEFORE wiping anything, wipes the derived index with ResetForReindex, records the new
// embed_meta, and returns Force=true. Force=true is technically redundant once ResetForReindex has
// emptied the entries table (there is no stored body_hash left to compare against), but it
// documents the intent and protects against a future ResetForReindex that stops short of deleting
// every entries row.
//
// The Ping check matters because ResetForReindex is destructive: it erases the entire derived
// index (every entry, chunk, FTS row and vector), which only kb reindex --all can then rebuild.
// Discovering that the embedder is unreachable only after that has already happened would leave
// the database empty with no way back except re-running against files that, luckily, are still
// the source of truth — but there is no reason to risk that when a Ping first tells us whether the
// rebuild can even proceed. A failed Ping is reported as an embedder-unavailable error (exit code
// 2) rather than a usage error, before ResetForReindex ever runs.
//
// embedder is the embed.Embedder interface, not the concrete *ollama.Client, because this helper
// is shared with `kb edit` (internal/cli/setup*.go's newEmbedder hook returns the interface so
// tests can substitute a fake); Ping is not part of embed.Embedder, so it is reached with an
// optional-interface type assertion (the pinger type below) rather than a parameter type change.
// In production embedder is always the concrete *ollama.Client, which does implement it. Only the
// all=true, mismatch path below ever calls Ping, and that is also the only path that is
// destructive enough to warrant it.
//
// Reindexing a single <entry> cannot recover from a mismatch on its own: doing so would leave the
// rest of the database indexed under the old model with no way to tell the two apart, so it is
// reported as a usage error instead, same as the store's error already suggests.
func ensureEmbedMetaForReindex(ctx context.Context, st *store.Store, embedder embed.Embedder, all bool, stderr io.Writer) (force bool, err error) {
	err = st.EnsureEmbedMeta(embedder.Model(), embedder.Dim())
	if err == nil {
		return false, nil
	}

	var mismatch *store.EmbedMetaMismatchError
	if !errors.As(err, &mismatch) || !all {
		return false, usageErr("kb reindex: %s", err)
	}

	fmt.Fprintln(stderr, err.Error())

	if p, ok := embedder.(pinger); ok {
		if pingErr := p.Ping(ctx); pingErr != nil {
			return false, embedderErr("kb reindex: refusing to reset the index, the embedder is not usable: %s", pingErr)
		}
	}

	if err := st.ResetForReindex(ctx); err != nil {
		return false, usageErr("kb reindex: %s", err)
	}
	if err := st.EnsureEmbedMeta(embedder.Model(), embedder.Dim()); err != nil {
		return false, usageErr("kb reindex: %s", err)
	}
	return true, nil
}

// pinger is satisfied by *ollama.Client (see ensureEmbedMetaForReindex's doc): an embedder that
// can check, before any destructive work, whether it is actually reachable and usable.
type pinger interface {
	Ping(ctx context.Context) error
}
