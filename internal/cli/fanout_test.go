package cli

import (
	"context"
	"encoding/json"
	"fmt"
	"github.com/nickstrad/kb/internal/store"
	"github.com/nickstrad/kb/internal/store/sqlite/sqlitetest"
	"github.com/nickstrad/kb/internal/workspace"
	"github.com/spf13/cobra"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/nickstrad/kb/internal/config"
	"github.com/nickstrad/kb/internal/embed"
	"github.com/nickstrad/kb/internal/embed/fake"
	"github.com/nickstrad/kb/internal/errs"
	"github.com/nickstrad/kb/internal/store/sqlite"
)

// fanOutWorld: the current location (loc) with stores default (primary), adr (same data/, so an
// ensemble), old (an older schema), empty (never indexed) and the alias notes -> other; another
// location (other) whose default store indexes a different entry. Every store uses the fake model.
type fanOutWorld struct{ loc, other string }

func newFanOutWorld(t *testing.T) fanOutWorld {
	t.Helper()
	useFakeEmbedder(t)
	w := fanOutWorld{loc: realTempDir(t), other: realTempDir(t)}
	makeLocation(t, w.loc, "default")
	makeLocation(t, w.other, "default")
	for _, s := range []string{"adr", "old", "empty"} {
		addStore(t, w.loc, s)
	}
	writeFile(t, filepath.Join(w.loc, ".kb", "kb.toml"), "version = 1\ndefault_store = \"default\"\n[aliases]\nnotes = \""+w.other+"\"\n")
	add := func(root, name, title string) {
		src := filepath.Join(t.TempDir(), name)
		writeFile(t, src, strings.Replace(validFrontMatter(title, "Zebracorn notes."), "Some body", "zebracorn body", 1))
		if _, stderr, code := run(t, "--root", root, "add", src); code != 0 {
			t.Fatalf("add %s: %d %q", name, code, stderr)
		}
	}
	add(w.loc, "wal.md", "Wal")
	add(w.other, "far.md", "Far")
	for _, s := range []string{"adr", "old"} {
		if _, stderr, code := run(t, "--root", w.loc, "--store", s, "reindex", "--all"); code != 0 {
			t.Fatalf("reindex %s: %d %q", s, code, stderr)
		}
	}
	installIndex(t, filepath.Join(w.loc, ".kb", "stores", "old", "kb.sqlite"), sqlite.SchemaVersion()-1)
	chdir(t, w.loc)
	return w
}

