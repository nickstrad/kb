package main

import (
	"bytes"
	"crypto/sha256"
	"encoding/binary"
	"encoding/json"
	"fmt"
	"math/rand"
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"time"
)

// withNone marks a command that runs on full-text search with no model involved. The store's
// config must say so: call h.useNone() first (it writes [embed] model = "none" to the scenario
// root and to the --global store). args are returned unchanged.
func withNone(args ...string) []string { return args }

// dataSource is the [[sources]] table kb init writes for a default store: the entries directory
// data/. setConfig appends it to a body that names no sources, so a scenario that only tunes
// [embed] or [search] keeps indexing data/ (PLAN 1.5: no [[sources]] means nothing indexed).
const dataSource = "\n[[sources]]\npath = \"data\"\nlayout = \"entries\"\n"

// setConfig writes body as the config of the scenario root's store (global=false) or of the
// --global store, at the path kb itself reports (kb config --path), so no layout is assumed.
// A body without [[sources]] gets dataSource.
func (h *H) setConfig(body string, global bool) {
	if !strings.Contains(body, "[[sources]]") {
		body += dataSource
	}
	r := h.kb("config", "--path")
	if global { // h.kb adds --root, so the global store is asked for without it
		r = h.kbAt(h.outside, "--global", "config", "--path")
	}
	h.must(r.Code == 0, "kb config --path: %s", r.Stderr)
	h.write(strings.TrimSpace(r.Stdout), body)
}

// useNone writes the FTS-only config into the scenario root and the --global store.
func (h *H) useNone() {
	const none = "[embed]\nmodel = \"none\"\n"
	h.setConfig(none, false)
	h.setConfig(none, true)
}

// scenarios is the table. To add one: append {Name, Desc, Run}; Run sets up files under h.root
// (or h.outside), calls h.kb, and reports with h.fail / h.expect / h.note. The universal
// invariants (no panic, exit code in 0/1/2 (3 is a finding), no model download) are checked for every run.
var scenarios = []scenario{
	{"exit-codes", "the 0/1/2/3 exit-code contract on usage errors and lookups", scExitCodes},
	{"fts-roundtrip", "add with an FTS-only config, then search, show, list, doctor", scRoundTrip},
	{"no-model-search", "with no model: --mode fts works, a none-indexed DB reports a mismatch, a fresh root is never created or downloaded to", scNoModelSearch},
	{"no-model-index", "reindex/add with no model and no network exit 2 without downloading or half-writing", scNoModelIndex},
	{"hostile-queries", "FTS5 syntax, quotes, *, NEAR, column filters, huge and unicode queries", scHostileQueries},
	{"markdown-edges", "CRLF, BOM, NUL, unicode, ## in fences, empty sections, huge and deep files", scMarkdownEdges},
	{"front-matter-hostile", "broken front matter: reindex warns and skips, add refuses and writes nothing", scFrontMatter},
	{"path-traversal", "../, absolute paths and symlinks never reach files outside the root", scPathTraversal},
	{"feedback-hostile", "feedback on missing searches, bad ranks and conflicting flags is refused and not logged", scFeedback},
	{"concurrent-fresh-db", "parallel add/reindex/search on a fresh root: migration and WAL races", scConcurrent},
	{"sigint-mid-reindex", "SIGINT during reindex --all leaves a usable DB; a rerun converges", scSigint},
	{"corrupt-db", "truncated and garbage kb.sqlite give a well-formed error, never a crash", scCorruptDB},
	{"newer-schema", "a DB from a newer kb is refused and left byte-for-byte unchanged", scNewerSchema},
	{"stores-layout", "init default and named stores, kb stores, --store, unknown and invalid stores, a missing default_store, a broken kb.toml, bare --root refusal", scStoresLayout},
	{"search-settings", "[search] tuning keys change kb search: rrf_k/list_weights move the score, every key at its max works, out-of-range and nan/inf values exit 1 naming file and key, nothing logged", scSearchSettings},
	{"workspace-discovery", "no --root: discovery from nested and symlinked dirs, not-found outside any store, kb init in a fresh dir, the $HOME guard (H28), --global under HOME, relative --root, --root with --global", scWorkspaceDiscovery},
	{"stores-alias", "store refs: alias, NAME@LOC, path, global, shadowing warns once, an alias with --root is refused, unknown words suggest names, hostile alias targets and refs exit 1 and never 3, kb stores lists a broken alias", scStoresAlias},
	{"cache-two-stores", "the shared embedding cache (H19): processes on two stores open one cache file at once without locking errors; a garbage or newer cache file warns and falls back, is left alone, and kb model cache --clear removes it; --stats and --clear on a missing cache exit 0", scCacheTwoStores},
	{"explain-hostile", "kb explain on hostile queries is valid JSON, --set overrides apply and bad ones exit 1, and it never writes the search log", scExplain},
	{"eval-basic", "kb eval on an empty log, a judged search, hostile flags and an index with deleted gold: exit 0/1 only, valid JSON, and it never writes the search log", scEval},
}

