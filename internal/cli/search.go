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
		expand string
		nbrs   bool
	)
	cmd := &cobra.Command{
		Use:   "search <query>",
		Short: "Search the knowledge store",
		Long: "Runs an FTS5/BM25 list and a sqlite-vec KNN list and fuses them with reciprocal\n" +
			"rank fusion. Exit code 2 means the embedder was unavailable (for example the model\n" +
			"is not downloaded: run kb model pull): hybrid falls back to FTS; vec fails without\n" +
			"results. Search never downloads a model. Agents and scripts should parse --json.\n\n" +
			"A query holding an identifier (foo_bar, pkg.Func, --flag, path/to, a:b, camelCase)\n" +
			"also runs an exact-identifier list that matches it only as a whole token, so\n" +
			"kb search reindex_test finds reindex_test, not \"reindex test\" (search.exact_list,\n" +
			"weight search.list_weights.exact; kb explain shows it). Put a query that starts\n" +
			"with - after the flags and --: kb search --json -- --embed-model.\n\n" +
			"When the full-text list finds fewer than 3 rows, a query word that no chunk holds\n" +
			"is also searched as the closest common indexed word (1 edit, 2 from 8 letters;\n" +
			"never an identifier, a number or a word under 4 letters), and a note on stderr\n" +
			"says so (search.spell = false turns it off).\n\n" +
			"Output opens with \"search N · M hits\" (N is the search id, so it survives | head).\n" +
			"Read a hit with kb show <path>; that read is recorded as an open of this search.\n" +
			"Feedback is optional: kb feedback N <rank> --useful (or kb feedback N --none) judges\n" +
			"a search, and --json carries those commands in its \"feedback\" field.\n\n" +
			"--stores a,b also searches other stores (names, aliases, global, NAME@LOC or a path;\n" +
			"all = every store of this location; at most 8). The default is [search] stores;\n" +
			"--stores '' searches this store only. Each store uses its own embedding model and\n" +
			"this store's [search] settings; the lists are merged on ranks. Another store's hit\n" +
			"shows as REF:path (read it with kb show REF:path). A store that cannot be searched\n" +
			"is skipped with a warning. The search is logged in this store only.\n\n" +
			"--neighbors adds a \"Related:\" section after the hits: the entries linked to each\n" +
			"hit's entry (kb related), without the entries already returned as hits. It is not\n" +
			"part of the logged result; --json carries it as \"neighbors\". Hits that name an\n" +
			"entry another one supersedes are marked \"superseded\" and ranked down by\n" +
			"[search] supersede_penalty (1 = off).",
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
				expand: expand,
				nbrs:   nbrs,
				fanOut: cmd.Flags().Changed("stores"),
			})
		},
	}
	cmd.Flags().BoolVar(&nbrs, "neighbors", false, "append a Related: section with the entries linked to the hits (not logged)")
	cmd.Flags().IntVarP(&k, "k", "k", search.DefaultK, "number of hits to return, 1..20 (config [search] k sets the default)")
	cmd.Flags().StringVar(&mode, "mode", search.ModeHybrid, "hybrid, fts or vec")
	cmd.Flags().StringVar(&tag, "tag", "", "only entries carrying this tag, matched case-insensitively (an alias is searched like the title)")
	cmd.Flags().BoolVar(&asJSON, "json", false, "machine-readable output")
	cmd.Flags().StringVar(&caller, "caller", "", "who is searching (config [agent] caller sets the default, else unknown)")
	cmd.Flags().IntVar(&budget, "budget", 0, "cap the human output at about N tokens (ceil(chars/4)); 0 = unlimited (config [output] budget sets the default)")
	cmd.Flags().StringVar(&stores, "stores", "", "also search these stores, comma-separated ('' = none, all = every store here; config [search] stores sets the default)")
	cmd.Flags().StringVar(&expand, "expand", render.ExpandNone, "show each hit's whole section or entry instead of the chunk: none, section or entry (config [output] expand sets the default)")
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
	expand string // --expand as typed
	nbrs   bool   // --neighbors
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
	switch opts.expand {
	case render.ExpandNone, render.ExpandSection, render.ExpandEntry:
	default:
		return usageErr("invalid --expand %q (want none, section or entry)", opts.expand)
	}
	ctx := cmd.Context()

	ss, err := d.openStoresSearch(cmd, stderr, "cli.search", opts.mode, opts.fanOut, opts.stores)
	if err != nil {
		return err
	}
	defer ss.close()
	cfg := ss.cfg
	req := search.Request{
		Query:  opts.query,
		Mode:   ss.mode,
		K:      search.ClampK(searchK(cmd.Flags().Changed("k"), opts.k, cfg.Search.K)),
		Tag:    opts.tag,
		Caller: callerFor(opts.caller, cfg),
	}
	res, err := ss.run(cmd, d, stderr, req)
	if err != nil {
		return err
	}
	out, logErr, lookups := res.out, res.logErr, res.lookups
	logged := logErr == nil
	budget := opts.budget
	if !cmd.Flags().Changed("budget") {
		budget = cfg.Output.Budget
	}
	expand := opts.expand
	if !cmd.Flags().Changed("expand") {
		expand = cfg.Output.Expand
	}
	var nbrs []render.NeighborLine
	if opts.nbrs {
		if nbrs, err = neighborLines(ctx, ss.st, out.Result.Hits); err != nil {
			return err
		}
	}
	if err := printResults(ctx, stdout, lookups, out.Result, opts.asJSON, logged, budget, expand, nbrs); err != nil {
		return err
	}
	if !logged {
		fmt.Fprintf(stderr, "warning: search not logged: %s\n", logErr)
	}
	noteSpell(stderr, out.Spell)

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

