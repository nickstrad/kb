package cli

import (
	"context"
	"encoding/json"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/nickstrad/kb/internal/errs"
	"github.com/nickstrad/kb/internal/store"
)

// staleNow is the pinned clock of the stale tests: noon UTC on 2026-10-30.
var staleNow = time.Date(2026, 10, 30, 12, 0, 0, 0, time.UTC)

// TestParseStaleDate: a calendar date and an RFC 3339 timestamp parse; blank and anything else is
// unknown (ok false), so the entry is listed under "no date".
func TestParseStaleDate(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		in   string
		want time.Time // zero when unknown
	}{
		{"2026-10-30", time.Date(2026, 10, 30, 0, 0, 0, 0, time.UTC)},
		{" 2026-10-30 ", time.Date(2026, 10, 30, 0, 0, 0, 0, time.UTC)},
		{"2026-10-30T09:00:00Z", time.Date(2026, 10, 30, 9, 0, 0, 0, time.UTC)},
		{"2026-10-30T09:00:00+02:00", time.Date(2026, 10, 30, 7, 0, 0, 0, time.UTC)},
		{"", time.Time{}},
		{"   ", time.Time{}},
		{"soon", time.Time{}},
		{"2026-13-01", time.Time{}},
		{"30/10/2026", time.Time{}},
		{"2026-10-30T09:00", time.Time{}}, // RFC 3339 needs a zone
		{"2026-10", time.Time{}},
	} {
		got, ok := parseStaleDate(tc.in)
		if wantOK := !tc.want.IsZero(); ok != wantOK || (ok && !got.Equal(tc.want)) {
			t.Errorf("parseStaleDate(%q) = %v, %v; want %v (ok %v)", tc.in, got, ok, tc.want, wantOK)
		}
	}
}

// TestBuildStaleBoundary: an entry is old when its date is at least days whole UTC days before
// now. Exactly N days old is stale, N-1 is not, N+1 is. Verified wins over updated (when set), and
// a future date counts as 0 days old.
func TestBuildStaleBoundary(t *testing.T) {
	t.Parallel()
	const days = 180
	ago := func(n int) string { return staleNow.AddDate(0, 0, -n).Format("2006-01-02") }
	for _, tc := range []struct {
		name     string
		updated  string
		verified string
		days     int
		old      bool
		age      int
	}{
		{"N-1 days", ago(days - 1), "", days, false, days - 1},
		{"exactly N days", ago(days), "", days, true, days},
		{"N+1 days", ago(days + 1), "", days, true, days + 1},
		{"days 0 lists today", staleNow.Format("2006-01-02"), "", 0, true, 0},
		{"future date is 0 days", "2027-01-01", "", 1, false, 0},
		{"verified recent beats old updated", ago(400), ago(1), days, false, 1},
		{"blank verified falls back to updated", ago(400), "  ", days, true, 400},
		{"RFC 3339 exactly N days", staleNow.AddDate(0, 0, -days).Format(time.RFC3339), "", days, true, days},
	} {
		t.Run(tc.name, func(t *testing.T) {
			e := store.EntryRow{ID: 1, Path: "a.md", Updated: tc.updated, Verified: tc.verified}
			rep := buildStale(staleNow, tc.days, []store.EntryRow{e}, nil)
			old := groupItems(rep, staleOld)
			if got := len(old) == 1; got != tc.old {
				t.Fatalf("old = %v, want %v (report %+v)", got, tc.old, rep)
			}
			if tc.old && *old[0].AgeDays != tc.age {
				t.Errorf("age = %d, want %d", *old[0].AgeDays, tc.age)
			}
		})
	}
}

