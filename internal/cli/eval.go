// `kb eval`: score search quality against the feedback in the search log.
package cli

import (
	"encoding/json"
	"fmt"
	"io"
	"path/filepath"
	"strings"
	"time"

	"github.com/spf13/cobra"

	"github.com/nickstrad/kb/internal/config"
	"github.com/nickstrad/kb/internal/embed"
	"github.com/nickstrad/kb/internal/errs"
	"github.com/nickstrad/kb/internal/eval"
	"github.com/nickstrad/kb/internal/search"
	"github.com/nickstrad/kb/internal/store"
	"github.com/nickstrad/kb/internal/workspace"
)

// newEvalCmd builds
// `kb eval [--mode hybrid|fts|vec] [-k 8] [--since 30d] [--caller c] [--per-query] [--json]
// [--set key=value]... [--save-baseline|--compare [NAME]]`, and
// `kb eval --sweep key=v1,v2[,…]... [--force]` (with the same --mode, -k, --since, --caller, --set
// and --json).
func newEvalCmd(d *deps, stdout, stderr io.Writer) *cobra.Command {
	var opts evalOpts
	cmd := &cobra.Command{
		Use:   "eval [NAME]",
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
			"Baselines: --save-baseline [NAME] stores this run in the store's eval/ directory (default\n" +
			"name \"default\"); --compare [NAME] runs again and prints, for each metric, the baseline and\n" +
			"new means, the delta with a paired 95% bootstrap interval, the queries that won, lost or tied,\n" +
			"and a two-sided exact sign-test p-value over the wins and losses. Queries are paired by\n" +
			"normalised text; a query scored on one side only is counted and left out. --set key=value\n" +
			"(repeatable) tries a setting for this run without writing it, so\n" +
			"kb eval --set search.rrf_k=20 --compare shows what that setting would change. A baseline\n" +
			"of another file-format version, or at another k, is refused.\n" +
			"Sweeps: --sweep key=v1,v2,... (repeatable, [search] keys only, at most 200 settings in\n" +
			"all) replays the log once per setting of the grid. The judged queries are split by a\n" +
			"seeded hash of their text: [eval] holdout of them (0.3) are held out, the rest choose\n" +
			"the setting with the best MRR (then nDCG, then hit rate; ties keep the current setting).\n" +
			"The top 5 are shown on both sides against the current settings (delta, paired 95%\n" +
			"interval, w/l/t, sign test), with a config snippet for the best. A best setting that wins\n" +
			"on the chosen queries but not on the held-out ones is reported as likely overfitting.\n" +
			"Below [eval] min_judged judged queries (30) a sweep is refused unless --force. Each query\n" +
			"is embedded once for the whole sweep. kb eval never writes the config.\n" +
			"Only the log is read; baselines are the only files kb eval writes.",
		Example: "  kb eval\n" +
			"  kb eval --mode fts --since 30d --per-query\n" +
			"  kb eval --caller claude --json\n" +
			"  kb eval --save-baseline && kb eval --set search.rrf_k=20 --compare\n" +
			"  kb eval --sweep search.rrf_k=20,40,60 --sweep search.bm25.title=1,3,5",
		Args:          cobra.MaximumNArgs(1),
		SilenceUsage:  true,
		SilenceErrors: true,
		RunE: func(cmd *cobra.Command, args []string) error {
			opts.kChanged = cmd.Flags().Changed("k")
			opts.name = eval.DefaultBaseline
			if len(args) == 1 {
				opts.name = args[0]
				if !opts.save && !opts.compare {
					return usageErr("a baseline NAME needs --save-baseline or --compare")
				}
			}
			if len(opts.sweeps) > 0 {
				if err := checkSweepFlags(opts); err != nil {
					return err
				}
				return runSweep(cmd, d, stdout, stderr, opts)
			}
			if opts.force {
				return usageErr("--force applies only to --sweep")
			}
			return runEval(cmd, d, stdout, stderr, opts)
		},
	}
	cmd.Flags().StringVar(&opts.mode, "mode", search.ModeHybrid, "hybrid, fts or vec")
	cmd.Flags().IntVarP(&opts.k, "k", "k", search.DefaultK, "score the top k hits (1..20)")
	cmd.Flags().StringVar(&opts.since, "since", "", "only searches this recent: a duration (7d, 24h) or a date (YYYY-MM-DD)")
	cmd.Flags().StringVar(&opts.caller, "caller", "", "only searches by this caller")
	cmd.Flags().BoolVar(&opts.perQuery, "per-query", false, "print one line per query")
	cmd.Flags().BoolVar(&opts.asJSON, "json", false, "machine-readable output (always includes per_query)")
	cmd.Flags().StringArrayVar(&opts.sets, "set", nil, "try a setting for this run: section.key=value (repeatable)")
	cmd.Flags().BoolVar(&opts.save, "save-baseline", false, "save this run as baseline NAME (default \"default\"), replacing it")
	cmd.Flags().BoolVar(&opts.compare, "compare", false, "compare this run with baseline NAME (default \"default\")")
	cmd.Flags().StringArrayVar(&opts.sweeps, "sweep", nil, "try every value of a [search] key: section.key=v1,v2,... (repeatable: a grid)")
	cmd.Flags().BoolVar(&opts.force, "force", false, "sweep even with fewer than [eval] min_judged judged queries")
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
	sets     []string
	save     bool
	compare  bool
	name     string // the baseline NAME argument
	sweeps   []string
	force    bool
}

