package main

// kb related, kb search --neighbors and kb stale (PLAN 5.3, 5.4) on the real binary, FTS only.

import (
	"encoding/json"
	"fmt"
	"strings"
	"time"
)

func init() {
	scenarios = append(scenarios, scenario{"links-stale",
		"a 400-entry link graph with cycles, self-links, mutual supersedes, a 500-item related list and hostile link targets: " +
			"reindex, kb related --depth 1..3 (valid JSON, bounded time), search --neighbors; hostile related args and --depth exit 1; " +
			"kb stale on hostile dates and --days values exits 0/1 with valid JSON; related and stale never write the index",
		scLinksStale})
}

func scLinksStale(h *H) {
	const n = 400
	name := func(i int) string { return fmt.Sprintf("g%03d.md", (i%n+n)%n) }
	hostile := []string{"../../etc/passwd", "/abs.md", "http://x.example/a.md", "[[wiki]]", "a b.md", "%00.md",
		"x.md#frag?q=1", strings.Repeat("../", 50) + "x.md", "", "   ", "日本.md"}
	for i := 0; i < n; i++ {
		rel := []string{name(i + 1), name(i * 7), name(i)} // next, a scatter, itself
		if i == 0 {
			for j := 0; j < 500; j++ {
				rel = append(rel, name(j))
			}
		}
		fm := "related: [" + strings.Join(rel, ", ") + "]\n"
		if i%10 == 0 { // mutual supersession with the next entry
			fm += "supersedes: [" + name(i+1) + "]\n"
		}
		if i%10 == 1 {
			fm += "supersedes: [" + name(i-1) + ", " + name(i) + "]\n"
		}
		if i%50 == 0 {
			q := make([]string, len(hostile))
			for k, t := range hostile {
				q[k] = fmt.Sprintf("%q", t)
			}
			fm += "depends_on: [" + strings.Join(q, ", ") + "]\n"
		}
		body := fmt.Sprintf("## Body\n\ngraphword node %d. See [next](%s), [me](%s) and [up](../../../out.md).\n", i, name(i+3), name(i))
		h.write("data/"+name(i), "---\ntitle: Node "+fmt.Sprint(i)+"\nsummary: node\nupdated: 2026-01-01\n"+fm+"---\n\n"+body)
	}
	h.useNone()
	r := h.kb(withNone("reindex", "--all")...)
	h.expect(r, 0)
	h.note("reindex of the %d-entry graph: %s", n, r.Dur.Round(time.Millisecond))

	before := fileHash(h.dbPath())
	for _, depth := range []string{"1", "2", "3"} {
		for _, p := range []string{"data/g000.md", "data/g123.md"} {
			r := h.kb(withNone("related", p, "--depth", depth, "--json")...)
			if !h.expect(r, 0) {
				continue
			}
			var out struct {
				Edges []struct {
					Path string `json:"path"`
				} `json:"edges"`
			}
			if err := json.Unmarshal([]byte(r.Stdout), &out); err != nil || len(out.Edges) == 0 {
				h.fail("related %s --depth %s: bad JSON or no edges (%v):\n%s", p, depth, err, clip(r.Stdout, 400))
			}
			if r.Dur > 5*time.Second {
				h.fail("related %s --depth %s took %s", p, depth, r.Dur)
			}
			if depth == "3" && p == "data/g000.md" {
				h.note("related g000 --depth 3: %d edges in %s", len(out.Edges), r.Dur.Round(time.Millisecond))
			}
		}
	}
	h.expect(h.kb(withNone("related", "data/g005.md")...), 0)
	for _, bad := range [][]string{
		{"related", ""}, {"related", "../etc/passwd"}, {"related", "/etc/passwd"}, {"related", "data/../../x.md"},
		{"related", "nope:data/g000.md"}, {"related", "REF:"}, {"related", strings.Repeat("a/", 2000) + "x.md"},
		{"related", "data/missing.md"}, {"related", "data/g000.md", "--depth", "0"}, {"related", "data/g000.md", "--depth", "4"},
		{"related", "data/g000.md", "--depth", "-1"}, {"related", "data/g000.md", "--depth", "abc"},
		{"related", "data/g000.md", "data/g001.md"}, {"related"},
	} {
		h.expect(h.kb(withNone(bad...)...), 1)
	}
	if after := fileHash(h.dbPath()); after != before {
		h.fail("kb related changed the index file")
	}
	r = h.kb(withNone("search", "graphword", "--mode", "fts", "--neighbors", "--json", "-k", "20")...)
	if h.expect(r, 0) {
		var out struct {
			Neighbors *[]json.RawMessage `json:"neighbors"`
		}
		if err := json.Unmarshal([]byte(r.Stdout), &out); err != nil || out.Neighbors == nil {
			h.fail("search --neighbors --json: no neighbors field (%v):\n%s", err, clip(r.Stdout, 400))
		}
	}
	h.expect(h.kb(withNone("search", "graphword", "--mode", "fts", "--neighbors")...), 0)

	// Hostile dates: stale lists them under no_date (or as dated), never fails.
	dates := []string{"0000-01-01", "9999-12-31", "2026-02-30", "1e9", "日本", strings.Repeat("9", 10000),
		"2026-10-30T25:00:00Z", "-2026-01-01", "2026-01-01T00:00:00+99:00", "0001-01-01T00:00:00Z", "[2026-01-01]", "null", "~"}
	for i, d := range dates {
		h.write(fmt.Sprintf("data/d%02d.md", i), fmt.Sprintf("---\ntitle: D%d\nsummary: d\nupdated: %q\nverified: %s\n---\n\n## B\n\ndateword\n", i, d, d))
	}
	r = h.kb(withNone("reindex", "--all")...)
	h.expect(r, 0, 1)
	h.note("reindex with hostile dates: exit %d", r.Code)
	before = fileHash(h.dbPath())
	for _, days := range []string{"", "0", "1", "180", "99999999999999", "9223372036854775807"} {
		args := []string{"stale", "--json"}
		if days != "" {
			args = append(args, "--days", days)
		}
		r := h.kb(withNone(args...)...)
		if h.expect(r, 0) && !json.Valid([]byte(r.Stdout)) {
			h.fail("stale --days %q --json is not valid JSON:\n%s", days, clip(r.Stdout, 400))
		}
		if days == "" {
			h.note("stale lists %d of the %d hostile-date entries", strings.Count(r.Stdout, `"path": "data/d`), len(dates))
		}
	}
	h.expect(h.kb(withNone("stale")...), 0)
	for _, bad := range []string{"-1", "abc", "1.5", "99999999999999999999"} {
		h.expect(h.kb(withNone("stale", "--days", bad)...), 1)
	}
	h.expect(h.kb(withNone("stale", "extra")...), 1)
	h.expect(h.kb(withNone("related", "data/g000.md", "--depth", "3")...), 0)
	if after := fileHash(h.dbPath()); after != before {
		h.fail("kb stale or kb related changed the index file")
	}
}
