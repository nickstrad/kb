package cli

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"knowledge/kb/internal/embed"
	"knowledge/kb/internal/embed/fake"
	"knowledge/kb/internal/search"
	"knowledge/kb/internal/store"
)

// statsFixture is a small real SQLite database (built the same way internal/stats' own fixture is)
// with two searches: one old, one recent, by two different callers, one judged useful.
type statsFixture struct {
	root      string
	oldID     int64
	recentID  int64
	recentTS  time.Time
	oldCaller string
	newCaller string
}

// buildStatsFixture indexes one entry and logs two searches: an old hybrid search by "alice" with
// a useful verdict (so overview/entries/gaps all have something to show), and a recent fts search
// by "bob" with no feedback (an unjudged search, so coverage is exercised too).
func buildStatsFixture(t *testing.T) *statsFixture {
	t.Helper()
	root := t.TempDir()
	dbPath := filepath.Join(root, ".kb", "kb.sqlite")

	st, err := store.Open(dbPath)
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	t.Cleanup(func() { st.Close() })

	emb := fake.New("fake-embed", 768)
	if err := st.EnsureEmbedMeta(emb.Model(), emb.Dim()); err != nil {
		t.Fatalf("embed meta: %v", err)
	}
	ctx := context.Background()
	text := "widget chunk about configuration"
	vecs, err := emb.Embed(ctx, []string{embed.DocPrefix + text})
	if err != nil {
		t.Fatalf("embed: %v", err)
	}
	if _, err := st.ReplaceEntryChunks(ctx, store.EntryInput{
		Path: "widget.md", Kind: "file", Title: "Widget", Summary: "s", Tags: []string{"test"}, BodyHash: "h",
	}, []store.ChunkInput{{
		Ord: 0, SourceFile: "widget.md", Heading: "", Text: text, TextHash: "th", Embedding: vecs[0],
	}}); err != nil {
		t.Fatalf("index: %v", err)
	}

	f := &statsFixture{root: root, oldCaller: "alice", newCaller: "bob"}

	sAlice := &search.Searcher{DB: st.DB()}
	oldOut := &search.Outcome{
		Mode: search.ModeHybrid, K: 5, NFTS: 1, NVec: 1, TotalMs: 20,
		Returned: []search.Candidate{
			{ChunkID: 1, FTSRank: pInt(1), FTSScore: pFloat(-1), VecRank: pInt(1), VecDistance: pFloat(0.1),
				RRFScore: 0.9, EntryPath: "widget.md", Heading: ""},
		},
	}
	oldOut.Candidates = append([]search.Candidate{}, oldOut.Returned...)
	id, err := sAlice.Log(ctx, search.Request{Query: "widget config", Caller: f.oldCaller}, oldOut)
	if err != nil {
		t.Fatalf("log old search: %v", err)
	}
	f.oldID = id
	if err := sAlice.Feedback(ctx, f.oldID, 1, true, "good"); err != nil {
		t.Fatalf("feedback: %v", err)
	}
	oldTS := time.Now().UTC().Add(-30 * 24 * time.Hour)
	if _, err := st.DB().Exec(`UPDATE searches SET ts = ? WHERE id = ?`, oldTS.Format(time.RFC3339), f.oldID); err != nil {
		t.Fatalf("set old ts: %v", err)
	}

	sBob := &search.Searcher{DB: st.DB()}
	newOut := &search.Outcome{
		Mode: search.ModeFTS, K: 5, NFTS: 1, NVec: 0, TotalMs: 10,
		Returned: []search.Candidate{
			{ChunkID: 1, FTSRank: pInt(1), FTSScore: pFloat(-1), RRFScore: 0.5, EntryPath: "widget.md", Heading: ""},
		},
	}
	newOut.Candidates = append([]search.Candidate{}, newOut.Returned...)
	id, err = sBob.Log(ctx, search.Request{Query: "widget config", Caller: f.newCaller}, newOut)
	if err != nil {
		t.Fatalf("log recent search: %v", err)
	}
	f.recentID = id
	f.recentTS = time.Now().UTC().Add(-1 * time.Hour)
	if _, err := st.DB().Exec(`UPDATE searches SET ts = ? WHERE id = ?`, f.recentTS.Format(time.RFC3339), f.recentID); err != nil {
		t.Fatalf("set recent ts: %v", err)
	}

	return f
}

func pInt(v int) *int           { return &v }
func pFloat(v float64) *float64 { return &v }