// TestSearchFanOut is H39 and PLAN 3.2's acceptance as a unit test: one ok store in the same
// location (an ensemble with the primary), one older schema, one with no database, an alias and the
// same store as NAME@LOC (deduplicated), and one unknown word. Bad stores are skipped with one
// warning each, the search exits 0 with the good stores' hits tagged, and only the primary logs,
// with each hit's canonical store.
func TestSearchFanOut(t *testing.T) {
	t.Parallel()
	w := newFanOutWorld(t)
	adrDB := filepath.Join(w.loc, ".kb", "stores", "adr", "kb.sqlite")
	adrBefore := readFileBytes(t, adrDB)
	stdout, stderr, code := run(t, "search", "zebracorn", "--stores", "adr,old,empty,notes,default@"+w.other+",nope")
	if code != 0 {
		t.Fatalf("exit %d, stderr %q", code, stderr)
	}
	for _, want := range []string{
		"warning: skipping store old:", "kb --store old@" + w.loc + " migrate up",
		"warning: skipping store empty: empty@" + w.loc + " has no database yet",
		`warning: skipping store nope: no store "nope"`,
	} {
		if !strings.Contains(stderr, want) {
			t.Errorf("stderr lacks %q:\n%s", want, stderr)
		}
	}
	if n := strings.Count(stderr, "warning:"); n != 3 {
		t.Errorf("%d warnings, want 3 (one per bad store, none for the duplicate):\n%s", n, stderr)
	}
	// wal.md: primary and adr index the same file (same location): one hit, the primary's tag (a
	// tie on rank). far.md: the other location's, tagged with the alias as typed.
	if !strings.Contains(stdout, "] data/wal.md") || !strings.Contains(stdout, "] notes:data/far.md") || strings.Contains(stdout, "adr:") {
		t.Errorf("stdout:\n%s", stdout)
	}
	if strings.Count(stdout, "data/wal.md › Section") != 1 {
		t.Errorf("the ensemble hit appears more than once:\n%s", stdout)
	}

	db := testDBPath(w.loc)
	rows := queryStrings(t, db, "SELECT coalesce(store, 'NULL') || ' ' || entry_path FROM search_results ORDER BY rank")
	if !slices.Contains(rows, "NULL data/wal.md") || !slices.Contains(rows, "default@"+w.other+" data/far.md") {
		t.Errorf("search_results = %v", rows)
	}
	// The ensemble: wal.md's first chunk is rank 1 in both default and adr, so its score is 2/(60+1).
	if got := queryStrings(t, db, "SELECT printf('%.6f', rrf_score) FROM search_results WHERE rank = 1"); !slices.Equal(got, []string{fmt.Sprintf("%.6f", 2.0/61)}) {
		t.Errorf("rank-1 rrf_score = %v, want the summed 2/61", got)
	}
	if n := queryStrings(t, db, "SELECT count(*) FROM search_candidates WHERE store IS NOT NULL AND store != 'default@"+w.other+"'"); n[0] != "0" {
		t.Errorf("candidates from stores other than the alias's: %v (adr's ensemble rows are tagged primary on the tie)", n)
	}
	if string(readFileBytes(t, adrDB)) != string(adrBefore) {
		t.Error("the search wrote to store adr (extra stores open read-only)")
	}
	if fileExists(filepath.Join(w.loc, ".kb", "stores", "empty", "kb.sqlite")) {
		t.Error("the search created store empty's database")
	}

	// --json carries the tag; the primary's hits have none.
	out, _, code := run(t, "search", "zebracorn", "--stores", "notes", "--json")
	var doc struct {
		Hits []struct {
			Store string `json:"store"`
			Path  string `json:"path"`
		} `json:"hits"`
	}
	if err := json.Unmarshal([]byte(out), &doc); err != nil || code != 0 {
		t.Fatalf("json: %v %d %s", err, code, out)
	}
	tags := map[string]string{}
	for _, h := range doc.Hits {
		tags[h.Path] = h.Store
	}
	if tags["data/wal.md"] != "" || tags["data/far.md"] != "notes" {
		t.Errorf("json tags = %v", tags)
	}
	if strings.Contains(out, `"store": ""`) {
		t.Error(`a primary hit carries "store": ""; it must be omitted`)
	}

	// kb show REF:path reads the other store's entry and records the open in the primary log,
	// against that store's hit only.
	shown, stderr, code := run(t, "show", "notes:data/far.md")
	if code != 0 || !strings.Contains(shown, "title: Far") {
		t.Fatalf("show notes:data/far.md: %d %q %q", code, shown, stderr)
	}
	if got := queryStrings(t, db, "SELECT entry_path FROM search_opens"); !slices.Equal(got, []string{"data/far.md"}) {
		t.Errorf("search_opens = %v, want the far.md open", got)
	}
	if _, _, code := run(t, "show", "data/far.md"); code == 0 {
		t.Error("an unqualified path of another store resolved in the primary")
	}
	if _, stderr, code := run(t, "show", "nope:data/far.md"); code != 1 || !strings.Contains(stderr, `no store "nope"`) {
		t.Errorf("show nope:… = %d %q, want the unknown-store error", code, stderr)
	}
}

