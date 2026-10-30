// `kb stats`: run the named search-effectiveness queries (see internal/stats) read-only against
// the search log. With no name and no --sql it lists the named queries; --sql runs ad-hoc SQLite
// SQL; --print-sql shows a named query's SQL without opening any database.
package cli

import (
	"encoding/json"
	"fmt"
	"io"
	"math"
	"os"
	"regexp"
	"strconv"
	"strings"
	"text/tabwriter"
	"time"

	"github.com/spf13/cobra"

	"github.com/nickstrad/kb/internal/errs"
	"github.com/nickstrad/kb/internal/stats"
)

// dayDurationRe matches the "Nd" duration suffix: a whole number of days. Anything else
// (fractional days, a bare "d") falls through to time.ParseDuration, which rejects it.
var dayDurationRe = regexp.MustCompile(`^([0-9]+)d$`)

// maxSinceDays is the largest "Nd" that fits in a time.Duration (about 292 years); one more day
// would overflow into a negative duration and silently filter out every search.
const maxSinceDays = int(math.MaxInt64 / int64(24*time.Hour))

// newStatsCmd builds
// `kb stats [<name>] [--since d|date] [--caller name] [--json] [--print-sql] [--sql "..."]`.
func newStatsCmd(d *deps, stdout, stderr io.Writer) *cobra.Command {
	var (
		since    string
		caller   string
		asJSON   bool
		printSQL bool
		sqlText  string
	)
	cmd := &cobra.Command{
		Use:   "stats [name]",
		Short: "Search-effectiveness analytics from the search log",
		Long:  statsLongHelp(),
		Args:  cobra.MaximumNArgs(1),
		Example: "  kb stats\n" +
			"  kb stats overview\n" +
			"  kb stats gaps --since 7d --json\n" +
			"  kb stats overview --print-sql\n" +
			"  kb stats --sql \"select mode, count(*) from searches group by mode\"",
		SilenceUsage:  true,
		SilenceErrors: true,
		RunE: func(cmd *cobra.Command, args []string) error {
			name := ""
			if len(args) == 1 {
				name = args[0]
			}
			return runStats(cmd, d, stdout, stderr, statsOpts{
				name:     name,
				since:    since,
				caller:   caller,
				asJSON:   asJSON,
				printSQL: printSQL,
				sqlText:  sqlText,
			})
		},
	}
	cmd.Flags().StringVar(&since, "since", "", "only searches at/after this bound: a Go duration (24h, 30m), Nd for N days (7d = 168h), or a YYYY-MM-DD date (UTC midnight)")
	cmd.Flags().StringVar(&caller, "caller", "", "only searches logged by this caller")
	cmd.Flags().BoolVar(&asJSON, "json", false, "machine-readable output")
	cmd.Flags().BoolVar(&printSQL, "print-sql", false, "print the named query's SQL and exit; opens no database")
	cmd.Flags().StringVar(&sqlText, "sql", "", "run ad-hoc read-only SQLite SQL against the log's tables "+
		"instead of a named query (--since and --caller do not apply; filter in the SQL)")
	return cmd
}

// statsLongHelp builds the long --help text from stats.Queries so the list of queries and their
// help paragraphs can never drift from the registry.
func statsLongHelp() string {
	var b strings.Builder
	b.WriteString("A search is a hit when at least one of its rated results was marked useful. Rates use only\n")
	b.WriteString("judged searches (searches with at least one feedback row) as the denominator and show\n")
	b.WriteString("coverage beside every rate, so an unjudged search is unknown, not a failure. A whole-search\n")
	b.WriteString("verdict (kb feedback <id> --none, stored as a rank-0 feedback row) makes a search judged and\n")
	b.WriteString("never a hit; it is the only way a zero-result search can be judged.\n\n")
	b.WriteString("--sql runs ad-hoc SQLite SQL, read-only, against the real tables (searches, search_results,\n")
	b.WriteString("search_candidates, search_feedback, entries). --since and --caller do not apply to it.\n")
	b.WriteString("--print-sql shows a named query's SQL; ?1 is the --since bound and ?2 the caller.\n\n")
	b.WriteString("Named queries:\n")
	for _, q := range stats.Queries {
		b.WriteString("\n")
		b.WriteString(q.Name)
		b.WriteString(" — ")
		b.WriteString(q.Summary)
		b.WriteString("\n")
		b.WriteString(q.Help)
		b.WriteString("\n")
	}
	return strings.TrimRight(b.String(), "\n")
}

