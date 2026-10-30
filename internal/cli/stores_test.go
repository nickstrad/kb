package cli

import (
	"encoding/json"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
)

// globalLocationRoot is where --global points under the test home.
func globalLocationRoot(t *testing.T) string {
	return filepath.Join(depsFor(t).workspaceEnv().Home, ".local", "share", "kb")
}

// useGlobalLocation makes the global location (under the test home) with the given default store
// and the stores default and adr, and removes it after the test.
func useGlobalLocation(t *testing.T, def string) string {
	t.Helper()
	root := globalLocationRoot(t)
	makeLocationFile(t, root, def)
	addStore(t, root, "default")
	addStore(t, root, "adr")
	t.Cleanup(func() { os.RemoveAll(filepath.Join(depsFor(t).workspaceEnv().Home, ".local")) })
	return root
}

// TestStoreResolutionDecisions is the decision table for workspace resolution: the location
// ({--root, --global, discovered}) x --store ({absent, an existing name, a missing name, an invalid
// name, global}) x kb.toml's default_store ({a store, a name with no store}). It checks the store
// `kb config --path` reports, or the error.
func TestStoreResolutionDecisions(t *testing.T) {
	t.Parallel()
	type outcome struct {
		kind string // "ok", "notfound", "usage"
		msg  string
	}
	for _, mode := range []string{"root", "global", "discovered"} {
		for _, flag := range []string{"", "adr", "nope", "Bad", "global"} {
			for _, def := range []string{"default", "ghost"} {
				name := mode + "/store=" + flag + "/default_store=" + def
				t.Run(name, func(t *testing.T) {
					loc := realTempDir(t)
					makeLocationFile(t, loc, def) // no store is created for def: "ghost" names none
					addStore(t, loc, "default")
					addStore(t, loc, "adr")
					globalRoot := useGlobalLocation(t, "default") // the global location always has a good default
					if mode == "global" {
						// --global resolves the global location, which carries this case's default_store.
						makeLocationFile(t, globalRoot, def)
					}
					args := []string{}
					switch mode {
					case "root":
						chdir(t, realTempDir(t))
						args = append(args, "--root", loc)
					case "global":
						chdir(t, loc) // discovery must lose to --global
						args = append(args, "--global")
					case "discovered":
						chdir(t, loc)
					}
					if flag != "" {
						args = append(args, "--store", flag)
					}
					stdout, stderr, code := run(t, append(args, "config", "--path")...)

					// The expectation, derived from the table in PLAN 1.7b.
					storePath := func(root, store string) string {
						return filepath.Join(root, ".kb", "stores", store, "config.toml")
					}
					var want outcome
					var wantPath string
					switch {
					case flag == "Bad":
						want = outcome{"usage", "invalid store name"}
					case flag == "global" && mode == "root":
						want = outcome{"usage", "cannot be combined with --root"}
					case flag == "global" && mode == "global":
						// the global location's own default_store applies
						if def == "ghost" {
							want = outcome{"notfound", "kb.toml"}
						} else {
							want, wantPath = outcome{"ok", ""}, storePath(globalRoot, "default")
						}
					case flag == "global":
						want, wantPath = outcome{"ok", ""}, storePath(globalRoot, "default")
					case flag == "adr":
						root := loc
						if mode == "global" {
							root = globalRoot
						}
						want, wantPath = outcome{"ok", ""}, storePath(root, "adr")
					case flag == "nope":
						want = outcome{"notfound", `no store "nope"`}
					case def == "ghost":
						want = outcome{"notfound", "kb.toml"}
					default:
						root := loc
						if mode == "global" {
							root = globalRoot
						}
						want, wantPath = outcome{"ok", ""}, storePath(root, "default")
					}

					switch want.kind {
					case "ok":
						if code != 0 || strings.TrimSpace(stdout) != wantPath {
							t.Fatalf("exit %d stdout %q stderr %q; want path %s", code, stdout, stderr, wantPath)
						}
					default:
						if code != ExitUsage || stdout != "" || !strings.Contains(stderr, want.msg) {
							t.Fatalf("exit %d stdout %q stderr %q; want exit 1 containing %q", code, stdout, stderr, want.msg)
						}
						if want.kind == "notfound" && !strings.Contains(stderr, "kb init --name") {
							t.Errorf("a missing store should say how to create it: %q", stderr)
						}
						if want.msg == `no store "nope"` && !strings.Contains(stderr, "stores: adr, default") {
							t.Errorf("a missing store should list the existing ones: %q", stderr)
						}
						if want.msg == "kb.toml" && (!strings.Contains(stderr, "default_store") || !strings.Contains(stderr, "stores: adr, default")) {
							t.Errorf("a bad default_store should be named, with the stores listed: %q", stderr)
						}
					}
				})
			}
		}
	}
}

