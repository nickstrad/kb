// `kb stale`: the entries that need a look (PLAN 5.4). It reads the index read-only, so run kb
// reindex first, and it never writes. An entry is listed under every reason it has: no usable
// date, a date at least --days old, superseded by another entry, or holding a path link that no
// longer resolves. Exit 0 whether or not anything is listed.
package cli

import (
	"encoding/json"
	"fmt"
	"io"
	"slices"
	"strings"
	"text/tabwriter"
	"time"

	"github.com/spf13/cobra"

	"github.com/nickstrad/kb/internal/links"
	"github.com/nickstrad/kb/internal/store"
)

// The report's reasons, in print order. The strings are the JSON "reason" values.
const (
	staleNoDate     = "no_date"
	staleOld        = "old"
	staleSuperseded = "superseded"
	staleBroken     = "broken_link"
)

// staleReasons is the fixed group order of a report; every reason appears, even when empty.
var staleReasons = []string{staleNoDate, staleOld, staleSuperseded, staleBroken}

// staleItem is one entry under one reason. Only the fields of that reason are set.
type staleItem struct {
	Path         string      `json:"path"`
	Field        string      `json:"field,omitempty"`    // "verified" or "updated": where the date came from
	Date         *string     `json:"date,omitempty"`     // the date as written; absent when missing
	AgeDays      *int        `json:"age_days,omitempty"` // whole UTC days before the clock; absent when the date is unknown
	SupersededBy []string    `json:"superseded_by,omitempty"`
	Links        []staleLink `json:"broken_links,omitempty"`
}

// staleLink is one unresolved path link of an entry.
type staleLink struct {
	Rel    string `json:"rel"`
	Target string `json:"target"`
}

// staleGroup holds the entries flagged for one reason, in print order.
type staleGroup struct {
	Reason string      `json:"reason"`
	Items  []staleItem `json:"items"`
}

// staleReport is the whole listing. Days is the threshold used; Flagged counts entries in at
// least one group.
type staleReport struct {
	AsOf    string       `json:"as_of"`
	Days    int          `json:"days"`
	Entries int          `json:"entries"`
	Flagged int          `json:"flagged"`
	Groups  []staleGroup `json:"groups"`
}

// parseStaleDate reads a front-matter date: a calendar date (YYYY-MM-DD, midnight UTC) or an
// RFC 3339 timestamp. ok is false for a blank or any other value.
func parseStaleDate(s string) (t time.Time, ok bool) {
	s = strings.TrimSpace(s)
	if s == "" {
		return time.Time{}, false
	}
	if t, err := time.Parse("2006-01-02", s); err == nil {
		return t, true
	}
	if t, err := time.Parse(time.RFC3339, s); err == nil {
		return t, true
	}
	return time.Time{}, false
}

// staleAgeDays is the number of whole UTC calendar days from date to now. A date after now counts
// as 0 days old, so it is never stale unless the threshold is 0.
func staleAgeDays(date, now time.Time) int {
	days := (utcDay(now).Unix() - utcDay(date).Unix()) / secondsPerDay
	return int(max(days, 0))
}

// secondsPerDay is the length of a day in seconds; UTC midnights are multiples of it.
const secondsPerDay = 24 * 60 * 60

// utcDay is the UTC midnight of t's calendar day.
func utcDay(t time.Time) time.Time {
	y, m, d := t.UTC().Date()
	return time.Date(y, m, d, 0, 0, 0, 0, time.UTC)
}

// staleDate is the coalesce(verified, updated) date of an entry and the field it came from. A blank
// value counts as unset, as it does in the index (NULL). Both blank gives ("", "updated").
func staleDate(e store.EntryRow) (value, field string) {
	if strings.TrimSpace(e.Verified) != "" {
		return e.Verified, "verified"
	}
	return e.Updated, "updated"
}