// TestBuildStaleReasons: each reason lists the right entries, an entry can sit in several groups,
// unresolved wikilinks, resolved path links and a self-supersede are not listed, and the group
// order is fixed.
func TestBuildStaleReasons(t *testing.T) {
	t.Parallel()
	entries := []store.EntryRow{
		{ID: 1, Path: "a/blank.md"},                    // no date at all
		{ID: 2, Path: "b/garbage.md", Updated: "soon"}, // unparseable
		{ID: 3, Path: "c/old.md", Updated: "2020-01-01"},
		{ID: 4, Path: "d/fresh.md", Updated: "2026-10-01"},
		{ID: 5, Path: "e/superseded.md", Updated: "2026-10-01"},
		{ID: 6, Path: "f/links.md", Updated: "2026-10-01"},
	}
	edges := []store.EdgeRow{
		{SrcEntryID: 4, SrcPath: "d/fresh.md", DstPath: "e/superseded.md", Rel: "supersedes", Kind: "path", DstEntryID: 5, DstEntryPath: "e/superseded.md"},
		// A self-supersede is ignored, as in the search demotion.
		{SrcEntryID: 4, SrcPath: "d/fresh.md", DstPath: "d/fresh.md", Rel: "supersedes", Kind: "path", DstEntryID: 4, DstEntryPath: "d/fresh.md"},
		{SrcEntryID: 6, SrcPath: "f/links.md", DstPath: "gone.md", Rel: "related", Kind: "path"},
		// A supersedes link to nothing is a broken link, not a supersede.
		{SrcEntryID: 6, SrcPath: "f/links.md", DstPath: "older.md", Rel: "supersedes", Kind: "path"},
		{SrcEntryID: 6, SrcPath: "f/links.md", DstPath: "gone-too", Rel: "related", Kind: "wikilink"},
		{SrcEntryID: 6, SrcPath: "f/links.md", DstPath: "d/fresh.md", Rel: "related", Kind: "path", DstEntryID: 4, DstEntryPath: "d/fresh.md"},
	}
	rep := buildStale(staleNow, 180, entries, edges)

	if got := reasonOrder(rep); strings.Join(got, ",") != "no_date,old,superseded,broken_link" {
		t.Errorf("group order = %v", got)
	}
	want := map[string][]string{
		staleNoDate:     {"a/blank.md", "b/garbage.md"},
		staleOld:        {"c/old.md"},
		staleSuperseded: {"e/superseded.md"},
		staleBroken:     {"f/links.md"},
	}
	for reason, paths := range want {
		if got := itemPaths(groupItems(rep, reason)); strings.Join(got, ",") != strings.Join(paths, ",") {
			t.Errorf("%s = %v, want %v", reason, got, paths)
		}
	}
	if got := groupItems(rep, staleSuperseded)[0].SupersededBy; len(got) != 1 || got[0] != "d/fresh.md" {
		t.Errorf("superseded_by = %v", got)
	}
	if links := groupItems(rep, staleBroken)[0].Links; len(links) != 2 || links[0] != (staleLink{Rel: "related", Target: "gone.md"}) ||
		links[1] != (staleLink{Rel: "supersedes", Target: "older.md"}) {
		t.Errorf("broken links = %+v, want related gone.md and supersedes older.md", links)
	}
	// A blank date has no date to show; an unparseable one is shown as written, with its field.
	if nd := groupItems(rep, staleNoDate); nd[0].Date != nil || nd[0].Field != "" || nd[1].Date == nil || *nd[1].Date != "soon" || nd[1].Field != "updated" {
		t.Errorf("no_date items = %+v, %+v; want blank without a date, garbage with updated \"soon\"", nd[0], nd[1])
	}
	if rep.Entries != 6 || rep.Flagged != 5 {
		t.Errorf("entries %d flagged %d, want 6 and 5 (only d/fresh.md is clean)", rep.Entries, rep.Flagged)
	}
}

// TestBuildStaleOrder: old entries list oldest first, then by path; the other groups by path.
func TestBuildStaleOrder(t *testing.T) {
	t.Parallel()
	entries := []store.EntryRow{
		{ID: 1, Path: "a.md", Updated: "2026-01-01"},
		{ID: 2, Path: "b.md", Updated: "2025-01-01"},
		{ID: 3, Path: "c.md", Updated: "2026-01-01"},
	}
	rep := buildStale(staleNow, 30, entries, nil)
	if got := strings.Join(itemPaths(groupItems(rep, staleOld)), ","); got != "b.md,a.md,c.md" {
		t.Errorf("old order = %s, want b.md,a.md,c.md", got)
	}
	// An empty group is an empty list, so --json prints [] and never null.
	for _, g := range rep.Groups {
		if g.Items == nil {
			t.Errorf("group %s: Items is nil, want an empty list", g.Reason)
		}
	}
}

// TestBuildStaleDaysZero: --days 0 lists every entry (the PLAN accept case).
func TestBuildStaleDaysZero(t *testing.T) {
	t.Parallel()
	entries := []store.EntryRow{
		{ID: 1, Path: "a.md", Updated: "2026-10-30"},
		{ID: 2, Path: "b.md", Updated: "2026-10-01"},
		{ID: 3, Path: "c.md"},
	}
	rep := buildStale(staleNow, 0, entries, nil)
	if rep.Flagged != len(entries) {
		t.Errorf("flagged %d of %d, want every entry", rep.Flagged, len(entries))
	}
}