// makeLocationFile rewrites only the kb.toml of a location.
func makeLocationFile(t *testing.T, root, def string) {
	t.Helper()
	writeFile(t, filepath.Join(root, ".kb", "kb.toml"), "version = 1\ndefault_store = \""+def+"\"\n")
}

func TestBrokenLocationFile(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct{ name, body, want string }{
		{"unknown key", "colour = 1\n", `unknown key "colour"`},
		{"bad store name", "default_store = \"../x\"\n", "default_store"},
		{"newer version", "version = 2\n", "newer kb"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			root := newTempRoot(t)
			writeFile(t, filepath.Join(root, ".kb", "kb.toml"), tc.body)
			_, stderr, code := run(t, "list")
			if code != ExitUsage || !strings.Contains(stderr, tc.want) || !strings.Contains(stderr, "kb.toml") {
				t.Fatalf("exit %d stderr %q; want exit 1 naming kb.toml and %q", code, stderr, tc.want)
			}
		})
	}
}

func TestStoreFlagWithRootFlagMissingLocation(t *testing.T) {
	t.Parallel()
	// --root at a directory with no kb.toml: NotFound saying kb init; also for a named store.
	dir := realTempDir(t)
	_, stderr, code := run(t, "--root", dir, "--store", "adr", "list")
	if code != ExitUsage || !strings.Contains(stderr, "run kb init") {
		t.Fatalf("exit %d stderr %q", code, stderr)
	}
}

func TestStoresText(t *testing.T) {
	t.Parallel()
	root := newTempRoot(t)
	writeConfig(t, root, "[embed]\nmodel = \"none\"\n")
	addStore(t, root, "adr")
	writeFile(t, filepath.Join(root, ".kb", "stores", "adr", "config.toml"), "[embed]\nmodel = \"BAAI/bge-base-en-v1.5\"\n")
	writeFile(t, filepath.Join(root, ".kb", "kb.toml"),
		"version = 1\ndefault_store = \"default\"\n[aliases]\nnotes = \"~/knowledge\"\nproj = \"adr@~/src/proj\"\n")
	if err := os.MkdirAll(filepath.Join(root, ".kb", "stores", "BAD"), 0o755); err != nil {
		t.Fatal(err)
	}
	stdout, stderr, code := run(t, "stores")
	if code != 0 || stderr != "" {
		t.Fatalf("exit %d stderr %q", code, stderr)
	}
	home := depsFor(t).workspaceEnv().Home
	want := `STORE      MODEL                  ENTRIES  DATABASE
adr        BAAI/bge-base-en-v1.5  -        not indexed
default *  none                   -        not indexed
warning: ignoring ` + root + `/.kb/stores/BAD: not a valid store name

aliases:
  notes  ~/knowledge     BROKEN: no kb location at ` + home + `/knowledge (no .kb/kb.toml); run kb init --root ` + home + `/knowledge
  proj   adr@~/src/proj  BROKEN: no kb location at ` + home + `/src/proj (no .kb/kb.toml); run kb init --root ` + home + `/src/proj
`
	if stdout != want {
		t.Fatalf("kb stores output:\n%s\nwant:\n%s", stdout, want)
	}
}