// scWorkspaceDiscovery attacks "kb finds the right store, or none, and never creates one by
// accident". Every other scenario passes --root; this one runs kb without it.
func scWorkspaceDiscovery(h *H) {
	h.write("data/w.md", entryText("W", "## A\n\ndiscoword here\n"))
	h.must(h.kb(withNone("reindex", "--all")...).Code == 0, "index the scenario root")

	nested := filepath.Join(h.root, "data", "deep", "er")
	h.must(os.MkdirAll(nested, 0o755) == nil, "mkdir nested")
	if r := h.kbAt(nested, withNone("list")...); h.expect(r, 0) && !strings.Contains(r.Stdout, "data/w.md") {
		h.fail("list from a nested dir did not find the store:\n%s", r)
	}
	if r := h.kbAt(nested, withNone("search", "discoword")...); h.expect(r, 0) && !strings.Contains(r.Stdout, "data/w.md") {
		h.fail("search from a nested dir did not find the entry:\n%s", r)
	}

	link := filepath.Join(h.outside, "link-into-store")
	h.must(os.Symlink(nested, link) == nil, "symlink")
	if r := h.kbAt(link, withNone("list")...); h.expect(r, 0) && !strings.Contains(r.Stdout, "data/w.md") {
		h.fail("list from a symlinked cwd did not find the store:\n%s", r)
	}

	if r := h.kbAt(h.outside, withNone("--root", "../root", "list")...); h.expect(r, 0) && !strings.Contains(r.Stdout, "data/w.md") {
		h.fail("relative --root did not resolve against the working directory:\n%s", r)
	}
	h.expect(h.kbAt(h.outside, withNone("--root", h.root, "--global", "list")...), 1)

	// Not found: a temp dir outside the repo (the sandbox lives inside it, and the repo may hold
	// a store). Skip if some ancestor is a store after all.
	none, err := os.MkdirTemp("", "kb-adv-nostore-")
	h.must(err == nil, "mkdtemp")
	defer os.RemoveAll(none)
	if anc := storeAbove(none); anc != "" {
		h.note("not-found check skipped: %s holds a store", anc)
	} else {
		src := h.write(filepath.Join(h.outside, "nf.md"), entryText("NF", "## A\n\nx\n"))
		for _, args := range [][]string{{"list"}, {"search", "x"}, {"add", src}, {"reindex", "--all"}, {"doctor"}} {
			r := h.kbAt(none, withNone(args...)...)
			want, got := "no kb location found", r.Stderr
			if args[0] == "doctor" { // doctor reports it as a failed check on stdout
				want, got = "not initialised: run kb init", r.Stdout
			}
			if h.expect(r, 1) && !strings.Contains(got, want) {
				h.fail("not-found message %q missing:\n%s", want, r)
			}
		}
		if _, err := os.Stat(filepath.Join(none, ".kb")); err == nil {
			h.fail("a command run outside any store created %s/.kb", none)
		}
		// kb init is what creates one, in the working directory itself.
		h.expect(h.kbAt(none, "init", "--model", "none"), 0)
		if _, err := os.Stat(filepath.Join(none, ".kb", "kb.toml")); err != nil {
			h.fail("kb init did not create %s/.kb/kb.toml: %v", none, err)
		}
		if r := h.kbAt(none, "list"); h.expect(r, 0) && !strings.Contains(r.Stdout, "no entries") {
			h.fail("list on a fresh store should say no entries:\n%s", r)
		}
		r := h.kbAt(none, "init", "--model", "none")
		if h.expect(r, 0) && !strings.Contains(r.Stdout, "already initialised") {
			h.fail("a second kb init should say already initialised:\n%s", r)
		}
	}

	// H28: a location at $HOME would capture every directory below it, so init refuses without
	// --force and writes nothing. (HOME is the scenario's temp home.)
	r := h.kbAt(h.home, "init", "--model", "none")
	if h.expect(r, 1) && !strings.Contains(r.Stderr, "refusing to create a kb location at") {
		h.fail("the home refusal is missing:\n%s", r)
	}
	if _, err := os.Stat(filepath.Join(h.home, ".kb")); err == nil {
		h.fail("a refused kb init created %s/.kb", h.home)
	}
	h.expect(h.kbAt(h.outside, "--root", "/", "init", "--model", "none"), 1)
	if _, err := os.Stat("/.kb"); err == nil {
		h.fail("kb init --root / created /.kb")
	}
	h.expect(h.kbAt(h.home, "init", "--model", "none", "--force"), 0)

	// --global lives under HOME (XDG_DATA_HOME is unset in the scenario env), whatever the cwd.
	src := h.write(filepath.Join(h.outside, "glob.md"), entryText("G", "## A\n\nglobalword\n"))
	h.expect(h.kbAt(nested, withNone("--global", "add", src)...), 0)
	if _, err := os.Stat(filepath.Join(h.home, ".local", "share", "kb", ".kb", "stores", "default", "kb.sqlite")); err != nil {
		h.fail("--global add did not write the global store's database under $HOME/.local/share/kb: %v", err)
	}
	if r := h.kbAt(nested, withNone("list")...); h.expect(r, 0) && strings.Contains(r.Stdout, "glob.md") {
		h.fail("--global add wrote into the discovered store:\n%s", r)
	}
}

// scStoresLayout attacks the location/store layout: names, resolution, a broken kb.toml, and the
// rule that only kb init creates anything. The scenario root is already initialised (default store).
func scStoresLayout(h *H) {
	h.write("data/s.md", entryText("S", "## A\n\nstoreword\n"))
	h.expect(h.kb("init", "--name", "adr", "--model", "none"), 0)
	if r := h.kb("stores"); h.expect(r, 0) && (!strings.Contains(r.Stdout, "default *") || !strings.Contains(r.Stdout, "adr")) {
		h.fail("kb stores should list default (marked) and adr:\n%s", r)
	}
	if r := h.kb("stores", "--json"); h.expect(r, 0) && !json.Valid([]byte(r.Stdout)) {
		h.fail("kb stores --json is not valid JSON:\n%s", r)
	}
	h.must(h.kb("reindex", "--all").Code == 0, "index the default store")
	h.must(h.kb("--store", "adr", "reindex", "--all").Code == 0, "index the adr store")
	// PLAN 1.5: the named store reads its own entries dir data-adr/, not the default store's data/.
	h.write("data-adr/a.md", entryText("A", "## A\n\nstoreword adronly\n"))
	h.must(h.kb("--store", "adr", "reindex", "--all").Code == 0, "index the adr store again")
	for _, tc := range []struct{ store, want, other string }{{"default", "data/s.md", "data-adr/a.md"}, {"adr", "data-adr/a.md", "data/s.md"}} {
		if _, err := os.Stat(filepath.Join(h.root, ".kb", "stores", tc.store, "kb.sqlite")); err != nil {
			h.fail("store %s has no database of its own: %v", tc.store, err)
		}
		r := h.kb("--store", tc.store, "search", "storeword")
		if h.expect(r, 0) && (!strings.Contains(r.Stdout, tc.want) || strings.Contains(r.Stdout, tc.other)) {
			h.fail("store %s should find %s and not %s:\n%s", tc.store, tc.want, tc.other, r)
		}
	}
	h.expect(h.kb("init", "--name", "adr", "--model", "none"), 0) // already initialised

	// Unknown and invalid stores.
	if r := h.kb("--store", "nope", "list"); h.expect(r, 1) && !strings.Contains(r.Stderr, "stores: adr, default") {
		h.fail("an unknown store should list the existing ones:\n%s", r)
	}
	for _, bad := range []string{"../x", "A", "a/b", "a@b", strings.Repeat("z", 33), "-x"} {
		h.expect(h.kb("--store", bad, "list"), 1)
		h.expect(h.kb("init", "--name", bad, "--model", "none"), 1)
	}
	h.expect(h.kb("init", "--name", "global", "--model", "none"), 1)
	h.expect(h.kb("--store", "global", "list"), 1) // with --root: global names the global location
	if entries, _ := os.ReadDir(filepath.Join(h.root, ".kb", "stores")); len(entries) != 2 {
		h.fail("invalid names created store directories: %d entries under .kb/stores, want 2", len(entries))
	}

	// A default_store naming no store is NotFound naming kb.toml; a named store still works.
	tomlPath := filepath.Join(h.root, ".kb", "kb.toml")
	h.write(tomlPath, "version = 1\ndefault_store = \"ghost\"\n")
	if r := h.kb("list"); h.expect(r, 1) && !strings.Contains(r.Stderr, "kb.toml") {
		h.fail("a missing default_store should name kb.toml:\n%s", r)
	}
	h.expect(h.kb("--store", "adr", "list"), 0)
	h.expect(h.kb("stores"), 0)
	if r := h.kb("doctor"); h.expect(r, 1) && !strings.Contains(r.Stdout, "FAIL store") {
		h.fail("doctor should fail the store check:\n%s", r)
	}

	// A broken or hostile kb.toml is a well-formed error, never a crash or exit 3.
	for name, body := range map[string]string{
		"unknown key":    "colour = 1\n",
		"bad store name": "default_store = \"../../etc\"\n",
		"newer version":  "version = 99\n",
		"garbage":        "\x00\x01 not toml [[[",
		"huge":           "# " + strings.Repeat("x", 2<<20) + "\n",
	} {
		h.write(tomlPath, body)
		if r := h.kb("list"); !h.expect(r, 1) {
			h.note("kb.toml %s: unexpected result", name)
		}
	}
	h.must(os.Remove(tomlPath) == nil, "remove kb.toml")
	h.must(os.Mkdir(tomlPath, 0o755) == nil, "kb.toml as a directory")
	h.expect(h.kb("list"), 1)
	h.must(os.Remove(tomlPath) == nil, "remove kb.toml dir")
	h.write(tomlPath, "version = 1\ndefault_store = \"default\"\n")
	h.expect(h.kb("list"), 0)

	// Only kb init creates a location or a store: a bare --root refuses and creates nothing.
	fresh := filepath.Join(h.outside, "bare-root")
	src := h.write(filepath.Join(h.outside, "bare.md"), entryText("Bare", "## A\n\nbare\n"))
	for _, args := range [][]string{{"add", src}, {"reindex", "--all"}, {"search", "x"}, {"stores"}} {
		r := h.kbAt(h.outside, append([]string{"--root", fresh}, args...)...)
		if h.expect(r, 1) && !strings.Contains(r.Stderr, "run kb init") {
			h.fail("a bare --root should say run kb init:\n%s", r)
		}
	}
	if _, err := os.Stat(fresh); err == nil {
		h.fail("a command on a bare --root created %s", fresh)
	}
}