// buildStale classifies the entries against the clock now and the threshold days. An entry is
// old when its date is at least days whole days before now, so days 0 lists every dated entry.
// entries must be sorted by path (Index.Entries is); edges come from Index.Edges.
func buildStale(now time.Time, days int, entries []store.EntryRow, edges []store.EdgeRow) staleReport {
	supersededBy := map[int64][]string{}
	brokenLinks := map[int64][]staleLink{}
	for _, g := range edges {
		switch {
		case g.Rel == links.RelSupersedes && g.DstEntryID != 0 && g.DstEntryID != g.SrcEntryID:
			// A self-supersede is ignored, as in the search demotion (Retriever.Superseded).
			supersededBy[g.DstEntryID] = append(supersededBy[g.DstEntryID], g.SrcPath)
		case g.Kind == "path" && g.DstEntryID == 0:
			brokenLinks[g.SrcEntryID] = append(brokenLinks[g.SrcEntryID], staleLink{Rel: g.Rel, Target: g.DstPath})
		}
	}

	groups := make(map[string][]staleItem, len(staleReasons))
	rep := staleReport{AsOf: now.UTC().Format(time.RFC3339), Days: days, Entries: len(entries)}
	for _, e := range entries {
		flagged := false
		add := func(reason string, item staleItem) {
			groups[reason] = append(groups[reason], item)
			flagged = true
		}

		value, field := staleDate(e)
		if t, ok := parseStaleDate(value); ok {
			if age := staleAgeDays(t, now); age >= days {
				add(staleOld, staleItem{Path: e.Path, Field: field, Date: &value, AgeDays: &age})
			}
		} else if strings.TrimSpace(value) == "" {
			add(staleNoDate, staleItem{Path: e.Path})
		} else {
			add(staleNoDate, staleItem{Path: e.Path, Field: field, Date: &value})
		}

		if src := supersededBy[e.ID]; len(src) > 0 {
			slices.Sort(src)
			add(staleSuperseded, staleItem{Path: e.Path, SupersededBy: src})
		}
		if broken := brokenLinks[e.ID]; len(broken) > 0 {
			add(staleBroken, staleItem{Path: e.Path, Links: broken})
		}
		if flagged {
			rep.Flagged++
		}
	}

	// Old entries oldest first, the rest by path (the input order).
	slices.SortStableFunc(groups[staleOld], func(a, b staleItem) int {
		return *b.AgeDays - *a.AgeDays
	})
	for _, reason := range staleReasons {
		items := groups[reason]
		if items == nil {
			items = []staleItem{}
		}
		rep.Groups = append(rep.Groups, staleGroup{Reason: reason, Items: items})
	}
	return rep
}

// newStaleCmd builds `kb stale [--days N] [--json]`.
func newStaleCmd(d *deps, stdout io.Writer) *cobra.Command {
	var (
		days   int
		asJSON bool
	)
	cmd := &cobra.Command{
		Use:   "stale",
		Short: "List entries with no date, an old date, a superseded status, or broken path links",
		Long: "Lists the entries that need a look, read from the index (run kb reindex first). An entry is\n" +
			"listed under each reason it has:\n" +
			"  no date        neither verified nor updated is set, or the date is not YYYY-MM-DD or RFC 3339\n" +
			"  old            the date (verified, else updated) is at least --days whole days before today (UTC)\n" +
			"  superseded     another entry lists it under supersedes\n" +
			"  broken link    a path link (related, supersedes or depends_on, or a Markdown link) names no entry\n" +
			"--days defaults to stale.days in the store config (180). --days 0 lists every entry.\n" +
			"Exit 0 whether or not anything is listed.",
		Example: "  kb stale\n" +
			"  kb stale --days 90\n" +
			"  kb stale --json",
		Args:          cobra.NoArgs,
		SilenceUsage:  true,
		SilenceErrors: true,
		RunE: func(cmd *cobra.Command, args []string) error {
			return runStale(cmd, d, stdout, days, cmd.Flags().Changed("days"), asJSON)
		},
	}
	cmd.Flags().IntVar(&days, "days", 0, "an entry dated this many whole days ago or more is stale (default: stale.days)")
	cmd.Flags().BoolVar(&asJSON, "json", false, "machine-readable output")
	return cmd
}

