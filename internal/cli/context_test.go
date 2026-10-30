package cli

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"flag"
	"fmt"
	"math/rand/v2"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"

	"github.com/nickstrad/kb/internal/embed"
	"github.com/nickstrad/kb/internal/embed/fake"
	"github.com/nickstrad/kb/internal/render"
	"github.com/nickstrad/kb/internal/store"
	"github.com/nickstrad/kb/internal/store/sqlite"
)

var updateContext = flag.Bool("update-context", false, "rewrite testdata/golden/context_*.golden")

// contextEntry is one fixture entry: its chunks as (heading, text) pairs.
type contextEntry struct {
	path, title, summary, updated string
	chunks                        [][2]string
}

// contextEntries is the small store the kb context goldens are made from.
var contextEntries = []contextEntry{
	{"tmux.md", "Tmux Notes", "sessions survive a logout and detach", "2026-09-13", [][2]string{
		{"", "Tmux Notes: sessions survive a logout when the server keeps running"},
		{"Detach", "press the prefix then d to detach, and the session stays alive after logout"},
		{"Logs", "rotate tmux logs weekly with logrotate so the pane buffer stays small"},
	}},
	{"ssh.md", "SSH Config", "keepalive settings", "2026-08-02", [][2]string{
		{"", "SSH Config: keepalive settings stop a logout from dropping the link"},
	}},
	{"git.md", "Git Tips", "rebase before a pull request", "2026-07-19", [][2]string{
		{"", "Git Tips: rebase onto main before opening a pull request"},
	}},
}

// newContextRepo indexes contextEntries into a fresh location with the fixture model, so hybrid
// search (what kb context runs) works offline.
func newContextRepo(t *testing.T) string {
	t.Helper()
	withFixtureEmbedder(t)
	root := useRoot(t, t.TempDir())
	writeConfig(t, root, "[embed]\nmodel = \""+testModel768+"\"\n")
	ctx := context.Background()
	st, err := sqlite.Open(ctx, testDBPath(root), sqlite.Options{})
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	if err := st.EnsureEmbedMeta(ctx, store.EmbedMeta{Model: testModel768, Dim: testDim768}); err != nil {
		t.Fatal(err)
	}
	emb := fake.New(testModel768, testDim768)
	for _, e := range contextEntries {
		texts := make([]string, len(e.chunks))
		for i, c := range e.chunks {
			texts[i] = embed.DocText(emb, c[1])
		}
		vecs, err := emb.Embed(ctx, texts)
		if err != nil {
			t.Fatal(err)
		}
		chunks := make([]store.ChunkInput, len(e.chunks))
		for i, c := range e.chunks {
			sum := sha256.Sum256([]byte(c[1]))
			chunks[i] = store.ChunkInput{Ord: i, SourceFile: e.path, Heading: c[0], Text: c[1],
				TextHash: hex.EncodeToString(sum[:]), Embedding: vecs[i]}
		}
		if _, err := st.ReplaceEntry(ctx, store.EntryInput{Path: e.path, Kind: "file", Title: e.title,
			Summary: e.summary, Updated: e.updated, BodyHash: "hash-" + e.path}, chunks); err != nil {
			t.Fatal(err)
		}
	}
	return root
}

// contextGoldenDir is resolved at init: tests chdir into a temp root.
var contextGoldenDir = func() string {
	wd, err := os.Getwd()
	if err != nil {
		panic(err)
	}
	return filepath.Join(wd, "testdata", "golden")
}()

func checkContextGolden(t *testing.T, name, got string) {
	t.Helper()
	path := filepath.Join(contextGoldenDir, name)
	if *updateContext {
		if err := os.WriteFile(path, []byte(got), 0o644); err != nil {
			t.Fatal(err)
		}
		return
	}
	want, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read golden: %v (run with -update-context)", err)
	}
	if string(want) != got {
		t.Errorf("output differs from %s (run with -update-context and review the diff):\n--- got\n%s\n--- want\n%s", path, got, want)
	}
}