// storeAbove returns the first directory at or above dir holding .kb/, or "".
func storeAbove(dir string) string {
	if real, err := filepath.EvalSymlinks(dir); err == nil {
		dir = real
	}
	for {
		if _, err := os.Stat(filepath.Join(dir, ".kb")); err == nil {
			return dir
		}
		p := filepath.Dir(dir)
		if p == dir {
			return ""
		}
		dir = p
	}
}

func scExitCodes(h *H) {
	h.expect(h.kb("version"), 0)
	h.expect(h.kb("no-such-command"), 1)
	h.expect(h.kb("search", "x", "--no-such-flag"), 1)
	h.expect(h.kb("search"), 1)
	h.expect(h.kb(withNone("search", "x", "--mode", "bogus")...), 1)
	h.expect(h.kb("search", "x", "--embedder", "none"), 1) // the flags are gone
	h.expect(h.kb("search", "x", "--embed-model", "no/such-model"), 1)
	h.setConfig("[embed]\nmodel = \"no/such-model\"\n", false)
	h.expect(h.kb("search", "x"), 1) // unknown model in the config
	h.setConfig("[search]\nk = 0\n", false)
	h.expect(h.kb("config"), 1) // out of range, names file and key
	h.setConfig("bogus = 1\n", false)
	h.expect(h.kb("config"), 1) // unknown key
	h.setConfig("", false)
	h.useNone()
	h.expect(h.kb(withNone("show", "data/nope.md")...), 1)
	h.expect(h.kb(withNone("stats", "no-such-query")...), 1)
	h.expect(h.kb(withNone("feedback", "1", "1", "--useful")...), 1) // no such search
	r := h.kb(withNone("search", "anything")...)                     // an initialised store with nothing indexed
	h.expect(r, 0)
	h.expect(h.kb(withNone("rm", "data/nope.md")...), 1)
	h.expect(h.kb(withNone("reindex", "--all", "data/x.md")...), 1)
}

func scRoundTrip(h *H) {
	src := h.write(filepath.Join(h.outside, "alpha-entry.md"), entryText("Alpha", "## Usage\n\nzebracorn appears here.\n"))
	h.expect(h.kb(withNone("add", src)...), 0)
	if _, err := os.Stat(src); err != nil {
		h.fail("kb add removed its source file %s", src)
	}
	r := h.kb(withNone("search", "zebracorn")...)
	if h.expect(r, 0) && !strings.Contains(r.Stdout, "data/alpha-entry.md") {
		h.fail("search did not find the added entry:\n%s", r)
	}
	r = h.kb(withNone("search", "zebracorn", "--json")...)
	if h.expect(r, 0) && !json.Valid([]byte(r.Stdout)) {
		h.fail("search --json is not valid JSON:\n%s", r)
	}
	h.expect(h.kb(withNone("show", "data/alpha-entry.md")...), 0)
	h.expect(h.kb(withNone("show", "--chunks", "data/alpha-entry.md")...), 0)
	h.expect(h.kb(withNone("list")...), 0)
	h.expect(h.kb(withNone("doctor")...), 0)
	h.expect(h.kb(withNone("add", src)...), 1) // already indexed
}

func scNoModelSearch(h *H) {
	// A database indexed with an FTS-only config: --mode fts needs no model, and the default
	// (local, hybrid) is a recorded-model mismatch the user must resolve, not a silent fallback.
	h.write("data/fallback.md", entryText("Fallback", "## A\n\nquokkaword lives here.\n"))
	h.useNone()
	h.must(h.kb(withNone("reindex", "--all")...).Code == 0, "index with an FTS-only config")
	r := h.kb("search", "quokkaword", "--mode", "fts")
	if h.expect(r, 0) && !strings.Contains(r.Stdout, "data/fallback.md") {
		h.fail("--mode fts found nothing:\n%s", r)
	}
	h.setConfig("", false) // the default model against a none-indexed database
	r = h.kb("search", "quokkaword")
	if h.expect(r, 1, 2) && r.Code == 1 && !strings.Contains(r.Stderr, "reindex --all") {
		h.fail("mismatch error does not say how to fix it:\n%s", r)
	}
	// A fresh root with no database: search never downloads and never creates the database.
	h2 := filepath.Join(h.dir, "fresh")
	_ = os.MkdirAll(filepath.Join(h2, "data"), 0o755)
	saved := h.root
	h.root = h.makeLocation(h2)
	h.expect(h.kb("search", "anything"), 1, 2)
	if _, err := os.Stat(h.dbPath()); err == nil {
		h.fail("kb search created a database on a fresh root")
	}
	h.root = saved
	h.note("the FTS fallback (exit 2 + `kb model pull` hint) needs a DB indexed with the local model, which needs the model: covered by internal/cli tests, not reachable here")
}

func scNoModelIndex(h *H) {
	h.write("data/needs-model.md", entryText("Needs", "## A\n\nbody text\n"))
	r := h.kb("reindex", "--all")
	h.expect(r, 2)
	if !strings.Contains(r.Stderr, "download") {
		h.note("reindex stderr does not mention the download attempt:\n%s", r)
	}
	src := h.write(filepath.Join(h.outside, "new-one.md"), entryText("New", "## A\n\nbody\n"))
	r = h.kb("add", src)
	h.expect(r, 2)
	if _, err := os.Stat(filepath.Join(h.root, "data", "new-one.md")); err == nil {
		h.fail("add exited %d but left data/new-one.md behind (not all-or-nothing)", r.Code)
	}
	r = h.kb(withNone("search", "body")...)
	if r.Code == 0 && strings.Contains(r.Stdout, "new-one") {
		h.fail("a failed add left new-one indexed:\n%s", r)
	}
}

// hostileQueries are passed verbatim as one argument.
var hostileQueries = []string{
	`"`, `""`, `"unterminated`, `*`, `a*`, `*a`, `-`, `- x`, `x -y`, `^`, `+`, `(`, `)`, `()`, `:`, `title:x`,
	`text:alpha OR`, `NEAR(alpha beta)`, `NEAR(`, `alpha NEAR/2 beta`, `AND`, `OR`, `NOT`,
	`alpha AND`, `OR alpha`, `NOT NOT`, `{alpha}`, `alpha'beta`, `'; DROP TABLE entries; --`,
	`\`, `%`, `_`, `ünïcödé`, `日本語`, `🙂`, "tab\there", "new\nline", "\u200b", "a\u0301",
	strings.Repeat("a ", 5000), strings.Repeat("x", 100000), strings.Repeat(`"`, 999),
}

