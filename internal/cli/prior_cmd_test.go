package cli

import (
	"encoding/json"
	"regexp"
	"strings"
	"testing"
)

// priorHits runs kb search widget --json (fts) with extra flags and returns each hit's decoded
// object.
func priorHits(t *testing.T, flags ...string) []map[string]json.RawMessage {
	t.Helper()
	stdout, stderr, exit := runKB(t, append([]string{"search", "widget", "--mode", "fts", "--json"}, flags...))
	if exit != 0 {
		t.Fatalf("search %v: exit %d: %s", flags, exit, stderr)
	}
	var res struct {
		Hits []map[string]json.RawMessage `json:"hits"`
	}
	if err := json.Unmarshal([]byte(stdout), &res); err != nil {
		t.Fatalf("json: %v\n%s", err, stdout)
	}
	if len(res.Hits) != 2 {
		t.Fatalf("%d hits, want 2:\n%s", len(res.Hits), stdout)
	}
	return res.Hits
}

// TestPriorEndToEnd is PLAN 6.2's accept case through the CLI: with [prior] enabled and too few
// judged searches, kb explain says "prior inactive: N/M judged searches" and kb search carries no
// "prior" field; once M searches are judged the prior applies (a "prior" field per hit, a term in
// explain) and --no-prior turns it off for one search.
func TestPriorEndToEnd(t *testing.T) {
	root := newSearchTestRepo(t, testModel768, testDim768)
	useRoot(t, root)
	writeConfig(t, root, "[embed]\nmodel = \""+testModel768+"\"\n[prior]\nenabled = true\nmin_judged = 2\n")

	explain := func(args ...string) string {
		t.Helper()
		stdout, stderr, exit := runKB(t, append([]string{"explain", "widget", "--mode", "fts"}, args...))
		if exit != 0 {
			t.Fatalf("explain %v: exit %d: %s", args, exit, stderr)
		}
		return stdout
	}

	for _, h := range priorHits(t) {
		if _, ok := h["prior"]; ok {
			t.Errorf("a hit carries a prior with nothing judged: %s", h["prior"])
		}
	}
	if out := explain(); !strings.Contains(out, "prior inactive: 0/2 judged searches\n") || strings.Contains(out, "prior  x") {
		t.Errorf("explain with nothing judged:\n%s", out)
	}
	judge := func(searchID string) {
		t.Helper()
		if _, stderr, exit := runKB(t, []string{"feedback", searchID, "2", "--useful"}); exit != 0 {
			t.Fatalf("feedback: exit %d: %s", exit, stderr)
		}
	}
	judge("1") // the search above
	if out := explain(); !strings.Contains(out, "prior inactive: 1/2 judged searches\n") {
		t.Errorf("explain with one judged search:\n%s", out)
	}
	priorHits(t) // search 2
	judge("2")

	hits := priorHits(t)
	for i, h := range hits {
		var mult float64
		if err := json.Unmarshal(h["prior"], &mult); err != nil || mult < 0.8 || mult > 1.2 {
			t.Errorf("hit %d prior = %s (%v), want a multiplier within [0.8, 1.2]", i+1, h["prior"], err)
		}
	}
	// The chunk judged useful (heading Configuration) outranks the other and has a multiplier above 1.
	var heading string
	_ = json.Unmarshal(hits[0]["heading"], &heading)
	var top float64
	_ = json.Unmarshal(hits[0]["prior"], &top)
	if heading != "Configuration" || top <= 1 {
		t.Errorf("top hit = %q with prior %v, want the judged Configuration chunk boosted", heading, top)
	}
	out := explain()
	for _, want := range []string{"prior: active · 2/2 judged searches · half-life 90 days · max boost 0.2\n"} {
		if !strings.Contains(out, want) {
			t.Errorf("explain lacks %q:\n%s", want, out)
		}
	}
	if !regexp.MustCompile(`(?m)^    prior  x1\.\d{4}$`).MatchString(out) {
		t.Errorf("explain shows no per-candidate prior term:\n%s", out)
	}
	var ex struct {
		Prior struct {
			Active    bool `json:"active"`
			Judged    int  `json:"judged"`
			MinJudged int  `json:"min_judged"`
		} `json:"prior"`
	}
	if err := json.Unmarshal([]byte(explain("--json")), &ex); err != nil || !ex.Prior.Active || ex.Prior.Judged != 2 || ex.Prior.MinJudged != 2 {
		t.Errorf("explain --json prior = %+v, %v", ex.Prior, err)
	}

	for _, h := range priorHits(t, "--no-prior") {
		if _, ok := h["prior"]; ok {
			t.Errorf("--no-prior hit carries a prior: %s", h["prior"])
		}
	}
	// A --set on explain tries a [prior] setting without writing it.
	if out := explain("--set", "prior.min_judged=5"); !strings.Contains(out, "prior inactive: 2/5 judged searches\n") {
		t.Errorf("explain --set prior.min_judged=5:\n%s", out)
	}
}

// TestPriorOffByDefault: the default config shows nothing about the prior and applies none, even
// with judged searches.
func TestPriorOffByDefault(t *testing.T) {
	root := newSearchTestRepo(t, testModel768, testDim768)
	useRoot(t, root)
	priorHits(t)
	if _, stderr, exit := runKB(t, []string{"feedback", "1", "2", "--useful"}); exit != 0 {
		t.Fatalf("feedback: %d %s", exit, stderr)
	}
	for _, h := range priorHits(t) {
		if _, ok := h["prior"]; ok {
			t.Errorf("prior applied by default: %s", h["prior"])
		}
	}
	stdout, _, exit := runKB(t, []string{"explain", "widget", "--mode", "fts"})
	if exit != 0 || strings.Contains(stdout, "prior") {
		t.Errorf("explain mentions the prior while it is off (exit %d):\n%s", exit, stdout)
	}
}

