package main

// The exact-identifier list (PLAN 4.5): a query holding an identifier also searches the
// identifier index (chunks_exact, PLAN 4.5). Built with the harnessfake tag for the fake:<dim>
// model, so hybrid search has vectors without a download.

import (
	"encoding/json"
	"strings"
)

func init() {
	scenarios = append(scenarios, scenario{"exact-identifiers",
		"identifier queries (snake_case, --flag after --, file.ext, a/b:c, camelCase, unicode, a 3000-rune identifier) rank the chunk holding the " +
			"identifier verbatim first with exact_rank 1 in hybrid and fts; hostile identifier queries exit 0 with valid JSON; explain shows the exact " +
			"list, --set search.exact_list=false removes it and a bad list_weights.exact exits 1; kb rm and a rewritten entry leave no exact rows; " +
			"stats sources reports with_exact",
		scExactIdentifiers})
}

// exactHit is the part of a kb search --json hit this scenario reads.
type exactHit struct {
	Path      string `json:"path"`
	ExactRank *int   `json:"exact_rank"`
}

func scExactIdentifiers(h *H) {
	h.must(h.kb("init", "--model", "fake:16").Code == 0, "init with the fake model (is kb built with -tags harnessfake?)")
	h.setConfig("[embed]\nmodel = \"fake:16\"\n", false)
	long := strings.Repeat("y", 3000) + "_tail"
	h.write("data/flags.md", entryText("Flags", "## Embed\n\nPass --embed-model to choose the embedder.\n"))
	h.write("data/split.md", entryText("Split", "## Words\n\nThe embed model flag picks an embed model; reindex test helpers test the reindex; kb toml; a b c; ensure embed meta.\n"))
	h.write("data/reindex.md", entryText("Reindex", "## Tests\n\nreindex_test covers vector reuse.\n"))
	h.write("data/config.md", entryText("Config", "## Keys\n\nThe file kb.toml holds aliases; see a/b:c for paths.\n"))
	h.write("data/camel.md", entryText("Camel", "## Meta\n\nEnsureEmbedMeta records the model; café_au_lait is unicode.\n"))
	h.write("data/long.md", entryText("Long", "## Long\n\nthe token "+long+" is long.\n"))
	h.must(h.kb("reindex", "--all").Code == 0, "reindex --all")

	search := func(args ...string) ([]exactHit, bool) {
		r := h.kb(append([]string{"search", "--json"}, args...)...)
		if !h.expect(r, 0) {
			return nil, false
		}
		var out struct {
			Hits []exactHit `json:"hits"`
		}
		if err := json.Unmarshal([]byte(r.Stdout), &out); err != nil {
			h.fail("kb %s: invalid JSON: %v\n%s", strings.Join(r.Args, " "), err, r)
			return nil, false
		}
		return out.Hits, true
	}
	for _, tc := range []struct{ query, want string }{
		{"reindex_test", "data/reindex.md"},
		{"--embed-model", "data/flags.md"},
		{"kb.toml", "data/config.md"},
		{"a/b:c", "data/config.md"},
		{"EnsureEmbedMeta", "data/camel.md"},
		{"café_au_lait", "data/camel.md"},
		{long, "data/long.md"},
		{"how is reindex_test run?", "data/reindex.md"},
	} {
		for _, mode := range []string{"hybrid", "fts"} {
			hits, ok := search("--mode", mode, "--", tc.query)
			if !ok {
				continue
			}
			if len(hits) == 0 || hits[0].Path != tc.want || hits[0].ExactRank == nil || *hits[0].ExactRank != 1 {
				h.fail("%s %.40q: top hit %+v, want %s with exact_rank 1", mode, tc.query, hits, tc.want)
			}
		}
	}

	// Hostile identifier queries: FTS5 syntax inside identifiers never reaches the parser.
	for _, q := range []string{
		`"foo_bar`, "a_*", "NEAR(a_b", "title:x_y", "text:kb.toml", "a_b OR c.d", "-- ... :: //", `x_"y"_z`,
		strings.Repeat("a_b ", 500), "日本語_識別子", "_", "-e", "^x.y", "{a.b}", "x.y*",
	} {
		search("--mode", "fts", "--", q)
	}

	// explain shows the list; turning it off removes it; a bad weight is refused.
	explain := func(args ...string) map[string]json.RawMessage {
		r := h.kb(append([]string{"explain", "--json"}, args...)...)
		if !h.expect(r, 0) {
			return nil
		}
		var ex map[string]json.RawMessage
		if err := json.Unmarshal([]byte(r.Stdout), &ex); err != nil {
			h.fail("explain: invalid JSON: %v\n%s", err, r)
		}
		return ex
	}
	if ex := explain("--", "kb.toml"); ex != nil && (string(ex["exact"]) == "null" || string(ex["n_exact"]) != "1") {
		h.fail("explain kb.toml: exact %s n_exact %s, want the list with 1 row", ex["exact"], ex["n_exact"])
	}
	if ex := explain("--set", "search.exact_list=false", "--", "kb.toml"); ex != nil && string(ex["exact"]) != "null" {
		h.fail("explain kb.toml with exact_list=false: exact %s, want null", ex["exact"])
	}
	explain("--set", "search.list_weights.exact=0", "--", "kb.toml")
	for _, bad := range []string{"search.list_weights.exact=-1", "search.list_weights.exact=nan", "search.exact_list=maybe"} {
		h.expect(h.kb("explain", "--set", bad, "--", "kb.toml"), 1)
	}

	// The write paths keep the identifier index in step: a removed entry and a rewritten one
	// leave no exact rows behind.
	h.expect(h.kb("rm", "data/reindex.md", "--yes"), 0)
	if ex := explain("--mode", "fts", "--", "reindex_test"); ex != nil && string(ex["n_exact"]) != "0" {
		h.fail("after kb rm: explain reindex_test n_exact %s, want 0", ex["n_exact"])
	}
	h.write("data/flags.md", entryText("Flags", "## Embed\n\nPass --model-name instead.\n"))
	h.expect(h.kb("reindex", "data/flags.md"), 0)
	if ex := explain("--mode", "fts", "--", "--embed-model"); ex != nil && string(ex["n_exact"]) != "0" {
		h.fail("after rewriting flags.md: explain --embed-model n_exact %s, want 0", ex["n_exact"])
	}
	if ex := explain("--mode", "fts", "--", "--model-name"); ex != nil && string(ex["n_exact"]) != "1" {
		h.fail("after rewriting flags.md: explain --model-name n_exact %s, want 1", ex["n_exact"])
	}
	h.expect(h.kb("reindex", "--all"), 0)
	if ex := explain("--mode", "fts", "--", "kb.toml"); ex != nil && string(ex["n_exact"]) != "1" {
		h.fail("after reindex --all: explain kb.toml n_exact %s, want 1 (ResetIndex then rebuild)", ex["n_exact"])
	}

	if r := h.kb("stats", "sources"); h.expect(r, 0) && !strings.Contains(r.Stdout, "with_exact") {
		h.fail("stats sources lacks the with_exact column:\n%s", r)
	}
}