// statsOpts is the parsed, not-yet-validated form of the command's flags.
type statsOpts struct {
	name     string
	since    string
	caller   string
	asJSON   bool
	printSQL bool
	sqlText  string
}

// runStats validates the flags, resolves what to run (a named query, ad-hoc SQL, or just the
// listing) and, unless it is only listing or printing SQL, runs it and prints the result.
func runStats(cmd *cobra.Command, d *deps, stdout, stderr io.Writer, opts statsOpts) error {
	if opts.sqlText != "" && opts.name != "" {
		return usageErr("a query name and --sql are mutually exclusive")
	}
	if opts.sqlText != "" && opts.printSQL {
		return usageErr("--print-sql and --sql are mutually exclusive")
	}
	if opts.sqlText != "" && (opts.since != "" || opts.caller != "") {
		return usageErr("--since and --caller do not apply to --sql; filter in the SQL instead")
	}

	since, err := parseSince(opts.since, d.clock())
	if err != nil {
		return usageErr("%s", err)
	}

	if opts.name == "" && opts.sqlText == "" {
		listQueries(stdout)
		return nil
	}

	var q stats.Query
	if opts.sqlText == "" {
		var ok bool
		q, ok = stats.Lookup(opts.name)
		if !ok {
			return usageErr("unknown query %q (want one of: %s)", opts.name, strings.Join(queryNames(), ", "))
		}
		if opts.printSQL {
			fmt.Fprintln(stdout, q.SQL)
			return nil
		}
	}

	// Checked here so a missing file gets this command's reindex hint.
	ws, err := d.workspace(cmd)
	if err != nil {
		return err
	}
	dbPath := ws.DBPath
	if _, err := os.Stat(dbPath); err != nil {
		return errs.Wrap(err, errs.NotFound, "cli.stats", "no search database at %s; run `kb reindex --all` first", dbPath)
	}

	// Read-only: stats never creates, migrates or writes the database.
	ctx := cmd.Context()
	st, err := d.openStore(ctx, dbPath, true)
	if err != nil {
		return err
	}
	defer st.Close()
	db := stats.New(st)

	filter := stats.Filter{Since: since, Caller: opts.caller}
	queryName := "sql"
	var tbl *stats.Table
	if opts.sqlText != "" {
		tbl, err = db.SQL(ctx, opts.sqlText)
	} else {
		queryName = q.Name
		tbl, err = db.Run(ctx, q, filter)
	}
	if err != nil {
		return err
	}
	searches, err := db.SearchesMatching(ctx, filter)
	if err != nil {
		return err
	}

	if opts.asJSON {
		return printStatsJSON(stdout, queryName, tbl, searches, since, opts.caller)
	}
	printStatsHuman(stdout, tbl, searches, since, opts.caller)
	return nil
}

// queryNames returns the registry's names in display order, for the "unknown query" error.
func queryNames() []string {
	names := make([]string, len(stats.Queries))
	for i, q := range stats.Queries {
		names[i] = q.Name
	}
	return names
}

// listQueries prints one line per named query: name, then summary, tab-aligned.
func listQueries(stdout io.Writer) {
	tw := tabwriter.NewWriter(stdout, 0, 4, 2, ' ', 0)
	for _, q := range stats.Queries {
		fmt.Fprintf(tw, "%s\t%s\n", q.Name, q.Summary)
	}
	tw.Flush()
}

// parseSince implements the --since grammar: a Go duration, an "Nd" whole-number-of-days
// suffix (7d = 168h), or a YYYY-MM-DD date meaning UTC midnight. The bound itself is now (in
// UTC) minus the duration; a literal date is returned as-is. An empty string means no bound (the
// zero time.Time), matching stats.Filter's zero value.
func parseSince(s string, now time.Time) (time.Time, error) {
	if s == "" {
		return time.Time{}, nil
	}
	if t, err := time.Parse("2006-01-02", s); err == nil {
		return t, nil
	}
	if d, ok := parseSinceDuration(s); ok {
		if d < 0 {
			return time.Time{}, fmt.Errorf("invalid --since %q: a duration must not be negative", s)
		}
		return now.UTC().Add(-d), nil
	}
	return time.Time{}, fmt.Errorf("invalid --since %q (want a duration like 7d or 24h, or a YYYY-MM-DD date)", s)
}