func scHostileQueries(h *H) {
	h.write("data/alpha.md", entryText("Alpha", "## A\n\nalpha beta gamma.\n"))
	h.must(h.kb(withNone("reindex", "--all")...).Code == 0, "index")
	bad := []string{"syntax error", "SQL logic error", "fts5:", "unterminated string", "no such column"}
	for _, q := range hostileQueries {
		for _, mode := range []string{"fts", "hybrid"} {
			// "--" ends flag parsing, so a query that starts with "-" is a query, not a flag.
			r := h.kb(withNone("search", "--mode", mode, "--json", "--", q)...)
			if r.Code != 0 {
				h.fail("query %q (--mode %s) did not exit 0:\n%s", clip(q, 40), mode, r)
				continue
			}
			for _, b := range bad {
				if strings.Contains(r.Stderr+r.Stdout, b) {
					h.fail("query %q leaked a SQLite error %q:\n%s", clip(q, 40), b, r)
				}
			}
			if !json.Valid([]byte(r.Stdout)) {
				h.fail("query %q: --json output is not valid JSON:\n%s", clip(q, 40), r)
			}
		}
	}
	r := h.kb(withNone("doctor")...)
	h.expect(r, 0) // hostile queries never damage the index
}

func scMarkdownEdges(h *H) {
	big := strings.Repeat("Paragraph with words to index. ", 40) + "\n\n"
	deep := ""
	for i := 2; i <= 6; i++ {
		deep += strings.Repeat("#", i) + " Level " + fmt.Sprint(i) + "\n\nbody " + fmt.Sprint(i) + "\n\n"
	}
	files := map[string]string{
		"data/crlf.md":       strings.ReplaceAll(entryText("Crlf", "## Sec\r\n\r\ncrlfword body\r\n"), "\n", "\r\n"),
		"data/bom.md":        "\ufeff" + entryText("Bom", "## Sec\n\nbomword body\n"),
		"data/nul.md":        entryText("Nul", "## Sec\n\nnul\x00word body\n"),
		"data/unicode.md":    entryText("Ünïcödé 日本", "## Séction\n\nüberword 日本語 🙂 body\n"),
		"data/fence.md":      entryText("Fence", "## Real\n\n```md\n## not a heading\n\n## still code\n```\n\nfenceword after\n"),
		"data/tilde.md":      entryText("Tilde", "## Real\n\n~~~\n## inside tilde\n~~~\n"),
		"data/open-fence.md": entryText("Open fence", "## Real\n\n```\n## never closed\nmore\n"),
		"data/empty-sec.md":  entryText("Empty", "## One\n\n## Two\n\n\n## Three\n\nonly three\n"),
		"data/no-body.md":    entryText("No body", ""),
		"data/deep.md":       entryText("Deep", deep),
		"data/huge.md":       entryText("Huge", "## Big\n\n"+strings.Repeat(big, 2000)),
		"data/one-para.md":   entryText("One para", "## Big\n\n"+strings.Repeat("unbroken ", 4000)+"\n"),
		"data/hashes.md":     entryText("Hashes", "##\n\n###\n\n## \n\nbody\n"),
	}
	for p, c := range files {
		h.write(p, c)
	}
	r := h.kb(withNone("reindex", "--all")...)
	h.expect(r, 0)
	if r.Dur > 20*time.Second {
		h.fail("reindex of %d edge-case files took %s", len(files), r.Dur)
	}
	for p := range files {
		r := h.kb(withNone("show", "--chunks", p)...)
		h.expect(r, 0)
	}
	for _, w := range []string{"crlfword", "bomword", "überword", "fenceword"} {
		r := h.kb(withNone("search", w, "--mode", "fts")...)
		if h.expect(r, 0) && strings.Contains(r.Stdout, "0 hits") {
			h.fail("search %q found nothing:\n%s", w, r)
		}
	}
	r = h.kb(withNone("search", "inside tilde", "--mode", "fts", "--json")...)
	h.note("text inside a fence is searchable (expected): exit %d", r.Code)
	h.expect(h.kb(withNone("doctor")...), 0)
}

func scFrontMatter(h *H) {
	bad := map[string]string{
		"no-title.md":    "---\nsummary: s\n---\n\nbody\n",
		"no-summary.md":  "---\ntitle: t\n---\n\nbody\n",
		"multi-line.md":  "---\ntitle: t\nsummary: |\n  one\n  two\n---\n\nbody\n",
		"unclosed.md":    "---\ntitle: t\nsummary: s\n\nbody\n",
		"empty-fm.md":    "---\n---\n\nbody\n",
		"tabs.md":        "---\ntitle:\tt\n\tsummary: s\n---\n",
		"binary.md":      "---\ntitle: \x00\x01\x02\nsummary: s\n---\n",
		"only-fence.md":  "---",
		"secret-like.md": entryText("Secret", "## A\n\naws_secret_access_key = AKIAIOSFODNN7EXAMPLE1234\n"),
		"huge-title.md":  "---\ntitle: " + strings.Repeat("T", 100000) + "\nsummary: s\n---\n\nbody\n",
		"dup-keys.md":    "---\ntitle: a\ntitle: b\nsummary: s\n---\n\nbody\n",
		"yaml-anchor.md": "---\ntitle: &a x\nsummary: *a\n---\n\nbody\n",
		"empty.md":       "",
	}
	h.write("data/good.md", entryText("Good", "## A\n\ngoodword\n"))
	for name, c := range bad {
		h.write("data/"+name, c)
	}
	r := h.kb(withNone("reindex", "--all")...)
	h.note("reindex with %d broken entries: exit %d, %d warning lines", len(bad), r.Code, strings.Count(r.Stderr, "warning"))
	h.expect(r, 0, 1)
	s := h.kb(withNone("search", "goodword")...)
	if h.expect(s, 0) && !strings.Contains(s.Stdout, "data/good.md") {
		h.fail("a broken entry stopped the good one from being indexed:\n%s", s)
	}
	h.expect(h.kb(withNone("list")...), 0)
	for name, c := range bad {
		src := h.write(filepath.Join(h.outside, "add-"+strings.TrimSuffix(name, ".md")+".md"), c)
		r := h.kb(withNone("add", src)...)
		dst := filepath.Join(h.root, "data", filepath.Base(src))
		if r.Code == 0 {
			h.note("kb add accepted %s", name)
			continue
		}
		h.expect(r, 1)
		if _, err := os.Stat(dst); err == nil {
			h.fail("kb add refused %s (exit %d) but wrote %s", name, r.Code, dst)
		}
	}
}

func scPathTraversal(h *H) {
	const secret = "TOPSECRETOUTSIDE"
	outsideFile := h.write(filepath.Join(h.outside, "secret.md"), entryText("Secret", "## A\n\n"+secret+"\n"))
	h.write("data/inside.md", entryText("Inside", "## A\n\ninside\n"))
	h.must(h.kb(withNone("reindex", "--all")...).Code == 0, "index")
	relOut, _ := filepath.Rel(h.root, outsideFile)
	targets := []string{
		relOut, "data/" + relOut, outsideFile, "data/../../outside/secret.md", "../outside/secret.md",
		"/etc/hosts", "data/../../../../../../etc/hosts", "data/./inside.md/..", "data\\..\\..\\outside\\secret.md",
	}
	for _, t := range targets {
		r := h.kb(withNone("show", t)...)
		if strings.Contains(r.Stdout, secret) || strings.Contains(r.Stdout, "localhost") {
			h.fail("show %q printed a file outside the root:\n%s", t, r)
		}
		if strings.Contains(t, "etc/hosts") {
			continue // never hand a system path to a destructive command, even one that should refuse
		}
		h.kb(withNone("rm", "-y", t)...)
		h.kb(withNone("reindex", t)...)
	}
	if _, err := os.Stat(outsideFile); err != nil {
		h.fail("a file outside the root was removed: %s", outsideFile)
	}
	// A symlinked entry pointing outside the root.
	link := filepath.Join(h.root, "data", "linked.md")
	h.must(os.Symlink(outsideFile, link) == nil, "symlink")
	r := h.kb(withNone("reindex", "--all")...)
	s := h.kb(withNone("search", secret, "--mode", "fts")...)
	if strings.Contains(s.Stdout, "linked") || r.Code != 0 || !strings.Contains(r.Stderr, "skipped data/linked.md") {
		// DEFAULTS #65, PLAN 1.5: such a link is skipped with a warning, never indexed.
		h.fail("a symlink under data/ to a file outside the root: want it skipped with a warning, exit 0\n%s\n%s", r, s)
	}
	h.kb(withNone("rm", "data/linked.md")...)
	if _, err := os.Stat(outsideFile); err != nil {
		h.fail("kb rm of a symlink removed its target outside the root")
	}
	// A symlinked data/ dir entry pointing outside.
	h.must(os.MkdirAll(filepath.Join(h.outside, "topic"), 0o755) == nil, "mkdir")
	h.write(filepath.Join(h.outside, "topic", "README.md"), entryText("Topic", "## A\n\nx\n"))
	h.must(os.Symlink(filepath.Join(h.outside, "topic"), filepath.Join(h.root, "data", "topic")) == nil, "dir symlink")
	h.kb(withNone("reindex", "--all")...)
	h.kb(withNone("rm", "-y", "data/topic/README.md")...)
	if _, err := os.Stat(filepath.Join(h.outside, "topic", "README.md")); err != nil {
		h.fail("kb rm -y of a symlinked directory entry deleted files outside the root")
	}
}

