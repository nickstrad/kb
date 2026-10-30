// `kb explain`: run a search and show why each candidate ranked where it did.
package cli

import (
	"encoding/json"
	"fmt"
	"io"
	"maps"
	"slices"
	"strings"

	"github.com/spf13/cobra"

	"github.com/nickstrad/kb/internal/config"
	"github.com/nickstrad/kb/internal/errs"
	"github.com/nickstrad/kb/internal/search"
)

// newExplainCmd builds
// `kb explain "<query>" [-k 8] [--mode hybrid|fts|vec] [--tag t] [--set key=value]... [--json]`.
func newExplainCmd(d *deps, stdout, stderr io.Writer) *cobra.Command {
	var (
		k      int
		mode   string
		tag    string
		asJSON bool
		sets   []string
	)
	cmd := &cobra.Command{
		Use:   "explain <query>",
		Short: "Show why a search ranked its results the way it did",
		Long: "Runs the search kb search would run and prints, for every candidate of the fused list\n" +
			"(not only the returned hits): the fused rank and whether it was returned, dropped by\n" +
			"the per-entry cap, or fell below k; its FTS rank and bm25 score, with the score of each\n" +
			"bm25 column (text, heading, title) on its own; its vector rank and cosine distance; and\n" +
			"each reciprocal-rank-fusion term weight/(rrf_k+rank). The header shows the FTS\n" +
			"expression used, whether the OR retry fired (the AND query matched nothing), and the\n" +
			"settings hash.\n\n" +
			"A query holding an identifier (foo_bar, pkg.Func, --flag, path/to, camelCase) also runs\n" +
			"the exact-identifier list, which matches those identifiers only as whole tokens: its\n" +
			"expression is shown as \"exact:\" and each candidate in it gets an exact rank, bm25 score\n" +
			"and RRF term (weight search.list_weights.exact; off with search.exact_list = false).\n\n" +
			"The per-column scores are diagnostic. FTS5 adds the column-weighted term frequencies\n" +
			"before saturating them, so the columns do not add up to the total: the total lies\n" +
			"between the largest column and their sum. A chunk's text starts with a context line\n" +
			"\"title › heading\", so a title term scores in the text column as well as the title one.\n\n" +
			"--set key=value (repeatable) tries a [search] setting for this run without writing it.\n" +
			"Explain does not log: the search log feeds kb stats and kb eval, which a diagnostic\n" +
			"run must not change.\n\n" +
			"Explain covers one store and never fans out: [search] stores does not apply. A\n" +
			"cross-store search merges each store's list on ranks only, so explain each store\n" +
			"with kb --store REF explain.",
		Example: "  kb explain \"tmux logout\"\n" +
			"  kb explain \"tmux logout\" --mode fts --set search.bm25.title=5 --json",
		Args:          cobra.MinimumNArgs(1),
		SilenceUsage:  true,
		SilenceErrors: true,
		RunE: func(cmd *cobra.Command, args []string) error {
			return runExplain(cmd, d, stdout, stderr, explainOpts{
				query: strings.Join(args, " "), k: k, mode: mode, tag: tag, asJSON: asJSON, sets: sets,
			})
		},
	}
	cmd.Flags().IntVarP(&k, "k", "k", search.DefaultK, "number of hits to return, 1..20 (config [search] k sets the default)")
	cmd.Flags().StringVar(&mode, "mode", search.ModeHybrid, "hybrid, fts or vec")
	cmd.Flags().StringVar(&tag, "tag", "", "only entries carrying this tag, matched case-insensitively (an alias is searched like the title)")
	cmd.Flags().BoolVar(&asJSON, "json", false, "machine-readable output")
	cmd.Flags().StringArrayVar(&sets, "set", nil, "override a key for this run: section.key=value (repeatable)")
	return cmd
}

type explainOpts struct {
	query  string
	k      int
	mode   string
	tag    string
	asJSON bool
	sets   []string
}