// evalSetup validates the flags kb eval and kb eval --sweep share and loads the config with --set
// applied.
func evalSetup(cmd *cobra.Command, d *deps, opts evalOpts) (*workspace.Workspace, config.Config, time.Time, error) {
	switch opts.mode {
	case search.ModeHybrid, search.ModeFTS, search.ModeVec:
	default:
		return nil, config.Config{}, time.Time{}, usageErr("invalid --mode %q (want hybrid, fts or vec)", opts.mode)
	}
	since, err := parseSince(opts.since, d.clock())
	if err != nil {
		return nil, config.Config{}, time.Time{}, usageErr("%s", err)
	}
	ws, cfg, err := d.workspaceConfig(cmd)
	if err != nil {
		return nil, config.Config{}, time.Time{}, err
	}
	// --set before NewSettings (as explain does): the replays, the hash and the baseline all
	// carry the variant.
	for _, s := range opts.sets {
		if cfg, err = config.Apply(cfg, s); err != nil {
			return nil, config.Config{}, time.Time{}, err
		}
	}
	if len(opts.sets) > 0 {
		if err := cfg.CheckModel("--set", knownModel); err != nil {
			return nil, config.Config{}, time.Time{}, err
		}
	}
	return ws, cfg, since, nil
}

// openJudged opens the index read-only and reads the judged searches in the window. The caller
// closes the store.
func openJudged(cmd *cobra.Command, d *deps, ws *workspace.Workspace, since time.Time, caller string) (store.Store, []store.JudgedSearch, error) {
	const op = "cli.eval"
	ctx := cmd.Context()
	st, err := d.openIndexReadOnly(ctx, ws.DBPath, op)
	if err != nil {
		return nil, nil, err
	}
	jl, ok := st.(store.JudgedLog)
	if !ok {
		st.Close()
		return nil, nil, errs.New(errs.Internal, op, "the store %T cannot read back the search log", st)
	}
	searches, err := jl.Judged(ctx, store.JudgedFilter{Since: since, Caller: caller})
	if err != nil {
		st.Close()
		return nil, nil, err
	}
	return st, searches, nil
}

