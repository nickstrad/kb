package cli

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"flag"
	"math"
	"os"
	"path/filepath"
	"slices"
	"sort"
	"strings"
	"testing"

	"github.com/nickstrad/kb/internal/config"
	"github.com/nickstrad/kb/internal/embed"
	"github.com/nickstrad/kb/internal/embed/fake"
	"github.com/nickstrad/kb/internal/search"
	"github.com/nickstrad/kb/internal/store"
	"github.com/nickstrad/kb/internal/store/sqlite"
)

var updateExplain = flag.Bool("update-explain", false, "rewrite testdata/golden/explain_*.golden")

// newExplainRepo indexes five small entries with the fixture model (the shared two-chunk fixture
// is too small for bm25: with every chunk matching, idf bottoms out and every score is 0). The
// corpus has the PLAN accept case: "tmux logout" hits an entry titled "Tmux Notes".
func newExplainRepo(t *testing.T) string {
	t.Helper()
	return newExplainRepoWith(t, explainEntries)
}

// explainEntry is one fixture entry of an explain repo: its chunks as (heading, text) pairs.
type explainEntry struct {
	path, title string
	chunks      [][2]string
}

// newExplainRepoWith indexes entries with the fixture model into a fresh search test repo.
func newExplainRepoWith(t *testing.T, entries []explainEntry) string {
	t.Helper()
	root := newSearchTestRepo(t, testModel768, testDim768)
	ctx := context.Background()
	st, err := sqlite.Open(ctx, testDBPath(root), sqlite.Options{})
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	emb := fake.New(testModel768, testDim768)
	for _, e := range entries {
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
			Summary: "fixture", BodyHash: "hash-" + e.path}, chunks); err != nil {
			t.Fatal(err)
		}
	}
	return root
}

// explainEntries are newExplainRepo's five entries.
var explainEntries = []explainEntry{
	{"tmux.md", "Tmux Notes", [][2]string{
		{"", "Tmux Notes: sessions survive a logout when the server keeps running"},
		{"Detach", "press the prefix then d to detach, and the session stays alive after logout"},
	}},
	{"ssh.md", "SSH Config", [][2]string{{"", "SSH Config: keepalive settings stop a logout from dropping the link"}}},
	{"git.md", "Git Tips", [][2]string{{"", "Git Tips: rebase onto main before opening a pull request"}}},
	{"docker.md", "Docker Volumes", [][2]string{{"", "Docker Volumes: named volumes outlive their container"}}},
	{"cache.md", "Build Cache", [][2]string{{"", "Build Cache: layers are reused when the inputs are unchanged"}}},
}

// withFixtureEmbedder makes the CLI embed with a fake of the fixture's model and width.
func withFixtureEmbedder(t *testing.T) {
	t.Helper()
	orig := depsFor(t).embedder
	depsFor(t).embedder = func(config.Embed) (embed.Embedder, error) { return fake.New(testModel768, testDim768), nil }
	t.Cleanup(func() { depsFor(t).embedder = orig })
}

// explainGoldenDir is resolved at init: tests chdir into a temp root.
var explainGoldenDir = func() string {
	wd, err := os.Getwd()
	if err != nil {
		panic(err)
	}
	return filepath.Join(wd, "testdata", "golden")
}()

func checkExplainGolden(t *testing.T, name, got string) {
	t.Helper()
	path := filepath.Join(explainGoldenDir, name)
	if *updateExplain {
		if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path, []byte(got), 0o644); err != nil {
			t.Fatal(err)
		}
		return
	}
	want, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read golden: %v (run with -update-explain)", err)
	}
	if string(want) != got {
		t.Errorf("output differs from %s (run with -update-explain and review the diff):\n--- got\n%s\n--- want\n%s", path, got, want)
	}
}