func scFeedback(h *H) {
	h.write("data/fb.md", entryText("Fb", "## A\n\nfeedbackword\n"))
	h.must(h.kb(withNone("reindex", "--all")...).Code == 0, "index")
	h.must(h.kb(withNone("search", "feedbackword")...).Code == 0, "search")
	cases := [][]string{
		{"999", "1", "--useful"}, {"1", "0", "--useful"}, {"1", "99", "--useful"}, {"1", "-1", "--useful"},
		{"1", "1", "--useful", "--not-useful"}, {"1", "1", "--none"}, {"1"}, {"abc", "1", "--useful"},
		{"1", "1.5", "--useful"}, {"--last", "1", "--useful", "--caller", "nobody-searched"},
		{"99999999999999999999", "1", "--useful"}, {"1", "1", "2", "--useful"},
	}
	for _, c := range cases {
		h.expect(h.kb(withNone(append([]string{"feedback"}, c...)...)...), 1)
	}
	h.expect(h.kb(withNone("feedback", "1", "1", "--useful")...), 0)
	r := h.kb(withNone("stats", "--sql", "SELECT count(*) AS n FROM search_feedback", "--json")...)
	if h.expect(r, 0) && !strings.Contains(strings.ReplaceAll(r.Stdout, " ", ""), "1") {
		h.fail("expected exactly one feedback row:\n%s", r)
	}
	if strings.Contains(r.Stdout, `"n":2`) || strings.Contains(r.Stdout, "[2]") {
		h.fail("a refused feedback was recorded:\n%s", r)
	}
	r = h.kb(withNone("stats", "--sql", "DELETE FROM searches")...)
	if r.Code == 0 {
		h.fail("stats --sql ran a write:\n%s", r)
	}
}

func scConcurrent(h *H) {
	const n = 6
	srcs := make([]string, n)
	for i := range n {
		srcs[i] = h.write(filepath.Join(h.outside, fmt.Sprintf("conc-%d.md", i)), entryText(fmt.Sprintf("Conc %d", i), "## A\n\nconcword body\n"))
	}
	type proc struct {
		args []string
		done chan res
	}
	var procs []proc
	start := func(args ...string) {
		p := proc{args: args, done: make(chan res, 1)}
		go func() { p.done <- h.kbQuiet(args...) }()
		procs = append(procs, p)
	}
	for i := range n {
		start(withNone("add", srcs[i])...)
	}
	for range 3 {
		start(withNone("search", "concword")...)
	}
	for range 2 {
		start(withNone("reindex", "--all")...)
	}
	for _, p := range procs {
		r := <-p.done
		h.invariants(r)
		if strings.Contains(r.Stderr, "locked") || strings.Contains(r.Stderr, "busy") {
			h.fail("SQLITE_BUSY reached the user:\n%s", r)
		}
		// A search may run before the first migration commits: that is "no database yet" (1).
		if p.args[0] == "search" {
			h.expect(r, 0, 1)
		} else {
			h.expect(r, 0)
		}
	}
	r := h.kb(withNone("search", "concword", "--k", "20", "--json")...)
	if h.expect(r, 0) {
		for i := range n {
			if !strings.Contains(r.Stdout, fmt.Sprintf("conc-%d.md", i)) {
				h.fail("entry conc-%d.md missing after concurrent writers:\n%s", i, r)
			}
		}
	}
	h.expect(h.kb(withNone("doctor")...), 0)
}

// kbQuiet is kb without the invariant check, for goroutines (H is not safe for concurrent use).
func (h *H) kbQuiet(args ...string) res {
	c, o, e := h.cmd(args...)
	t0 := time.Now()
	err := c.Run()
	return res{Args: args, Code: exitCode(err), Stdout: o.String(), Stderr: e.String(), Dur: time.Since(t0)}
}

func scSigint(h *H) {
	for i := range 400 {
		h.write(fmt.Sprintf("data/e-%03d.md", i), entryText(fmt.Sprintf("Entry %d", i),
			strings.Repeat(fmt.Sprintf("## S\n\nsigword %d body text for chunking.\n\n", i), 20)))
	}
	interrupted := false
	for _, delay := range []time.Duration{80 * time.Millisecond, 250 * time.Millisecond, 600 * time.Millisecond} {
		c, o, e := h.cmd(withNone("reindex", "--all")...)
		h.must(c.Start() == nil, "start reindex")
		time.Sleep(delay)
		_ = c.Process.Signal(syscall.SIGINT)
		err := c.Wait()
		r := res{Args: []string{"reindex", "--all", "(SIGINT after " + delay.String() + ")"}, Code: exitCode(err), Stdout: o.String(), Stderr: e.String()}
		h.invariants(r)
		if r.Code != 0 {
			interrupted = true
		}
		h.note("SIGINT after %s: exit %d, stderr %q", delay, r.Code, clip(r.Stderr, 120))
		d := h.kb(withNone("doctor")...)
		if strings.Contains(d.Stdout+d.Stderr, "malformed") || strings.Contains(d.Stdout, "FAIL driver") {
			h.fail("database damaged by SIGINT:\n%s", d)
		}
	}
	if !interrupted {
		h.note("reindex finished before every SIGINT; the interrupt path was not exercised")
	}
	h.expect(h.kb(withNone("reindex", "--all")...), 0)
	h.expect(h.kb(withNone("doctor")...), 0)
	r := h.kb(withNone("search", "sigword", "--json")...)
	h.expect(r, 0)
}

func (h *H) indexOne() {
	h.write("data/c.md", entryText("C", "## A\n\ncorruptword\n"))
	h.must(h.kb(withNone("reindex", "--all")...).Code == 0, "index")
	_, err := os.Stat(h.dbPath() + "-wal")
	h.must(os.IsNotExist(err), "WAL file left after kb exited (checkpoint on close)")
}