// TestSearchStoresDefaultAndOff: [search] stores is the default fan-out; --stores ” turns it off;
// all means every store here (the primary and the duplicates drop out).
func TestSearchStoresDefaultAndOff(t *testing.T) {
	t.Parallel()
	w := newFanOutWorld(t)
	writeConfig(t, w.loc, "[search]\nstores = [\"notes\"]\n")
	if out, stderr, _ := run(t, "search", "zebracorn"); !strings.Contains(out, "notes:data/far.md") {
		t.Errorf("[search] stores ignored:\n%s\n%s", out, stderr)
	}
	if out, stderr, code := run(t, "search", "zebracorn", "--stores", ""); code != 0 || strings.Contains(out, "far.md") || stderr != "" {
		t.Errorf("--stores '' = %d:\n%s\n%s", code, out, stderr)
	}
	out, stderr, code := run(t, "search", "zebracorn", "--stores", "all")
	if code != 0 || !strings.Contains(stderr, "skipping store old") || !strings.Contains(stderr, "skipping store empty") || strings.Contains(stderr, "skipping store default") {
		t.Errorf("--stores all = %d:\n%s\n%s", code, out, stderr)
	}
}

// TestSearchStoresLimit: more than search.MaxExtraStores distinct stores is a Usage error before
// any is opened; exactly the limit is fine. Duplicates do not count.
func TestSearchStoresLimit(t *testing.T) {
	t.Parallel()
	root := newTempRoot(t)
	useNoneModel(t, root)
	var names []string
	for i := range 9 {
		n := fmt.Sprintf("s%d", i)
		addStore(t, root, n)
		names = append(names, n)
	}
	src := filepath.Join(t.TempDir(), "a.md")
	writeFile(t, src, validFrontMatter("A", "Alpha."))
	if _, stderr, code := run(t, "add", src); code != 0 {
		t.Fatal(stderr)
	}
	_, stderr, code := run(t, "search", "alpha", "--stores", strings.Join(names, ","))
	if code != 1 || !strings.Contains(stderr, "at most 8") || strings.Contains(stderr, "no database") {
		t.Errorf("9 stores = %d %q, want Usage before opening any", code, stderr)
	}
	_, stderr, code = run(t, "search", "alpha", "--stores", strings.Join(append(names[:8], names[0]), ","))
	if code != 0 || strings.Count(stderr, "no database yet") != 8 {
		t.Errorf("8 stores (one twice) = %d %q", code, stderr)
	}
	if _, stderr, code := run(t, "search", "alpha", "--stores", "Bad"); code != 1 || !strings.Contains(stderr, "Bad") {
		t.Errorf("malformed ref = %d %q, want Usage", code, stderr)
	}
}

func TestFanOutRefs(t *testing.T) {
	t.Parallel()
	cfg := []string{"notes"}
	for _, tc := range []struct {
		set  bool
		flag string
		want []string
	}{
		{false, "", cfg},
		{false, "x", cfg},
		{true, "", nil},
		{true, " , ", nil},
		{true, "a, b,,c ", []string{"a", "b", "c"}},
	} {
		if got := fanOutRefs(tc.set, tc.flag, cfg); !slices.Equal(got, tc.want) {
			t.Errorf("fanOutRefs(%v, %q) = %v, want %v", tc.set, tc.flag, got, tc.want)
		}
	}
}

// TestEmbedderSet: one embedder per distinct [embed] config; models lists the distinct real ones.
func TestEmbedderSet(t *testing.T) {
	t.Parallel()
	built := 0
	build := func(m string) func() (embed.Embedder, error) {
		return func() (embed.Embedder, error) { built++; return fake.New(m, 8), nil }
	}
	s := embedderSet{}
	a1, _ := s.get(config.Embed{Model: "a"}, build("a"))
	a2, _ := s.get(config.Embed{Model: "a"}, build("a"))
	s.get(config.Embed{Model: "b"}, build("b"))
	s.get(config.Embed{Model: "none"}, func() (embed.Embedder, error) { return embed.None{}, nil })
	if a1 != a2 || built != 2 {
		t.Errorf("built %d embedders, same instance %v; want 2 and true", built, a1 == a2)
	}
	if got := s.models(); !slices.Equal(got, []string{"a", "b"}) {
		t.Errorf("models = %v", got)
	}
	if _, err := s.get(config.Embed{Model: "c"}, func() (embed.Embedder, error) { return nil, errs.New(errs.Usage, "t", "x") }); err == nil {
		t.Error("a build error was swallowed")
	}
	if _, ok := s[config.Embed{Model: "c"}]; ok {
		t.Error("a failed build was cached")
	}
	n := 0
	var none embedderSet
	none.get(config.Embed{}, func() (embed.Embedder, error) { n++; return embed.None{}, nil })
	none.get(config.Embed{}, func() (embed.Embedder, error) { n++; return embed.None{}, nil })
	if n != 2 {
		t.Errorf("a nil set built %d times, want 2", n)
	}
}