// TestExplainGoldenFTS pins the human output of an FTS explain: the header, the AND match, the
// per-column scores (a title term: the title column is nonzero) and the RRF arithmetic.
func TestExplainGoldenFTS(t *testing.T) {
	t.Parallel()
	useRoot(t, newExplainRepo(t))
	stdout, stderr, exit := runKB(t, []string{"explain", "tmux", "logout", "--mode", "fts"})
	if exit != 0 || stderr != "" {
		t.Fatalf("exit %d stderr %q", exit, stderr)
	}
	checkExplainGolden(t, "explain_fts.golden", stdout)
	if !strings.Contains(stdout, `fts: match "tmux" AND "logout" · AND`) {
		t.Errorf("no AND match line:\n%s", stdout)
	}
}

// TestExplainGoldenHybrid pins a hybrid explain: both lists, both RRF terms, vector distance.
func TestExplainGoldenHybrid(t *testing.T) {
	t.Parallel()
	useRoot(t, newExplainRepo(t))
	withFixtureEmbedder(t)
	stdout, stderr, exit := runKB(t, []string{"explain", "tmux logout"})
	if exit != 0 || stderr != "" {
		t.Fatalf("exit %d stderr %q", exit, stderr)
	}
	checkExplainGolden(t, "explain_hybrid.golden", stdout)
}

// TestExplainJSONShape pins the --json contract: the key sets of the top level, a candidate and
// its parts, with null (not absent) for what a hit lacks.
func TestExplainJSONShape(t *testing.T) {
	t.Parallel()
	useRoot(t, newSearchTestRepo(t, testModel768, testDim768))
	withFixtureEmbedder(t)
	stdout, stderr, exit := runKB(t, []string{"explain", "widget", "--json"})
	if exit != 0 {
		t.Fatalf("exit %d: %s", exit, stderr)
	}
	var top map[string]json.RawMessage
	if err := json.Unmarshal([]byte(stdout), &top); err != nil {
		t.Fatalf("json: %v\n%s", err, stdout)
	}
	wantKeys(t, "top", top, "candidates", "exact", "fts", "k", "mode", "n_exact", "n_fts", "n_vec", "prior", "query", "settings", "settings_hash")
	if string(top["exact"]) != "null" {
		t.Errorf("exact = %s, want null: widget holds no identifier, so no exact list ran", top["exact"])
	}
	var settings map[string]json.RawMessage
	_ = json.Unmarshal(top["settings"], &settings)
	wantKeys(t, "settings", settings, "bm25_heading", "bm25_text", "bm25_title", "candidate_limit", "exact_list",
		"exact_weight", "fts_weight", "list_limit", "per_entry_cap", "phrase_boost", "rrf_k", "supersede_penalty", "vec_weight")
	var fts map[string]json.RawMessage
	_ = json.Unmarshal(top["fts"], &fts)
	wantKeys(t, "fts", fts, "match", "op", "or_fallback")

	var cands []map[string]json.RawMessage
	if err := json.Unmarshal(top["candidates"], &cands); err != nil || len(cands) != 2 {
		t.Fatalf("candidates: %v %s", err, top["candidates"])
	}
	for _, c := range cands {
		wantKeys(t, "candidate", c, "chunk_id", "columns", "exact", "fts", "heading", "path", "rank", "returned_rank",
			"rrf_score", "rrf_terms", "status", "vec")
		var f, v, col map[string]json.RawMessage
		_ = json.Unmarshal(c["fts"], &f)
		_ = json.Unmarshal(c["vec"], &v)
		_ = json.Unmarshal(c["columns"], &col)
		wantKeys(t, "candidate.fts", f, "bm25", "rank")
		wantKeys(t, "candidate.vec", v, "distance", "rank")
		wantKeys(t, "candidate.columns", col, "heading", "text", "title")
	}
}

func wantKeys(t *testing.T, what string, m map[string]json.RawMessage, want ...string) {
	t.Helper()
	got := make([]string, 0, len(m))
	for k := range m {
		got = append(got, k)
	}
	sort.Strings(got)
	sort.Strings(want)
	if strings.Join(got, ",") != strings.Join(want, ",") {
		t.Errorf("%s keys = %v, want %v", what, got, want)
	}
}