// runExplain shares runSearch's setup (openSearch: never creates a database, no model download,
// none embedder means fts) but searches without logging. Exit codes as kb search: 2 when the embedder
// was unavailable (the explanation of the fts fallback is still printed first).
func runExplain(cmd *cobra.Command, d *deps, stdout, stderr io.Writer, opts explainOpts) error {
	switch opts.mode {
	case search.ModeHybrid, search.ModeFTS, search.ModeVec:
	default:
		return usageErr("invalid --mode %q (want hybrid, fts or vec)", opts.mode)
	}
	ctx := cmd.Context()
	ws, cfg, err := d.workspaceConfig(cmd)
	if err != nil {
		return err
	}
	for _, s := range opts.sets {
		if cfg, err = config.Apply(cfg, s); err != nil {
			return err
		}
	}
	if len(opts.sets) > 0 {
		if err := cfg.CheckModel("--set", knownModel); err != nil {
			return err
		}
	}
	// Read-only: explain never writes, not even the search log.
	st, embedder, mode, err := d.openSearch(cmd, ws, cfg, opts.mode, "cli.explain", true)
	if err != nil {
		return err
	}
	defer st.Close()

	// No Log: explain must not write the search log.
	searcher, _, err := d.newSearcher(cmd, ws, cfg, st, embedder, nil, false)
	if err != nil {
		return err
	}
	ex, err := searcher.Explain(ctx, search.Request{
		Query:  opts.query,
		Mode:   mode,
		K:      search.ClampK(searchK(cmd.Flags().Changed("k"), opts.k, cfg.Search.K)),
		Tag:    opts.tag,
		Caller: "explain",
	})
	if err != nil {
		return err
	}
	if opts.asJSON {
		var buf strings.Builder
		enc := json.NewEncoder(&buf)
		enc.SetEscapeHTML(false)
		enc.SetIndent("", "  ")
		if err := enc.Encode(ex); err != nil {
			return errs.Wrap(err, errs.Internal, "cli.explain", "encode explanation: %v", err)
		}
		fmt.Fprint(stdout, buf.String())
	} else {
		fmt.Fprint(stdout, renderExplanation(ex))
	}
	if ex.FTS != nil {
		noteSpell(stderr, ex.FTS.Spell)
	}
	if ex.Mode == search.ModeFTSFallback {
		fmt.Fprintf(stderr, "warning: %s; explained --mode fts instead\n", ex.FallbackReason)
		return exitSilently(ExitEmbedder)
	}
	return nil
}