// contextTokens is the size of a whole output as the budget counts it (the final newline is not
// part of the Markdown).
func contextTokens(out string) int {
	return render.EstimateTokens(strings.TrimSuffix(out, "\n"))
}

// TestContextGolden pins the Markdown at three budgets: unlimited, a small one that condenses and
// leaves hits out, and the exact size of the unlimited output (which must fit with nothing cut).
// Each run gets a fresh store, so every output is search 1 and the goldens do not depend on order.
func TestContextGolden(t *testing.T) {
	t.Parallel()
	run := func(args ...string) string {
		t.Helper()
		newContextRepo(t)
		out, stderr, exit := runKB(t, append([]string{"context", "tmux logout"}, args...))
		if exit != 0 || stderr != "" {
			t.Fatalf("exit %d stderr %q", exit, stderr)
		}
		return out
	}

	full := run("--budget", "0")
	checkContextGolden(t, "context_unlimited.golden", full)
	if strings.Contains(full, "raise --budget") {
		t.Errorf("unlimited output carries a tail:\n%s", full)
	}

	small := run("--budget", "200")
	checkContextGolden(t, "context_small.golden", small)
	if !strings.Contains(small, "raise --budget") {
		t.Errorf("the small budget should condense or leave out hits:\n%s", small)
	}

	exact := run("--budget", strconv.Itoa(contextTokens(full)))
	if exact != full {
		t.Errorf("exact-fit budget %d changed the output:\n%s\nwant:\n%s", contextTokens(full), exact, full)
	}
	checkContextGolden(t, "context_exact.golden", exact)
}

// TestContextBudgetBound: for every budget from the floor up to the unlimited size, the whole
// output (heading, hits and footer) fits. The floor is the output at budget 1, which keeps the
// frame: below it nothing can fit, and the output says so with the "raise --budget" note.
func TestContextBudgetBound(t *testing.T) {
	t.Parallel()
	newContextRepo(t)
	floorOut, stderr, exit := runKB(t, []string{"context", "tmux logout", "--budget", "1"})
	if exit != 0 {
		t.Fatalf("budget 1: exit %d %s", exit, stderr)
	}
	floor := contextTokens(floorOut)
	full, stderr, exit := runKB(t, []string{"context", "tmux logout", "--budget", "0"})
	if exit != 0 {
		t.Fatalf("budget 0: exit %d %s", exit, stderr)
	}
	top := contextTokens(full)
	if top <= floor {
		t.Fatalf("unlimited output (%d tokens) is not above the floor (%d): nothing to check", top, floor)
	}
	for b := floor; b <= top; b++ {
		out, stderr, exit := runKB(t, []string{"context", "tmux logout", "--budget", strconv.Itoa(b)})
		if exit != 0 || stderr != "" {
			t.Fatalf("budget %d: exit %d stderr %q", b, exit, stderr)
		}
		if got := contextTokens(out); got > b {
			t.Errorf("budget %d: output is %d tokens:\n%s", b, got, out)
		}
	}
}

// TestContextFlagErrors: a bad budget, an unknown expand mode and an empty task are usage errors
// (exit 1) with nothing on stdout.
func TestContextFlagErrors(t *testing.T) {
	t.Parallel()
	newContextRepo(t)
	for _, tc := range []struct {
		name string
		args []string
		want string
	}{
		{"negative budget", []string{"context", "tmux", "--budget", "-1"}, "--budget"},
		{"bad expand", []string{"context", "tmux", "--expand", "page"}, "--expand"},
		{"empty task", []string{"context", ""}, "task"},
		{"blank task", []string{"context", "   "}, "task"},
		{"no task", []string{"context"}, ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			stdout, stderr, exit := runKB(t, tc.args)
			if exit != 1 || stdout != "" || !strings.Contains(stderr, tc.want) {
				t.Errorf("exit %d stdout %q stderr %q; want exit 1, no stdout, stderr naming %q", exit, stdout, stderr, tc.want)
			}
		})
	}
}

