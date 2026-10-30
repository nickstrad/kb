// `kb search`: hybrid FTS5 + sqlite-vec search over the indexed chunks.
package cli

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"strings"

	"github.com/spf13/cobra"

	"github.com/nickstrad/kb/internal/config"
	"github.com/nickstrad/kb/internal/embed"
	"github.com/nickstrad/kb/internal/errs"
	"github.com/nickstrad/kb/internal/render"
	"github.com/nickstrad/kb/internal/search"
	"github.com/nickstrad/kb/internal/store"
	"github.com/nickstrad/kb/internal/workspace"
)

// newSearchCmd builds
// `kb search "<q>" [-k 8] [--mode hybrid|fts|vec] [--tag t] [--json] [--caller name]`.
func newSearchCmd(d *deps, stdout, stderr io.Writer) *cobra.Command {
	var (
		k      int
		mode   string
		tag    string
		asJSON bool
		caller string
		budget int
		stores string
	)
	cmd := &cobra.Command{
		Use:   "search <query>",
		Short: "Search the knowledge store",
		Long: "Runs an FTS5/BM25 list and a sqlite-vec KNN list and fuses them with reciprocal\n" +
			"rank fusion. Exit code 2 means the embedder was unavailable (for example the model\n" +
			"is not downloaded: run kb model pull): hybrid falls back to FTS; vec fails without\n" +
			"results. Search never downloads a model. Agents and scripts should parse --json.\n\n" +
			"Output opens with \"search N · M hits\" (N is the search id, so it survives | head).\n" +
			"Read a hit with kb show <path>; that read is recorded as an open of this search.\n" +
			"Feedback is optional: kb feedback N <rank> --useful (or kb feedback N --none) judges\n" +
			"a search, and --json carries those commands in its \"feedback\" field.\n\n" +
			"--stores a,b also searches other stores (names, aliases, global, NAME@LOC or a path;\n" +
			"all = every store of this location; at most 8). The default is [search] stores;\n" +
			"--stores '' searches this store only. Each store uses its own embedding model and\n" +
			"this store's [search] settings; the lists are merged on ranks. Another store's hit\n" +
			"shows as REF:path (read it with kb show REF:path). A store that cannot be searched\n" +
			"is skipped with a warning. The search is logged in this store only.",
		Args:          cobra.MinimumNArgs(1),
		SilenceUsage:  true,
		SilenceErrors: true,
		RunE: func(cmd *cobra.Command, args []string) error {
			return runSearch(cmd, d, stdout, stderr, searchOpts{
				query:  strings.Join(args, " "),
				k:      k,
				mode:   mode,
				tag:    tag,
				asJSON: asJSON,
				caller: caller,
				budget: budget,
				stores: stores,
				fanOut: cmd.Flags().Changed("stores"),
			})
		},
	}
	cmd.Flags().IntVarP(&k, "k", "k", search.DefaultK, "number of hits to return (1..20; default [search] k, 8)")
	cmd.Flags().StringVar(&mode, "mode", search.ModeHybrid, "hybrid, fts or vec")
	cmd.Flags().StringVar(&tag, "tag", "", "only entries carrying this tag")
	cmd.Flags().BoolVar(&asJSON, "json", false, "machine-readable output")
	cmd.Flags().StringVar(&caller, "caller", "", "who is searching (default [agent] caller, else unknown)")
	cmd.Flags().IntVar(&budget, "budget", 0, "cap the human output at about N tokens (ceil(chars/4)); 0 = unlimited (default [output] budget)")
	cmd.Flags().StringVar(&stores, "stores", "", "also search these stores, comma-separated ('' = none, all = every store here; default [search] stores)")
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
	budget int
	stores string // --stores as typed
	fanOut bool   // --stores was given (else [search] stores applies)
}

