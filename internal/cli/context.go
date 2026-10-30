// `kb context "<task>"`: the knowledge a task needs, as Markdown ready to paste into a prompt. It
// is a logged hybrid search (k 8) whose hits are expanded to their section and cut to a token
// budget, so an agent gets one bounded block instead of a list to read.
package cli

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"strings"

	"github.com/spf13/cobra"

	"github.com/nickstrad/kb/internal/errs"
	"github.com/nickstrad/kb/internal/render"
	"github.com/nickstrad/kb/internal/search"
)

// contextK is how many hits kb context asks for: fixed, so the budget, not a flag, sets the size.
const contextK = 8

// The levels a hit is shown at (render's levels, which are unexported): 1 heading, 2 with the
// summary, 3 with the text.
const (
	levelSummary = 2
	levelFull    = 3
)

// newContextCmd builds
// `kb context "<task>" [--budget N] [--stores …] [--expand section|entry] [--json]`.
func newContextCmd(d *deps, stdout, stderr io.Writer) *cobra.Command {
	var (
		budget int
		stores string
		expand string
		asJSON bool
	)
	cmd := &cobra.Command{
		Use:   "context <task>",
		Short: "Print the knowledge a task needs as Markdown, within a token budget",
		Long: "Runs one logged hybrid search for the task (the same search as kb search, with its\n" +
			"own result count) and prints the hits as Markdown: a heading per hit with its source,\n" +
			"path › heading, and its updated date, then the text of the section it sits in\n" +
			"(--expand entry for the whole entry, none for the matching chunk). The output,\n" +
			"heading and footer included, fits --budget tokens (default [output] context_budget,\n" +
			"2000; 0 = unlimited, but only as --budget 0: the config needs 1 or more); hits that do not fit are left out with a note.\n\n" +
			"The footer names the search (search N) and the kb show and kb feedback commands for\n" +
			"it. --stores works as in kb search. --json prints the search id, the feedback commands\n" +
			"and every hit with the level it was shown at (0 = left out by the budget).",
		Args:          cobra.MinimumNArgs(1),
		SilenceUsage:  true,
		SilenceErrors: true,
		RunE: func(cmd *cobra.Command, args []string) error {
			return runContext(cmd, d, stdout, stderr, contextOpts{
				task:   strings.Join(args, " "),
				budget: budget,
				stores: stores,
				expand: expand,
				asJSON: asJSON,
				fanOut: cmd.Flags().Changed("stores"),
			})
		},
	}
	cmd.Flags().IntVar(&budget, "budget", 0, "cap the whole output at about N tokens (ceil(chars/4)); 0 = unlimited, only as a flag (config [output] context_budget sets the default: 2000, at least 1)")
	cmd.Flags().StringVar(&stores, "stores", "", "also search these stores, comma-separated ('' = none, all = every store here; config [search] stores sets the default)")
	cmd.Flags().StringVar(&expand, "expand", render.ExpandSection, "show each hit's whole section or entry: none, section or entry")
	cmd.Flags().BoolVar(&asJSON, "json", false, "machine-readable output")
	return cmd
}

// contextOpts is the parsed, not-yet-validated form of kb context's flags.
type contextOpts struct {
	task   string
	budget int
	stores string
	expand string
	asJSON bool
	fanOut bool // --stores was given (else [search] stores applies)
}