// TestContextJSON pins the --json shape: the search id and feedback, the query and mode, the
// budget and its token count, and every hit with its level (0 when the budget left it out).
func TestContextJSON(t *testing.T) {
	t.Parallel()
	newContextRepo(t)
	stdout, stderr, exit := runKB(t, []string{"context", "tmux logout", "--json", "--budget", "60"})
	if exit != 0 || stderr != "" {
		t.Fatalf("exit %d stderr %q", exit, stderr)
	}
	var got struct {
		SearchID *int64 `json:"search_id"`
		Feedback *struct {
			Useful string `json:"useful"`
			None   string `json:"none"`
		} `json:"feedback"`
		Query  string `json:"query"`
		Mode   string `json:"mode"`
		Budget int    `json:"budget"`
		Tokens int    `json:"tokens"`
		Hits   []struct {
			Rank    int    `json:"rank"`
			Path    string `json:"path"`
			Heading string `json:"heading"`
			Updated string `json:"updated"`
			Summary string `json:"summary"`
			Level   int    `json:"level"`
		} `json:"hits"`
	}
	if err := json.Unmarshal([]byte(stdout), &got); err != nil {
		t.Fatalf("not JSON: %v\n%s", err, stdout)
	}
	if got.SearchID == nil || *got.SearchID != 1 {
		t.Errorf("search_id = %v, want 1", got.SearchID)
	}
	if got.Feedback == nil || !strings.HasPrefix(got.Feedback.Useful, "kb feedback 1 RANK --useful") || got.Feedback.None != "kb feedback 1 --none --note \"why\"" {
		t.Errorf("feedback = %+v", got.Feedback)
	}
	if got.Query != "tmux logout" || got.Mode != "hybrid" || got.Budget != 60 {
		t.Errorf("query %q mode %q budget %d", got.Query, got.Mode, got.Budget)
	}
	if got.Tokens > 60 || got.Tokens < 1 {
		t.Errorf("tokens = %d, want 1..60", got.Tokens)
	}
	if len(got.Hits) == 0 {
		t.Fatalf("no hits: %s", stdout)
	}
	shown, left := 0, 0
	for _, h := range got.Hits {
		if h.Path == "" || h.Rank < 1 {
			t.Errorf("hit without path or rank: %+v", h)
		}
		if h.Level == 0 {
			left++
		} else {
			shown++
		}
	}
	if shown == 0 || left == 0 {
		t.Errorf("budget 60 should show some hits and leave some out: shown %d, left %d\n%s", shown, left, stdout)
	}
	if got.Hits[0].Path != "tmux.md" || got.Hits[0].Updated != "2026-09-13" {
		t.Errorf("best hit = %+v, want tmux.md with its date", got.Hits[0])
	}
}

// TestContextConfigBudget: [output] context_budget is the default; --budget wins.
func TestContextConfigBudget(t *testing.T) {
	t.Parallel()
	root := newContextRepo(t)
	writeConfig(t, root, "[embed]\nmodel = \""+testModel768+"\"\n[output]\ncontext_budget = 70\n")
	fromConfig, _, exit := runKB(t, []string{"context", "tmux logout"})
	if exit != 0 {
		t.Fatalf("exit %d", exit)
	}
	if got := contextTokens(fromConfig); got > 70 {
		t.Errorf("config budget 70: output is %d tokens:\n%s", got, fromConfig)
	}
	override, _, _ := runKB(t, []string{"context", "tmux logout", "--budget", "0"})
	if strings.Contains(override, "raise --budget") {
		t.Errorf("--budget 0 did not override the config budget:\n%s", override)
	}
}