func TestStatsListsQueries(t *testing.T) {
	t.Setenv("KB_ROOT", t.TempDir()) // hermetic: never look at the real repository
	stdout, stderr, exit := runKB(t, []string{"stats"})
	if exit != 0 {
		t.Fatalf("exit = %d, want 0; stderr=%s", exit, stderr)
	}
	for _, name := range []string{"overview", "sources", "trend", "entries", "gaps"} {
		if !strings.Contains(stdout, name) {
			t.Errorf("stats listing missing %q:\n%s", name, stdout)
		}
	}
}

func TestStatsListNeedsNoDatabase(t *testing.T) {
	root := t.TempDir() // no .kb at all
	t.Setenv("KB_ROOT", root)

	stdout, stderr, exit := runKB(t, []string{"stats"})
	if exit != 0 {
		t.Fatalf("exit = %d, want 0; stderr=%s", exit, stderr)
	}
	if !strings.Contains(stdout, "overview") {
		t.Errorf("listing without a database missing overview:\n%s", stdout)
	}
}

func TestStatsNamedQueriesRun(t *testing.T) {
	f := buildStatsFixture(t)
	t.Setenv("KB_ROOT", f.root)

	for _, name := range []string{"overview", "sources", "trend", "entries", "gaps"} {
		t.Run(name, func(t *testing.T) {
			stdout, stderr, exit := runKB(t, []string{"stats", name})
			if exit != 0 {
				t.Fatalf("exit = %d, want 0; stderr=%s", exit, stderr)
			}
			if !strings.Contains(stdout, "searches loaded:") {
				t.Errorf("%s output missing footer:\n%s", name, stdout)
			}
		})
	}
}

func TestStatsOverviewHasExpectedColumns(t *testing.T) {
	f := buildStatsFixture(t)
	t.Setenv("KB_ROOT", f.root)

	stdout, stderr, exit := runKB(t, []string{"stats", "overview"})
	if exit != 0 {
		t.Fatalf("exit = %d, want 0; stderr=%s", exit, stderr)
	}
	for _, col := range []string{"mode", "searches", "judged", "coverage_pct", "hit_rate_pct", "mrr", "zero_result_pct", "p50_ms", "p95_ms", "avg_embed_ms"} {
		if !strings.Contains(stdout, col) {
			t.Errorf("overview header missing column %q:\n%s", col, stdout)
		}
	}
	if !strings.Contains(stdout, "hybrid") || !strings.Contains(stdout, "fts") {
		t.Errorf("overview missing expected mode rows:\n%s", stdout)
	}
	if !strings.Contains(stdout, "searches loaded: 2 (since all, caller all)") {
		t.Errorf("overview footer wrong:\n%s", stdout)
	}
}

// TestStatsFloatsShortestForm proves human output renders float64 in its shortest exact decimal
// form (strconv.FormatFloat(v, 'f', -1, 64)), not padded to a fixed number of decimals: a rate the
// SQL already rounded must print as "100" or "1", never "100.000" or "1.000".
func TestStatsFloatsShortestForm(t *testing.T) {
	f := buildStatsFixture(t)
	t.Setenv("KB_ROOT", f.root)

	stdout, stderr, exit := runKB(t, []string{"stats", "overview"})
	if exit != 0 {
		t.Fatalf("exit = %d, want 0; stderr=%s", exit, stderr)
	}
	if strings.Contains(stdout, ".000") {
		t.Errorf("floats should render in their shortest exact form, not padded with zeros:\n%s", stdout)
	}

	var hybridFields, ftsFields []string
	for _, line := range strings.Split(stdout, "\n") {
		fields := strings.Fields(line)
		if len(fields) == 0 {
			continue
		}
		switch fields[0] {
		case "hybrid":
			hybridFields = fields
		case "fts":
			ftsFields = fields
		}
	}
	if hybridFields == nil || ftsFields == nil {
		t.Fatalf("missing hybrid/fts rows in overview output:\n%s", stdout)
	}
	// Columns: mode searches judged coverage_pct hit_rate_pct mrr zero_result_pct p50_ms p95_ms avg_embed_ms.
	// hybrid is alice's old, judged, useful-at-rank-1 search: everything judged, everything useful.
	if want := []string{"hybrid", "1", "1", "100", "100", "1", "0", "20", "20", "0"}; !equalFields(hybridFields, want) {
		t.Errorf("hybrid row = %v, want %v", hybridFields, want)
	}
	// fts is bob's recent, unjudged search: coverage/hit-rate/mrr/avg_embed_ms are all NULL or zero.
	if want := []string{"fts", "1", "0", "0", "-", "-", "0", "10", "10", "-"}; !equalFields(ftsFields, want) {
		t.Errorf("fts row = %v, want %v", ftsFields, want)
	}
}