// TestExplainDoesNotLogOrChangeIDs: explain leaves the search log alone, so the next real search
// is still search 1.
func TestExplainDoesNotLog(t *testing.T) {
	t.Parallel()
	useRoot(t, newSearchTestRepo(t, testModel768, testDim768))
	if _, stderr, exit := runKB(t, []string{"explain", "widget", "--mode", "fts"}); exit != 0 {
		t.Fatalf("explain: exit %d %s", exit, stderr)
	}
	stdout, _, exit := runKB(t, []string{"search", "widget", "--mode", "fts"})
	if exit != 0 || !strings.HasPrefix(stdout, "search 1 · ") {
		t.Errorf("first logged search = %q, want it to be search 1 (exit %d)", strings.SplitN(stdout, "\n", 2)[0], exit)
	}
}

// TestExplainSetOverride: --set changes the settings explain runs with and hashes them.
func TestExplainSetOverride(t *testing.T) {
	t.Parallel()
	useRoot(t, newSearchTestRepo(t, testModel768, testDim768))
	plain, _, _ := runKB(t, []string{"explain", "widget", "--mode", "fts"})
	set, stderr, exit := runKB(t, []string{"explain", "widget", "--mode", "fts", "--set", "search.rrf_k=1", "--set", "search.bm25.title=9"})
	if exit != 0 {
		t.Fatalf("exit %d %s", exit, stderr)
	}
	if !strings.Contains(set, "rrf_k=1 ") || !strings.Contains(set, "title=9 ") || !strings.Contains(set, "1/(1+1)=0.500000") {
		t.Errorf("--set not reflected:\n%s", set)
	}
	if line := func(s string) string { return strings.SplitN(s, "\n", 2)[0] }; line(plain) == line(set) {
		t.Errorf("settings hash did not change with --set: %q", line(set))
	}
}

// TestExplainFlagErrors is the usage-error table: each is exit 1 with the problem named.
func TestExplainFlagErrors(t *testing.T) {
	t.Parallel()
	root := newSearchTestRepo(t, testModel768, testDim768)
	useRoot(t, root)
	for _, tc := range []struct {
		name string
		args []string
		want string
	}{
		{"bad mode", []string{"explain", "x", "--mode", "bogus"}, "invalid --mode"},
		{"bad set key", []string{"explain", "x", "--set", "search.nope=1"}, "--set search.nope=1"},
		{"bad set value", []string{"explain", "x", "--set", "search.rrf_k=0"}, "rrf_k"},
		{"set to an unknown model", []string{"explain", "x", "--set", "embed.model=nope"}, "--set"},
		{"blank query", []string{"explain", " ", "--mode", "fts"}, "empty query"},
		{"no query", []string{"explain"}, "requires at least 1 arg"},
		{"vec with none embedder", []string{"explain", "x", "--mode", "vec", "--set", "embed.model=none"}, "embedder is none"},
	} {
		_, stderr, exit := runKB(t, tc.args)
		if exit != 1 || !strings.Contains(stderr, tc.want) {
			t.Errorf("%s: exit %d stderr %q, want exit 1 naming %q", tc.name, exit, stderr, tc.want)
		}
	}
	if err := os.Remove(testDBPath(root)); err != nil {
		t.Fatal(err)
	}
	if _, stderr, exit := runKB(t, []string{"explain", "x"}); exit != 1 || !strings.Contains(stderr, "no index at") {
		t.Errorf("no database: exit %d stderr %q", exit, stderr)
	}
}

// TestExplainNoResults: a query nothing matches prints the header and "no candidates", exit 0.
func TestExplainNoResults(t *testing.T) {
	t.Parallel()
	useRoot(t, newSearchTestRepo(t, testModel768, testDim768))
	stdout, _, exit := runKB(t, []string{"explain", "fluffernutter", "--mode", "fts"})
	if exit != 0 || !strings.Contains(stdout, "no candidates") || !strings.Contains(stdout, `match "fluffernutter" · AND`) {
		t.Errorf("exit %d:\n%s", exit, stdout)
	}
}

// TestExplainEmbedderUnavailable: hybrid with no downloaded model explains the fts fallback,
// warns on stderr and exits 2 like kb search.
func TestExplainEmbedderUnavailable(t *testing.T) {
	t.Parallel()
	useRoot(t, newSearchTestRepo(t, testModel768, testDim768))
	stdout, stderr, exit := runKB(t, []string{"explain", "widget"})
	if exit != ExitEmbedder || !strings.Contains(stderr, "explained --mode fts instead") ||
		!strings.Contains(stdout, "mode fts-fallback") || !strings.Contains(stdout, "fallback: ") {
		t.Errorf("exit %d\nstdout:\n%s\nstderr:\n%s", exit, stdout, stderr)
	}
}

