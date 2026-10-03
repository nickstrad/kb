// `kb feedback`: the step after every search. It records whether a returned hit was useful, or that
// nothing the search returned was (--none, which also covers a search with zero results), so search
// quality can be analysed with plain SQL and `kb stats` later. The search id comes from the first
// line `kb search` prints (search N · M hits) and the rank from the `#rank` of the hit being
// marked; --last stands in for the id when the caller just searched.
package cli

import (
	"errors"
	"fmt"
	"io"
	"os"
	"strconv"

	"github.com/spf13/cobra"

	"knowledge/kb/internal/search"
	"knowledge/kb/internal/store"
)

// newFeedbackCmd builds
// `kb feedback (<search_id> | --last) [<rank>] --useful|--not-useful|--none [--note "..."] [--caller name]`.
func newFeedbackCmd(stdout, stderr io.Writer) *cobra.Command {
	var opts feedbackOpts
	cmd := &cobra.Command{
		Use:   "feedback (<search_id> | --last) [<rank>] --useful|--not-useful|--none",
		Short: "Judge a search: mark one hit useful or not, or the whole search --none",
		Long: "Record a verdict on an earlier search. Run it after every search, including one that\n" +
			"found nothing: an unjudged search counts as unknown in kb stats, not as a miss.\n\n" +
			"search_id is the number on the first line of `kb search` output (search N · M hits, also\n" +
			"printed as search_id=N at the end); rank is the #rank of the hit. --useful and\n" +
			"--not-useful judge one hit and need its rank. --none judges the whole search as having\n" +
			"nothing useful and takes no rank; it is the only verdict a zero-result search can get.\n" +
			"Marking the same hit twice replaces the earlier verdict. A later --useful withdraws an\n" +
			"earlier --none; --none is refused while a hit is marked useful (re-mark it --not-useful\n" +
			"first).\n\n" +
			"--last targets the newest search logged for the caller (--caller, else $KB_CALLER, the\n" +
			"same resolution kb search uses); it refuses an unset or \"unknown\" caller. The log has no\n" +
			"notion of a session, so two sessions searching under the same caller name at once can\n" +
			"pick up each other's searches: --last prints the query it judged, and an explicit\n" +
			"search_id is the safe form whenever searches may overlap.",
		Example: "  kb feedback 42 1 --useful\n" +
			"  kb feedback 42 3 --not-useful --note \"about pgbouncer, not postgres\"\n" +
			"  kb feedback 42 --none --note \"nothing on tmux socket permissions\"\n" +
			"  kb feedback --last 1 --useful --caller claude",
		Args:          cobra.MaximumNArgs(2),
		SilenceUsage:  true,
		SilenceErrors: true,
		RunE: func(cmd *cobra.Command, args []string) error {
			opts.args = args
			return runFeedback(cmd, stdout, opts)
		},
	}
	cmd.Flags().BoolVar(&opts.useful, "useful", false, "the hit at <rank> answered the question")
	cmd.Flags().BoolVar(&opts.notUseful, "not-useful", false, "the hit at <rank> did not answer the question")
	cmd.Flags().BoolVar(&opts.none, "none", false, "nothing in the search helped (whole search; no rank)")
	cmd.Flags().BoolVar(&opts.last, "last", false, "judge the caller's newest search instead of naming a search_id")
	cmd.Flags().StringVar(&opts.caller, "caller", "", "whose newest search --last means (default $KB_CALLER)")
	cmd.Flags().StringVar(&opts.note, "note", "", "free-text note stored with the feedback")
	return cmd
}

// feedbackOpts is the parsed, not-yet-validated form of the command's flags and arguments.
type feedbackOpts struct {
	args      []string
	useful    bool
	notUseful bool
	none      bool
	last      bool
	caller    string
	note      string
}

