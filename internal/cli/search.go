// `kb search`: hybrid FTS5 + sqlite-vec search over the indexed chunks. P3.4 implements it.
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

	"knowledge/kb/internal/embed"
	"knowledge/kb/internal/search"
	"knowledge/kb/internal/store"
)

// hitTextLimit is the ~600-char (rune) cap on a hit's chunk text in human output. --json always
// carries the full, untrimmed text.
const hitTextLimit = 600

// newSearchCmd builds
// `kb search "<q>" [-k 8] [--mode hybrid|fts|vec] [--tag t] [--json] [--caller name]`.
func newSearchCmd(stdout, stderr io.Writer) *cobra.Command {
	var (
		k      int
		mode   string
		tag    string
		asJSON bool
		caller string
	)
	cmd := &cobra.Command{
		Use:   "search <query>",
		Short: "Search the knowledge store",
		Long: "Runs an FTS5/BM25 list and a sqlite-vec KNN list and fuses them with reciprocal\n" +
			"rank fusion. Exit code 2 means the embedder was unavailable: hybrid falls back\n" +
			"to FTS; vec fails without results. Agents and scripts should parse --json.\n\n" +
			"Output opens with \"search N · M hits\" (N is the search id, so it survives | head) and\n" +
			"ends with the kb feedback commands that judge this search; --json carries the same\n" +
			"commands in its \"feedback\" field. Judge every search, including one that found\n" +
			"nothing: kb feedback N <rank> --useful, or kb feedback N --none --note \"why\".",
		Args:          cobra.MinimumNArgs(1),
		SilenceUsage:  true,
		SilenceErrors: true,
		RunE: func(cmd *cobra.Command, args []string) error {
			return runSearch(cmd, stdout, stderr, searchOpts{
				query:  strings.Join(args, " "),
				k:      k,
				mode:   mode,
				tag:    tag,
				asJSON: asJSON,
				caller: caller,
			})
		},
	}
	cmd.Flags().IntVarP(&k, "k", "k", search.DefaultK, "number of hits to return (1..20)")
	cmd.Flags().StringVar(&mode, "mode", search.ModeHybrid, "hybrid, fts or vec")
	cmd.Flags().StringVar(&tag, "tag", "", "only entries carrying this tag")
	cmd.Flags().BoolVar(&asJSON, "json", false, "machine-readable output")
	cmd.Flags().StringVar(&caller, "caller", "", "who is searching (default $KB_CALLER, else unknown)")
	return cmd
}

// searchOpts is the parsed, not-yet-validated form of the command's flags.
type searchOpts struct {
	query  string
	k      int
	mode   string
	tag    string
	asJSON bool
	caller string
}