func scCorruptDB(h *H) {
	h.indexOne()
	orig, err := os.ReadFile(h.dbPath())
	h.must(err == nil, "read db")
	rng := rand.New(rand.NewSource(1))
	garbage := make([]byte, 8192)
	rng.Read(garbage)
	headerOK := append(append([]byte{}, orig[:100]...), garbage...)
	variants := []struct {
		name string
		data []byte
	}{
		{"truncated-half", orig[:len(orig)/2]},
		{"truncated-header", orig[:50]},
		{"garbage", garbage},
		{"valid-header-garbage-pages", headerOK},
		{"zero-length", nil},
	}
	for _, v := range variants {
		h.must(os.WriteFile(h.dbPath(), v.data, 0o644) == nil, "write variant")
		for _, args := range [][]string{{"search", "corruptword"}, {"doctor"}, {"list"}, {"stats", "overview"}, {"reindex", "--all"}} {
			r := h.kb(withNone(args...)...)
			if r.Code == 0 && v.name != "zero-length" && args[0] != "list" && args[0] != "reindex" {
				h.fail("%s: kb %s exited 0 on a corrupt database:\n%s", v.name, args[0], r)
			}
			if r.Code != 0 && !strings.Contains(r.Stderr+r.Stdout, ".kb") && !strings.Contains(r.Stderr+r.Stdout, "database") {
				h.note("%s: kb %s error does not name the database:\n%s", v.name, args[0], r)
			}
			if args[0] == "reindex" {
				h.note("%s: reindex --all exit %d", v.name, r.Code)
			}
		}
		_ = os.Remove(h.dbPath() + "-wal")
		_ = os.Remove(h.dbPath() + "-shm")
	}
}

// setUserVersion writes PRAGMA user_version (big-endian uint32 at byte 60 of the header).
func setUserVersion(path string, v uint32) error {
	b, err := os.ReadFile(path)
	if err != nil {
		return err
	}
	if len(b) < 100 || !bytes.HasPrefix(b, []byte("SQLite format 3\x00")) {
		return fmt.Errorf("%s is not a SQLite database", path)
	}
	binary.BigEndian.PutUint32(b[60:64], v)
	return os.WriteFile(path, b, 0o644)
}

func scNewerSchema(h *H) {
	h.indexOne()
	h.must(setUserVersion(h.dbPath(), 99) == nil, "set user_version")
	before := fileHash(h.dbPath())
	for _, args := range [][]string{{"search", "corruptword"}, {"reindex", "--all"}, {"doctor"}, {"stats", "overview"}, {"feedback", "1", "--none"}} {
		r := h.kb(withNone(args...)...)
		if r.Code == 0 {
			h.fail("kb %s accepted a database at schema version 99:\n%s", args[0], r)
		} else if !strings.Contains(r.Stderr+r.Stdout, "version") && !strings.Contains(r.Stderr+r.Stdout, "newer") {
			h.note("kb %s refusal does not say why (version):\n%s", args[0], r)
		}
	}
	if fileHash(h.dbPath()) != before {
		h.fail("the newer-schema database was modified")
	}
}

func fileHash(p string) string {
	b, err := os.ReadFile(p)
	if err != nil {
		return "missing"
	}
	return fmt.Sprintf("%x", sha256.Sum256(b))
}

// scSearchSettings is H32: the [search] tuning keys reach kb search (PLAN 2.1) and their bounds
// (config.Max*) are enforced at the config boundary, so a hostile value is a well-formed exit 1
// that names the file and key, before anything is opened or logged.
func scSearchSettings(h *H) {
	const none = "[embed]\nmodel = \"none\"\n"
	h.write("data/settings-a.md", entryText("Settings A", "## Body\n\nquasarword appears here.\n"))
	h.write("data/settings-b.md", entryText("Settings B", "## Body\n\nquasarword appears here too, quasarword twice.\n"))
	h.must(h.kb(withNone("reindex", "--all")...).Code == 0, "index with an FTS-only config")

	// rrf_k = 1 with the FTS list at weight 1: the top hit scores 1/(1+1) = 0.5.
	h.setConfig(none+"[search]\nrrf_k = 1\n[search.list_weights]\nvec = 0.0\n", false)
	r := h.kb(withNone("search", "quasarword", "--json")...)
	if h.expect(r, 0) && !strings.Contains(r.Stdout, `"rrf_score": 0.5,`) {
		h.fail("rrf_k = 1 did not reach the fused score (want a top rrf_score of 0.5):\n%s", r)
	}
	// fts weight 0: every hit scores 0 but is still listed.
	h.setConfig(none+"[search.list_weights]\nfts = 0.0\n", false)
	r = h.kb(withNone("search", "quasarword", "--json")...)
	if h.expect(r, 0) {
		if !json.Valid([]byte(r.Stdout)) || !strings.Contains(r.Stdout, "settings-a") {
			h.fail("fts weight 0 lost the hits or broke --json:\n%s", r)
		}
	}

	// Every key at its upper bound: search still works and --json still encodes.
	maxed := none + "[search]\nlist_limit = 4096\ncandidate_limit = 8192\nper_entry_cap = 1000\nrrf_k = 100000\nvec_overfetch = 4096\n" +
		"[search.bm25]\ntext = 1000.0\nheading = 1000.0\ntitle = 1000.0\n[search.list_weights]\nfts = 1000.0\nvec = 1000.0\n"
	h.setConfig(maxed, false)
	r = h.kb(withNone("search", "quasarword", "--json")...)
	if h.expect(r, 0) && (!json.Valid([]byte(r.Stdout)) || strings.Contains(r.Stdout, "NaN") || strings.Contains(r.Stdout, "Inf")) {
		h.fail("search with every [search] key at its max did not encode finite JSON:\n%s", r)
	}

	// Out of range: exit 1 naming the file and the key; the database is not touched.
	before := fileHash(h.dbPath())
	for _, tc := range []struct{ key, body string }{
		{"search.list_limit", "[search]\nlist_limit = 4097\n"},
		{"search.list_limit", "[search]\nlist_limit = 0\n"},
		{"search.candidate_limit", "[search]\ncandidate_limit = 8193\n"},
		{"search.rrf_k", "[search]\nrrf_k = 100001\n"},
		{"search.rrf_k", "[search]\nrrf_k = -1\n"},
		{"search.vec_overfetch", "[search]\nvec_overfetch = 4097\n"},
		{"search.per_entry_cap", "[search]\nper_entry_cap = 0\n"},
		{"search.bm25.title", "[search.bm25]\ntitle = 1000.5\n"},
		{"search.bm25.text", "[search.bm25]\ntext = -1.0\n"},
		{"search.bm25.heading", "[search.bm25]\nheading = nan\n"},
		{"search.list_weights.fts", "[search.list_weights]\nfts = inf\n"},
		{"search.list_weights.vec", "[search.list_weights]\nvec = -inf\n"},
	} {
		h.setConfig(none+tc.body, false)
		r := h.kb(withNone("search", "quasarword")...)
		if !h.expect(r, 1) {
			continue
		}
		if !strings.Contains(r.Stderr, "config.toml") || !strings.Contains(r.Stderr, tc.key) {
			h.fail("%s out of range: the error should name the file and %s:\n%s", strings.TrimSpace(tc.body), tc.key, r)
		}
	}
	if fileHash(h.dbPath()) != before {
		h.fail("a search refused for its config still changed the database (logged?)")
	}
}