// parseSinceDuration recognises the "Nd" days suffix before falling back to time.ParseDuration, so
// "7d" means 168h rather than being rejected (time.ParseDuration has no day unit) or silently
// misread by any other rule.
func parseSinceDuration(s string) (time.Duration, bool) {
	if m := dayDurationRe.FindStringSubmatch(s); m != nil {
		days, err := strconv.Atoi(m[1])
		if err != nil || days > maxSinceDays {
			return 0, false // too many days for a time.Duration; rejected rather than wrapped
		}
		return time.Duration(days) * 24 * time.Hour, true
	}
	if d, err := time.ParseDuration(s); err == nil {
		return d, true
	}
	return 0, false
}

// printStatsHuman renders a Table as a tabwriter table (NULL as "-", floats in their shortest
// exact decimal form, timestamps as RFC3339, bools as true/false) followed by a blank line and the
// footer describing how many searches were loaded and under which filters.
func printStatsHuman(stdout io.Writer, tbl *stats.Table, searchesLoaded int64, since time.Time, caller string) {
	tw := tabwriter.NewWriter(stdout, 0, 4, 2, ' ', 0)
	fmt.Fprintln(tw, strings.Join(tbl.Columns, "\t"))
	for _, row := range tbl.Rows {
		cells := make([]string, len(row))
		for i, v := range row {
			cells[i] = statsCellText(v)
		}
		fmt.Fprintln(tw, strings.Join(cells, "\t"))
	}
	tw.Flush()
	fmt.Fprintln(stdout)
	fmt.Fprintf(stdout, "searches loaded: %d (since %s, caller %s)\n",
		searchesLoaded, statsBoundText(since), statsNameText(caller))
}

// statsCellText renders one Table cell for the human table per the documented rules: NULL as "-",
// float64 in its shortest exact decimal form (so a rate the SQL already rounded prints as 80 or
// 66.7, never 80.000), time.Time as RFC3339, bool as true/false, everything else via fmt.Sprint
// (int64 and string need nothing more).
func statsCellText(v any) string {
	switch x := v.(type) {
	case nil:
		return "-"
	case float64:
		return strconv.FormatFloat(x, 'f', -1, 64)
	case time.Time:
		return x.UTC().Format(time.RFC3339)
	case bool:
		if x {
			return "true"
		}
		return "false"
	default:
		return fmt.Sprint(x)
	}
}

// statsBoundText renders the footer's --since bound: "all" when unset, else RFC3339.
func statsBoundText(since time.Time) string {
	if since.IsZero() {
		return "all"
	}
	return since.UTC().Format(time.RFC3339)
}

// statsNameText renders the footer's --caller value: "all" when unset.
func statsNameText(caller string) string {
	if caller == "" {
		return "all"
	}
	return caller
}

// statsJSON is the --json shape; it is a stable contract.
type statsJSON struct {
	Query          string           `json:"query"`
	Columns        []string         `json:"columns"`
	Rows           []map[string]any `json:"rows"`
	SearchesLoaded int64            `json:"searches_loaded"`
	Since          *string          `json:"since"`
	Caller         *string          `json:"caller"`
}

// printStatsJSON marshals a Table into the documented --json shape: rows as objects keyed by
// column name, time.Time rendered as RFC3339 text, NULL as JSON null.
func printStatsJSON(stdout io.Writer, queryName string, tbl *stats.Table, searchesLoaded int64, since time.Time, caller string) error {
	rows := make([]map[string]any, len(tbl.Rows))
	for i, row := range tbl.Rows {
		m := make(map[string]any, len(tbl.Columns))
		for j, col := range tbl.Columns {
			m[col] = statsJSONValue(row[j])
		}
		rows[i] = m
	}

	out := statsJSON{
		Query:          queryName,
		Columns:        tbl.Columns,
		Rows:           rows,
		SearchesLoaded: searchesLoaded,
	}
	if !since.IsZero() {
		s := since.UTC().Format(time.RFC3339)
		out.Since = &s
	}
	if caller != "" {
		out.Caller = &caller
	}

	data, err := json.MarshalIndent(out, "", "  ")
	if err != nil {
		// out is built from strings/ints/floats/bools/nils, so this should not happen; a CLI
		// still reports it as a failure rather than crashing.
		return errs.Wrap(err, errs.Internal, "cli.stats", "marshal result: %v", err)
	}
	fmt.Fprintf(stdout, "%s\n", data)
	return nil
}

// statsJSONValue converts one Table cell for encoding/json: time.Time becomes RFC3339 text (its
// default MarshalJSON uses RFC3339Nano); everything else marshals as-is.
func statsJSONValue(v any) any {
	if t, ok := v.(time.Time); ok {
		return t.UTC().Format(time.RFC3339)
	}
	return v
}