// TestExplainFormatHelpers: the small formatters at their edges (a missing score, negative zero,
// a short and a full-length hash).
func TestExplainFormatHelpers(t *testing.T) {
	t.Parallel()
	negZero := math.Copysign(0, -1)
	one := -1.23456
	for _, tc := range []struct{ got, want string }{
		{col(nil), "-"}, {col(&negZero), "0.0000"}, {col(&one), "-1.2346"},
		{shortHash(""), ""}, {shortHash("abc"), "abc"}, {shortHash("0123456789ab"), "0123456789ab"}, {shortHash("0123456789abc"), "0123456789ab"},
		{num(1), "1"}, {num(0.5), "0.5"},
	} {
		if tc.got != tc.want {
			t.Errorf("got %q, want %q", tc.got, tc.want)
		}
	}
}

// identEntries are explainEntries plus three entries naming Go identifiers, two of them with the
// identifier's words split apart (which the main FTS list also matches).
var identEntries = append(slices.Clone(explainEntries),
	explainEntry{"store.md", "Store Notes", [][2]string{
		{"", "Store Notes: how the index records its model"},
		{"Embed meta", "EnsureEmbedMeta records the model and dim on a fresh index; reindex_test covers it"},
	}},
	explainEntry{"meta.md", "Meta Notes", [][2]string{{"", "Meta Notes: ensure embed meta is recorded before a reindex test"}}},
	explainEntry{"flags.md", "Flags", [][2]string{{"", "Flags: pass --embed-model to choose the model; see kb.toml"}}},
)

// TestExplainExactList is the PLAN 4.5 accept case: kb explain "EnsureEmbedMeta" shows the chunk
// holding it at exact rank 1, with an exact RRF term; a query whose identifier's words are split
// in other chunks ranks the verbatim chunk first; exact_list=false removes the list.
func TestExplainExactList(t *testing.T) {
	t.Parallel()
	useRoot(t, newExplainRepoWith(t, identEntries))
	withFixtureEmbedder(t)
	stdout, stderr, exit := runKB(t, []string{"explain", "EnsureEmbedMeta"})
	if exit != 0 || stderr != "" {
		t.Fatalf("exit %d stderr %q", exit, stderr)
	}
	checkExplainGolden(t, "explain_exact.golden", stdout)
	for _, want := range []string{`exact: match "EnsureEmbedMeta" · AND · weight 1`, "exact rank 1  bm25", "exact 1/(60+1)="} {
		if !strings.Contains(stdout, want) {
			t.Errorf("no %q in:\n%s", want, stdout)
		}
	}

	stdout, _, _ = runKB(t, []string{"explain", "reindex_test", "--mode", "fts", "--set", "search.exact_list=false"})
	if !strings.Contains(stdout, " per_entry_cap=3 exact=off\n") {
		t.Errorf("settings line lacks exact=off with the exact list turned off:\n%s", stdout)
	}

	for _, tc := range []struct {
		args []string
		want string // path of the top candidate
		ran  bool   // an exact list ran
	}{
		{[]string{"explain", "reindex_test", "--mode", "fts", "--json"}, "store.md", true},
		{[]string{"explain", "--mode", "fts", "--json", "--", "--embed-model"}, "flags.md", true}, // -- ends the flags
		{[]string{"explain", "kb.toml", "--json"}, "flags.md", true},
		{[]string{"explain", "reindex_test", "--mode", "fts", "--json", "--set", "search.exact_list=false"}, "", false},
		{[]string{"explain", "reindex_test", "--mode", "vec", "--json"}, "", false},
	} {
		stdout, stderr, exit := runKB(t, tc.args)
		if exit != 0 {
			t.Fatalf("%v: exit %d %s", tc.args, exit, stderr)
		}
		var ex struct {
			Exact      *struct{ Match string }
			Candidates []struct {
				Path  string
				Exact *struct{ Rank int }
			}
		}
		if err := json.Unmarshal([]byte(stdout), &ex); err != nil {
			t.Fatalf("%v: %v", tc.args, err)
		}
		if (ex.Exact != nil) != tc.ran {
			t.Errorf("%v: exact = %+v, want ran=%v", tc.args, ex.Exact, tc.ran)
		}
		if !tc.ran {
			for _, c := range ex.Candidates {
				if c.Exact != nil {
					t.Errorf("%v: %s has an exact rank with no exact list", tc.args, c.Path)
				}
			}
			continue
		}
		if len(ex.Candidates) == 0 || ex.Candidates[0].Path != tc.want || ex.Candidates[0].Exact == nil || ex.Candidates[0].Exact.Rank != 1 {
			t.Errorf("%v: top candidate %+v, want %s at exact rank 1", tc.args, ex.Candidates, tc.want)
		}
	}
}