// scCacheTwoStores is H19. It runs on the default model, which cannot be downloaded here, so no
// vector is ever computed: it attacks the shared file, not the reuse (the Go test
// TestEmbedCacheSharedAcrossStores covers reuse). Commands still open the cache before they find
// the model missing, so reindex exits 2 and the cache must stay quiet, intact and unlocked.
func scCacheTwoStores(h *H) {
	cacheFile := filepath.Join(h.home, "Library", "Caches", "kb", "embed-cache.sqlite")
	second := h.makeLocation(filepath.Join(h.outside, "second"))
	h.write("data/c.md", entryText("C", "## A\n\ncacheword body\n"))
	h.write(filepath.Join(second, "data", "c.md"), entryText("C", "## A\n\ncacheword body\n"))

	// A missing cache is "nothing cached", not an error, and neither flag creates it.
	os.Remove(cacheFile)
	for _, flag := range []string{"--stats", "--clear"} {
		r := h.kb("model", "cache", flag)
		if h.expect(r, 0) && !strings.Contains(r.Stdout, "not created yet") {
			h.fail("kb model cache %s on a missing cache: want 'not created yet':\n%s", flag, r)
		}
		if _, err := os.Stat(cacheFile); err == nil {
			h.fail("kb model cache %s created the cache file", flag)
		}
	}

	// Two stores and a stats call open the one file at the same time.
	type proc struct {
		args []string
		done chan res
	}
	var procs []proc
	start := func(args ...string) {
		p := proc{args: args, done: make(chan res, 1)}
		go func() { p.done <- h.kbQuiet(args...) }()
		procs = append(procs, p)
	}
	for range 3 {
		start("reindex", "--all")
		start("--root", second, "reindex", "--all")
		start("model", "cache", "--stats")
	}
	for _, p := range procs {
		r := <-p.done
		h.invariants(r)
		for _, bad := range []string{"locked", "busy", "embedding cache not used", "embedding cache disabled"} {
			if strings.Contains(r.Stderr, bad) {
				h.fail("a healthy shared cache reported %q:\n%s", bad, r)
			}
		}
		if p.args[len(p.args)-1] == "--stats" {
			h.expect(r, 0)
		} else {
			h.expect(r, 2) // the model is missing; nothing else may differ
		}
	}

	// A garbage file: warn, fall back (exit as without a cache), never modify, --clear removes.
	garbage := bytes.Repeat([]byte("not a database "), 512)
	h.must(os.WriteFile(cacheFile, garbage, 0o644) == nil, "write garbage cache")
	r := h.kb("reindex", "--all")
	h.expect(r, 2)
	if !strings.Contains(r.Stderr, "embedding cache not used") || !strings.Contains(r.Stderr, "kb model cache --clear") {
		h.fail("a garbage cache gave no warning naming the way out:\n%s", r)
	}
	if b, _ := os.ReadFile(cacheFile); !bytes.Equal(b, garbage) {
		h.fail("a garbage cache file was modified")
	}
	if r := h.kb("model", "cache", "--stats"); h.expect(r, 1) && !strings.Contains(r.Stderr, "--clear") {
		h.fail("stats of a garbage cache does not name --clear:\n%s", r)
	}
	h.expect(h.kb("model", "cache", "--clear"), 0)
	if _, err := os.Stat(cacheFile); err == nil {
		h.fail("--clear left the garbage cache file")
	}

	// A newer schema: refused with the same warning, the version untouched, --clear removes it.
	h.expect(h.kb("model", "cache", "--stats"), 0)
	h.expect(h.kb("reindex", "--all"), 2) // creates a fresh cache
	os.Remove(cacheFile + "-wal")
	os.Remove(cacheFile + "-shm")
	h.must(setUserVersion(cacheFile, 99) == nil, "set cache user_version")
	r = h.kb("reindex", "--all")
	h.expect(r, 2)
	if !strings.Contains(r.Stderr, "embedding cache not used") {
		h.fail("a newer cache gave no warning:\n%s", r)
	}
	if b, _ := os.ReadFile(cacheFile); len(b) < 64 || binary.BigEndian.Uint32(b[60:64]) != 99 {
		h.fail("the newer cache's user_version was changed")
	}
	h.expect(h.kb("model", "cache", "--stats"), 1)
	h.expect(h.kb("model", "cache", "--clear"), 0)
	h.expect(h.kb("model", "cache", "--stats"), 0)
}

// scExplain attacks kb explain: it must be valid JSON on hostile queries (FTS5 syntax, quotes,
// huge input), take --set overrides, refuse bad ones with exit 1, and never write the search log
// (the next real search is still search 1).
func scExplain(h *H) {
	const none = "[embed]\nmodel = \"none\"\n"
	h.write("data/explain-a.md", entryText("Explain A", "## Body\n\nquasarword appears here.\n"))
	h.write("data/explain-b.md", entryText("Explain B", "## Body\n\nquasarword and nebulaword live here.\n"))
	h.setConfig(none, false)
	h.must(h.kb(withNone("reindex", "--all")...).Code == 0, "index with an FTS-only config")

	for _, q := range []string{
		"quasarword", "quasarword nebulaword", "quasarword missingword", `"quasarword`, "NEAR(quasarword", "quasar*",
		"title:quasarword", "quasarword AND OR NOT", "&&", "'; DROP TABLE chunks; --", strings.Repeat("quasarword ", 2000), "日本語 quasarword",
	} {
		r := h.kb(withNone("explain", q, "--mode", "fts", "--json")...)
		if !h.expect(r, 0) {
			continue
		}
		if !json.Valid([]byte(r.Stdout)) {
			h.fail("explain %.40q --json is not valid JSON:\n%s", q, r)
		}
	}
	r := h.kb(withNone("explain", "quasarword", "--mode", "fts", "--set", "search.rrf_k=1", "--set", "search.bm25.title=1000")...)
	if h.expect(r, 0) && !strings.Contains(r.Stdout, "rrf_k=1 ") {
		h.fail("--set search.rrf_k=1 not reflected:\n%s", r)
	}
	for _, bad := range [][]string{
		{"--set", "search.rrf_k=0"}, {"--set", "search.nope=1"}, {"--set", "search.bm25.title=nan"}, {"--mode", "bogus"}, {"--set", "embed.model=none", "--mode", "vec"},
	} {
		h.expect(h.kb(withNone(append([]string{"explain", "quasarword"}, bad...)...)...), 1)
	}
	r = h.kb(withNone("search", "quasarword", "--mode", "fts")...)
	if h.expect(r, 0) && !strings.HasPrefix(r.Stdout, "search 1 ·") {
		h.fail("explain wrote the search log (first real search is not search 1):\n%s", r)
	}
}