func TestStoresJSONAndIndexed(t *testing.T) {
	t.Parallel()
	useFakeEmbedder(t)
	root := newTempRoot(t)
	useNoneModel(t, root)
	addStore(t, root, "adr")
	src := filepath.Join(t.TempDir(), "one.md")
	writeFile(t, src, validFrontMatter("One", "First."))
	if _, stderr, code := run(t, "add", src); code != 0 {
		t.Fatalf("add: %d %q", code, stderr)
	}
	stdout, stderr, code := run(t, "stores", "--json")
	if code != 0 || stderr != "" {
		t.Fatalf("exit %d stderr %q", code, stderr)
	}
	var doc storesJSON
	if err := json.Unmarshal([]byte(stdout), &doc); err != nil {
		t.Fatalf("not JSON: %v\n%s", err, stdout)
	}
	if doc.Location != root || doc.DefaultStore != "default" || len(doc.Stores) != 2 {
		t.Fatalf("doc: %+v", doc)
	}
	adr, def := doc.Stores[0], doc.Stores[1]
	if adr.Name != "adr" || adr.Default || adr.Indexed || adr.Entries != nil || adr.DBBytes != nil || adr.Model == "" {
		t.Errorf("adr row: %+v", adr)
	}
	if def.Name != "default" || !def.Default || !def.Indexed || def.Entries == nil || *def.Entries != 1 || def.DBBytes == nil || *def.DBBytes <= 0 || def.Model != "none" {
		t.Errorf("default row: %+v", def)
	}
	// Keys are a contract: null for the absent numbers, an empty object for no aliases.
	for _, key := range []string{`"entries": null`, `"db_bytes": null`, `"aliases": {}`, `"stores": [`} {
		if !strings.Contains(stdout, key) {
			t.Errorf("JSON lacks %s:\n%s", key, stdout)
		}
	}
	text, _, _ := run(t, "stores")
	if !regexp.MustCompile(`(?m)^default \*\s+none\s+1\s+[0-9.]+ KiB$`).MatchString(text) || !strings.Contains(text, "not indexed") || strings.Contains(text, "aliases:") {
		t.Errorf("text output:\n%s", text)
	}
}

func TestStoresProblemsAreRowsNotAborts(t *testing.T) {
	t.Parallel()
	root := newTempRoot(t)
	addStore(t, root, "adr")
	writeFile(t, filepath.Join(root, ".kb", "stores", "adr", "config.toml"), "colour = 1\n")
	writeFile(t, testDBPath(root), "not a database")
	writeFile(t, filepath.Join(root, ".kb", "stores", "adr", "kb.sqlite"), "not a database either")
	stdout, _, code := run(t, "stores")
	if code != 0 || !strings.Contains(stdout, "problem:") || !strings.Contains(stdout, `unknown key "colour"`) {
		t.Fatalf("exit %d stdout %q", code, stdout)
	}
	if !strings.Contains(stdout, "default *") {
		t.Errorf("the store with a broken database is missing:\n%s", stdout)
	}
	// adr has a bad config and a bad database: the first problem (the config) is the one shown.
	js, _, _ := run(t, "stores", "--json")
	var doc storesJSON
	if err := json.Unmarshal([]byte(js), &doc); err != nil {
		t.Fatal(err)
	}
	adr, def := doc.Stores[0], doc.Stores[1]
	if !strings.Contains(adr.Problem, `unknown key "colour"`) || adr.Model != "" || adr.Entries != nil {
		t.Errorf("adr row: %+v", adr)
	}
	if def.Problem == "" || strings.Contains(def.Problem, "colour") || !def.Indexed || def.Entries != nil || def.Model == "" {
		t.Errorf("default row (a broken database, a good config): %+v", def)
	}
}

func TestStoresUnreadableStoresDir(t *testing.T) {
	t.Parallel()
	root := newTempRoot(t)
	if err := os.RemoveAll(filepath.Join(root, ".kb", "stores")); err != nil {
		t.Fatal(err)
	}
	writeFile(t, filepath.Join(root, ".kb", "stores"), "a file, not a directory")
	if _, stderr, code := run(t, "stores"); code != ExitUsage || !strings.Contains(stderr, "cannot read") {
		t.Fatalf("exit %d stderr %q", code, stderr)
	}
}

func TestStoresDefaultStoreMissingWarns(t *testing.T) {
	t.Parallel()
	root := newTempRoot(t)
	makeLocationFile(t, root, "ghost")
	stdout, _, code := run(t, "stores")
	if code != 0 || !strings.Contains(stdout, `default_store "ghost"`) {
		t.Fatalf("exit %d stdout %q", code, stdout)
	}
}

func TestStoresOutsideLocation(t *testing.T) {
	t.Parallel()
	chdir(t, realTempDir(t))
	_, stderr, code := run(t, "stores")
	if code != ExitUsage || !strings.Contains(stderr, "no kb location found") {
		t.Fatalf("exit %d stderr %q", code, stderr)
	}
}

func TestHumanBytes(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		n    int64
		want string
	}{
		{0, "0 B"}, {1023, "1023 B"}, {1024, "1.0 KiB"}, {1536, "1.5 KiB"},
		{1<<20 - 1, "1024.0 KiB"}, {1 << 20, "1.0 MiB"}, {3 << 30, "3.0 GiB"},
	} {
		if got := humanBytes(tc.n); got != tc.want {
			t.Errorf("humanBytes(%d) = %q, want %q", tc.n, got, tc.want)
		}
	}
}