// renderExplanation is the human form: a header (with an "exact:" line only when the exact list
// ran or is turned off), then one block per candidate (an "exact" line only for a chunk in that
// list).
func renderExplanation(ex *search.Explanation) string {
	var b strings.Builder
	fmt.Fprintf(&b, "explain %q · mode %s · k %d · settings %s\n", ex.Query, ex.Mode, ex.K, shortHash(ex.SettingsHash))
	s := ex.Settings
	fmt.Fprintf(&b, "settings: rrf_k=%d fts_weight=%s vec_weight=%s bm25 text=%s heading=%s title=%s list_limit=%d candidate_limit=%d per_entry_cap=%d exact=%s\n",
		s.RRFK, num(s.FTSWeight), num(s.VecWeight), num(s.BM25Text), num(s.BM25Heading), num(s.BM25Title),
		s.ListLimit, s.CandidateLimit, s.PerEntryCap, exactSetting(s))
	switch {
	case ex.FTS == nil:
		b.WriteString("fts: not run (no list)\n")
	case ex.FTS.OrFallback:
		fmt.Fprintf(&b, "fts: match %s · OR retry fired (the AND query matched nothing)\n", ex.FTS.Match)
	default:
		fmt.Fprintf(&b, "fts: match %s · AND\n", ex.FTS.Match)
	}
	if ex.FTS != nil && len(ex.FTS.Synonyms) > 0 {
		terms := slices.Sorted(maps.Keys(ex.FTS.Synonyms))
		parts := make([]string, len(terms))
		for i, t := range terms {
			parts[i] = t + " = " + strings.Join(ex.FTS.Synonyms[t], " | ")
		}
		fmt.Fprintf(&b, "synonyms: %s\n", strings.Join(parts, "; "))
	}
	if ex.FTS != nil && len(ex.FTS.Spell) > 0 {
		parts := make([]string, len(ex.FTS.Spell))
		for i, f := range ex.FTS.Spell {
			parts[i] = fmt.Sprintf("%s -> %s (%d %s, %d chunks)", f.Term, f.Correction, f.Distance, plural(f.Distance, "edit", "edits"), f.Doc)
		}
		fmt.Fprintf(&b, "spell: %s (the FTS list had fewer than %d rows)\n", strings.Join(parts, "; "), search.SpellBelow)
	}
	switch {
	case ex.Exact != nil && ex.Exact.OrFallback:
		fmt.Fprintf(&b, "exact: match %s · OR retry fired · weight %s\n", ex.Exact.Match, num(s.ExactWeight))
	case ex.Exact != nil:
		fmt.Fprintf(&b, "exact: match %s · AND · weight %s\n", ex.Exact.Match, num(s.ExactWeight))
	case !s.ExactList:
		b.WriteString("exact: off (search.exact_list = false)\n")
	}
	switch pr := ex.Prior; {
	case pr.Active:
		fmt.Fprintf(&b, "prior: active · %d/%d judged searches · half-life %s days · max boost %s\n", pr.Judged, pr.MinJudged, num(pr.HalfLifeDays), num(pr.MaxBoost))
	case pr.Inactive != "":
		fmt.Fprintf(&b, "%s\n", pr.Inactive)
	}
	if m := ex.Memory; m != nil {
		if m.Active {
			fmt.Fprintf(&b, "memory: key %q · weight %s · %d earlier judged %s · %d %s", m.Key, num(m.Weight), m.Searches,
				plural(m.Searches, "search", "searches"), m.Items, plural(m.Items, "row", "rows"))
			if m.Skipped > 0 {
				fmt.Fprintf(&b, " · %d no longer in the index", m.Skipped)
			}
			b.WriteString("\n")
		} else {
			fmt.Fprintf(&b, "%s\n", m.Inactive)
		}
	}
	if ex.FallbackReason != "" {
		fmt.Fprintf(&b, "fallback: %s\n", ex.FallbackReason)
	}
	exactRows := ""
	if ex.Exact != nil {
		exactRows = fmt.Sprintf(" · exact %d rows", ex.NExact)
	}
	if ex.Memory != nil && ex.Memory.Active {
		exactRows += fmt.Sprintf(" · memory %d rows", ex.Memory.Items)
	}
	fmt.Fprintf(&b, "lists: fts %d rows · vec %d rows%s · %d candidates\n", ex.NFTS, ex.NVec, exactRows, len(ex.Candidates))
	if len(ex.Candidates) == 0 {
		b.WriteString("no candidates\n")
		return b.String()
	}
	for _, h := range ex.Candidates {
		b.WriteString("\n")
		status := h.Status
		if h.ReturnedRank != nil {
			status = fmt.Sprintf("returned as hit #%d", *h.ReturnedRank)
		}
		loc := h.Path
		if h.Heading != "" {
			loc += " › " + h.Heading
		}
		fmt.Fprintf(&b, "#%d  rrf %.6f  %s  [%s]\n", h.Rank, h.RRFScore, loc, status)
		if h.FTS != nil {
			fmt.Fprintf(&b, "    fts  rank %d  bm25 %.4f", h.FTS.Rank, h.FTS.BM25)
		} else {
			b.WriteString("    fts  -")
		}
		if h.Columns != nil {
			fmt.Fprintf(&b, "  columns text %s · heading %s · title %s",
				col(h.Columns.Text), col(h.Columns.Heading), col(h.Columns.Title))
		}
		b.WriteString("\n")
		if h.Vec != nil {
			fmt.Fprintf(&b, "    vec  rank %d  distance %.4f\n", h.Vec.Rank, h.Vec.Distance)
		} else {
			b.WriteString("    vec  -\n")
		}
		if h.Exact != nil {
			fmt.Fprintf(&b, "    exact rank %d  bm25 %.4f\n", h.Exact.Rank, h.Exact.BM25)
		}
		if h.Memory != nil {
			fmt.Fprintf(&b, "    memory rank %d  u %.4f\n", h.Memory.Rank, h.Memory.U)
		}
		terms := make([]string, len(h.RRF))
		for i, t := range h.RRF {
			terms[i] = fmt.Sprintf("%s %s/(%d+%d)=%.6f", t.List, num(t.Weight), ex.Settings.RRFK, t.Rank, t.Term)
		}
		fmt.Fprintf(&b, "    rrf  %s\n", strings.Join(terms, " + "))
		if h.Prior != nil {
			fmt.Fprintf(&b, "    prior  x%.4f\n", *h.Prior)
		}
	}
	return b.String()
}

// col formats one single-column bm25 score; a chunk that does not match the expression has none.
func col(v *float64) string {
	if v == nil {
		return "-"
	}
	return fmt.Sprintf("%.4f", *v+0) // +0 turns -0 into 0
}

// num prints a setting in its shortest form (1, not 1.000000).
func num(v float64) string { return fmt.Sprintf("%g", v) }

// shortHash is the first 12 characters of a settings hash.
func shortHash(h string) string {
	if len(h) > 12 {
		return h[:12]
	}
	return h
}

// exactSetting is the settings line's exact-list entry: its weight, or "off" (search.exact_list = false).
func exactSetting(s search.ExplainParams) string {
	if !s.ExactList {
		return "off"
	}
	return num(s.ExactWeight)
}