// runContext validates the task and flags, runs the search and prints the Markdown (or --json).
// It follows runSearch's steps: the same embedder, extra stores, logging and fallback exit code.
//
// The budget counts the whole output. render.Render sizes the hits on their own text, so the
// frame (heading and footer) is added afterwards and the render budget shrinks by what it
// overshoots; see fitContext.
//
// Exit codes: 1 for a bad task, flag or budget, a missing index or an embed_meta mismatch; 2 when
// the embedder was unavailable and the search fell back to FTS (output is printed, then the
// warning); 0 otherwise.
func runContext(cmd *cobra.Command, d *deps, stdout, stderr io.Writer, opts contextOpts) error {
	if strings.TrimSpace(opts.task) == "" {
		return usageErr("kb context needs a task: kb context \"<task>\"")
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

	ss, err := d.openStoresSearch(cmd, stderr, "cli.context", search.ModeHybrid, opts.fanOut, opts.stores)
	if err != nil {
		return err
	}
	defer ss.close()
	cfg := ss.cfg
	req := search.Request{
		Query:  opts.task,
		Mode:   ss.mode,
		K:      search.ClampK(contextK),
		Caller: callerFor("", cfg),
	}
	res, err := ss.run(cmd, d, stderr, req)
	if err != nil {
		return err
	}
	out, logErr, lookups := res.out, res.logErr, res.lookups
	logged := logErr == nil

	budget := opts.budget
	if !cmd.Flags().Changed("budget") {
		budget = cfg.Output.ContextBudget
	}
	if err := printContext(ctx, stdout, lookups, out.Result, opts.asJSON, logged, budget, opts.expand); err != nil {
		return err
	}
	if !logged {
		fmt.Fprintf(stderr, "warning: search not logged: %s\n", logErr)
	}
	noteSpell(stderr, out.Spell)

	if out.Mode == search.ModeFTSFallback {
		fmt.Fprintf(stderr, "warning: embedder unavailable (%s); falling back to --mode fts\n", out.FallbackErr)
		return exitSilently(ExitEmbedder)
	}
	return nil
}

// printContext looks up each hit's summary, date and expansion, dedupes them the way kb search
// does, fits them to budget and prints the Markdown, or the --json form.
func printContext(ctx context.Context, stdout io.Writer, lookups map[string]entryLookup, result *search.Result, asJSON, logged bool, budget int, expand string) error {
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

	var searchID *int64
	var id int64
	if logged {
		searchID = &result.SearchID
		id = result.SearchID
	}
	heading := fmt.Sprintf("## kb context (search %d)", id)
	if !logged {
		heading = "## kb context (search not logged)"
	}
	text, levels := fitContext(rendered, budget, func(body string) string {
		return contextFrame(heading, body, contextFooter(id, logged, rendered))
	})

	if asJSON {
		var feedback *feedbackCommands
		if logged {
			feedback = newFeedbackCommands(result.SearchID, len(result.Hits))
		}
		jhits := make([]contextHit, len(rendered))
		for i, h := range rendered {
			jhits[i] = contextHit{
				Rank: h.Rank, Store: h.Store, Path: h.Path, Heading: h.Heading, Updated: h.Updated,
				Summary: h.Summary, Text: h.Text, Expanded: h.Expanded, Level: levels[i], Also: h.Also,
			}
		}
		var buf strings.Builder
		enc := json.NewEncoder(&buf)
		enc.SetEscapeHTML(false)
		enc.SetIndent("", "  ")
		if err := enc.Encode(contextJSON{
			SearchID: searchID,
			Feedback: feedback,
			Query:    result.Query,
			Mode:     result.Mode,
			Budget:   budget,
			Tokens:   render.EstimateTokens(text),
			Hits:     jhits,
		}); err != nil {
			return errs.Wrap(err, errs.Internal, "cli.context", "encode result: %v", err)
		}
		fmt.Fprint(stdout, buf.String())
		return nil
	}
	fmt.Fprintln(stdout, text)
	return nil
}

// contextJSON is the --json output of kb context. Hits lists every hit after dedupe; level 0
// marks one the budget left out.
type contextJSON struct {
	SearchID *int64            `json:"search_id"`
	Feedback *feedbackCommands `json:"feedback"`
	Query    string            `json:"query"`
	Mode     string            `json:"mode"`
	Budget   int               `json:"budget"`
	Tokens   int               `json:"tokens"`
	Hits     []contextHit      `json:"hits"`
}

// contextHit is one hit in the --json output.
type contextHit struct {
	Rank     int    `json:"rank"`
	Store    string `json:"store,omitempty"`
	Path     string `json:"path"`
	Heading  string `json:"heading"`
	Updated  string `json:"updated"`
	Summary  string `json:"summary"`
	Text     string `json:"text"`
	Expanded string `json:"expanded,omitempty"`
	Level    int    `json:"level"`
	Also     []int  `json:"also,omitempty"`
}

// fitContext chooses each hit's level so the whole output fits budget (0 = unlimited) and returns
// that output with the levels (per hit: 0 left out, 1 heading, 2 with summary, 3 with text).
// frame wraps a body in the heading and footer. render.Render sizes the body alone, so when the
// frame overshoots the budget, the render budget drops by the overshoot and Render runs again.
// Each round lowers the render budget by at least one token, so the loop ends. At budget 1 only
// the frame and the "raise --budget" note are left; a budget below that size can be exceeded.
func fitContext(hits []render.Hit, budget int, frame func(body string) string) (string, []int) {
	if budget == 0 {
		res := render.Render(hits, 0)
		return frame(contextBody(hits, res.Levels)), res.Levels
	}
	b := budget
	for {
		res := render.Render(hits, b)
		text := frame(contextBody(hits, res.Levels))
		over := render.EstimateTokens(text) - budget
		if over <= 0 || b == 1 {
			return text, res.Levels
		}
		b -= over
		if b < 1 {
			b = 1
		}
	}
}

// contextBody is the hits part of the output: a block per shown hit, then the tail when hits were
// left out or condensed (the wording render uses). With no hits it says so.
func contextBody(hits []render.Hit, levels []int) string {
	if len(hits) == 0 {
		return "No results."
	}
	var blocks []string
	hidden, condensed := 0, 0
	for i, h := range hits {
		lv := levels[i]
		if lv == 0 {
			hidden++
			continue
		}
		if lv < levelFull {
			condensed++
		}
		blocks = append(blocks, contextBlock(h, lv))
	}
	if hidden > 0 || condensed > 0 {
		var parts []string
		if hidden > 0 {
			parts = append(parts, fmt.Sprintf("+%d hits not shown", hidden))
		}
		if condensed > 0 {
			parts = append(parts, fmt.Sprintf("%s%d condensed", plusIf(len(parts) == 0), condensed))
		}
		blocks = append(blocks, "("+strings.Join(parts, ", ")+"; raise --budget)")
	}
	return strings.Join(blocks, "\n\n")
}

// plusIf is "+" when first, for the "+N hits not shown" tail that starts the note.
func plusIf(first bool) string {
	if first {
		return "+"
	}
	return ""
}

// contextBlock is one hit as Markdown: "### [rank] source · updated D", then at level 2 the
// summary and at level 3 the text (the whole section or entry when expanded).
func contextBlock(h render.Hit, level int) string {
	src := h.Path
	if h.Store != "" {
		src = h.Store + ":" + h.Path
	}
	if h.Heading != "" {
		src += " › " + h.Heading
	}
	s := fmt.Sprintf("### [%d] %s", h.Rank, src)
	if h.Updated != "" {
		s += " · updated " + h.Updated
	}
	if len(h.Also) > 0 {
		ranks := make([]string, len(h.Also))
		for i, r := range h.Also {
			ranks[i] = fmt.Sprintf("[%d]", r)
		}
		s += " · also " + strings.Join(ranks, " ")
	}
	if level >= levelSummary && h.Summary != "" {
		s += "\n\n" + render.Trim(h.Summary, render.SummaryLimit)
	}
	if level >= levelFull {
		text := h.Expanded
		if text == "" {
			text = render.Trim(h.Text, render.TextLimit)
		}
		if text != "" {
			s += "\n\n" + text
		}
	}
	return s
}

// contextFooter names the search and the commands that follow it: kb show for the best hit, and
// kb feedback (judging the search is optional, as in kb search).
func contextFooter(id int64, logged bool, hits []render.Hit) string {
	if !logged {
		return "search not logged"
	}
	if len(hits) == 0 {
		return fmt.Sprintf("search %d · kb feedback %d --none", id, id)
	}
	ref := hits[0].Path
	if hits[0].Store != "" {
		ref = hits[0].Store + ":" + ref
	}
	return fmt.Sprintf("More: kb show %s\nsearch %d · kb feedback %d RANK --useful or --not-useful", ref, id, id)
}

// contextFrame joins the heading, body and footer with blank lines.
func contextFrame(heading, body, footer string) string {
	return strings.Join([]string{heading, body, footer}, "\n\n")
}