// TestContextLogsSearch: the footer's search id is a logged search, so kb feedback accepts it, and
// the next context run gets the next id.
func TestContextLogsSearch(t *testing.T) {
	t.Parallel()
	newContextRepo(t)
	out, _, exit := runKB(t, []string{"context", "tmux logout"})
	if exit != 0 || !strings.Contains(out, "## kb context (search 1)") || !strings.Contains(out, "kb feedback 1 RANK") {
		t.Fatalf("exit %d; output:\n%s", exit, out)
	}
	if _, stderr, code := runKB(t, []string{"feedback", "1", "--none", "--note", "test"}); code != 0 {
		t.Fatalf("feedback on the context search: exit %d %q", code, stderr)
	}
	second, _, _ := runKB(t, []string{"context", "rebase"})
	if !strings.Contains(second, "## kb context (search 2)") {
		t.Errorf("second run should be search 2:\n%s", second)
	}
}

// TestContextSynonyms: kb context loads the store's synonyms.txt like kb search (it used to skip
// them on the primary store while fan-out stores loaded theirs): the logged FTS query holds the
// alternative and the settings hash changes with the file.
func TestContextSynonyms(t *testing.T) {
	t.Parallel()
	root := newContextRepo(t)
	if _, stderr, exit := runKB(t, []string{"context", "tmux"}); exit != 0 {
		t.Fatalf("without synonyms: exit %d %s", exit, stderr)
	}
	writeFile(t, filepath.Join(testWS(root).StoreDir, "synonyms.txt"), "tmux, terminalmux\n")
	if _, stderr, exit := runKB(t, []string{"context", "tmux"}); exit != 0 {
		t.Fatalf("with synonyms: exit %d %s", exit, stderr)
	}
	out, stderr, exit := runKB(t, []string{"stats", "--sql", "select fts_query, settings_hash from searches order by id", "--json"})
	if exit != 0 {
		t.Fatalf("stats --sql: exit %d %s", exit, stderr)
	}
	var res struct {
		Rows []struct {
			FTSQuery     string `json:"fts_query"`
			SettingsHash string `json:"settings_hash"`
		} `json:"rows"`
	}
	err := json.Unmarshal([]byte(out), &res)
	rows := res.Rows
	if err != nil || len(rows) != 2 {
		t.Fatalf("stats rows %q: %v", out, err)
	}
	if strings.Contains(rows[0].FTSQuery, "terminalmux") || !strings.Contains(rows[1].FTSQuery, "terminalmux") {
		t.Errorf("fts queries %q, %q: want the synonym only in the second", rows[0].FTSQuery, rows[1].FTSQuery)
	}
	if rows[0].SettingsHash == rows[1].SettingsHash {
		t.Errorf("settings hash %q unchanged by synonyms.txt", rows[0].SettingsHash)
	}
}

// TestFitContextBound is the budget fitter's bound on synthetic hits (no store, no CLI): for
// seeded random hit sets (unicode, empty summaries, expansions, store-prefixed hits, 0 to 8 hits)
// and every budget from the floor (the output at budget 1) to the unlimited size, the framed
// output fits and every hit gets a level.
func TestFitContextBound(t *testing.T) {
	t.Parallel()
	rng := rand.New(rand.NewPCG(10, 43))
	words := []string{"tmux", "détaché", "session", "日本語", "logout", "rebase", "a", "pull-request"}
	text := func(n int) string {
		var b strings.Builder
		for range n {
			b.WriteString(words[rng.IntN(len(words))] + " ")
		}
		return b.String()
	}
	for set := range 40 {
		var hits []render.Hit
		for i := range rng.IntN(9) {
			h := render.Hit{Rank: i + 1, Path: fmt.Sprintf("e%d.md", i), Heading: text(rng.IntN(4)), Summary: text(rng.IntN(12)), Text: text(rng.IntN(120))}
			if rng.IntN(3) == 0 {
				h.Expanded = text(rng.IntN(300))
			}
			if rng.IntN(4) == 0 {
				h.Store = "notes"
			}
			hits = append(hits, h)
		}
		frame := func(body string) string {
			return contextFrame("## kb context (search 7)", body, contextFooter(7, true, hits))
		}
		floorText, _ := fitContext(hits, 1, frame)
		full, _ := fitContext(hits, 0, frame)
		for b := render.EstimateTokens(floorText); b <= render.EstimateTokens(full); b++ {
			out, levels := fitContext(hits, b, frame)
			if got := render.EstimateTokens(out); got > b || len(levels) != len(hits) {
				t.Fatalf("set %d (%d hits), budget %d: %d tokens, %d levels", set, len(hits), b, got, len(levels))
			}
		}
	}
}

