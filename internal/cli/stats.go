// `kb stats`: run the named search-effectiveness queries (see internal/stats) against a copy of
// the search log loaded into an in-memory DuckDB. With no name and no --sql it lists the named
// queries; --sql is the escape hatch for anything the registry does not cover; --print-sql shows a
// named query's SQL without touching any database.
package cli

import (
	"encoding/json"
	"fmt"
	"io"
	"os"
	"regexp"
	"strconv"
	"strings"
	"text/tabwriter"
	"time"

	"github.com/spf13/cobra"

	"knowledge/kb/internal/stats"
)

// dayDurationRe matches the plan's "Nd" duration suffix: a whole number of days. Anything else
// (fractional days, a bare "d") falls through to time.ParseDuration, which rejects it.
var dayDurationRe = regexp.MustCompile(`^([0-9]+)d$`)

// newStatsCmd builds
// `kb stats [<name>] [--since d|date] [--caller name] [--json] [--print-sql] [--sql "..."]`.
func newStatsCmd(stdout, stderr io.Writer) *cobra.Command {
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
			return runStats(cmd, stdout, stderr, statsOpts{
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
	cmd.Flags().StringVar(&sqlText, "sql", "", "run ad-hoc DuckDB SQL against the loaded tables instead of a named query "+
		"(DuckDB widens sum() over an INTEGER column to HUGEINT, which prints here as a string; use "+
		"count(*) FILTER (WHERE ...) or CAST(sum(x) AS BIGINT) instead)")
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
	b.WriteString("--sql runs ad-hoc DuckDB SQL against the loaded tables; DuckDB widens sum() over an INTEGER\n")
	b.WriteString("column to HUGEINT, which kb stats prints as a string, so prefer count(*) FILTER (WHERE ...) or\n")
	b.WriteString("CAST(sum(x) AS BIGINT) in ad-hoc queries that need a whole-number sum.\n\n")
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
// listing), and — unless it is only listing or printing SQL — loads the search log and prints the
// result.
func runStats(cmd *cobra.Command, stdout, stderr io.Writer, opts statsOpts) error {
	if opts.sqlText != "" && opts.name != "" {
		return usageErr("kb stats: a query name and --sql are mutually exclusive")
	}
	if opts.sqlText != "" && opts.printSQL {
		return usageErr("kb stats: --print-sql and --sql are mutually exclusive")
	}

	since, err := parseSince(opts.since)
	if err != nil {
		return usageErr("kb stats: %s", err)
	}

	if opts.name == "" && opts.sqlText == "" {
		listQueries(stdout)
		return nil
	}

	queryName := "sql"
	sqlToRun := opts.sqlText
	if opts.sqlText == "" {
		q, ok := stats.Lookup(opts.name)
		if !ok {
			return usageErr("kb stats: unknown query %q (want one of: %s)", opts.name, strings.Join(queryNames(), ", "))
		}
		if opts.printSQL {
			fmt.Fprintln(stdout, q.SQL)
			return nil
		}
		queryName = q.Name
		sqlToRun = q.SQL
	}

	// Checked before opening: stats.Load only reads the file, so a missing one would otherwise
	// surface as an os.Stat error inside Load rather than this command's own reindex hint.
	dbPath := DBPath(Root())
	if _, err := os.Stat(dbPath); err != nil {
		return usageErr("kb stats: no search database at %s; run `kb reindex --all` first", dbPath)
	}

	db, err := stats.Load(cmd.Context(), dbPath, stats.Filter{Since: since, Caller: opts.caller})
	if err != nil {
		return usageErr("kb stats: %s", err)
	}
	defer db.Close()

	tbl, err := db.Query(cmd.Context(), sqlToRun)
	if err != nil {
		return usageErr("kb stats: %s", err)
	}

	if opts.asJSON {
		return printStatsJSON(stdout, queryName, tbl, db.SearchesLoaded(), since, opts.caller)
	}
	printStatsHuman(stdout, tbl, db.SearchesLoaded(), since, opts.caller)
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

// parseSince implements the plan's --since grammar: a Go duration, an "Nd" whole-number-of-days
// suffix (7d = 168h), or a YYYY-MM-DD date meaning UTC midnight. The bound itself is
// time.Now().UTC() minus the duration; a literal date is returned as-is. An empty string means no
// bound (the zero time.Time), matching stats.Filter's zero value.
func parseSince(s string) (time.Time, error) {
	if s == "" {
		return time.Time{}, nil
	}
	if t, err := time.Parse("2006-01-02", s); err == nil {
		return t, nil
	}
	if d, ok := parseSinceDuration(s); ok {
		return time.Now().UTC().Add(-d), nil
	}
	return time.Time{}, fmt.Errorf("invalid --since %q (want a duration like 7d or 24h, or a YYYY-MM-DD date)", s)
}

// parseSinceDuration recognises the "Nd" days suffix before falling back to time.ParseDuration, so
// "7d" means 168h rather than being rejected (time.ParseDuration has no day unit) or silently
// misread by any other rule.
func parseSinceDuration(s string) (time.Duration, bool) {
	if m := dayDurationRe.FindStringSubmatch(s); m != nil {
		days, err := strconv.Atoi(m[1])
		if err != nil {
			return 0, false
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

// statsJSON is the exact --json shape from the plan's CLI section.
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
		return usageErr("kb stats: marshal result: %v", err)
	}
	fmt.Fprintf(stdout, "%s\n", data)
	return nil
}

// statsJSONValue converts one Table cell to the value encoding/json should see: time.Time becomes
// an RFC3339 string (its default MarshalJSON uses RFC3339Nano, which is not what the plan asks
// for); everything else (nil, int64, float64, string, bool) already marshals correctly as-is.
func statsJSONValue(v any) any {
	if t, ok := v.(time.Time); ok {
		return t.UTC().Format(time.RFC3339)
	}
	return v
}