// TestStaleCommand: the CLI reads the index and the pinned clock, prints the grouped human form and
// the JSON, honours the config default and the --days flag, and rejects a negative --days.
func TestStaleCommand(t *testing.T) {
	t.Parallel()
	useFakeEmbedder(t)
	root := newTempRoot(t)
	orig := depsFor(t).now
	t.Cleanup(func() { depsFor(t).now = orig })
	depsFor(t).now = func() time.Time { return staleNow }

	writeFile(t, filepath.Join(root, "data", "fresh.md"), validFrontMatter("Fresh", "fresh summary"))
	writeFile(t, filepath.Join(root, "data", "old.md"), strings.Replace(
		validFrontMatter("Old", "old summary"), "updated: 2026-09-13", "updated: 2026-01-02", 1))
	writeFile(t, filepath.Join(root, "data", "nodate.md"),
		"---\ntitle: No Date\nsummary: no date here\n---\n\n## Section\n\nBody text.\n")
	writeFile(t, filepath.Join(root, "data", "new.md"), strings.Replace(
		validFrontMatter("New", "new summary"), "tags: [test]\n", "tags: [test]\nsupersedes: [old.md]\n", 1))
	writeFile(t, filepath.Join(root, "data", "broken.md"), strings.Replace(
		validFrontMatter("Broken", "broken summary"), "tags: [test]\n", "tags: [test]\nrelated: [gone.md]\n", 1))
	if _, stderr, code := run(t, "reindex", "--all"); code != ExitOK {
		t.Fatalf("reindex: exit %d\n%s", code, stderr)
	}

	stdout, stderr, code := run(t, "stale")
	if code != ExitOK || stderr != "" {
		t.Fatalf("stale: exit %d stderr %q", code, stderr)
	}
	for _, want := range []string{
		"3 of 5 entries need a look, as of 2026-10-30, threshold 180 days\n",
		"\nNo usable date: 1\n  data/nodate.md",
		"\nAt least 180 days old: 1\n  data/old.md",
		"updated 2026-01-02 (301 days old)",
		"\nSuperseded: 1\n  data/old.md",
		"superseded by data/new.md",
		"\nBroken path links: 1\n  data/broken.md",
		"related -> data/gone.md",
	} {
		if !strings.Contains(stdout, want) {
			t.Errorf("stale output lacks %q:\n%s", want, stdout)
		}
	}
	if strings.Contains(stdout, "data/fresh.md") {
		t.Errorf("a fresh entry was listed:\n%s", stdout)
	}

	// The config default applies unless --days is given; --days 0 lists all five.
	writeConfig(t, root, "[stale]\ndays = 30\n")
	if stdout, _ := mustKB(t, "stale"); !strings.Contains(stdout, "threshold 30 days") {
		t.Errorf("config stale.days not used:\n%s", stdout)
	}
	stdout, _ = mustKB(t, "stale", "--days", "0", "--json")
	var rep staleReport
	if err := json.Unmarshal([]byte(stdout), &rep); err != nil {
		t.Fatalf("decode %q: %v", stdout, err)
	}
	if rep.Days != 0 || rep.Entries != 5 || rep.Flagged != 5 || len(rep.Groups) != 4 {
		t.Errorf("--days 0 --json: days %d entries %d flagged %d groups %d, want 0 5 5 4",
			rep.Days, rep.Entries, rep.Flagged, len(rep.Groups))
	}
}

// TestStaleCommandErrors: a negative --days is a usage error before anything is read, and a store
// with no index is a NotFound failure with the reindex hint, and a broken store config fails
// naming the key.
func TestStaleCommandErrors(t *testing.T) {
	t.Parallel()
	root := useRoot(t, t.TempDir())
	if stdout, stderr, exit := runKB(t, []string{"stale", "--days", "-1"}); exit != ExitUsage || stdout != "" || !strings.Contains(stderr, "invalid --days -1") {
		t.Errorf("--days -1: exit %d stdout %q stderr %q", exit, stdout, stderr)
	}
	if _, stderr, exit := runKB(t, []string{"stale"}); exit != ExitUsage || !strings.Contains(stderr, "no index at") {
		t.Errorf("no index: exit %d stderr %q", exit, stderr)
	}
	writeConfig(t, root, "[stale]\nnope = 1\n")
	if stdout, stderr, exit := runKB(t, []string{"stale"}); exit != ExitUsage || stdout != "" || !strings.Contains(stderr, "nope") {
		t.Errorf("bad store config: exit %d stdout %q stderr %q; want a failure naming the key", exit, stdout, stderr)
	}
}