// runFeedback validates the arguments before touching the database, resolves the target search
// (an explicit id, or the caller's newest search for --last), records the verdict and prints one
// confirmation line.
func runFeedback(cmd *cobra.Command, stdout io.Writer, opts feedbackOpts) error {
	// Exactly one verdict: none says nothing, two say contradictory things.
	verdicts := 0
	for _, set := range []bool{opts.useful, opts.notUseful, opts.none} {
		if set {
			verdicts++
		}
	}
	if verdicts != 1 {
		return usageErr("kb feedback: pass exactly one of --useful, --not-useful or --none")
	}

	// Split the positionals: with --last there is no search id, so the only one left is the rank.
	var idArg, rankArg string
	switch {
	case opts.last && len(opts.args) == 2:
		return usageErr("kb feedback: --last replaces the search id; pass at most a rank")
	case opts.last && len(opts.args) == 1:
		rankArg = opts.args[0]
	case !opts.last && len(opts.args) == 0:
		return usageErr("kb feedback: pass the search id from the first line of `kb search` output, or --last")
	case !opts.last:
		idArg = opts.args[0]
		if len(opts.args) == 2 {
			rankArg = opts.args[1]
		}
	}
	if opts.none && rankArg != "" {
		return usageErr("kb feedback: --none judges the whole search and takes no rank; drop %q", rankArg)
	}
	if !opts.none && rankArg == "" {
		return usageErr("kb feedback: --useful and --not-useful need the #rank of a hit; use --none to judge the whole search")
	}

	var searchID int64
	if idArg != "" {
		id, err := strconv.ParseInt(idArg, 10, 64)
		if err != nil || id < 1 {
			return usageErr("kb feedback: search id must be a positive integer, got %q", idArg)
		}
		searchID = id
	}
	rank := search.WholeSearchRank
	if rankArg != "" {
		r, err := strconv.Atoi(rankArg)
		if err != nil || r < 1 {
			return usageErr("kb feedback: rank must be a positive integer, got %q", rankArg)
		}
		rank = r
	}
	caller := Caller(opts.caller)
	if opts.last && caller == "unknown" {
		// "unknown" is also what every caller-less search was logged as, so its newest search could
		// belong to anyone; refuse it rather than guess.
		return usageErr("kb feedback: --last needs to know who searched; pass --caller (claude, codex) or set KB_CALLER, as for kb search")
	}

	// Check for the file before opening: store.Open creates an empty database, which
	// would turn "you never indexed anything" into "no search with id N".
	dbPath := DBPath(Root())
	if _, err := os.Stat(dbPath); err != nil {
		return usageErr("kb feedback: no search database at %s; run `kb reindex --all` first", dbPath)
	}
	st, err := store.Open(dbPath)
	if err != nil {
		return usageErr("kb feedback: %s", err)
	}
	defer st.Close()

	s := &search.Searcher{DB: st.DB()}
	ctx := cmd.Context()
	target := ""
	if opts.last {
		last, err := s.Last(ctx, caller)
		if errors.Is(err, search.ErrNoSearch) {
			return usageErr("kb feedback --last: no search logged for caller %q; run kb search --caller %s first, or pass a search id", caller, caller)
		}
		if err != nil {
			return usageErr("kb feedback: %s", err)
		}
		searchID = last.ID
		target = fmt.Sprintf(" (newest search for %s, %s: %q)", caller, last.TS, last.Query)
	}

	if opts.none {
		if err := s.FeedbackNone(ctx, searchID, opts.note); err != nil {
			return usageErr("kb feedback: %s", err)
		}
		fmt.Fprintf(stdout, "recorded: search %d none (nothing useful)%s\n", searchID, target)
		return nil
	}

	if err := s.Feedback(ctx, searchID, rank, opts.useful, opts.note); err != nil {
		return usageErr("kb feedback: %s", err)
	}
	usefulValue := 0
	if opts.useful {
		usefulValue = 1
	}
	fmt.Fprintf(stdout, "recorded: search %d rank %d useful=%d%s\n", searchID, rank, usefulValue, target)
	return nil
}