// noteSpell says on stderr which words spell correction searched an alternative for (DEFAULTS
// #7: it never changes a search silently), one line each.
func noteSpell(stderr io.Writer, fixes []search.SpellFix) {
	for _, f := range fixes {
		fmt.Fprintf(stderr, "note: also searched %q for %q\n", f.Correction, f.Term)
	}
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
// expand is none, section or entry (--expand): the human form shows each hit's section or whole
// entry as its text, once per entry or section, and --json adds an "expanded" field per hit.
//
// nbrs, when not nil, is the --neighbors section: after the hits in the human form, a "neighbors"
// field in --json (an empty list when no hit has a neighbour).
//
// lookups holds each store's entry lookup by the hit's Store tag ("" = the primary store).
func printResults(ctx context.Context, stdout io.Writer, lookups map[string]entryLookup, result *search.Result, asJSON, logged bool, budget int, expand string, nbrs []render.NeighborLine) error {
	if asJSON {
		var searchID *int64
		var feedback *feedbackCommands
		if logged {
			searchID = &result.SearchID
			feedback = newFeedbackCommands(result.SearchID, len(result.Hits))
		}
		exp, err := expandHits(ctx, lookups, result.Hits, expand)
		if err != nil {
			return err
		}
		jhits := make([]jsonHit, len(result.Hits))
		for i, h := range result.Hits {
			jhits[i] = jsonHit{Hit: h, Expanded: exp[i].Text}
		}
		// An Encoder rather than json.MarshalIndent so <, > and & in hit text (entries quote things
		// like <entry>) stay readable instead of becoming \u003c and friends. Decoded values are
		// unchanged either way.
		var buf strings.Builder
		enc := json.NewEncoder(&buf)
		enc.SetEscapeHTML(false)
		enc.SetIndent("", "  ")
		if err := enc.Encode(struct {
			SearchID  *int64            `json:"search_id"`
			Feedback  *feedbackCommands `json:"feedback"`
			Query     string            `json:"query"`
			Mode      string            `json:"mode"`
			Hits      []jsonHit         `json:"hits"`
			Neighbors *[]jsonNeighbor   `json:"neighbors,omitempty"`
		}{searchID, feedback, result.Query, result.Mode, jhits, jsonNeighbors(nbrs)}); err != nil {
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
		exp, err := expandHits(ctx, lookups, result.Hits, expand)
		if err != nil {
			return err
		}
		keys := make([]string, len(rendered))
		for i := range rendered {
			rendered[i].Expanded = exp[i].Text
			keys[i] = exp[i].Key
		}
		if expand != render.ExpandNone {
			rendered = render.Dedupe(rendered, keys)
		}
		if text := render.Render(rendered, budget).Text; text != "" {
			fmt.Fprintln(stdout, text)
		}
		if text := render.FormatNeighbors(nbrs); text != "" {
			fmt.Fprintf(stdout, "\n%s\n", text)
		}
	}
	if !logged {
		fmt.Fprintln(stdout, "search_id=none (not logged)")
		return nil
	}
	fmt.Fprintf(stdout, "search_id=%d\n", result.SearchID)
	return nil
}

// jsonNeighbor is one --neighbors entry of the --json output: a link of the entry of hit Rank.
type jsonNeighbor struct {
	Rank      int    `json:"rank"`
	Path      string `json:"path"`
	Rel       string `json:"rel"`
	Direction string `json:"direction"`
}

// jsonNeighbors is the flat --json form of lines; nil (the field is left out) when --neighbors was
// not given, a pointer to an empty list when it was and no hit has a neighbour.
func jsonNeighbors(lines []render.NeighborLine) *[]jsonNeighbor {
	if lines == nil {
		return nil
	}
	out := []jsonNeighbor{}
	for _, l := range lines {
		for _, k := range l.Links {
			out = append(out, jsonNeighbor{l.Rank, k.Path, k.Rel, k.Direction})
		}
	}
	return &out
}

// neighborLines reads the depth-1 links of the primary store's hits and lays out the --neighbors
// section (render.Neighbors). A dangling link has no entry to show and is left out; an entry of
// another store is not looked up. It never touches the search log.
func neighborLines(ctx context.Context, st store.Relater, hits []search.Hit) ([]render.NeighborLine, error) {
	rh := make([]render.Hit, len(hits))
	links := map[string][]render.Link{}
	for i, h := range hits {
		rh[i] = render.Hit{Rank: h.Rank, Store: h.Store, Path: h.Path}
		if _, done := links[h.Path]; done || h.Store != "" {
			continue
		}
		rows, err := st.Related(ctx, h.Path, 1)
		if err != nil {
			if errs.KindOf(err) == errs.NotFound { // removed from the index since the search
				continue
			}
			return nil, err
		}
		ls := []render.Link{}
		for _, r := range rows {
			if r.Path != "" && r.Path != h.Path {
				ls = append(ls, render.Link{Rel: r.Rel, Direction: r.Direction, Path: r.Path})
			}
		}
		links[h.Path] = ls
	}
	out := render.Neighbors(rh, links)
	if out == nil {
		out = []render.NeighborLine{}
	}
	return out, nil
}

// jsonHit is a --json hit: the search's fields plus, with --expand, the hit's section or entry.
type jsonHit struct {
	search.Hit
	Expanded string `json:"expanded,omitempty"`
}

// warnSkipped warns about the extra stores whose search failed. An Internal error is a kb bug and
// goes through skipExtra (an incident, with its id in the warning); any other kind is one plain
// line. The reason is the error without the "store REF: " prefix, since the warning names the store.
func (d *deps) warnSkipped(cmd *cobra.Command, skipped []search.Skipped) {
	for _, s := range skipped {
		reason := errs.Wrap(s.Err, errs.KindOf(s.Err), "cli.search", "%s", skipReason(s))
		d.skipExtra(cmd, s.Part.Label, reason)
	}
}

// skipReason is why a store was skipped, without the "store REF: " wrap SearchStores adds (the
// warning already names the store): the wrapped cause's message when the error is that wrap.
func skipReason(s search.Skipped) string {
	var e *errs.Error
	if errors.As(s.Err, &e) && e.Op == search.OpSearchStores && e.Err != nil {
		return e.Err.Error()
	}
	return s.Err.Error()
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

// entryRows reads the entry rows of the hits, one EntriesByPath per store (by Store tag). A store
// with no lookup has no rows; any lookup failure is returned.
func entryRows(ctx context.Context, lookups map[string]entryLookup, hits []search.Hit) (map[string]map[string]store.EntryRow, error) {
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
	return rows, nil
}

// renderHits adds each hit's entry summary and updated date (search.Hit carries neither), read
// from the hit's own store (lookups by Store tag) with one EntriesByPath per store. An entry
// missing from its index (the file was removed since), or a store with no lookup, leaves those two
// fields empty: the hit still renders from what the search returned. Any other failure, a cancel or
// a database error, is returned: it is not "no such entry".
func renderHits(ctx context.Context, lookups map[string]entryLookup, hits []search.Hit) ([]render.Hit, error) {
	rows, err := entryRows(ctx, lookups, hits)
	if err != nil {
		return nil, err
	}
	out := make([]render.Hit, len(hits))
	for i, h := range hits {
		out[i] = render.Hit{Rank: h.Rank, Store: h.Store, Path: h.Path, Heading: h.Heading, Text: h.Text, Superseded: h.Superseded}
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