// runEval reads the log read-only, loads the embedder only when there is something to replay,
// and replays without logging. Exit 2 when the embedder was unavailable and some query was
// replayed in fts-fallback (the report is still printed first).
func runEval(cmd *cobra.Command, d *deps, stdout, stderr io.Writer, opts evalOpts) error {
	const op = "cli.eval"
	if opts.save && opts.compare {
		return usageErr("--save-baseline and --compare are exclusive: compare first, then save")
	}
	if (opts.save || opts.compare) && !eval.ValidBaselineName(opts.name) {
		return usageErr("invalid baseline name %q (lowercase letters, digits, '.', '_' or '-', at most 64)", opts.name)
	}
	ctx := cmd.Context()
	ws, cfg, since, err := evalSetup(cmd, d, opts)
	if err != nil {
		return err
	}
	evalDir := filepath.Join(ws.StoreDir, "eval")
	var base *eval.Baseline
	if opts.compare {
		// Fail before the replays when there is nothing to compare with.
		if base, err = eval.LoadBaseline(evalDir, opts.name); err != nil {
			return err
		}
	}
	st, searches, err := openJudged(cmd, d, ws, since, opts.caller)
	if err != nil {
		return err
	}
	defer st.Close()

	syn, err := d.loadSynonyms(cmd, ws, cfg)
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
	if err != nil { // unreachable: Internal from encoding a config that validation already accepted (Q86)
		return err
	}
	settings = settings.WithSynonyms(syn.Hash()) // as search does: the hash and the baseline carry the synonyms
	// No Log: replays must not write the log they are scored against.
	searcher := &search.Searcher{Store: st, Embedder: embedder, KBVersion: Version, Settings: settings, Synonyms: syn}
	rep, err := eval.Evaluate(ctx, searches, st, searcher, eval.Options{
		Mode:         mode,
		K:            search.ClampK(searchK(opts.kChanged, opts.k, cfg.Search.K)),
		MinJudged:    cfg.Eval.MinJudged,
		SettingsHash: settings.Hash,
	})
	if err != nil {
		return err
	}
	cur := &eval.Baseline{
		Name: opts.name, KBVersion: Version, CreatedAt: d.clock().UTC(), EmbedModel: cfg.Embed.Model,
		Settings: settings.JSON, SettingsHash: settings.Hash, Report: *rep,
	}
	if c, ok := st.(entryCounter); ok {
		if cur.EntryCount, err = c.EntryCount(ctx); err != nil {
			return err
		}
	}
	var cmp *eval.Comparison
	if base != nil {
		if cmp, err = eval.Compare(base, cur); err != nil {
			return err
		}
	}

	switch {
	case opts.asJSON && cmp != nil:
		err = writeJSON(stdout, struct {
			Comparison *eval.Comparison `json:"comparison"`
			Report     *eval.Report     `json:"report"`
		}{cmp, rep})
	case opts.asJSON:
		err = writeJSON(stdout, rep)
	case cmp != nil:
		fmt.Fprint(stdout, renderCompare(cmp, rep, opts.perQuery))
	default:
		fmt.Fprint(stdout, renderEval(rep, opts.perQuery))
	}
	if err != nil { // unreachable: a report of plain values always encodes (Q86)
		return err
	}
	if cmp != nil { // warnings go to stderr (CODING_STYLE §9); --json also carries them
		for _, w := range cmp.Warnings {
			fmt.Fprintf(stderr, "warning: %s\n", w)
		}
	}
	if rep.FallbackQueries > 0 {
		fmt.Fprintf(stderr, "warning: %s; %d of the replays ran --mode fts instead\n", rep.FallbackReason, rep.FallbackQueries)
		if opts.save {
			fmt.Fprintf(stderr, "warning: baseline %q not saved: a fallback run is not a fair reference\n", opts.name)
		}
		return exitSilently(ExitEmbedder)
	}
	if opts.save {
		if err := eval.SaveBaseline(evalDir, *cur); err != nil {
			return err
		}
		if !opts.asJSON {
			fmt.Fprintf(stdout, "saved baseline %q (%d queries, settings %s) to %s\n", opts.name, rep.Chunk.Queries, shortHash(settings.Hash),
				filepath.Join(evalDir, opts.name+".json"))
		}
	}
	return nil
}

// writeJSON prints v as indented JSON without HTML escaping.
func writeJSON(w io.Writer, v any) error {
	var buf strings.Builder
	enc := json.NewEncoder(&buf)
	enc.SetEscapeHTML(false)
	enc.SetIndent("", "  ")
	if err := enc.Encode(v); err != nil { // unreachable: v holds only strings, ints and finite floats
		return errs.Wrap(err, errs.Internal, "cli.eval", "encode the report: %v", err)
	}
	fmt.Fprint(w, buf.String())
	return nil
}

