package main

// kb eval with the feedback prior and the query memory (PLAN 6.4) on the real binary, FTS only.

import (
	"encoding/json"
	"strings"
	"time"
)

func init() {
	scenarios = append(scenarios, scenario{"eval-prior",
		"kb eval --set prior.enabled=true (and query_memory): compare prints deltas and the prior line; --json carries prior " +
			"counts (active <= enabled = replays); eval never writes the log; feedback stamped in the future or after every search " +
			"never activates the prior; a [prior] sweep below eval.min_judged is refused even with --force; a hand-damaged search " +
			"timestamp exits 1 naming the search; a 10k-search judged log evaluates with the prior in one read",
		scEvalPrior})
}

type evalPriorReport struct {
	Prior struct {
		Enabled int `json:"enabled"`
		Active  int `json:"active"`
		Memory  int `json:"memory"`
	} `json:"prior"`
	Chunk struct {
		Queries int `json:"queries"`
	} `json:"chunk"`
}

func scEvalPrior(h *H) {
	h.write("data/p1.md", entryText("P one", "## Body\n\nsprocket calibration steps.\n"))
	h.write("data/p2.md", entryText("P two", "## Body\n\nsprocket calibration history.\n"))
	h.write("data/p3.md", entryText("P three", "## Body\n\nsprocket wear.\n"))
	h.setConfig("[embed]\nmodel = \"none\"\n[prior]\nmin_judged = 1\n", false)
	h.must(h.kb("reindex", "--all").Code == 0, "index with an FTS-only config")

	// A judged log with explicit times: searches an hour apart, each judged a minute later.
	h.helper("exec", h.dbPath(), `
		WITH RECURSIVE seq(i) AS (SELECT 1 UNION ALL SELECT i + 1 FROM seq WHERE i < 6)
		INSERT INTO searches (id, ts, query, mode, k, n_returned, caller, kb_version, settings, settings_hash)
		SELECT i, strftime('%Y-%m-%dT%H:%M:%SZ', '2026-01-01 00:00:00', '+' || i || ' hours'),
		       CASE i % 3 WHEN 0 THEN 'sprocket calibration' WHEN 1 THEN 'calibration sprocket' ELSE 'sprocket wear' END,
		       'fts', 8, 3, 'h', 'x', '{}', 'h' FROM seq;
		WITH RECURSIVE seq(i) AS (SELECT 1 UNION ALL SELECT i + 1 FROM seq WHERE i < 6), r(k) AS (VALUES (1), (2), (3))
		INSERT INTO search_results (search_id, rank, chunk_id, entry_path, heading, rrf_score)
		SELECT i, k, 1, 'data/p' || k || '.md', 'Body', 0.01 FROM seq, r;
		INSERT INTO search_feedback (search_id, rank, useful, ts)
		SELECT id, 2, 1, strftime('%Y-%m-%dT%H:%M:%SZ', ts, '+1 minutes') FROM searches;`)

	count := func() string { return h.kb("stats", "--sql", "SELECT count(*) AS n FROM searches", "--json").Stdout }
	report := func(args ...string) (evalPriorReport, bool) {
		var rep evalPriorReport
		r := h.kb(append([]string{"eval", "--mode", "fts", "--json"}, args...)...)
		if !h.expect(r, 0) {
			return rep, false
		}
		if err := json.Unmarshal([]byte(r.Stdout), &rep); err != nil {
			h.fail("eval %v --json: %v\n%s", args, err, r)
			return rep, false
		}
		return rep, true
	}
	before := count()
	if rep, ok := report(); ok && (rep.Prior.Enabled != 0 || rep.Chunk.Queries == 0) {
		h.fail("prior off: prior counts %+v, chunk queries %d", rep.Prior, rep.Chunk.Queries)
	}
	for _, sets := range [][]string{{"--set", "prior.enabled=true"}, {"--set", "prior.enabled=true", "--set", "prior.query_memory=true"}} {
		if rep, ok := report(sets...); ok && (rep.Prior.Enabled != rep.Chunk.Queries || rep.Prior.Active > rep.Prior.Enabled ||
			rep.Prior.Active == 0 || rep.Prior.Memory > rep.Prior.Active) {
			h.fail("eval %v: prior counts %+v over %d replays: want enabled = replays >= active > 0, memory <= active", sets, rep.Prior, rep.Chunk.Queries)
		}
	}
	h.expect(h.kb("eval", "--mode", "fts", "--save-baseline"), 0)
	if r := h.kb("eval", "--mode", "fts", "--set", "prior.enabled=true", "--compare"); h.expect(r, 0) &&
		(!strings.Contains(r.Stdout, "compare ·") || !strings.Contains(r.Stdout, "prior: active in ")) {
		h.fail("eval --set prior.enabled=true --compare: want the table and the prior line\n%s", r)
	}
	if r := h.kb("eval", "--mode", "fts", "--set", "prior.enabled=true", "--compare", "--json"); h.expect(r, 0) && !json.Valid([]byte(r.Stdout)) {
		h.fail("compare --json is not valid JSON:\n%s", r)
	}
	if after := count(); after != before {
		h.fail("eval with the prior wrote the log: %s -> %s", before, after)
	}

	// [prior] sweeps: refused below eval.min_judged (30) whatever --force says; a grid that never
	// enables the prior is refused too.
	for _, bad := range [][]string{
		{"--sweep", "prior.max_boost=0.1,0.3", "--set", "prior.enabled=true", "--force"},
		{"--sweep", "prior.enabled=false,true", "--force"},
		{"--sweep", "prior.max_boost=0.1,0.3", "--set", "eval.min_judged=1"},
		{"--sweep", "prior.bogus=1", "--set", "eval.min_judged=1", "--set", "prior.enabled=true"},
		{"--sweep", "prior.max_boost=2", "--set", "eval.min_judged=1", "--set", "prior.enabled=true"},
	} {
		h.expect(h.kb(append([]string{"eval", "--mode", "fts"}, bad...)...), 1)
	}
	if r := h.kb("eval", "--mode", "fts", "--set", "eval.min_judged=1", "--set", "prior.enabled=true", "--sweep", "prior.max_boost=0,0.5,1", "--json"); h.expect(r, 0) && !json.Valid([]byte(r.Stdout)) {
		h.fail("a [prior] sweep at the gate: invalid JSON\n%s", r)
	}

	// Feedback stamped after every search (or in the future) never reaches a replay.
	h.helper("exec", h.dbPath(), "UPDATE search_feedback SET ts = '2999-01-01T00:00:00Z'")
	if rep, ok := report("--set", "prior.enabled=true"); ok && rep.Prior.Active != 0 {
		h.fail("future-stamped feedback activated the prior in %d replays", rep.Prior.Active)
	}

	// A hand-damaged search time: exit 1 naming the search, prior on or off; never 3.
	h.helper("exec", h.dbPath(), "UPDATE searches SET ts = 'noon' WHERE id = 6")
	for _, args := range [][]string{{"eval"}, {"eval", "--set", "prior.enabled=true"}} {
		if r := h.kb(args...); h.expect(r, 1) && !strings.Contains(r.Stderr, "search 6 has timestamp \"noon\"") {
			h.fail("kb %v on a damaged search time: want the search named\n%s", args, r)
		}
	}
	h.helper("exec", h.dbPath(), "UPDATE searches SET ts = '2026-01-01T06:00:00Z' WHERE id = 6")

	// A large judged log: 10k searches over 50 queries, 30k verdicts; one read for the whole run.
	h.helper("exec", h.dbPath(), `
		WITH RECURSIVE seq(i) AS (SELECT 1 UNION ALL SELECT i + 1 FROM seq WHERE i < 10000)
		INSERT INTO searches (id, ts, query, mode, k, n_returned, caller, kb_version, settings, settings_hash)
		SELECT 100000 + i, strftime('%Y-%m-%dT%H:%M:%SZ', '2026-02-01 00:00:00', '+' || i || ' minutes'),
		       'sprocket q' || (i % 50), 'fts', 8, 3, 'bulk', 'x', '{}', 'h' FROM seq;
		WITH RECURSIVE seq(i) AS (SELECT 1 UNION ALL SELECT i + 1 FROM seq WHERE i < 10000), r(k) AS (VALUES (1), (2), (3))
		INSERT INTO search_results (search_id, rank, chunk_id, entry_path, heading, rrf_score)
		SELECT 100000 + i, k, 1, 'data/p' || k || '.md', 'Body', 0.01 FROM seq, r;
		INSERT INTO search_feedback (search_id, rank, useful, ts)
		SELECT search_id, rank, rank % 2, strftime('%Y-%m-%dT%H:%M:%SZ', s.ts, '+30 seconds')
		FROM search_results JOIN searches AS s ON s.id = search_id WHERE search_id > 100000;`)
	start := time.Now()
	if rep, ok := report("--set", "prior.enabled=true", "--set", "prior.query_memory=true"); ok {
		h.note("eval with the prior over a 10k-search judged log (%d replays, %d active) took %s",
			rep.Chunk.Queries, rep.Prior.Active, time.Since(start).Round(time.Millisecond))
		if rep.Prior.Active == 0 {
			h.fail("bulk log: the prior was active in no replay: %+v", rep.Prior)
		}
	}
}