// runSearch validates the flags, opens the database (it never creates one), builds the embedder
// only when the mode needs it, runs the search and prints the result. With the none embedder a
// hybrid search runs as plain fts.
//
// Exit codes: 1 for bad flags, no database or an embed_meta mismatch; 2 when the embedder was
// unavailable, either for --mode vec or because hybrid fell back to fts (results are printed, then
// the warning); 0 otherwise, including a search whose results could not be logged.
func runSearch(cmd *cobra.Command, d *deps, stdout, stderr io.Writer, opts searchOpts) error {
	switch opts.mode {
	case search.ModeHybrid, search.ModeFTS, search.ModeVec:
	default:
		return usageErr("invalid --mode %q (want hybrid, fts or vec)", opts.mode)
	}
	if opts.budget < 0 {
		return usageErr("invalid --budget %d (want 0 or more tokens)", opts.budget)
	}
	ctx := cmd.Context()

	ws, cfg, err := d.workspaceConfig(cmd)
	if err != nil {
		return err
	}
	embs := embedderSet{}
	st, embedder, mode, err := d.openSearchWith(cmd, ws, cfg, opts.mode, "cli.search", false, embs)
	if err != nil {
		return err
	}
	defer st.Close()

	settings, err := search.NewSettings(cfg)
	if err != nil {
		return err
	}
	memo := &search.QueryMemo{}
	searcher := &search.Searcher{Store: st, Log: st, Embedder: embedder, KBVersion: Version, Settings: settings, Now: d.clock, Queries: memo}
	refs := fanOutRefs(opts.fanOut, opts.stores, cfg.Search.Stores)
	extras, err := d.openExtraStores(cmd, ws, refs, mode, settings, memo, embs)
	defer extras.close()
	if err != nil {
		return err
	}
	if n := embs.models(); len(n) > 2 {
		fmt.Fprintf(stderr, "warning: this search loads %d embedding models (%s); each one takes memory and time\n", len(n), strings.Join(n, ", "))
	}
	req := search.Request{
		Query:  opts.query,
		Mode:   mode,
		K:      search.ClampK(searchK(cmd.Flags().Changed("k"), opts.k, cfg.Search.K)),
		Tag:    opts.tag,
		Caller: callerFor(opts.caller, cfg),
	}

	parts := append([]search.Part{{Root: ws.Root, Searcher: searcher}}, extras.parts...)
	mo, err := search.SearchStores(ctx, parts, req)
	if err != nil {
		return err
	}
	for _, s := range mo.Skipped {
		fmt.Fprintf(stderr, "warning: skipping store %s: %s\n", s.Part.Label, s.Err)
	}
	for _, s := range mo.Degraded {
		fmt.Fprintf(stderr, "warning: store %s searched with FTS only: %s\n", s.Part.Label, s.Err)
	}
	out := mo.Outcome
	_, logErr := searcher.Record(ctx, req, out)
	logged := logErr == nil
	budget := opts.budget
	if !cmd.Flags().Changed("budget") {
		budget = cfg.Output.Budget
	}
	lookups := extras.lookups()
	lookups[""] = st
	if err := printResults(ctx, stdout, lookups, out.Result, opts.asJSON, logged, budget); err != nil {
		return err
	}
	if !logged {
		fmt.Fprintf(stderr, "warning: search not logged: %s\n", logErr)
	}

	if out.Mode == search.ModeFTSFallback {
		// The index lacking vectors (schema 3, not reindexed yet) is not the embedder's fault:
		// say what is missing and how to fix it. Same exit code: the search was FTS only.
		if errors.Is(out.FallbackErr, search.ErrNoVectors) {
			fmt.Fprintf(stderr, "warning: %s; falling back to --mode fts\n", out.FallbackErr)
		} else {
			fmt.Fprintf(stderr, "warning: embedder unavailable (%s); falling back to --mode fts\n", out.FallbackErr)
		}
		return exitSilently(ExitEmbedder)
	}
	return nil
}

// searchK picks the result count: an explicit -k wins, else [search] k from the config.
func searchK(flagChanged bool, flagK, configK int) int {
	if flagChanged {
		return flagK
	}
	return configK
}

// callerFor picks who is searching: --caller, else [agent] caller, else "unknown".
func callerFor(flag string, cfg config.Config) string {
	if flag != "" {
		return flag
	}
	if cfg.Agent.Caller != "" {
		return cfg.Agent.Caller
	}
	return "unknown"
}

// printResults writes the search result to stdout. --json emits the result fields (with full,
// untrimmed text) plus the feedback commands for its search_id, and nothing else. The human form
// opens with a "search N · M hits" line, so the id survives `kb search ... | head`, then one block
// per hit, then the search_id=N footer (kept for scripts that grep it). Feedback is optional, so
// the human form carries no hint; --json keeps the `kb feedback` commands.
// A result that could not be logged has no usable search id, so its human header and footer say
// so, and its JSON search_id and feedback are null rather than the zero value Go
// leaves in Result.SearchID. An encoding failure (a NaN or Inf score from a model) is returned
// as an Internal error with nothing written.
//
// lookups holds each store's entry lookup by the hit's Store tag ("" = the primary store).
func printResults(ctx context.Context, stdout io.Writer, lookups map[string]entryLookup, result *search.Result, asJSON, logged bool, budget int) error {
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
			// json rejects NaN and Inf floats, and a model can produce them.
			return errs.Wrap(err, errs.Internal, "cli.search", "encode result: %v", err)
		}
		fmt.Fprint(stdout, buf.String())
		return nil
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
		rendered, err := renderHits(ctx, lookups, result.Hits)
		if err != nil {
			return err
		}
		if text := render.Render(rendered, budget).Text; text != "" {
			fmt.Fprintln(stdout, text)
		}
	}
	if !logged {
		fmt.Fprintln(stdout, "search_id=none (not logged)")
		return nil
	}
	fmt.Fprintf(stdout, "search_id=%d\n", result.SearchID)
	return nil
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

