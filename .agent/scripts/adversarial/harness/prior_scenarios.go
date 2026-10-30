package main

// The feedback prior and the query-memory list (PLAN 6.2, 6.3) on the real binary, FTS only.

import (
	"encoding/json"
	"strings"
	"time"
)

func init() {
	scenarios = append(scenarios, scenario{"prior-hostile",
		"with [prior] on: --json multipliers stay in [1-max_boost, 1+max_boost] and are logged NULL exactly when inactive; " +
			"--no-prior drops them; explain never writes the log; out-of-range [prior] --set values exit 1; feedback stamped in the " +
			"future is not counted; a 10k-search judged log still searches; a hand-damaged feedback or open timestamp exits 1 " +
			"naming the row (search and eval), never 3",
		scPriorHostile})
}

type priorHit struct {
	Path       string   `json:"path"`
	Prior      *float64 `json:"prior"`
	MemoryRank *int     `json:"memory_rank"`
}

func scPriorHostile(h *H) {
	h.write("data/p1.md", entryText("P one", "## Body\n\nsprocket calibration steps.\n"))
	h.write("data/p2.md", entryText("P two", "## Body\n\nsprocket calibration history.\n"))
	h.write("data/p3.md", entryText("P three", "## Body\n\nsprocket wear.\n"))
	h.setConfig("[embed]\nmodel = \"none\"\n[prior]\nenabled = true\nmin_judged = 2\nmax_boost = 0.2\nquery_memory = true\n", false)
	h.must(h.kb("reindex", "--all").Code == 0, "index with an FTS-only config")

	search := func(args ...string) ([]priorHit, string, bool) {
		r := h.kb(append([]string{"search", "sprocket calibration", "--mode", "fts", "--caller", "h", "--json"}, args...)...)
		if !h.expect(r, 0) {
			return nil, "", false
		}
		var out struct {
			ID   int64      `json:"search_id"`
			Hits []priorHit `json:"hits"`
		}
		if err := json.Unmarshal([]byte(r.Stdout), &out); err != nil {
			h.fail("search --json: %v\n%s", err, r)
			return nil, "", false
		}
		return out.Hits, strings.TrimSpace(strings.Split(strings.SplitN(r.Stdout, `"search_id":`, 2)[1], ",")[0]), true
	}
	// logged maps a search id to the number of its result rows with a non-NULL prior.
	logged := func() map[string]int {
		r := h.kb("stats", "--sql", "SELECT CAST(search_id AS TEXT) AS s, count(prior) AS n FROM search_results GROUP BY search_id", "--json")
		var out struct {
			Rows []struct {
				S string `json:"s"`
				N int    `json:"n"`
			} `json:"rows"`
		}
		got := map[string]int{}
		if h.expect(r, 0) && json.Unmarshal([]byte(r.Stdout), &out) == nil {
			for _, row := range out.Rows {
				got[row.S] = row.N
			}
		}
		return got
	}

	// Below min_judged: inactive, no multiplier anywhere.
	for i := 0; i < 2; i++ {
		hits, id, ok := search()
		if !ok {
			return
		}
		for _, x := range hits {
			if x.Prior != nil {
				h.fail("search %s: %s has prior %v below min_judged", id, x.Path, *x.Prior)
			}
		}
		verdict := "--useful"
		if i == 1 {
			verdict = "--not-useful"
		}
		h.expect(h.kb("feedback", id, "1", verdict), 0)
		h.expect(h.kb("feedback", id, "1", "--useful"), 0) // a changed verdict: the latest wins
		h.expect(h.kb("feedback", id, "99", "--useful"), 1)
	}
	h.expect(h.kb("show", "data/p2.md"), 0) // an open after the last search
	h.expect(h.kb("feedback", "--none"), 0, 1)

	// Active: every hit carries a bounded multiplier; --no-prior carries none and is logged NULL.
	hits, id, ok := search()
	if !ok {
		return
	}
	if len(hits) == 0 {
		h.fail("search %s: no hits", id)
	}
	for _, x := range hits {
		if x.Prior == nil || *x.Prior < 0.8-1e-9 || *x.Prior > 1.2+1e-9 {
			h.fail("search %s: %s prior %v, want within [0.8, 1.2]", id, x.Path, x.Prior)
		}
	}
	off, offID, ok := search("--no-prior")
	if !ok {
		return
	}
	for _, x := range off {
		if x.Prior != nil || x.MemoryRank != nil {
			h.fail("--no-prior search %s: %s has prior %v / memory rank %v", offID, x.Path, x.Prior, x.MemoryRank)
		}
	}
	if n := logged(); n[id] != len(hits) || n[offID] != 0 || n["1"] != 0 {
		h.fail("logged priors per search = %v; want %d for active search %s, 0 for --no-prior search %s and inactive search 1", n, len(hits), id, offID)
	}

	// explain reads the log and never writes it.
	before := h.kb("stats", "--sql", "SELECT count(*) AS n FROM searches", "--json").Stdout
	if r := h.kb("explain", "sprocket calibration", "--mode", "fts"); h.expect(r, 0) && !strings.Contains(r.Stdout, "prior: active") {
		h.fail("explain does not show the active prior:\n%s", r)
	}
	if after := h.kb("stats", "--sql", "SELECT count(*) AS n FROM searches", "--json").Stdout; after != before {
		h.fail("explain wrote the log: %s -> %s", before, after)
	}

	// Out-of-range [prior] values are usage errors.
	for _, set := range []string{"prior.max_boost=2", "prior.max_boost=-0.1", "prior.max_boost=NaN", "prior.half_life_days=0",
		"prior.half_life_days=-5", "prior.min_judged=-1", "prior.enabled=maybe", "prior.min_judged=1e99"} {
		h.expect(h.kb("explain", "sprocket", "--mode", "fts", "--set", set), 1)
	}

	// Feedback stamped in the future (a skewed clock) is not evidence yet.
	h.helper("exec", h.dbPath(), "UPDATE search_feedback SET ts = '2999-01-01T00:00:00Z'; UPDATE search_opens SET ts = '2999-01-01T00:00:00Z'")
	if r := h.kb("explain", "sprocket calibration", "--mode", "fts"); h.expect(r, 0) && !strings.Contains(r.Stdout, "prior inactive: 0/2 judged searches") {
		h.fail("future-stamped feedback was counted:\n%s", r)
	}

	// A large judged log: 10k searches, 30k verdicts.
	start := time.Now()
	h.helper("exec", h.dbPath(), `
		WITH RECURSIVE seq(i) AS (SELECT 1 UNION ALL SELECT i + 1 FROM seq WHERE i < 10000)
		INSERT INTO searches (id, ts, query, mode, k, n_returned, caller, kb_version, settings, settings_hash)
		SELECT 100000 + i, '2026-01-01T00:00:00Z', 'q' || (i % 50), 'fts', 8, 3, 'bulk', 'x', '{}', 'h' FROM seq;
		WITH RECURSIVE seq(i) AS (SELECT 1 UNION ALL SELECT i + 1 FROM seq WHERE i < 10000),
		     r(k) AS (VALUES (1), (2), (3))
		INSERT INTO search_results (search_id, rank, chunk_id, entry_path, heading, rrf_score)
		SELECT 100000 + i, k, 1, 'data/p' || k || '.md', 'Body', 0.01 FROM seq, r;
		INSERT INTO search_feedback (search_id, rank, useful, ts)
		SELECT search_id, rank, rank % 2, '2026-01-01T00:01:00Z' FROM search_results WHERE search_id > 100000;`)
	h.note("bulk log written in %s", time.Since(start).Round(time.Millisecond))
	start = time.Now()
	if hits, id, ok := search(); ok {
		h.note("search %s over a 10k-search judged log took %s", id, time.Since(start).Round(time.Millisecond))
		for _, x := range hits {
			if x.Prior == nil || *x.Prior < 0.8-1e-9 || *x.Prior > 1.2+1e-9 {
				h.fail("bulk log: %s prior %v, want within [0.8, 1.2]", x.Path, x.Prior)
			}
		}
	}

	// A hand-damaged timestamp: exit 1 naming the row, for search and eval alike.
	for _, bad := range []string{
		"UPDATE search_feedback SET ts = 'yesterday' WHERE search_id = 100001 AND rank = 1",
		"UPDATE search_feedback SET ts = '2026-01-01T00:01:00Z' WHERE search_id = 100001; UPDATE search_opens SET ts = 'garbage'",
	} {
		h.helper("exec", h.dbPath(), bad)
		for _, args := range [][]string{{"search", "sprocket", "--mode", "fts"}, {"eval"}} {
			if r := h.kb(args...); h.expect(r, 1) && !strings.Contains(r.Stderr, "the search log in ") {
				h.fail("kb %s on a damaged log: want the damaged-log message\n%s", args[0], r)
			}
		}
	}
	h.expect(h.kb("search", "sprocket", "--mode", "fts", "--no-prior"), 0) // --no-prior never reads the log
}