// runSearch validates the flags, opens the database read-only-in-spirit (it never creates one),
// builds the embedder only when the mode needs it, runs the search and prints the result. With
// the none embedder a hybrid search runs as plain fts.
//
// Exit codes (plan.md's "CLI" section):
//   - 1 (ExitUsage): bad flags, no database, or an embed_meta/embedder mismatch.
//   - 2 (ExitEmbedder): the embedder was unavailable, either because --mode vec had nothing to
//     fall back to, or because a hybrid search degraded to fts-fallback (results are still
//     printed in that case; the warning is what carries the exit code).
//   - 0: everything else, including a search whose results were printed but could not be logged.
func runSearch(cmd *cobra.Command, stdout, stderr io.Writer, opts searchOpts) error {
	switch opts.mode {
	case search.ModeHybrid, search.ModeFTS, search.ModeVec:
	default:
		return usageErr("kb search: invalid --mode %q (want hybrid, fts or vec)", opts.mode)
	}

	root := Root()
	dbPath := DBPath(root)
	if _, err := os.Stat(dbPath); err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			return usageErr("no index at %s; run kb reindex --all", dbPath)
		}
		return usageErr("kb search: stat %s: %s", dbPath, err)
	}

	st, err := store.Open(dbPath)
	if err != nil {
		return usageErr("kb search: %s", err)
	}
	defer st.Close()

	mode := opts.mode
	var embedder embed.Embedder
	var isUnavailable func(error) bool
	if mode == search.ModeHybrid || mode == search.ModeVec {
		client, err := newEmbedder(cmd)
		if err != nil {
			return usageErr("kb search: %s", err)
		}
		if embed.IsNone(client) {
			// KB_EMBEDDER=none: FTS is the whole search, not a fallback, so it exits 0.
			if mode == search.ModeVec {
				return usageErr("kb search: --mode vec needs an embedder, but the embedder is none")
			}
			mode = search.ModeFTS
		} else {
			if storedModel, storedDim, ok, err := st.EmbedMeta(); err != nil {
				return usageErr("kb search: %s", err)
			} else if ok && (storedModel != client.Model() || storedDim != client.Dim()) {
				mismatch := &store.EmbedMetaMismatchError{
					StoredModel: storedModel, StoredDim: storedDim,
					WantModel: client.Model(), WantDim: client.Dim(),
				}
				return usageErr("%s", mismatch.Error())
			}
			embedder = client
			isUnavailable = func(err error) bool { return errors.Is(err, embed.ErrUnavailable) }
		}
	}

	searcher := &search.Searcher{
		DB:            st.DB(),
		Embedder:      embedder,
		KBVersion:     search.GitShortHash(root),
		IsUnavailable: isUnavailable,
	}

	req := search.Request{
		Query:  opts.query,
		Mode:   mode,
		K:      search.ClampK(opts.k),
		Tag:    opts.tag,
		Caller: Caller(opts.caller),
	}

	out, err := searcher.SearchAndLog(cmd.Context(), req)
	if err != nil && out == nil {
		// A vec-mode search with nothing to fall back to returns the raw embedder error rather
		// than search.ErrEmbedderUnavailable (that sentinel marks the fallback path a hybrid
		// search takes instead), so unavailability is recognised the same way the Searcher itself
		// recognises it: either sentinel, via the same isUnavailable hook.
		if errors.Is(err, search.ErrEmbedderUnavailable) || (isUnavailable != nil && isUnavailable(err)) {
			return embedderErr("%s", err)
		}
		return usageErr("kb search: %s", trimSearchErrorPrefix(err.Error()))
	}
	if err != nil && !errors.Is(err, search.ErrLogFailed) {
		return usageErr("kb search: %s", trimSearchErrorPrefix(err.Error()))
	}

	logFailed := errors.Is(err, search.ErrLogFailed)
	printResults(stdout, out.Result, opts.asJSON, !logFailed)
	if logFailed {
		fmt.Fprintf(stderr, "warning: search not logged: %s\n", err)
	}

	if out.Mode == search.ModeFTSFallback {
		return embedderErr("warning: embedder unavailable (%s); falling back to --mode fts", out.FallbackErr)
	}
	return nil
}