// runStale validates --days, reads the index and the clock, and prints the report. days is the
// flag value when set; otherwise the config's stale.days applies.
func runStale(cmd *cobra.Command, d *deps, stdout io.Writer, flagDays int, flagSet, asJSON bool) error {
	const op = "cli.stale"
	if flagSet && flagDays < 0 {
		return usageErr("invalid --days %d (want 0 or more)", flagDays)
	}
	ws, cfg, err := d.workspaceConfig(cmd)
	if err != nil {
		return err
	}
	days := cfg.Stale.Days
	if flagSet {
		days = flagDays
	}

	ctx := cmd.Context()
	st, err := d.openIndexReadOnly(ctx, ws.DBPath, op)
	if err != nil {
		return err
	}
	defer st.Close()
	entries, err := st.Entries(ctx)
	if err != nil {
		return err
	}
	edges, err := st.Edges(ctx)
	if err != nil {
		return err
	}
	rep := buildStale(d.clock(), days, entries, edges)

	if asJSON {
		enc := json.NewEncoder(stdout)
		enc.SetEscapeHTML(false)
		enc.SetIndent("", "  ")
		return enc.Encode(rep)
	}
	renderStale(stdout, rep)
	return nil
}

// renderStale prints the human form: a summary line, then one block per non-empty reason with a
// row per entry (per link for a broken link).
func renderStale(w io.Writer, r staleReport) {
	asOf, _, _ := strings.Cut(r.AsOf, "T")
	if r.Entries == 0 {
		fmt.Fprintln(w, "no entries")
		return
	}
	if r.Flagged == 0 {
		fmt.Fprintf(w, "no stale entries: %d %s, as of %s, threshold %d days\n",
			r.Entries, plural(r.Entries, "entry", "entries"), asOf, r.Days)
		return
	}
	fmt.Fprintf(w, "%d of %d %s %s a look, as of %s, threshold %d days\n",
		r.Flagged, r.Entries, plural(r.Entries, "entry", "entries"), plural(r.Flagged, "needs", "need"), asOf, r.Days)
	for _, g := range r.Groups {
		if len(g.Items) == 0 {
			continue
		}
		fmt.Fprintf(w, "\n%s: %d\n", staleHeading(g.Reason, r.Days), len(g.Items))
		tw := tabwriter.NewWriter(w, 0, 2, 2, ' ', 0)
		for _, it := range g.Items {
			for _, row := range staleRows(g.Reason, it) {
				fmt.Fprintf(tw, "  %s\t%s\n", it.Path, row)
			}
		}
		tw.Flush()
	}
}

// staleHeading is the title of one reason's block.
func staleHeading(reason string, days int) string {
	switch reason {
	case staleNoDate:
		return "No usable date"
	case staleOld:
		return fmt.Sprintf("At least %d %s old", days, plural(days, "day", "days"))
	case staleSuperseded:
		return "Superseded"
	case staleBroken:
		return "Broken path links"
	}
	return reason
}

// staleRows is the detail text of one item: one string per printed row.
func staleRows(reason string, it staleItem) []string {
	switch reason {
	case staleNoDate:
		if it.Date == nil {
			return []string{"no verified or updated date"}
		}
		return []string{fmt.Sprintf("%s %q is not a date", it.Field, *it.Date)}
	case staleOld:
		return []string{fmt.Sprintf("%s %s (%d %s old)", it.Field, *it.Date, *it.AgeDays, plural(*it.AgeDays, "day", "days"))}
	case staleSuperseded:
		return []string{"superseded by " + strings.Join(it.SupersededBy, ", ")}
	case staleBroken:
		rows := make([]string, 0, len(it.Links))
		for _, l := range it.Links {
			rows = append(rows, fmt.Sprintf("%s -> %s", l.Rel, l.Target))
		}
		return rows
	}
	return nil
}
