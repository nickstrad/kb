// `kb feedback`: record whether a returned hit was useful, so search quality can be analysed with
// plain SQL later. The search id comes from the footer `kb search` prints (search_id=N) and the
// rank from the `#rank` of the hit being marked.
package cli

import (
	"fmt"
	"io"
	"os"
	"strconv"

	"github.com/spf13/cobra"

	"knowledge/kb/internal/search"
	"knowledge/kb/internal/store"
)

// newFeedbackCmd builds
// `kb feedback <search_id> <rank> --useful|--not-useful [--note "..."]`.
func newFeedbackCmd(stdout, stderr io.Writer) *cobra.Command {
	var (
		useful    bool
		notUseful bool
		note      string
	)
	cmd := &cobra.Command{
		Use:   "feedback <search_id> <rank>",
		Short: "Mark one search result useful or not useful",
		Long: "Record a verdict on one hit of an earlier search.\n\n" +
			"search_id is the number `kb search` prints in its footer (search_id=N); rank is the\n" +
			"#rank of the hit. Marking the same hit twice replaces the earlier verdict.",
		Example: "  kb feedback 42 1 --useful\n" +
			"  kb feedback 42 3 --not-useful --note \"about pgbouncer, not postgres\"",
		Args:          cobra.ExactArgs(2),
		SilenceUsage:  true,
		SilenceErrors: true,
		RunE: func(cmd *cobra.Command, args []string) error {
			// Exactly one verdict: neither flag says nothing, both say two things.
			if useful == notUseful {
				return usageErr("kb feedback: pass exactly one of --useful or --not-useful")
			}

			searchID, err := strconv.ParseInt(args[0], 10, 64)
			if err != nil || searchID < 1 {
				return usageErr("kb feedback: search id must be a positive integer, got %q", args[0])
			}
			rank, err := strconv.Atoi(args[1])
			if err != nil || rank < 1 {
				return usageErr("kb feedback: rank must be a positive integer, got %q", args[1])
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
			if err := s.Feedback(cmd.Context(), searchID, rank, useful, note); err != nil {
				return usageErr("kb feedback: %s", err)
			}

			usefulValue := 0
			if useful {
				usefulValue = 1
			}
			fmt.Fprintf(stdout, "recorded: search %d rank %d useful=%d\n", searchID, rank, usefulValue)
			return nil
		},
	}
	cmd.Flags().BoolVar(&useful, "useful", false, "the result answered the question")
	cmd.Flags().BoolVar(&notUseful, "not-useful", false, "the result did not answer the question")
	cmd.Flags().StringVar(&note, "note", "", "free-text note stored with the feedback")
	return cmd
}