// scStoresAlias attacks store refs (PLAN 3.1): every grammar form on the real binary, with
// discovery (no --root), plus hostile alias targets in kb.toml.
func scStoresAlias(h *H) {
	other := filepath.Join(h.outside, "other")
	h.expect(h.kbAt(h.outside, "--root", other, "init", "--model", "none"), 0)
	h.expect(h.kbAt(h.outside, "--root", other, "init", "--name", "adr", "--model", "none"), 0)
	h.expect(h.kb("init", "--name", "adr", "--model", "none"), 0)
	tomlPath := filepath.Join(h.root, ".kb", "kb.toml")
	h.write(tomlPath, "version = 1\ndefault_store = \"default\"\n[aliases]\n"+
		"ext = \"adr@"+other+"\"\nwhere = \""+other+"\"\nadr = \"default\"\nbroken = \"adr@"+other+"/nowhere\"\nloop = \"loop\"\n")
	at := func(args ...string) res { return h.kbAt(h.root, args...) }
	cfgPath := func(root, store string) string { return filepath.Join(root, ".kb", "stores", store, "config.toml") }
	wantPath := func(r res, path string) {
		if h.expect(r, 0) && strings.TrimSpace(r.Stdout) != path {
			h.fail("%s: config path %q, want %q", strings.Join(r.Args, " "), strings.TrimSpace(r.Stdout), path)
		}
	}
	wantPath(at("--store", "ext", "config", "--path"), cfgPath(other, "adr"))
	wantPath(at("--store", "where", "config", "--path"), cfgPath(other, "default"))
	wantPath(at("--store", "adr@"+other, "config", "--path"), cfgPath(other, "adr"))
	wantPath(at("--store", other+"/", "config", "--path"), cfgPath(other, "default"))
	wantPath(at("--store", "default@global", "config", "--path"), cfgPath(filepath.Join(h.home, ".local", "share", "kb"), "default"))
	wantPath(at("--store", "global", "config", "--path"), cfgPath(filepath.Join(h.home, ".local", "share", "kb"), "default"))

	// The store adr shadows the alias adr: the store wins, with one warning.
	if r := at("--store", "adr", "config", "--path"); h.expect(r, 0) {
		wantPath(r, cfgPath(h.root, "adr"))
		if strings.Count(r.Stderr, "shadows the alias") != 1 {
			h.fail("want one shadow warning:\n%s", r)
		}
	}
	// An alias names its own location, so --root and --global refuse it.
	for _, flag := range []string{"--root=" + h.root, "--global"} {
		if r := at(flag, "--store", "ext", "config", "--path"); h.expect(r, 1) && !strings.Contains(r.Stderr, "alias") && !strings.Contains(r.Stderr, "no store") {
			h.fail("%s with an alias: %s", flag, r)
		}
		h.expect(at(flag, "--store", "adr@"+other, "config", "--path"), 1)
		h.expect(at(flag, "--store", other, "config", "--path"), 1)
	}
	// Unknown words list what exists and suggest the nearest.
	if r := at("--store", "exte", "config", "--path"); h.expect(r, 1) && !strings.Contains(r.Stderr, `did you mean "ext"`) {
		h.fail("no suggestion for a near-miss:\n%s", r)
	}
	h.expect(at("--store", "loop", "config", "--path"), 1) // an alias never chains
	h.expect(at("--store", "broken", "config", "--path"), 1)
	h.expect(at("--store", "adr@"+other+"/nowhere", "config", "--path"), 1)
	h.expect(at("--store", "ghost@"+other, "config", "--path"), 1)
	h.expect(at("--store", filepath.Join(h.outside, "absent"), "config", "--path"), 1)
	// kb stores shows the aliases, a broken one as a row, and still exits 0.
	if r := at("stores"); h.expect(r, 0) && (!strings.Contains(r.Stdout, "BROKEN") || !strings.Contains(r.Stdout, "-> adr@"+other)) {
		h.fail("kb stores alias rows:\n%s", r)
	}
	if r := at("stores", "--json"); h.expect(r, 0) && !json.Valid([]byte(r.Stdout)) {
		h.fail("kb stores --json is not valid JSON:\n%s", r)
	}
	// Hostile refs on the command line: Usage, never an incident.
	for _, ref := range []string{"adr@", "@", "@@", "a@b@c", "~bob/x", "~bob", "docs/x", "c:x", "Adr@/p", "global@/p", "x\x01y",
		"/" + strings.Repeat("a", 5000), strings.Repeat("a", 33) + "@/p", "adr@~bob", "../../../../../../etc"} {
		if r := at("--store", ref, "config", "--path"); r.Code == 3 {
			h.fail("--store %.40q reached an incident:\n%s", ref, r)
		} else {
			h.expect(r, 1)
		}
	}
	// Hostile alias targets in kb.toml are a well-formed error or an unresolvable alias, not a crash.
	for name, target := range map[string]string{
		"empty": "", "control": "a\\u0001b", "huge": strings.Repeat("a", 5000), "tilde-user": "~bob/x", "nul": "a\\u0000b",
		"dots": "../../../../../../../../etc", "at": "@", "colon": "adr:x",
	} {
		h.write(tomlPath, "version = 1\ndefault_store = \"default\"\n[aliases]\nbad = \""+target+"\"\n")
		if r := at("--store", "bad", "config", "--path"); r.Code == 3 {
			h.fail("alias target %s reached an incident:\n%s", name, r)
		} else {
			h.expect(r, 1)
		}
		if r := at("stores"); r.Code == 3 {
			h.fail("kb stores with alias target %s reached an incident:\n%s", name, r)
		}
	}
}

// scEval attacks kb eval: an empty log scores nothing, a judged FTS search scores mrr 1, hostile
// flags exit 1 (out-of-range -k is clamped), --json is valid, deleting the gold entry moves it to
// "gold not in index", and the replays never reach the search log.
func scEval(h *H) {
	const none = "[embed]\nmodel = \"none\"\n"
	h.write("data/eval-a.md", entryText("Eval A", "## Body\n\npulsarword appears here.\n"))
	h.write("data/eval-b.md", entryText("Eval B", "## Body\n\npulsarword and quasarword live here.\n"))
	h.setConfig(none, false)
	h.must(h.kb(withNone("reindex", "--all")...).Code == 0, "index with an FTS-only config")

	if r := h.kb("eval"); h.expect(r, 0) && !strings.Contains(r.Stdout, "queries=0") {
		h.fail("eval on an empty log:\n%s", r)
	}
	r := h.kb("search", "pulsarword", "--mode", "fts")
	h.must(r.Code == 0 && strings.HasPrefix(r.Stdout, "search 1 ·"), "first search is search 1: %s", r)
	h.must(h.kb("feedback", "1", "1", "--useful").Code == 0, "feedback on search 1")

	if r := h.kb("eval"); h.expect(r, 0) && !strings.Contains(r.Stdout, "queries=1 hit@8=1.000 [1.000, 1.000] mrr=1.000") {
		h.fail("eval of one judged search (hybrid with a none embedder runs fts):\n%s", r)
	}
	for _, flags := range [][]string{{"--json"}, {"--json", "--per-query", "-k", "1"}, {"--json", "-k", "99"}, {"--json", "-k", "0"},
		{"--json", "--caller", "'; DROP TABLE searches; --"}, {"--json", "--since", "0s"}, {"--json", "--since", "1970-01-01"}} {
		r := h.kb(append([]string{"eval"}, flags...)...)
		if h.expect(r, 0) && !json.Valid([]byte(r.Stdout)) {
			h.fail("eval %v --json is not valid JSON:\n%s", flags, r)
		}
	}
	for _, bad := range [][]string{{"--mode", "bogus"}, {"--mode", ""}, {"--since", "-1h"}, {"--since", "99999999999999999999d"},
		{"--since", "2026-02-30"}, {"-k", "x"}, {"query"}} {
		h.expect(h.kb(append([]string{"eval"}, bad...)...), 1)
	}
	h.expect(h.kb("eval", "--mode", "vec"), 1) // vec needs an embedder; none is configured

	// Delete the gold entry: eval drops it and says so.
	h.must(os.Remove(filepath.Join(h.root, "data", "eval-a.md")) == nil, "remove eval-a.md")
	h.must(os.Remove(filepath.Join(h.root, "data", "eval-b.md")) == nil, "remove eval-b.md")
	h.must(h.kb(withNone("reindex", "--all")...).Code == 0, "reindex after deleting the entries")
	if r := h.kb("eval"); h.expect(r, 0) && !strings.Contains(r.Stdout, "gold not in index: 1 chunks (1 queries dropped) · 1 entries") {
		h.fail("deleted gold is not reported as not in the index:\n%s", r)
	}
	if r := h.kb("search", "pulsarword", "--mode", "fts"); h.expect(r, 0) && !strings.HasPrefix(r.Stdout, "search 2 ·") {
		h.fail("eval wrote the search log (the next real search is not search 2):\n%s", r)
	}
}