// renderCompare is the human form of a comparison: the sides, one table per scored variant and the
// reading rules. The warnings are not part of it: runEval prints them on stderr.
func renderCompare(c *eval.Comparison, r *eval.Report, perQuery bool) string {
	var b strings.Builder
	bs, cs := c.Baseline, c.Current
	fmt.Fprintf(&b, "compare · baseline %q (%s · kb %s · mode %s · settings %s · %s · %d entries)\n",
		bs.Name, bs.CreatedAt, bs.KBVersion, bs.Mode, shortHash(bs.SettingsHash), bs.EmbedModel, bs.EntryCount)
	fmt.Fprintf(&b, "        now (mode %s · settings %s · %s · %d entries) · k %d\n",
		cs.Mode, shortHash(cs.SettingsHash), cs.EmbedModel, cs.EntryCount, cs.K)
	metricName := map[string]string{"hit_at_k": fmt.Sprintf("hit@%d", cs.K), "mrr": "mrr", "ndcg_at_k": fmt.Sprintf("ndcg@%d", cs.K)}
	for _, v := range []struct {
		label string
		d     eval.VariantDiff
	}{{"chunk", c.Chunk}, {"entry", c.Entry}, {"opens", c.Opens}} {
		if v.d.Paired+v.d.OnlyBase+v.d.OnlyCur == 0 {
			if v.label == "chunk" {
				b.WriteString("chunk: no scored queries on either side; mark hits with kb feedback <search-id> <rank> --useful\n")
			}
			continue
		}
		fmt.Fprintf(&b, "\n%s: %d paired queries (%d only in the baseline, %d only now)\n", v.label, v.d.Paired, v.d.OnlyBase, v.d.OnlyCur)
		if len(v.d.Metrics) == 0 {
			continue
		}
		fmt.Fprintf(&b, "  %-9s %8s %8s %8s  %-18s %-9s %s\n", "metric", "baseline", "now", "delta", "95% CI of delta", "w/l/t", "sign-test p")
		for _, m := range v.d.Metrics {
			fmt.Fprintf(&b, "  %-9s %8.3f %8.3f %+8.3f  [%+.3f, %+.3f]  %-9s %.3f\n", metricName[m.Name], m.Baseline, m.Current, m.Delta, m.Lo, m.Hi,
				fmt.Sprintf("%d/%d/%d", m.Wins, m.Losses, m.Ties), m.P)
		}
	}
	b.WriteString("\n")
	b.WriteString("p: two-sided exact sign test over the queries that won or lost (ties dropped); w/l/t counts now against the baseline.\n")
	b.WriteString("A delta whose interval spans 0 is noise; across several metrics a lone p under 0.05 is weak evidence.\n")
	if c.SmallSample {
		fmt.Fprintf(&b, "small sample: %d paired queries < eval.min_judged %d; read these as hints, not results\n", c.Chunk.Paired, c.MinJudged)
	}
	if perQuery {
		b.WriteString("\n")
		b.WriteString(renderEval(r, true))
	}
	return b.String()
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

// checkSweepFlags refuses the flags --sweep does not combine with.
func checkSweepFlags(opts evalOpts) error {
	switch {
	case opts.save || opts.compare:
		return usageErr("--sweep compares with the current settings itself; it does not take --save-baseline or --compare")
	case opts.perQuery:
		return usageErr("--per-query does not apply to --sweep; use --json for every setting's scores")
	}
	return nil
}

// runSweep is kb eval --sweep: expand the grid (bad keys and values fail first), read the log,
// refuse below [eval] min_judged unless --force, then replay once per distinct setting with one
// shared query-vector memo, so each query is embedded once. It never writes the config or the
// log. Exit 2 when some replay fell back to fts (the report is still printed first).
func runSweep(cmd *cobra.Command, d *deps, stdout, stderr io.Writer, opts evalOpts) error {
	axes := make([]eval.Axis, len(opts.sweeps))
	for i, s := range opts.sweeps {
		var err error
		if axes[i], err = eval.ParseAxis(s); err != nil {
			return err
		}
	}
	ctx := cmd.Context()
	ws, cfg, since, err := evalSetup(cmd, d, opts)
	if err != nil {
		return err
	}
	for _, a := range axes {
		for _, s := range opts.sets {
			if k, _, _ := strings.Cut(s, "="); strings.TrimSpace(k) == a.Key {
				return usageErr("%s is both --set and --sweep; drop one", a.Key)
			}
		}
	}
	grid, err := eval.ExpandGrid(cfg, axes, eval.MaxGrid)
	if err != nil {
		return err
	}
	st, searches, err := openJudged(cmd, d, ws, since, opts.caller)
	if err != nil {
		return err
	}
	defer st.Close()
	p, err := eval.Prepare(ctx, searches, st)
	if err != nil {
		return err
	}
	if err := eval.Gate(len(p.Judged()), cfg.Eval.MinJudged, opts.force); err != nil {
		return err
	}
	embedder, mode, err := d.searchEmbedder(cmd, st, cfg, opts.mode, "cli.eval")
	if err != nil {
		return err
	}
	syn, err := d.loadSynonyms(cmd, ws, cfg)
	if err != nil {
		return err
	}
	memo := &search.QueryMemo{} // one per sweep: the vector of a query does not depend on [search]
	newSearcher := func(s search.Settings) eval.Searcher {
		// No Log: replays must not write the log they are scored against.
		return &search.Searcher{Store: st, Embedder: embedder, KBVersion: Version, Settings: s, Queries: memo, Synonyms: syn}
	}
	rep, err := eval.Sweep(ctx, p, cfg, axes, grid, newSearcher, eval.SweepOptions{
		Mode: mode, K: search.ClampK(searchK(opts.kChanged, opts.k, cfg.Search.K)),
		MinJudged: cfg.Eval.MinJudged, Holdout: cfg.Eval.Holdout,
	})
	if err != nil {
		return err
	}
	if opts.asJSON {
		if err := writeJSON(stdout, rep); err != nil { // unreachable: plain values always encode (Q86)
			return err
		}
	} else {
		fmt.Fprint(stdout, renderSweep(rep, ws.ConfigPath))
	}
	for _, w := range rep.Warnings { // warnings go to stderr (CODING_STYLE §9); --json also carries them
		fmt.Fprintf(stderr, "warning: %s\n", w)
	}
	if rep.FallbackQueries > 0 {
		fmt.Fprintf(stderr, "warning: %s; %d of the replays ran --mode fts instead\n", rep.FallbackReason, rep.FallbackQueries)
		return exitSilently(ExitEmbedder)
	}
	return nil
}

// renderSweep is the human form of a sweep: the split, the current settings, the top settings on
// the training queries with their held-out scores and deltas, and the best one as a snippet.
func renderSweep(r *eval.SweepReport, configPath string) string {
	var b strings.Builder
	fmt.Fprintf(&b, "sweep · mode %s · k %d · %d settings (%d replays) · %d judged queries: %d to choose, %d held out (eval.holdout %g, seed %d)\n",
		r.Mode, r.K, r.Settings, r.Replays, r.Judged, len(r.Train), len(r.Test), r.Holdout, r.Seed)
	ci := func(e eval.Estimate) string { return fmt.Sprintf("%.3f [%.3f, %.3f]", e.Mean, e.Lo, e.Hi) }
	test := func(s *eval.SplitScore) string {
		if s == nil {
			return "-"
		}
		return ci(s.MRR)
	}
	fmt.Fprintf(&b, "current settings %s: mrr %s chosen · %s held out\n\n", shortHash(r.Current.SettingsHash), ci(r.Current.Train.MRR), test(r.Current.Test))
	fmt.Fprintf(&b, "%-4s %-22s %-22s %-24s %-9s %s\n", "rank", "mrr (choose)", "mrr (held out)", "held-out Δ vs current", "w/l/t", "p")
	for _, row := range r.Top {
		delta, wlt, p := "-", "-", "-"
		if row.TestVsCurrent != nil {
			m, _ := row.TestVsCurrent.Metric("mrr") // paired: the held-out side is never empty here
			delta = fmt.Sprintf("%+.3f [%+.3f, %+.3f]", m.Delta, m.Lo, m.Hi)
			wlt, p = fmt.Sprintf("%d/%d/%d", m.Wins, m.Losses, m.Ties), fmt.Sprintf("%.3f", m.P)
		}
		fmt.Fprintf(&b, "%-4d %-22s %-22s %-24s %-9s %s\n", row.Rank, ci(row.Train.MRR), test(row.Test), delta, wlt, p)
		label := strings.Join(row.Set, " ")
		if row.Current {
			label += "  (= current)"
		}
		fmt.Fprintf(&b, "     %s\n", label)
	}
	b.WriteString("\n")
	switch {
	case r.BestIsCurrent:
		b.WriteString("best on the chosen queries: the current settings; nothing to change\n")
	case r.Overfit:
		b.WriteString("best on the chosen queries does not win on the held-out ones: keep the current settings\n")
	default:
		fmt.Fprintf(&b, "best on the chosen queries: %s\nto adopt it, add to %s:\n%s", strings.Join(r.Top[0].Set, " "), configPath, r.TOML)
	}
	b.WriteString("A held-out delta whose interval spans 0 is noise; p is a two-sided exact sign test. kb eval never writes the config.\n")
	return b.String()
}