func readFileBytes(t *testing.T, path string) []byte {
	t.Helper()
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	return b
}

// queryStrings runs a one-column query against the store database at path (read-only).
func queryStrings(t *testing.T, path, q string) []string {
	t.Helper()
	return sqlitetest.Strings(t, path, q)
}

// TestSearchManyModelsWarns: a search whose stores load more than two distinct models warns once
// and still answers (design N14).
func TestSearchManyModelsWarns(t *testing.T) {
	t.Parallel()
	orig := depsFor(t).embedder
	t.Cleanup(func() { depsFor(t).embedder = orig })
	depsFor(t).embedder = func(e config.Embed) (embed.Embedder, error) { return fake.New(e.Model, 8), nil }
	var roots []string
	for _, m := range []string{"BAAI/bge-small-en-v1.5", "BAAI/bge-base-en-v1.5", "nomic-ai/nomic-embed-text-v1.5"} {
		root := realTempDir(t)
		makeLocation(t, root, "default")
		writeConfig(t, root, "[embed]\nmodel = \""+m+"\"\n")
		src := filepath.Join(t.TempDir(), "a.md")
		writeFile(t, src, validFrontMatter("A", "Alpha."))
		if _, stderr, code := run(t, "--root", root, "add", src); code != 0 {
			t.Fatalf("add (%s): %s", m, stderr)
		}
		roots = append(roots, root)
	}
	chdir(t, roots[0])
	_, stderr, code := run(t, "search", "alpha", "--stores", roots[1]+","+roots[2])
	if code != 0 || strings.Count(stderr, "loads 3 embedding models") != 1 {
		t.Errorf("exit %d, stderr %q; want one 3-model warning", code, stderr)
	}
	if _, stderr, _ := run(t, "search", "alpha", "--stores", roots[1]); strings.Contains(stderr, "embedding models") {
		t.Errorf("two models warned: %q", stderr)
	}
	// A store whose config no longer matches its index (embed_meta) is skipped with the fix; the
	// search goes on without it.
	writeConfig(t, roots[1], "[embed]\nmodel = \"sentence-transformers/all-MiniLM-L6-v2\"\n")
	out, stderr, code := run(t, "search", "alpha", "--stores", roots[1])
	if code != 0 || !strings.Contains(stderr, "warning: skipping store "+roots[1]) || !strings.Contains(stderr, "kb reindex --all") || !strings.Contains(out, "search ") {
		t.Errorf("mismatched store = %d %q %q; want it skipped with the reindex hint", code, out, stderr)
	}
}

// TestExplainDoesNotFanOut: explain covers the primary store only, even with [search] stores set,
// and says so in its help.
func TestExplainDoesNotFanOut(t *testing.T) {
	t.Parallel()
	w := newFanOutWorld(t)
	writeConfig(t, w.loc, "[search]\nstores = [\"notes\"]\n")
	out, stderr, code := run(t, "explain", "zebracorn", "--mode", "fts")
	if code != 0 || strings.Contains(out, "far.md") || !strings.Contains(out, "wal.md") || stderr != "" {
		t.Errorf("explain with [search] stores = %d:\n%s\n%s", code, out, stderr)
	}
	if help, _, _ := run(t, "explain", "--help"); !strings.Contains(help, "never fans out") {
		t.Errorf("explain --help does not say it covers one store:\n%s", help)
	}
}

