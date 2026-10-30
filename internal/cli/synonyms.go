// `kb synonyms suggest`: candidate synonym groups mined from the search log (PLAN 5.6).
package cli

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/spf13/cobra"

	"github.com/nickstrad/kb/internal/errs"
	"github.com/nickstrad/kb/internal/search"
	"github.com/nickstrad/kb/internal/store"
	"github.com/nickstrad/kb/internal/workspace"
)

// newSynonymsCmd builds `kb synonyms`, whose one subcommand is suggest.
func newSynonymsCmd(d *deps, stdout, stderr io.Writer) *cobra.Command {
	cmd := &cobra.Command{
		Use:   "synonyms",
		Short: "Work with the store's synonyms (synonyms.txt)",
		Long: "kb search expands a query word with its synonyms from <store>/synonyms.txt (or\n" +
			"search.synonyms): one comma-separated group per line, # starts a comment line.\n" +
			"kb synonyms suggest proposes groups from the search log; kb never writes the file.",
		Args:          cobra.NoArgs,
		SilenceUsage:  true,
		SilenceErrors: true,
	}
	cmd.AddCommand(newSynonymsSuggestCmd(d, stdout, stderr))
	return cmd
}

// synonymsSuggestOpts are the parsed flags of kb synonyms suggest.
type synonymsSuggestOpts struct {
	since   string
	window  time.Duration
	min     int
	maxGaps int
	asJSON  bool
}

// newSynonymsSuggestCmd builds
// `kb synonyms suggest [--since d|date] [--window 5m] [--min 1] [--gaps 20] [--json]`.
func newSynonymsSuggestCmd(d *deps, stdout, stderr io.Writer) *cobra.Command {
	var o synonymsSuggestOpts
	cmd := &cobra.Command{
		Use:   "suggest",
		Short: "Propose synonym groups from reworded searches in the log",
		Long: "Reads this store's search log (read-only) for reworded searches: two searches\n" +
			"by one caller at most --window apart, the first with no useful verdict and no\n" +
			"kb show open, the second with one, whose queries differ in one word. Each such\n" +
			"word pair is a candidate group, printed as a synonyms.txt line under a comment\n" +
			"with its evidence (pairs, callers, how many chunks hold each word). Groups that\n" +
			"synonyms.txt already has are left out. A pair that is a typo (search.spell\n" +
			"already corrects it) says so. Gaps are words of unsuccessful searches that no\n" +
			"chunk holds. Every line but the groups starts with #, so the output can be\n" +
			"pasted whole; kb never writes synonyms.txt: you decide what goes in.",
		Args:          cobra.NoArgs,
		SilenceUsage:  true,
		SilenceErrors: true,
		RunE: func(cmd *cobra.Command, args []string) error {
			return runSynonymsSuggest(cmd, d, stdout, o)
		},
	}
	cmd.Flags().StringVar(&o.since, "since", "", "only searches at/after this bound: a Go duration (24h), Nd for N days, or a YYYY-MM-DD date")
	cmd.Flags().DurationVar(&o.window, "window", search.DefaultSuggestWindow, "most time between the two searches of a reworded pair")
	cmd.Flags().IntVar(&o.min, "min", 1, "only groups seen in at least this many reworded pairs")
	cmd.Flags().IntVar(&o.maxGaps, "gaps", 20, "list at most this many gap words (0 = none)")
	cmd.Flags().BoolVar(&o.asJSON, "json", false, "machine-readable output")
	return cmd
}