// entryLookup is what rendering needs from a store: its entry rows, in one read per store.
type entryLookup interface {
	EntriesByPath(ctx context.Context, paths []string) (map[string]store.EntryRow, error)
}

// renderHits adds each hit's entry summary and updated date (search.Hit carries neither), read
// from the hit's own store (lookups by Store tag) with one EntriesByPath per store. An entry
// missing from its index (the file was removed since), or a store with no lookup, leaves those two
// fields empty: the hit still renders from what the search returned. Any other failure, a cancel or
// a database error, is returned: it is not "no such entry".
func renderHits(ctx context.Context, lookups map[string]entryLookup, hits []search.Hit) ([]render.Hit, error) {
	paths := map[string][]string{}
	for _, h := range hits {
		paths[h.Store] = append(paths[h.Store], h.Path)
	}
	rows := map[string]map[string]store.EntryRow{}
	for tag, ps := range paths {
		l, ok := lookups[tag]
		if !ok || l == nil {
			continue
		}
		got, err := l.EntriesByPath(ctx, ps)
		if err != nil {
			return nil, err // a cancel or a database error is not "no such entry"
		}
		rows[tag] = got
	}
	out := make([]render.Hit, len(hits))
	for i, h := range hits {
		out[i] = render.Hit{Rank: h.Rank, Store: h.Store, Path: h.Path, Heading: h.Heading, Text: h.Text}
		if row, ok := rows[h.Store][h.Path]; ok {
			out[i].Updated = row.Updated
			out[i].Summary = row.Summary
		}
	}
	return out, nil
}

// openSearch is the setup kb search and kb explain share: the index must exist (it is never
// created here), and for hybrid or vec the embedder is built and checked against embed_meta. A
// none embedder makes hybrid an fts search (not a fallback: exit 0) and vec a Usage error. It
// returns the mode actually run and a nil embedder for fts. op names the command in errors. On
// success the caller closes st.
func (d *deps) openSearch(cmd *cobra.Command, ws *workspace.Workspace, cfg config.Config, mode, op string, readOnly bool) (store.Store, embed.Embedder, string, error) {
	return d.openSearchWith(cmd, ws, cfg, mode, op, readOnly, nil)
}

// openSearchWith is openSearch building its embedder through embs (nil: build a new one).
func (d *deps) openSearchWith(cmd *cobra.Command, ws *workspace.Workspace, cfg config.Config, mode, op string, readOnly bool, embs embedderSet) (store.Store, embed.Embedder, string, error) {
	ctx := cmd.Context()
	if _, err := os.Stat(ws.DBPath); err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			return nil, nil, "", errs.Wrap(err, errs.NotFound, op, "no index at %s; run kb reindex --all", ws.DBPath)
		}
		return nil, nil, "", errs.Wrap(err, errs.Usage, op, "cannot read the index %s: check the file and its permissions", ws.DBPath).With("path", ws.DBPath)
	}
	st, err := d.openStore(ctx, ws.DBPath, readOnly)
	if err != nil {
		return nil, nil, "", err
	}
	embedder, mode, err := d.searchEmbedderWith(cmd, st, cfg, mode, op, embs)
	if err != nil {
		st.Close()
		return nil, nil, "", err
	}
	return st, embedder, mode, nil
}

// searchEmbedder is openSearch's embedder step.
func (d *deps) searchEmbedder(cmd *cobra.Command, st store.Store, cfg config.Config, mode, op string) (embed.Embedder, string, error) {
	return d.searchEmbedderWith(cmd, st, cfg, mode, op, nil)
}

// searchEmbedderWith is searchEmbedder building through embs, which, when not nil, shares one
// embedder per distinct [embed] config between the stores of a cross-store search (one loaded
// model each).
func (d *deps) searchEmbedderWith(cmd *cobra.Command, st store.Store, cfg config.Config, mode, op string, embs embedderSet) (embed.Embedder, string, error) {
	if mode != search.ModeHybrid && mode != search.ModeVec {
		return nil, mode, nil
	}
	client, err := embs.get(cfg.Embed, func() (embed.Embedder, error) { return d.newEmbedder(cmd, cfg.Embed) })
	if err != nil {
		return nil, "", err
	}
	if embed.IsNone(client) {
		if mode == search.ModeVec {
			return nil, "", usageErr("--mode vec needs an embedder, but the embedder is none")
		}
		return nil, search.ModeFTS, nil
	}
	stored, ok, err := st.EmbedMeta(cmd.Context())
	if err != nil {
		return nil, "", err
	}
	if want := (store.EmbedMeta{Model: client.Model(), Dim: client.Dim()}); ok && stored != want {
		mismatch := &store.EmbedMetaMismatchError{
			StoredModel: stored.Model, StoredDim: stored.Dim, WantModel: want.Model, WantDim: want.Dim,
		}
		return nil, "", errs.Wrap(mismatch, errs.Conflict, op, "%s", mismatch.Error())
	}
	return client, mode, nil
}