// TestExplainPhraseTier is the PLAN 4.6 accept case in the CLI: a multi-word query shows the
// tiers line with each tier's expression and row count, and each candidate the tier that placed
// it (the chunk holding the words adjacent is the phrase tier, ahead of the NEAR and AND ones);
// phrase_boost=false removes the tiers; a bad value is a usage error.
func TestExplainPhraseTier(t *testing.T) {
	t.Parallel()
	useRoot(t, newExplainRepo(t))
	stdout, stderr, exit := runKB(t, []string{"explain", "alive after logout", "--mode", "fts"})
	if exit != 0 || stderr != "" {
		t.Fatalf("exit %d stderr %q", exit, stderr)
	}
	checkExplainGolden(t, "explain_phrase.golden", stdout)
	for _, want := range []string{
		`tiers: phrase "alive after logout" (1 row) · near NEAR("alive" "after" "logout", 8) (1 row) · and "alive" AND "after" AND "logout" (1 row)`,
		"    fts  rank 1  tier phrase  bm25 ",
	} {
		if !strings.Contains(stdout, want) {
			t.Errorf("no %q in:\n%s", want, stdout)
		}
	}

	stdout, _, exit = runKB(t, []string{"explain", "alive after logout", "--mode", "fts", "--set", "search.phrase_boost=false"})
	if exit != 0 || !strings.Contains(stdout, "tiers: off (search.phrase_boost = false)\n") || strings.Contains(stdout, "tier phrase") {
		t.Errorf("phrase_boost=false: exit %d, want the tiers: off line and no tier on a candidate:\n%s", exit, stdout)
	}

	stdout, _, exit = runKB(t, []string{"explain", "alive after logout", "--mode", "fts", "--json"})
	if exit != 0 {
		t.Fatalf("exit %d", exit)
	}
	var ex search.Explanation
	if err := json.Unmarshal([]byte(stdout), &ex); err != nil {
		t.Fatal(err)
	}
	if ex.FTS == nil || len(ex.FTS.Tiers) != 3 || ex.FTS.Tiers[0].Tier != "phrase" || ex.Candidates[0].FTS.Tier != "phrase" {
		t.Errorf("JSON tiers = %+v, first candidate %+v", ex.FTS, ex.Candidates[0].FTS)
	}

	// An AND that matches nothing falls back to OR, which is the only tier that ran.
	stdout, _, exit = runKB(t, []string{"explain", "alive zzzunknown", "--mode", "fts"})
	if exit != 0 || !strings.Contains(stdout, `tiers: or "alive" OR "zzzunknown" (1 row)`+"\n") || !strings.Contains(stdout, "    fts  rank 1  tier or  bm25 ") {
		t.Errorf("OR fallback: exit %d, want a one-tier tiers line and tier or:\n%s", exit, stdout)
	}

	_, stderr, exit = runKB(t, []string{"explain", "x y", "--mode", "fts", "--set", "search.phrase_boost=maybe"})
	if exit != 1 || !strings.Contains(stderr, "phrase_boost") {
		t.Errorf("bad phrase_boost: exit %d stderr %q, want a usage error naming the key", exit, stderr)
	}
}