// groupItems returns the items of one reason's group.
func groupItems(r staleReport, reason string) []staleItem {
	for _, g := range r.Groups {
		if g.Reason == reason {
			return g.Items
		}
	}
	return nil
}

// reasonOrder returns the reasons of a report's groups in order.
func reasonOrder(r staleReport) []string {
	out := make([]string, len(r.Groups))
	for i, g := range r.Groups {
		out[i] = g.Reason
	}
	return out
}

// itemPaths returns the paths of items, in order.
func itemPaths(items []staleItem) []string {
	out := make([]string, len(items))
	for i, it := range items {
		out[i] = it.Path
	}
	return out
}

// TestRenderStale: the human form's three summaries (no entries, nothing flagged, some flagged) at
// their 0/1 edges, and only non-empty groups get a block.
func TestRenderStale(t *testing.T) {
	t.Parallel()
	old := 200
	date := "2026-04-13"
	groups := func(oldItems ...staleItem) []staleGroup {
		return []staleGroup{{Reason: staleNoDate, Items: []staleItem{}}, {Reason: staleOld, Items: oldItems},
			{Reason: staleSuperseded, Items: []staleItem{}}, {Reason: staleBroken, Items: []staleItem{}}}
	}
	asOf := "2026-10-30T12:00:00Z"
	for _, tc := range []struct {
		name string
		rep  staleReport
		want string
	}{
		{"no entries", staleReport{AsOf: asOf, Days: 180, Groups: groups()}, "no entries\n"},
		{"one entry, none flagged", staleReport{AsOf: asOf, Days: 180, Entries: 1, Groups: groups()},
			"no stale entries: 1 entry, as of 2026-10-30, threshold 180 days\n"},
		{"one flagged, empty groups skipped", staleReport{AsOf: asOf, Days: 180, Entries: 1, Flagged: 1,
			Groups: groups(staleItem{Path: "a.md", Field: "updated", Date: &date, AgeDays: &old})},
			"1 of 1 entry needs a look, as of 2026-10-30, threshold 180 days\n\nAt least 180 days old: 1\n  a.md  updated 2026-04-13 (200 days old)\n"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var b strings.Builder
			renderStale(&b, tc.rep)
			if b.String() != tc.want {
				t.Errorf("renderStale =\n%q\nwant\n%q", b.String(), tc.want)
			}
		})
	}
}

// staleFaultStore fails Entries or Edges after a successful read-only open (Q126).
type staleFaultStore struct {
	store.Store
	entries, edges error
}

func (s staleFaultStore) Entries(ctx context.Context) ([]store.EntryRow, error) {
	if s.entries != nil {
		return nil, s.entries
	}
	return s.Store.Entries(ctx)
}

func (s staleFaultStore) Edges(ctx context.Context) ([]store.EdgeRow, error) {
	if s.edges != nil {
		return nil, s.edges
	}
	return s.Store.Edges(ctx)
}

// TestStaleReadFaults (Q126): a read error after the index opened is the command's error with its
// kind, never a partial or empty report.
func TestStaleReadFaults(t *testing.T) {
	t.Parallel()
	useFakeEmbedder(t)
	root := newTempRoot(t)
	writeFile(t, filepath.Join(root, "data", "a.md"), validFrontMatter("A", "a summary"))
	if _, stderr, code := run(t, "reindex", "--all"); code != ExitOK {
		t.Fatalf("reindex: exit %d\n%s", code, stderr)
	}
	boom := errs.New(errs.Conflict, "test", "disk exploded")
	orig := depsFor(t).openStore
	t.Cleanup(func() { depsFor(t).openStore = orig })
	for _, tc := range []struct {
		name string
		wrap func(store.Store) store.Store
	}{
		{"entries", func(s store.Store) store.Store { return staleFaultStore{Store: s, entries: boom} }},
		{"edges", func(s store.Store) store.Store { return staleFaultStore{Store: s, edges: boom} }},
	} {
		t.Run(tc.name, func(t *testing.T) {
			depsFor(t).openStore = func(ctx context.Context, path string, ro bool) (store.Store, error) {
				st, err := orig(ctx, path, ro)
				if err != nil {
					return nil, err
				}
				return tc.wrap(st), nil
			}
			stdout, stderr, exit := runKB(t, []string{"stale"})
			if exit != ExitUsage || stdout != "" || !strings.Contains(stderr, "disk exploded") {
				t.Errorf("exit %d stdout %q stderr %q; want %d, no report, the cause", exit, stdout, stderr, ExitUsage)
			}
		})
	}
}