// printResults writes the search result to stdout. --json emits the result fields (with full,
// untrimmed text) plus the feedback commands for its search_id, and nothing else. The human form
// opens with a "search N · M hits" line, so the id survives `kb search ... | head`, then one block
// per hit, then the search_id=N footer (kept for scripts that grep it) and a one-line hint naming
// the exact `kb feedback` commands that judge this search.
// A result that could not be logged has no usable search id, so its human header and footer say
// so and print no hint, and its JSON search_id and feedback are null rather than the zero value Go
// leaves in Result.SearchID.
func printResults(stdout io.Writer, result *search.Result, asJSON, logged bool) {
	if asJSON {
		var searchID *int64
		var feedback *feedbackCommands
		if logged {
			searchID = &result.SearchID
			feedback = newFeedbackCommands(result.SearchID, len(result.Hits))
		}
		// An Encoder rather than json.MarshalIndent so <, > and & in hit text (entries quote things
		// like <entry>) stay readable instead of becoming \u003c and friends. Decoded values are
		// unchanged either way.
		var buf strings.Builder
		enc := json.NewEncoder(&buf)
		enc.SetEscapeHTML(false)
		enc.SetIndent("", "  ")
		if err := enc.Encode(struct {
			SearchID *int64            `json:"search_id"`
			Feedback *feedbackCommands `json:"feedback"`
			Query    string            `json:"query"`
			Mode     string            `json:"mode"`
			Hits     []search.Hit      `json:"hits"`
		}{searchID, feedback, result.Query, result.Mode, result.Hits}); err != nil {
			// Result is a plain struct of strings/ints/floats; encoding cannot fail on it.
			panic(fmt.Sprintf("kb search: marshal result: %v", err))
		}
		fmt.Fprint(stdout, buf.String())
		return
	}

	hits := "hits"
	if len(result.Hits) == 1 {
		hits = "hit"
	}
	if logged {
		fmt.Fprintf(stdout, "search %d · %d %s\n", result.SearchID, len(result.Hits), hits)
	} else {
		fmt.Fprintf(stdout, "search not logged · %d %s\n", len(result.Hits), hits)
	}

	if len(result.Hits) == 0 {
		fmt.Fprintln(stdout, "no results")
	} else {
		blocks := make([]string, len(result.Hits))
		for i, h := range result.Hits {
			header := fmt.Sprintf("#%d  %.4f  %s", h.Rank, h.RRFScore, h.Path)
			if h.Heading != "" {
				header += " › " + h.Heading
			}
			blocks[i] = header + "\n" + trimText(h.Text, hitTextLimit)
		}
		fmt.Fprintln(stdout, strings.Join(blocks, "\n\n"))
	}
	if !logged {
		fmt.Fprintln(stdout, "search_id=none (not logged)")
		return
	}
	fmt.Fprintf(stdout, "search_id=%d\n", result.SearchID)
	fmt.Fprintln(stdout, feedbackHint(result.SearchID, len(result.Hits)))
}

// feedbackCommands is the --json "feedback" object: the exact commands that judge one search.
// Useful and NotUseful are omitted for a zero-result search, which has no rank to mark.
type feedbackCommands struct {
	Useful    string `json:"useful,omitempty"`
	NotUseful string `json:"not_useful,omitempty"`
	None      string `json:"none"`
}

func newFeedbackCommands(searchID int64, nHits int) *feedbackCommands {
	fc := &feedbackCommands{None: fmt.Sprintf("kb feedback %d --none --note \"why\"", searchID)}
	if nHits > 0 {
		fc.Useful = fmt.Sprintf("kb feedback %d RANK --useful", searchID)
		fc.NotUseful = fmt.Sprintf("kb feedback %d RANK --not-useful --note \"why\"", searchID)
	}
	return fc
}

// feedbackHint is the last line of human output. It deliberately avoids "|" between the
// alternatives: a line copied whole into a shell would otherwise pipe one kb command into another.
func feedbackHint(searchID int64, nHits int) string {
	if nHits == 0 {
		return fmt.Sprintf("→ judge: kb feedback %d --none --note \"what you were looking for\"", searchID)
	}
	return fmt.Sprintf("→ judge: kb feedback %d RANK --useful (or --not-useful) · none helped: kb feedback %d --none --note \"why\"",
		searchID, searchID)
}

// trimSearchErrorPrefix avoids user-facing errors such as "kb search: search: empty query".
// Search errors are already namespaced because search is also usable below the CLI layer.
func trimSearchErrorPrefix(message string) string {
	return strings.TrimPrefix(message, "search: ")
}

// trimText caps text at limit runes, cutting at a rune boundary. When it has to cut, it prefers
// the last newline or space at or before the limit (so a word is not sliced in half) and marks
// the cut with a trailing " …"; text at or under the limit is returned unchanged.
func trimText(text string, limit int) string {
	runes := []rune(text)
	if len(runes) <= limit {
		return text
	}
	cut := limit
	firstBoundary := limit - 80
	if firstBoundary < 0 {
		firstBoundary = 0
	}
	for i := limit; i > firstBoundary; i-- {
		if runes[i-1] == '\n' || runes[i-1] == ' ' {
			cut = i - 1
			break
		}
	}
	trimmed := strings.TrimRight(string(runes[:cut]), " \n")
	return trimmed + " …"
}
