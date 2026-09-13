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
	"knowledge/kb/internal/embed/ollama"
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
			"to FTS; vec fails without results. Agents and scripts should parse --json.",
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
// builds the embedder only when the mode needs it, runs the search and prints the result.
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

	needEmbedder := opts.mode == search.ModeHybrid || opts.mode == search.ModeVec

	var embedder embed.Embedder
	var isUnavailable func(error) bool
	if needEmbedder {
		client := ollama.New("", "", 0)
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
		isUnavailable = func(err error) bool { return errors.Is(err, ollama.ErrUnavailable) }
	}

	searcher := &search.Searcher{
		DB:            st.DB(),
		Embedder:      embedder,
		KBVersion:     search.GitShortHash(root),
		IsUnavailable: isUnavailable,
	}

	req := search.Request{
		Query:  opts.query,
		Mode:   opts.mode,
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

// printResults writes the search result to stdout: --json emits the result fields (with full,
// untrimmed text) and nothing else; the human form is one block per hit followed by the search_id
// footer that `kb feedback` takes.
// A result that could not be logged has no usable search id, so its human footer says so and its
// JSON search_id is null rather than the zero value Go leaves in Result.SearchID.
func printResults(stdout io.Writer, result *search.Result, asJSON, logged bool) {
	if asJSON {
		var searchID *int64
		if logged {
			searchID = &result.SearchID
		}
		data, err := json.MarshalIndent(struct {
			SearchID *int64       `json:"search_id"`
			Query    string       `json:"query"`
			Mode     string       `json:"mode"`
			Hits     []search.Hit `json:"hits"`
		}{searchID, result.Query, result.Mode, result.Hits}, "", "  ")
		if err != nil {
			// Result is a plain struct of strings/ints/floats; MarshalIndent cannot fail on it.
			panic(fmt.Sprintf("kb search: marshal result: %v", err))
		}
		fmt.Fprintf(stdout, "%s\n", data)
		return
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