// TestContextFrame is the frame's decision table, on pure functions: the footer (not logged, no
// hits, a store-prefixed best hit), a block's levels (summary only when set; level 3 shows the
// expansion, else the trimmed text) and the body's tail for hidden or condensed hits.
func TestContextFrame(t *testing.T) {
	t.Parallel()
	hit := render.Hit{Rank: 2, Path: "tmux.md", Heading: "Detach", Updated: "2026-07-01", Summary: "S", Text: "  body  "}
	for _, tc := range []struct{ name, got, want string }{
		{"footer: not logged", contextFooter(0, false, []render.Hit{hit}), "search not logged"},
		{"footer: no hits", contextFooter(4, true, nil), "search 4 · kb feedback 4 --none"},
		{"footer: store hit", contextFooter(4, true, []render.Hit{{Store: "notes", Path: "a.md"}}), "More: kb show notes:a.md\nsearch 4 · kb feedback 4 RANK --useful or --not-useful"},
		{"block: level 1", contextBlock(hit, 1), "### [2] tmux.md › Detach · updated 2026-07-01"},
		{"block: level 2", contextBlock(hit, 2), "### [2] tmux.md › Detach · updated 2026-07-01\n\nS"},
		{"block: level 2, no summary", contextBlock(render.Hit{Rank: 1, Path: "a.md"}, 2), "### [1] a.md"},
		{"block: level 3, text", contextBlock(hit, 3), "### [2] tmux.md › Detach · updated 2026-07-01\n\nS\n\n  body  "},
		{"block: level 3, expanded", contextBlock(render.Hit{Rank: 1, Store: "notes", Path: "a.md", Text: "t", Expanded: "whole"}, 3), "### [1] notes:a.md\n\nwhole"},
		{"block: also ranks", contextBlock(render.Hit{Rank: 1, Path: "a.md", Also: []int{3, 5}}, 1), "### [1] a.md · also [3] [5]"},
		{"body: no hits", contextBody(nil, nil), "No results."},
		{"body: hidden and condensed", contextBody([]render.Hit{hit, hit, hit}, []int{3, 1, 0}), contextBlock(hit, 3) + "\n\n" + contextBlock(hit, 1) + "\n\n(+1 hits not shown, 1 condensed; raise --budget)"},
		{"body: condensed only", contextBody([]render.Hit{hit}, []int{2}), contextBlock(hit, 2) + "\n\n(+1 condensed; raise --budget)"},
	} {
		if tc.got != tc.want {
			t.Errorf("%s:\n got %q\nwant %q", tc.name, tc.got, tc.want)
		}
	}
}

// TestContextFallbackExits2: with the embedder down, kb context still prints the pack from FTS,
// warns, and exits 2 (the kb search fallback contract).
func TestContextFallbackExits2(t *testing.T) {
	t.Parallel()
	newContextRepo(t)
	countingEmbedders(t, true)
	stdout, stderr, exit := runKB(t, []string{"context", "tmux logout"})
	if exit != ExitEmbedder || !strings.HasPrefix(stdout, "## kb context (search 1)") || !strings.Contains(stderr, "falling back to --mode fts") {
		t.Errorf("exit %d\nstdout %q\nstderr %q", exit, stdout, stderr)
	}
}
