// `kb eval`: score search quality against the feedback in the search log.
package cli

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"strings"

	"github.com/spf13/cobra"

	"github.com/nickstrad/kb/internal/embed"
	"github.com/nickstrad/kb/internal/errs"
	"github.com/nickstrad/kb/internal/eval"
	"github.com/nickstrad/kb/internal/search"
	"github.com/nickstrad/kb/internal/store"
)

// newEvalCmd builds
// `kb eval [--mode hybrid|fts|vec] [-k 8] [--since 30d] [--caller c] [--per-query] [--json]`.
func newEvalCmd(d *deps, stdout, stderr io.Writer) *cobra.Command {
	var opts evalOpts
	cmd := &cobra.Command{
		Use:   "eval",
		Short: "Score search quality against the feedback in the search log",
		Long: "Replays every judged query from the search log with the current settings and scores\n" +
			"the ranking: hit@k (a gold hit in the top k), MRR (1/rank of the first gold hit) and\n" +
			"nDCG@k (binary gains), each with a 95% bootstrap interval (1000 resamples, fixed seed).\n\n" +
			"Gold: a hit marked useful with kb feedback, matched on its path and heading. Searches of\n" +
			"the same query (case and spacing ignored) count once, with their gold combined.\n" +
			"Searches judged only --none, only not-useful, or not at all have no gold and are only\n" +
			"counted. Not-useful hits are reported as negatives found in the top k. Three lines:\n" +
			"  chunk  useful verdicts matched on path and heading (the headline)\n" +
			"  entry  the same gold matched on path alone\n" +
			"  opens  entries read with kb show after a search: a weaker signal, kept apart\n" +
			"Gold the index no longer has (a deleted entry, a renamed heading) is left out and\n" +
			"counted as \"gold not in index\". kb eval replays this store only: gold on another\n" +
			"store's hit of a cross-store search (kb search --stores) is counted, not scored. Below [eval] min_judged queries the report says the\n" +
			"sample is small: differences inside the interval are noise.\n\n" +
			"kb eval reads the log and never writes it.",
		Example: "  kb eval\n" +
			"  kb eval --mode fts --since 30d --per-query\n" +
			"  kb eval --caller claude --json",
		Args:          cobra.NoArgs,
		SilenceUsage:  true,
		SilenceErrors: true,
		RunE: func(cmd *cobra.Command, args []string) error {
			opts.kChanged = cmd.Flags().Changed("k")
			return runEval(cmd, d, stdout, stderr, opts)
		},
	}
	cmd.Flags().StringVar(&opts.mode, "mode", search.ModeHybrid, "hybrid, fts or vec")
	cmd.Flags().IntVarP(&opts.k, "k", "k", search.DefaultK, "score the top k hits (1..20; default [search] k, 8)")
	cmd.Flags().StringVar(&opts.since, "since", "", "only searches this recent: a duration (7d, 24h) or a date (YYYY-MM-DD)")
	cmd.Flags().StringVar(&opts.caller, "caller", "", "only searches by this caller")
	cmd.Flags().BoolVar(&opts.perQuery, "per-query", false, "print one line per query")
	cmd.Flags().BoolVar(&opts.asJSON, "json", false, "machine-readable output (always includes per_query)")
	return cmd
}

type evalOpts struct {
	mode     string
	k        int
	kChanged bool
	since    string
	caller   string
	perQuery bool
	asJSON   bool
}

// runEval reads the log read-only, loads the embedder only when there is something to replay,
// and replays without logging. Exit 2 when the embedder was unavailable and some query was
// replayed in fts-fallback (the report is still printed first).
func runEval(cmd *cobra.Command, d *deps, stdout, stderr io.Writer, opts evalOpts) error {
	const op = "cli.eval"
	switch opts.mode {
	case search.ModeHybrid, search.ModeFTS, search.ModeVec:
	default:
		return usageErr("invalid --mode %q (want hybrid, fts or vec)", opts.mode)
	}
	since, err := parseSince(opts.since, d.clock())
	if err != nil {
		return usageErr("%s", err)
	}
	ctx := cmd.Context()
	ws, cfg, err := d.workspaceConfig(cmd)
	if err != nil {
		return err
	}
	if _, err := os.Stat(ws.DBPath); err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			return errs.Wrap(err, errs.NotFound, op, "no index at %s; run kb reindex --all", ws.DBPath)
		}
		return errs.Wrap(err, errs.Usage, op, "cannot read the index %s: check the file and its permissions", ws.DBPath).With("path", ws.DBPath)
	}
	st, err := d.openStore(ctx, ws.DBPath, true)
	if err != nil {
		return err
	}
	defer st.Close()
	jl, ok := st.(store.JudgedLog)
	if !ok {
		return errs.New(errs.Internal, op, "the store %T cannot read back the search log", st)
	}
	searches, err := jl.Judged(ctx, store.JudgedFilter{Since: since, Caller: opts.caller})
	if err != nil {
		return err
	}

	mode := opts.mode
	var embedder embed.Embedder
	if queries, _ := eval.Extract(searches); eval.Replayable(queries) {
		// Only now: loading the model is the slow part, and an empty log needs none.
		if embedder, mode, err = d.searchEmbedder(cmd, st, cfg, mode, op); err != nil {
			return err
		}
	}
	settings, err := search.NewSettings(cfg)
	if err != nil {
		return err
	}
	// No Log: replays must not write the log they are scored against.
	searcher := &search.Searcher{Store: st, Embedder: embedder, KBVersion: Version, Settings: settings}
	rep, err := eval.Evaluate(ctx, searches, st, searcher, eval.Options{
		Mode:         mode,
		K:            search.ClampK(searchK(opts.kChanged, opts.k, cfg.Search.K)),
		MinJudged:    cfg.Eval.MinJudged,
		SettingsHash: settings.Hash,
	})
	if err != nil {
		return err
	}
	if opts.asJSON {
		var buf strings.Builder
		enc := json.NewEncoder(&buf)
		enc.SetEscapeHTML(false)
		enc.SetIndent("", "  ")
		if err := enc.Encode(rep); err != nil {
			return errs.Wrap(err, errs.Internal, op, "encode the report: %v", err)
		}
		fmt.Fprint(stdout, buf.String())
	} else {
		fmt.Fprint(stdout, renderEval(rep, opts.perQuery))
	}
	if rep.FallbackQueries > 0 {
		fmt.Fprintf(stderr, "warning: %s; %d of the replays ran --mode fts instead\n", rep.FallbackReason, rep.FallbackQueries)
		return exitSilently(ExitEmbedder)
	}
	return nil
}