// TestShowTargetDecisions is showTarget's decision table: which args are store-qualified
// (REF:path), which stay plain paths, and which are errors.
func TestShowTargetDecisions(t *testing.T) {
	t.Parallel()
	w := newFanOutWorld(t)
	// kb add refuses a colon in a name, but a file written by hand is still an entry.
	writeFile(t, filepath.Join(w.loc, "data", "nope:odd.md"), validFrontMatter("Odd", "A colon in the name."))
	for _, tc := range []struct {
		name, arg string
		code      int
		out, err  string // substrings of stdout / stderr; stderr's resolve "…" names the path looked up
	}{
		{"plain path", "data/wal.md", 0, "title: Wal", ""},
		{"alias REF:path reads the other store", "notes:data/far.md", 0, "title: Far", ""},
		{"same-location store REF:path", "adr:data/wal.md", 0, "title: Wal", ""},
		{"a ref naming this store is a plain path", "default:data/wal.md", 0, "title: Wal", ""},
		{"NAME@LOC prefix is not a ref word: plain path", "default@" + w.other + ":data/far.md", 1, "", `resolve "default@`},
		{"prefix not a store word: plain path", "Bad:data/wal.md", 1, "", `resolve "Bad:data/wal.md": not a knowledge entry`},
		{"empty prefix: plain path", ":data/wal.md", 1, "", `resolve ":data/wal.md": not a knowledge entry`},
		{"empty path after the colon: plain path", "notes:", 1, "", `resolve "notes:": not a knowledge entry`},
		{"unknown ref, no such entry: the ref error", "nope:data/wal.md", 1, "", `no store "nope"`},
		{"unknown ref, but the whole arg is an entry here", "data/nope:odd.md", 0, "title: Odd", ""},
		{"known ref, path missing there: resolved in that store", "notes:data/wal.md", 1, "", `resolve "data/wal.md": not a knowledge entry`},
	} {
		t.Run(tc.name, func(t *testing.T) {
			out, stderr, code := run(t, "show", "--no-log", tc.arg)
			if code != tc.code || !strings.Contains(out, tc.out) || !strings.Contains(stderr, tc.err) {
				t.Errorf("show %q = %d\nstdout %q\nstderr %q\nwant %d, %q, %q", tc.arg, code, out, stderr, tc.code, tc.out, tc.err)
			}
		})
	}
}

// TestFanOutSkipAndKeepDecisions: all mixed with another ref keeps both; an extra store whose own
// config is broken is skipped with one warning naming it (the search goes on); kb show of that
// store's REF:path fails on the same config.
func TestFanOutSkipAndKeepDecisions(t *testing.T) {
	t.Parallel()
	w := newFanOutWorld(t)
	out, stderr, code := run(t, "search", "zebracorn", "--stores", "all,notes")
	if code != 0 || !strings.Contains(out, "notes:data/far.md") {
		t.Errorf("--stores all,notes = %d, the alias dropped:\n%s\n%s", code, out, stderr)
	}
	writeFile(t, filepath.Join(w.loc, ".kb", "stores", "adr", "config.toml"), "[search\n")
	out, stderr, code = run(t, "search", "zebracorn", "--stores", "adr,notes")
	if code != 0 || strings.Count(stderr, "warning: skipping store adr:") != 1 || !strings.Contains(stderr, "config.toml") || !strings.Contains(out, "notes:data/far.md") {
		t.Errorf("broken adr config = %d:\n%s\n%s", code, out, stderr)
	}
	if _, stderr, code := run(t, "show", "--no-log", "adr:data/wal.md"); code != 1 || !strings.Contains(stderr, "config.toml") {
		t.Errorf("show adr:… with a broken adr config = %d %q", code, stderr)
	}
}

