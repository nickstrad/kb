package main

// Spell correction and kb synonyms suggest (PLAN 5.6) on the real binary, FTS only.

import (
	"encoding/json"
	"os"
	"strings"
)

func init() {
	scenarios = append(scenarios, scenario{"spell-suggest",
		"a typo that finds nothing is also searched as its correction with one stderr note; a known word, an identifier, a number, " +
			"search.spell=false and hostile typo queries (unicode, 2000 fields, FTS syntax) never error; kb synonyms suggest mines a reworded " +
			"pair into a paste-safe group and valid --json, never writes synonyms.txt, and bad flags exit 1",
		scSpellSuggest})
}

func scSpellSuggest(h *H) {
	h.write("data/k1.md", entryText("K one", "## Body\n\nkubernetes deploys pods.\n"))
	h.write("data/k2.md", entryText("K two", "## Body\n\nkubernetes rollout notes.\n"))
	h.write("data/k3.md", entryText("K three", "## Body\n\nkb.toml and foo_bar for kubernetes, 2024.\n"))
	h.useNone()
	h.must(h.kb(withNone("reindex", "--all")...).Code == 0, "index with an FTS-only config")

	r := h.kb(withNone("search", "kuberntes", "--mode", "fts", "--caller", "h")...)
	if h.expect(r, 0) && (strings.Count(r.Stderr, "note: also searched \"kubernetes\" for \"kuberntes\"") != 1 || !strings.Contains(r.Stdout, "data/k1.md")) {
		h.fail("typo search: want the kubernetes hits and one note:\n%s", r)
	}
	for _, q := range []string{"kubernetes", "foo_baz", "2025", "kb.tmol", "pods"} {
		if r := h.kb(withNone("search", q, "--mode", "fts")...); h.expect(r, 0) && strings.Contains(r.Stderr, "note:") {
			h.fail("%q must not be corrected:\n%s", q, r)
		}
	}
	for _, q := range []string{"kübernetes", strings.Repeat("kuberntes ", 2000), `"kuberntes`, "NEAR(kuberntes", "kuberntes*", "title:kuberntes", "日本語 kuberntes", "kuberntes OR AND"} {
		if r := h.kb(withNone("search", q, "--mode", "fts", "--json")...); h.expect(r, 0) && !json.Valid([]byte(r.Stdout)) {
			h.fail("search %.40q --json is not valid JSON:\n%s", q, r)
		}
	}
	if r := h.kb(withNone("explain", "kuberntes", "--mode", "fts", "--set", "search.spell=false")...); h.expect(r, 0) && strings.Contains(r.Stdout+r.Stderr, "kubernetes") {
		h.fail("search.spell=false still corrected:\n%s", r)
	}

	// A reworded pair: k8s found nothing, kubernetes was judged useful.
	h.expect(h.kb(withNone("search", "k8s", "--mode", "fts", "--caller", "h")...), 0)
	r = h.kb(withNone("search", "kubernetes", "--mode", "fts", "--caller", "h")...)
	if h.expect(r, 0) {
		id := strings.Fields(strings.TrimPrefix(r.Stdout, "search "))[0]
		h.expect(h.kb(withNone("feedback", id, "1", "--useful")...), 0)
	}
	r = h.kb(withNone("synonyms", "suggest")...)
	if h.expect(r, 0) {
		if !strings.Contains(r.Stdout, "\nk8s, kubernetes\n") {
			h.fail("suggest lacks the k8s group:\n%s", r)
		}
		for _, line := range strings.Split(strings.TrimSpace(r.Stdout), "\n") {
			if line != "" && !strings.HasPrefix(line, "#") && !strings.Contains(line, ", ") {
				h.fail("suggest line %q would not paste into synonyms.txt", line)
			}
		}
	}
	if r := h.kb(withNone("synonyms", "suggest", "--json")...); h.expect(r, 0) && !json.Valid([]byte(r.Stdout)) {
		h.fail("suggest --json is not valid JSON:\n%s", r)
	}
	if r := h.kb("config", "--path"); r.Code == 0 {
		syn := strings.TrimSuffix(strings.TrimSpace(r.Stdout), "config.toml") + "synonyms.txt"
		if _, err := os.Stat(syn); err == nil {
			h.fail("kb synonyms suggest wrote %s", syn)
		}
	}
	for _, bad := range [][]string{{"--window", "0s"}, {"--window", "-1m"}, {"--min", "0"}, {"--gaps", "-1"}, {"--since", "nope"}, {"extra"}} {
		h.expect(h.kb(withNone(append([]string{"synonyms", "suggest"}, bad...)...)...), 1)
	}
}