// renderEval is the human form of a report.
func renderEval(r *eval.Report, perQuery bool) string {
	var b strings.Builder
	fmt.Fprintf(&b, "eval · mode %s · k %d · settings %s\n", r.Mode, r.K, shortHash(r.SettingsHash))
	s := r.Searches
	fmt.Fprintf(&b, "searches: %d logged · %d useful · %d --none · %d not-useful only · %d unjudged · %d with opens\n",
		s.Total, s.Useful, s.NoneOnly, s.NotUsefulOnly, s.Unjudged, s.WithOpens)
	if r.Chunk.Queries == 0 {
		b.WriteString("queries=0: no useful verdicts to score; mark hits with kb feedback <search-id> <rank> --useful\n")
	} else {
		b.WriteString(variantLine("", r.Chunk, r.K))
	}
	if r.Entry.Queries > 0 {
		b.WriteString(variantLine("entry: ", r.Entry, r.K))
	}
	if r.Opens.Queries > 0 {
		b.WriteString(variantLine("opens: ", r.Opens, r.K))
	}
	fmt.Fprintf(&b, "gold not in index: %d chunks (%d queries dropped) · %d entries · %d opens\n",
		r.Chunk.GoldNotInIndex, r.Chunk.QueriesDropped, r.Entry.GoldNotInIndex, r.Opens.GoldNotInIndex)
	if o := r.OtherStore; o.Chunks+o.Opens > 0 {
		fmt.Fprintf(&b, "gold from other stores (not scored; kb eval replays this store only): %d chunks · %d opens · %d queries with no gold here\n",
			o.Chunks, o.Opens, o.Queries)
	}
	fmt.Fprintf(&b, "negatives in top %d: %d\n", r.K, r.NegativesInTopK)
	fmt.Fprintf(&b, "intervals: %g%% bootstrap, %d resamples, seed %d\n", r.Confidence*100, r.Resamples, r.Seed)
	if r.SmallSample {
		fmt.Fprintf(&b, "small sample: differences under ±CI are noise (%d queries < eval.min_judged %d)\n", r.Chunk.Queries, r.MinJudged)
	}
	if perQuery {
		fmt.Fprintf(&b, "\nper query: first gold rank (chunk · entry · opens; - = none in top %d, . = no such gold)\n", r.K)
		for _, q := range r.PerQuery {
			fmt.Fprintf(&b, "%-3s %-3s %-3s %q  (%d searches, %d gold, %d opened, %d negatives in top %d)\n",
				firstRank(q.Chunk), firstRank(q.Entry), firstRank(q.Opens), q.Text,
				len(q.SearchIDs), len(q.Gold), len(q.Opened), q.NegativesInTopK, r.K)
		}
	}
	return b.String()
}

// variantLine is "queries=N hit@k=x [lo, hi] mrr=… ndcg@k=…".
func variantLine(prefix string, v eval.Variant, k int) string {
	est := func(e *eval.Estimate) string { return fmt.Sprintf("%.3f [%.3f, %.3f]", e.Mean, e.Lo, e.Hi) }
	return fmt.Sprintf("%squeries=%d hit@%d=%s mrr=%s ndcg@%d=%s\n", prefix, v.Queries, k, est(v.HitAtK), est(v.MRR), k, est(v.NDCG))
}

// firstRank is a per-query column: blank without gold, "-" with no gold hit in the top k.
func firstRank(s *eval.Score) string {
	switch {
	case s == nil:
		return "."
	case s.FirstRank == 0:
		return "-"
	default:
		return fmt.Sprint(s.FirstRank)
	}
}