// runSynonymsSuggest reads the log and the vocabulary read-only and prints the suggestions.
func runSynonymsSuggest(cmd *cobra.Command, d *deps, stdout io.Writer, o synonymsSuggestOpts) error {
	const op = "cli.synonyms"
	if o.window <= 0 {
		return usageErr("invalid --window %s (want a positive duration like 5m)", o.window)
	}
	if o.min < 1 {
		return usageErr("invalid --min %d (want 1 or more)", o.min)
	}
	if o.maxGaps < 0 {
		return usageErr("invalid --gaps %d (want 0 or more)", o.maxGaps)
	}
	since, err := parseSince(o.since, d.clock())
	if err != nil {
		return usageErr("%v", err)
	}
	ws, cfg, err := d.workspaceConfig(cmd)
	if err != nil {
		return err
	}
	syn, err := d.loadSynonyms(cmd, ws, cfg)
	if err != nil {
		return err
	}
	searches, docFreq, err := readSuggestInputs(cmd, d, ws, since, op)
	if err != nil {
		return err
	}
	sug := search.Suggest(searches, docFreq, search.SuggestOptions{Window: o.window, MinPairs: o.min, MaxGaps: o.maxGaps, Synonyms: syn})
	if o.maxGaps == 0 { // --gaps 0 lists none (MaxGaps 0 would be no cap)
		sug.Gaps = []search.Gap{}
	}
	if o.asJSON {
		enc := json.NewEncoder(stdout)
		enc.SetEscapeHTML(false)
		enc.SetIndent("", "  ")
		if err := enc.Encode(sug); err != nil {
			return errs.Wrap(err, errs.Internal, op, "encode suggestions: %v", err)
		}
		return nil
	}
	file := cfg.Search.Synonyms
	if file == "" {
		file = filepath.Join(ws.StoreDir, search.SynonymsFile)
	}
	fmt.Fprint(stdout, renderSuggestions(sug, file))
	return nil
}

// readSuggestInputs opens the index read-only and reads the searches since the bound and the
// document count of their words.
func readSuggestInputs(cmd *cobra.Command, d *deps, ws *workspace.Workspace, since time.Time, op string) ([]store.JudgedSearch, map[string]int, error) {
	ctx := cmd.Context()
	if _, err := os.Stat(ws.DBPath); err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			return nil, nil, errs.Wrap(err, errs.NotFound, op, "no index at %s; run kb reindex --all", ws.DBPath)
		}
		return nil, nil, errs.Wrap(err, errs.Usage, op, "cannot read the index %s: check the file and its permissions", ws.DBPath).With("path", ws.DBPath)
	}
	st, err := d.openStore(ctx, ws.DBPath, true)
	if err != nil {
		return nil, nil, err
	}
	defer st.Close()
	jl, okLog := st.(store.JudgedLog)
	v, okVocab := st.(store.Vocab)
	if !okLog || !okVocab {
		return nil, nil, errs.New(errs.Internal, op, "the store %T cannot read back the search log and its vocabulary", st)
	}
	searches, err := jl.Judged(ctx, store.JudgedFilter{Since: since})
	if err != nil {
		return nil, nil, err
	}
	docFreq, err := v.DocFreq(ctx, search.SuggestTerms(searches))
	if err != nil {
		return nil, nil, err
	}
	return searches, docFreq, nil
}

// renderSuggestions is the human form: every line a # comment except the group lines, so the
// whole output can be pasted into synonyms.txt.
func renderSuggestions(s search.Suggestions, file string) string {
	var b strings.Builder
	fmt.Fprintf(&b, "# kb synonyms suggest · %d searches · %d reworded %s (window %s)\n", s.Searches, s.Pairs, plural(s.Pairs, "pair", "pairs"), s.Window)
	if len(s.Suggestions) == 0 {
		b.WriteString("# no groups to suggest\n")
	} else {
		fmt.Fprintf(&b, "# paste the groups you approve into %s (kb never writes it)\n", file)
	}
	for _, g := range s.Suggestions {
		b.WriteString("\n")
		var from []string
		for _, t := range g.Terms {
			if n := g.From[t]; n > 0 {
				other := g.Terms[0]
				if other == t {
					other = g.Terms[1]
				}
				from = append(from, fmt.Sprintf("%s -> %s %d", t, other, n))
			}
		}
		fmt.Fprintf(&b, "# %d %s, %d %s (%s); chunks: %s %d, %s %d",
			g.Pairs, plural(g.Pairs, "pair", "pairs"), g.Callers, plural(g.Callers, "caller", "callers"), strings.Join(from, ", "),
			g.Terms[0], g.Doc[g.Terms[0]], g.Terms[1], g.Doc[g.Terms[1]])
		if g.Typo {
			b.WriteString("; a typo: search.spell already corrects it")
		}
		fmt.Fprintf(&b, "\n%s\n", g.Line)
	}
	if len(s.Gaps) > 0 {
		b.WriteString("\n# gaps: words of searches without a useful hit or open that no chunk holds\n")
		for _, g := range s.Gaps {
			fmt.Fprintf(&b, "#   %s (%d %s)\n", g.Term, g.Searches, plural(g.Searches, "search", "searches"))
		}
	}
	return b.String()
}

// plural is one when n is 1, else many.
func plural(n int, one, many string) string {
	if n == 1 {
		return one
	}
	return many
}