// TestSkipExtraDecisions: an extra store's failure is skipped with one warning whatever its kind
// (DEFAULTS 175/206); an Internal one is also an incident named in the warning (a kb bug is not a
// store problem), and --debug adds the chain.
func TestSkipExtraDecisions(t *testing.T) {
	t.Parallel()
	cache := t.TempDir()
	orig := depsFor(t).userCacheDir
	t.Cleanup(func() { depsFor(t).userCacheDir = orig })
	depsFor(t).userCacheDir = func() (string, error) { return cache, nil }
	for _, tc := range []struct {
		name     string
		err      error
		debug    bool
		incident bool
	}{
		{"usage", errs.New(errs.Usage, "t", "bad config"), false, false},
		{"unavailable", errs.New(errs.Unavailable, "t", "no embedder"), true, false},
		{"internal", errs.New(errs.Internal, "t", "boom"), false, true},
		{"internal under debug", errs.New(errs.Internal, "t", "boom"), true, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			_ = os.Remove(filepath.Join(cache, "kb", "kb.log"))
			cmd := &cobra.Command{Use: "search"}
			cmd.Flags().Bool("debug", tc.debug, "")
			var buf strings.Builder
			cmd.SetErr(&buf)
			depsFor(t).skipExtra(cmd, "adr", tc.err)
			got := buf.String()
			if !strings.HasPrefix(got, "warning: skipping store adr: ") || strings.Count(got, "warning: skipping store") != 1 {
				t.Fatalf("stderr %q: want one skip warning naming adr", got)
			}
			incs := readIncidents(t, filepath.Join(cache, "kb", "kb.log"))
			if tc.incident {
				if len(incs) != 1 || incs[0].Kind != "internal" || !strings.Contains(got, "kb bug: id "+incs[0].ID) {
					t.Errorf("incidents %+v, stderr %q: want one internal incident named in the warning", incs, got)
				}
			} else if len(incs) != 0 || strings.Contains(got, "kb bug") {
				t.Errorf("a %s skip left an incident or a bug note: %+v %q", tc.name, incs, got)
			}
			if lines := strings.Count(strings.TrimSpace(got), "\n") + 1; tc.debug && tc.incident && lines < 2 {
				t.Errorf("--debug printed no chain: %q", got)
			}
			if !tc.debug && strings.Count(got, "\n") != 1 {
				t.Errorf("without --debug exactly one line, got %q", got)
			}
		})
	}
}

// TestFanOutHardErrors: a store list that cannot be read, a malformed ref and a hard open failure of
// an extra store (not a schema Conflict) fail the search; none is skipped.
func TestFanOutHardErrors(t *testing.T) {
	t.Parallel()
	w := newFanOutWorld(t)
	if _, stderr, code := run(t, "search", "zebracorn", "--stores", "Bad"); code != 1 || !strings.Contains(stderr, "Bad") {
		t.Errorf("malformed ref = %d %q", code, stderr)
	}
	orig := depsFor(t).openStore
	t.Cleanup(func() { depsFor(t).openStore = orig })
	depsFor(t).openStore = func(ctx context.Context, path string, ro bool) (store.Store, error) {
		if strings.Contains(path, filepath.Join("stores", "adr")) {
			return nil, errs.New(errs.Internal, "test", "adr exploded")
		}
		return orig(ctx, path, ro)
	}
	if _, stderr, code := run(t, "search", "zebracorn", "--stores", "adr"); code != 3 || strings.Contains(stderr, "skipping store adr") {
		t.Errorf("hard open error of an extra store = %d %q, want exit 3 and no skip", code, stderr)
	}
	depsFor(t).openStore = orig

	// expandAll: the stores directory cannot be listed.
	loc, err := workspace.ResolveLocation(w.loc, false, workspace.Env{})
	if err != nil {
		t.Fatal(err)
	}
	dir := loc.StoresDir()
	if err := os.Chmod(dir, 0); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Chmod(dir, 0o755) })
	if _, err := expandAll(loc, []string{allStores}); err == nil {
		t.Error("expandAll with an unreadable stores dir returned no error")
	}
}