func equalFields(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}

func TestStatsJSONShape(t *testing.T) {
	f := buildStatsFixture(t)
	t.Setenv("KB_ROOT", f.root)

	stdout, stderr, exit := runKB(t, []string{"stats", "overview", "--json"})
	if exit != 0 {
		t.Fatalf("exit = %d, want 0; stderr=%s", exit, stderr)
	}
	var parsed struct {
		Query          string           `json:"query"`
		Columns        []string         `json:"columns"`
		Rows           []map[string]any `json:"rows"`
		SearchesLoaded int64            `json:"searches_loaded"`
		Since          *string          `json:"since"`
		Caller         *string          `json:"caller"`
	}
	if err := json.Unmarshal([]byte(stdout), &parsed); err != nil {
		t.Fatalf("json.Unmarshal: %v\nstdout:\n%s", err, stdout)
	}
	if parsed.Query != "overview" {
		t.Errorf("query = %q, want overview", parsed.Query)
	}
	if len(parsed.Columns) == 0 {
		t.Error("columns is empty")
	}
	if len(parsed.Rows) == 0 {
		t.Error("rows is empty")
	}
	for _, col := range parsed.Columns {
		if _, ok := parsed.Rows[0][col]; !ok {
			t.Errorf("row missing column %q as a key: %+v", col, parsed.Rows[0])
		}
	}
	if parsed.SearchesLoaded != 2 {
		t.Errorf("searches_loaded = %d, want 2", parsed.SearchesLoaded)
	}
	if parsed.Since != nil {
		t.Errorf("since = %v, want nil (no --since given)", parsed.Since)
	}
	if parsed.Caller != nil {
		t.Errorf("caller = %v, want nil (no --caller given)", parsed.Caller)
	}
}

func TestStatsPrintSQLNeedsNoDatabase(t *testing.T) {
	root := t.TempDir() // no .kb/kb.sqlite anywhere
	t.Setenv("KB_ROOT", root)

	stdout, stderr, exit := runKB(t, []string{"stats", "overview", "--print-sql"})
	if exit != 0 {
		t.Fatalf("exit = %d, want 0; stderr=%s", exit, stderr)
	}
	if !strings.Contains(stdout, "FROM searches") {
		t.Errorf("--print-sql output does not look like SQL:\n%s", stdout)
	}
	// The promise is stronger than "works without a database": it must not create one either.
	if _, err := os.Stat(filepath.Join(root, ".kb")); !os.IsNotExist(err) {
		t.Errorf("--print-sql created %s/.kb (stat err = %v); it must open no database", root, err)
	}
}

func TestStatsAdHocSQL(t *testing.T) {
	f := buildStatsFixture(t)
	t.Setenv("KB_ROOT", f.root)

	stdout, stderr, exit := runKB(t, []string{"stats", "--sql", "select count(*) as n from searches"})
	if exit != 0 {
		t.Fatalf("exit = %d, want 0; stderr=%s", exit, stderr)
	}
	if !strings.Contains(stdout, "n") || !strings.Contains(stdout, "2") {
		t.Errorf("--sql output missing expected count:\n%s", stdout)
	}

	stdout, stderr, exit = runKB(t, []string{"stats", "--sql", "select count(*) as n from searches", "--json"})
	if exit != 0 {
		t.Fatalf("exit = %d, want 0; stderr=%s", exit, stderr)
	}
	var raw map[string]any
	if err := json.Unmarshal([]byte(stdout), &raw); err != nil {
		t.Fatalf("unmarshal --sql --json: %v\n%s", err, stdout)
	}
	if raw["query"] != "sql" {
		t.Errorf(`query = %v, want "sql"`, raw["query"])
	}
}

func TestStatsSinceDuration(t *testing.T) {
	f := buildStatsFixture(t)
	t.Setenv("KB_ROOT", f.root)

	stdout, stderr, exit := runKB(t, []string{"stats", "overview", "--since", "7d"})
	if exit != 0 {
		t.Fatalf("exit = %d, want 0; stderr=%s", exit, stderr)
	}
	if !strings.Contains(stdout, "searches loaded: 1 (since ") {
		t.Errorf("--since 7d should drop the 30-day-old search:\n%s", stdout)
	}
}

