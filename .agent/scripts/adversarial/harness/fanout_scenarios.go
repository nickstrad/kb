package main

// Cross-store fan-out search (PLAN 3.2, H39). These scenarios need vectors from more than one
// model, so kb is built with the harnessfake tag (main.go) and the stores use the fake:<dim> model.

import (
	"encoding/json"
	"path/filepath"
	"strings"
)

func init() {
	scenarios = append(scenarios, scenario{"stores-fanout",
		"kb search --stores (H39): an ok store, an older schema, no database, an alias and the same store as NAME@LOC, an unknown word; " +
			"bad stores skipped with one warning each, exit 0, tagged hits, the same-location ensemble merged once, only the primary logs " +
			"(store column), extra stores never written; >8 stores and malformed refs exit 1; >2 models warn; kb show REF:path",
		scStoresFanout})
}

func scStoresFanout(h *H) {
	other := filepath.Join(h.outside, "other")
	third := filepath.Join(h.outside, "third")
	h.must(h.kb("init", "--model", "fake:16").Code == 0, "init the primary with the fake model (is kb built with -tags harnessfake?)")
	for _, s := range []string{"adr", "old"} {
		h.expect(h.kb("init", "--name", s, "--model", "fake:16"), 0)
	}
	// The scenario root may already hold a store: pin the primary's model. empty is a store with
	// a config and no database (a cloned repo, never indexed); kb init would create the database.
	h.write(filepath.Join(h.root, ".kb", "stores", "default", "config.toml"), "[embed]\nmodel = \"fake:16\"\n"+dataSource)
	h.write(filepath.Join(h.root, ".kb", "stores", "empty", "config.toml"), "[embed]\nmodel = \"fake:16\"\n"+dataSource)
	h.expect(h.kbAt(h.outside, "--root", other, "init", "--model", "fake:8"), 0)
	h.expect(h.kbAt(h.outside, "--root", third, "init", "--model", "fake:4"), 0)
	// adr indexes the primary's data/ too (same location, same files): an ensemble.
	h.write(filepath.Join(h.root, ".kb", "stores", "adr", "config.toml"), "[embed]\nmodel = \"fake:16\"\n"+dataSource)
	h.write(filepath.Join(h.root, ".kb", "stores", "old", "config.toml"), "[embed]\nmodel = \"fake:16\"\n"+dataSource)
	h.write(filepath.Join(h.root, ".kb", "kb.toml"), "version = 1\ndefault_store = \"default\"\n[aliases]\nnotes = \""+other+"\"\n")

	h.write("data/wal.md", entryText("Wal", "## Section\n\nfanword in the primary\n"))
	h.write(filepath.Join(other, "data", "far.md"), entryText("Far", "## Section\n\nfanword in the other location\n"))
	h.write(filepath.Join(third, "data", "third.md"), entryText("Third", "## Section\n\nfanword in the third location\n"))
	for _, args := range [][]string{{"reindex", "--all"}, {"--store", "adr", "reindex", "--all"}, {"--store", "old", "reindex", "--all"}} {
		h.must(h.kb(args...).Code == 0, "kb %s", strings.Join(args, " "))
	}
	for _, root := range []string{other, third} {
		h.must(h.kbAt(h.outside, "--root", root, "reindex", "--all").Code == 0, "reindex %s", root)
	}
	oldDB := filepath.Join(h.root, ".kb", "stores", "old", "kb.sqlite")
	haveOld := loadTool != ""
	if haveOld {
		h.helper("setver", oldDB, "4") // a store an older kb left: skipped, with the migrate hint
	} else {
		h.note("no loadfixture helper (-bin): the older-schema store is not made older")
	}
	adrDB := filepath.Join(h.root, ".kb", "stores", "adr", "kb.sqlite")
	adrSum, otherSum := h.sum(adrDB), h.sum(filepath.Join(other, ".kb", "stores", "default", "kb.sqlite"))

	r := h.kb("search", "fanword", "--stores", "adr,old,empty,notes,default@"+other+",nope")
	if h.expect(r, 0) {
		want := []string{"skipping store empty", "has no database yet", `skipping store nope`}
		if haveOld {
			want = append(want, "skipping store old", "migrate up")
		}
		for _, w := range want {
			if !strings.Contains(r.Stderr, w) {
				h.fail("stderr lacks %q:\n%s", w, r)
			}
		}
		wantSkips := 2
		if haveOld {
			wantSkips = 3
		}
		if n := strings.Count(r.Stderr, "warning:"); n != wantSkips {
			h.fail("%d warnings, want %d (one per bad store; the NAME@LOC duplicate of notes must not warn):\n%s", n, wantSkips, r)
		}
		if !strings.Contains(r.Stdout, "] notes:data/far.md") || strings.Count(r.Stdout, "data/wal.md › Section") != 1 || strings.Contains(r.Stdout, "adr:") {
			h.fail("want far.md tagged notes, wal.md once (ensemble, primary tag), no adr tag:\n%s", r)
		}
	}
	if h.sum(adrDB) != adrSum || h.sum(filepath.Join(other, ".kb", "stores", "default", "kb.sqlite")) != otherSum {
		h.fail("a fan-out search wrote to an extra store's database")
	}
	// Only the primary logs, each hit under its canonical store.
	if r := h.kb("stats", "--sql", "SELECT coalesce(store, '') AS s, entry_path AS p FROM search_results ORDER BY rank", "--json"); h.expect(r, 0) {
		if !strings.Contains(r.Stdout, "default@"+other) || !strings.Contains(r.Stdout, "data/wal.md") {
			h.fail("search_results lacks the canonical store or the primary hit:\n%s", r)
		}
	}
	if r := h.kbAt(h.outside, "--root", other, "stats", "--sql", "SELECT count(*) AS n FROM searches", "--json"); h.expect(r, 0) && !strings.Contains(r.Stdout, `"n": 0`) && !strings.Contains(r.Stdout, `"n":0`) {
		h.fail("the other store logged the search:\n%s", r)
	}

	// --json is valid and carries the tag.
	if r := h.kb("search", "fanword", "--stores", "notes", "--json"); h.expect(r, 0) {
		var doc struct {
			Hits []struct{ Store, Path string } `json:"hits"`
		}
		if err := json.Unmarshal([]byte(r.Stdout), &doc); err != nil {
			h.fail("--json: %v\n%s", err, r)
		}
		tagged := false
		for _, hit := range doc.Hits {
			tagged = tagged || (hit.Store == "notes" && hit.Path == "data/far.md")
		}
		if !tagged {
			h.fail("--json has no notes-tagged far.md:\n%s", r)
		}
	}
	// Three distinct models warn; the search still works.
	if r := h.kb("search", "fanword", "--stores", "notes,"+third); h.expect(r, 0) && !strings.Contains(r.Stderr, "3 embedding models") {
		h.fail("no warning for 3 models:\n%s", r)
	}
	// kb show REF:path reads the other store's entry.
	if r := h.kb("show", "notes:data/far.md"); h.expect(r, 0) && !strings.Contains(r.Stdout, "fanword in the other location") {
		h.fail("show notes:data/far.md:\n%s", r)
	}
	h.expect(h.kb("show", "nope:data/far.md"), 1)

	// Hostile --stores: blanks are none; more than 8 distinct stores, malformed refs exit 1, never 3.
	h.expect(h.kb("search", "fanword", "--stores", ",, ,"), 0)
	h.expect(h.kb("search", "fanword", "--stores", strings.Repeat("adr,", 20)+"adr"), 0)
	for i := range 9 {
		h.expect(h.kb("init", "--name", "s"+string(rune('0'+i)), "--model", "fake:16"), 0)
	}
	if r := h.kb("search", "fanword", "--stores", "s0,s1,s2,s3,s4,s5,s6,s7,s8"); h.expect(r, 1) && !strings.Contains(r.Stderr, "at most 8") {
		h.fail("9 stores:\n%s", r)
	}
	for _, bad := range []string{"Bad", "a@", "@x", "x:y", "../" + strings.Repeat("a", 300), "a\x01b"} {
		h.expect(h.kb("search", "fanword", "--stores", bad), 1)
	}
}