// TestPriorBadSettings: an out-of-range [prior] value is a Usage error (exit 1) naming the key, on
// both the --set path and a config file, and a --no-prior with a value is a flag error.
func TestPriorBadSettings(t *testing.T) {
	root := newSearchTestRepo(t, testModel768, testDim768)
	useRoot(t, root)
	_, stderr, exit := runKB(t, []string{"explain", "widget", "--mode", "fts", "--set", "prior.half_life_days=0"})
	if exit != 1 || !strings.Contains(stderr, "prior.half_life_days") {
		t.Errorf("--set prior.half_life_days=0: exit %d, stderr %q; want exit 1 naming the key", exit, stderr)
	}
	writeConfig(t, root, "[embed]\nmodel = \""+testModel768+"\"\n[prior]\nmax_boost = 1.5\n")
	_, stderr, exit = runKB(t, []string{"search", "widget", "--mode", "fts"})
	if exit != 1 || !strings.Contains(stderr, "prior.max_boost") {
		t.Errorf("max_boost 1.5 in the config: exit %d, stderr %q; want exit 1 naming the key", exit, stderr)
	}
	_, _, exit = runKB(t, []string{"search", "widget", "--mode", "fts", "--no-prior=maybe"})
	if exit != 1 {
		t.Errorf("--no-prior=maybe: exit %d, want 1", exit)
	}
}

// TestQueryMemoryEndToEnd is PLAN 6.3's accept case through the CLI: after a hit is marked useful
// for a query, the same query (other case, same words) puts it at rank 1 and explain shows its
// memory rank and the key; another query is unchanged. The prior's own boost is off
// (max_boost = 0) so the order is the memory list's doing.
func TestQueryMemoryEndToEnd(t *testing.T) {
	root := newSearchTestRepo(t, testModel768, testDim768)
	useRoot(t, root)
	writeConfig(t, root, "[embed]\nmodel = \""+testModel768+"\"\n[prior]\nenabled = true\nmin_judged = 1\nmax_boost = 0\nquery_memory = true\n")

	hits := func(args ...string) []map[string]json.RawMessage {
		t.Helper()
		return priorHits(t, args...)
	}
	heading := func(h map[string]json.RawMessage) string {
		var s string
		_ = json.Unmarshal(h["heading"], &s)
		return s
	}
	first := hits()
	if heading(first[0]) == "Configuration" {
		t.Fatalf("the fixture already ranks Configuration first: %v", first)
	}
	if _, stderr, exit := runKB(t, []string{"feedback", "1", "2", "--useful"}); exit != 0 {
		t.Fatalf("feedback: exit %d: %s", exit, stderr)
	}

	again := hits()
	if heading(again[0]) != "Configuration" {
		t.Errorf("repeated query: top = %q, want the remembered Configuration chunk", heading(again[0]))
	}
	var rank int
	if err := json.Unmarshal(again[0]["memory_rank"], &rank); err != nil || rank != 1 {
		t.Errorf("memory_rank = %s (%v), want 1", again[0]["memory_rank"], err)
	}
	if _, ok := again[1]["memory_rank"]; ok {
		t.Errorf("the other hit carries a memory_rank: %s", again[1]["memory_rank"])
	}

	stdout, stderr, exit := runKB(t, []string{"explain", "WIDGET", "--mode", "fts"})
	if exit != 0 {
		t.Fatalf("explain: exit %d: %s", exit, stderr)
	}
	if !strings.Contains(stdout, "memory: key \"widget\" · weight 1 · 1 earlier judged search · 1 row\n") ||
		!regexp.MustCompile(`(?m)^    memory rank 1  u \d\.\d{4}$`).MatchString(stdout) ||
		!strings.Contains(stdout, "memory 1/(60+1)=") {
		t.Errorf("explain shows no memory:\n%s", stdout)
	}
	var ex struct {
		Memory struct {
			Active bool   `json:"active"`
			Key    string `json:"key"`
			Items  int    `json:"items"`
		} `json:"memory"`
		Candidates []struct {
			Heading string `json:"heading"`
			Memory  *struct {
				Rank int `json:"rank"`
			} `json:"memory"`
		} `json:"candidates"`
	}
	stdout, _, _ = runKB(t, []string{"explain", "widget", "--mode", "fts", "--json"})
	if err := json.Unmarshal([]byte(stdout), &ex); err != nil || !ex.Memory.Active || ex.Memory.Key != "widget" || ex.Memory.Items != 1 ||
		ex.Candidates[0].Heading != "Configuration" || ex.Candidates[0].Memory == nil || ex.Candidates[0].Memory.Rank != 1 {
		t.Errorf("explain --json = %+v, %v", ex, err)
	}

	// --no-prior turns the memory off too; another key remembers nothing.
	for _, h := range hits("--no-prior") {
		if _, ok := h["memory_rank"]; ok {
			t.Errorf("--no-prior hit carries a memory_rank: %s", h["memory_rank"])
		}
	}
	if out, _, _ := runKB(t, []string{"explain", "widget subsystem", "--mode", "fts"}); !strings.Contains(out, "memory: key \"subsystem widget\" · weight 1 · 0 earlier judged searches · 0 rows\n") {
		t.Errorf("explain with another key:\n%s", out)
	}
}