func TestStatsSinceDate(t *testing.T) {
	f := buildStatsFixture(t)
	t.Setenv("KB_ROOT", f.root)

	yesterday := time.Now().UTC().Add(-24 * time.Hour).Format("2006-01-02")
	stdout, stderr, exit := runKB(t, []string{"stats", "overview", "--since", yesterday})
	if exit != 0 {
		t.Fatalf("exit = %d, want 0; stderr=%s", exit, stderr)
	}
	if !strings.Contains(stdout, "searches loaded: 1 ") {
		t.Errorf("--since <yesterday> should drop the 30-day-old search:\n%s", stdout)
	}
}

func TestStatsSinceBogus(t *testing.T) {
	f := buildStatsFixture(t)
	t.Setenv("KB_ROOT", f.root)

	_, stderr, exit := runKB(t, []string{"stats", "overview", "--since", "bogus"})
	if exit != 1 {
		t.Fatalf("exit = %d, want 1; stderr=%s", exit, stderr)
	}
	if !strings.Contains(stderr, "--since") {
		t.Errorf("stderr missing --since complaint: %s", stderr)
	}
}

func TestStatsCallerFilter(t *testing.T) {
	f := buildStatsFixture(t)
	t.Setenv("KB_ROOT", f.root)

	stdout, stderr, exit := runKB(t, []string{"stats", "overview", "--caller", f.newCaller})
	if exit != 0 {
		t.Fatalf("exit = %d, want 0; stderr=%s", exit, stderr)
	}
	if !strings.Contains(stdout, "searches loaded: 1 (since all, caller "+f.newCaller+")") {
		t.Errorf("--caller %s footer wrong:\n%s", f.newCaller, stdout)
	}
}

func TestStatsUnknownName(t *testing.T) {
	f := buildStatsFixture(t)
	t.Setenv("KB_ROOT", f.root)

	_, stderr, exit := runKB(t, []string{"stats", "bogus-query"})
	if exit != 1 {
		t.Fatalf("exit = %d, want 1; stderr=%s", exit, stderr)
	}
	for _, name := range []string{"overview", "sources", "trend", "entries", "gaps"} {
		if !strings.Contains(stderr, name) {
			t.Errorf("unknown-name error missing %q from the valid list: %s", name, stderr)
		}
	}
}

func TestStatsNameAndSQLConflict(t *testing.T) {
	f := buildStatsFixture(t)
	t.Setenv("KB_ROOT", f.root)

	_, stderr, exit := runKB(t, []string{"stats", "overview", "--sql", "select 1"})
	if exit != 1 {
		t.Fatalf("exit = %d, want 1; stderr=%s", exit, stderr)
	}
	if !strings.Contains(stderr, "mutually exclusive") {
		t.Errorf("stderr missing mutual-exclusion complaint: %s", stderr)
	}
}

func TestStatsPrintSQLAndSQLConflict(t *testing.T) {
	f := buildStatsFixture(t)
	t.Setenv("KB_ROOT", f.root)

	_, stderr, exit := runKB(t, []string{"stats", "--sql", "select 1", "--print-sql"})
	if exit != 1 {
		t.Fatalf("exit = %d, want 1; stderr=%s", exit, stderr)
	}
	if !strings.Contains(stderr, "mutually exclusive") {
		t.Errorf("stderr missing mutual-exclusion complaint: %s", stderr)
	}
}

func TestStatsMissingDatabase(t *testing.T) {
	root := t.TempDir() // no .kb/kb.sqlite
	t.Setenv("KB_ROOT", root)

	_, stderr, exit := runKB(t, []string{"stats", "overview"})
	if exit != 1 {
		t.Fatalf("exit = %d, want 1; stderr=%s", exit, stderr)
	}
	if !strings.Contains(stderr, "kb reindex --all") {
		t.Errorf("stderr missing reindex hint: %s", stderr)
	}
}

func TestStatsHelpShowsDefinitionsAndQueries(t *testing.T) {
	t.Setenv("KB_ROOT", t.TempDir()) // hermetic: never look at the real repository
	stdout, stderr, exit := runKB(t, []string{"stats", "--help"})
	if exit != 0 {
		t.Fatalf("exit = %d, want 0; stderr=%s", exit, stderr)
	}
	if !strings.Contains(stdout, "judged searches") || !strings.Contains(stdout, "unknown, not a failure") {
		t.Errorf("stats --help missing D4 definitions:\n%s", stdout)
	}
	for _, name := range []string{"overview", "sources", "trend", "entries", "gaps"} {
		if !strings.Contains(stdout, name) {
			t.Errorf("stats --help missing query %q:\n%s", name, stdout)
		}
	}
	if !strings.Contains(stdout, "Decision:") {
		t.Errorf("stats --help missing help paragraphs (no Decision: sentence found):\n%s", stdout)
	}
}
