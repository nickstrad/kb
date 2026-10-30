package cli

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"

	"github.com/nickstrad/kb/internal/errs"
	"github.com/nickstrad/kb/internal/store"
	"github.com/nickstrad/kb/internal/store/sqlite"
)

// refWorld is the fixture of the ref tests: the current location (loc) with stores default and adr
// and the alias al -> adr@<other>, another location (other) with default and adr, and the global
// location (default, adr).
type refWorld struct{ loc, other, global string }

func newRefWorld(t *testing.T) refWorld {
	t.Helper()
	w := refWorld{loc: realTempDir(t), other: realTempDir(t), global: useGlobalLocation(t, "default")}
	for _, root := range []string{w.loc, w.other} {
		makeLocation(t, root, "default")
		addStore(t, root, "adr")
	}
	writeFile(t, filepath.Join(w.loc, ".kb", "kb.toml"),
		"version = 1\ndefault_store = \"default\"\n[aliases]\nal = \"adr@"+w.other+"\"\nbroken = \"adr@"+w.other+"/nowhere\"\n")
	return w
}

// TestStoreRefDecisions: every ref form x {--root, --global, discovered} x {exists, missing}. It
// checks the config path of the store the ref resolved to, or the error kind and message.
func TestStoreRefDecisions(t *testing.T) {
	type want struct {
		path string // config.toml of the expected store, "" for an error
		msg  string
	}
	for _, mode := range []string{"discovered", "root", "global"} {
		cfg := func(root, store string) string { return filepath.Join(root, ".kb", "stores", store, "config.toml") }
		forms := func(w refWorld) []struct {
			name, ref string
			ok        map[string]want
		} {
			// ok maps a mode to its expectation.
			here := map[string]string{"discovered": w.loc, "root": w.loc, "global": w.global}[mode]
			usage := want{msg: "names its own location"}
			return []struct {
				name, ref string
				ok        map[string]want
			}{
				{"local store", "adr", map[string]want{mode: {path: cfg(here, "adr")}}},
				{"missing local store", "nope", map[string]want{mode: {msg: `no store "nope"`}}},
				{"alias", "al", map[string]want{
					"discovered": {path: cfg(w.other, "adr")},
					"root":       {msg: "is an alias"},
					"global":     {msg: `no store "al"`},
				}},
				{"broken alias", "broken", map[string]want{
					"discovered": {msg: "no kb location at " + w.other + "/nowhere"},
					"root":       {msg: "is an alias"},
					"global":     {msg: `no store "broken"`},
				}},
				{"global", "global", map[string]want{
					"discovered": {path: cfg(w.global, "default")},
					"root":       {msg: "cannot be combined with --root"},
					"global":     {path: cfg(w.global, "default")},
				}},
				{"NAME@LOC", "adr@" + w.other, map[string]want{"discovered": {path: cfg(w.other, "adr")}, "root": usage, "global": usage}},
				{"NAME@LOC missing store", "nope@" + w.other, map[string]want{"discovered": {msg: `no store "nope"`}, "root": usage, "global": usage}},
				{"NAME@LOC missing location", "adr@" + w.other + "/x", map[string]want{"discovered": {msg: "no kb location at"}, "root": usage, "global": usage}},
				{"NAME@global", "adr@global", map[string]want{"discovered": {path: cfg(w.global, "adr")}, "root": usage, "global": usage}},
				{"path", w.other, map[string]want{"discovered": {path: cfg(w.other, "default")}, "root": usage, "global": usage}},
				{"path trailing slash", w.other + "/", map[string]want{"discovered": {path: cfg(w.other, "default")}, "root": usage, "global": usage}},
				{"path missing", w.other + "/x", map[string]want{"discovered": {msg: "no kb location at"}, "root": usage, "global": usage}},
				{"relative path", "../" + filepath.Base(w.other), map[string]want{"discovered": {path: cfg(w.other, "default")}, "root": usage, "global": usage}},
			}
		}
		w := newRefWorld(t)
		for _, f := range forms(w) {
			t.Run(mode+"/"+f.name, func(t *testing.T) {
				exp, ok := f.ok[mode]
				if !ok {
					t.Fatalf("no expectation for mode %s", mode)
				}
				args := []string{"--store", f.ref}
				switch mode {
				case "discovered":
					t.Chdir(filepath.Join(w.loc))
				case "root":
					t.Chdir(realTempDir(t))
					args = append(args, "--root", w.loc)
				case "global":
					t.Chdir(w.loc)
					args = append(args, "--global")
				}
				if f.name == "relative path" {
					t.Chdir(w.loc) // ../<other> from the location
				}
				stdout, stderr, code := run(t, append(args, "config", "--path")...)
				if exp.path != "" {
					if code != 0 || strings.TrimSpace(stdout) != exp.path {
						t.Fatalf("exit %d stdout %q stderr %q; want %s", code, stdout, stderr, exp.path)
					}
					return
				}
				if code != ExitUsage || stdout != "" || !strings.Contains(stderr, exp.msg) {
					t.Fatalf("exit %d stdout %q stderr %q; want exit 1 containing %q", code, stdout, stderr, exp.msg)
				}
			})
		}
	}
}

func TestStoreRefMalformedIsUsage(t *testing.T) {
	newRefWorld(t)
	for _, ref := range []string{"Bad", "a b", "adr@", "~bob/x", "docs/x", "c:x"} {
		_, stderr, code := run(t, "--store", ref, "config", "--path")
		if code != ExitUsage || !strings.Contains(stderr, "store") {
			t.Errorf("--store %q: exit %d stderr %q", ref, code, stderr)
		}
	}
}

// A bare word with no location and no matching global alias reports the missing location.
func TestStoreRefNoLocation(t *testing.T) {
	useGlobalLocation(t, "default")
	t.Chdir(realTempDir(t))
	if _, stderr, code := run(t, "--store", "adr", "config", "--path"); code != ExitUsage || !strings.Contains(stderr, "no kb location found") {
		t.Errorf("bare word, no location: exit %d %q", code, stderr)
	}
	// A global alias works from outside any location.
	writeFile(t, filepath.Join(globalLocationRoot(), ".kb", "kb.toml"),
		"version = 1\ndefault_store = \"default\"\n[aliases]\ntop = \"adr\"\n")
	stdout, stderr, code := run(t, "--store", "top", "config", "--path")
	want := filepath.Join(globalLocationRoot(), ".kb", "stores", "adr", "config.toml")
	if code != 0 || strings.TrimSpace(stdout) != want {
		t.Errorf("global alias: exit %d %q %q; want %s", code, stdout, stderr, want)
	}
}

func TestStoreRefShadowWarning(t *testing.T) {
	w := newRefWorld(t)
	writeFile(t, filepath.Join(w.loc, ".kb", "kb.toml"),
		"version = 1\ndefault_store = \"default\"\n[aliases]\nadr = \"adr@"+w.other+"\"\n")
	t.Chdir(w.loc)
	stdout, stderr, code := run(t, "--store", "adr", "config", "--path")
	if code != 0 || strings.TrimSpace(stdout) != filepath.Join(w.loc, ".kb", "stores", "adr", "config.toml") {
		t.Fatalf("exit %d %q %q: the local store must win", code, stdout, stderr)
	}
	if strings.Count(stderr, "warning:") != 1 || !strings.Contains(stderr, `shadows the alias "adr"`) {
		t.Errorf("stderr = %q, want one shadow warning", stderr)
	}
	// Without the shadow there is no warning, and kb stores marks it.
	text, stderr, _ := run(t, "stores")
	if stderr != "" || !strings.Contains(text, "(shadowed by the store adr)") {
		t.Errorf("kb stores: %q %q", text, stderr)
	}
}

func TestStoresAliasRows(t *testing.T) {
	w := newRefWorld(t)
	writeFile(t, filepath.Join(globalLocationRoot(), ".kb", "kb.toml"),
		"version = 1\ndefault_store = \"default\"\n[aliases]\nal = \"adr\"\ntop = \"adr\"\n")
	t.Chdir(w.loc)
	text, stderr, code := run(t, "stores")
	if code != 0 || stderr != "" {
		t.Fatalf("exit %d %q", code, stderr)
	}
	for _, want := range []string{
		"-> adr@" + globalLocationRoot() + "  (global)",
		"BROKEN: no kb location at " + w.other + "/nowhere",
		"(overridden by an alias of this location)",
		"-> adr@" + globalLocationRoot(),
	} {
		if !strings.Contains(text, want) {
			t.Errorf("kb stores lacks %q:\n%s", want, text)
		}
	}
	if !regexp.MustCompile(`(?m)^  al +adr@` + regexp.QuoteMeta(w.other) + ` +-> adr@` + regexp.QuoteMeta(w.other) + `$`).MatchString(text) {
		t.Errorf("no resolved row for al:\n%s", text)
	}
	js, _, _ := run(t, "stores", "--json")
	for _, key := range []string{`"alias_status": [`, `"resolves": false`, `"scope": "global"`, `"overridden": true`} {
		if !strings.Contains(js, key) {
			t.Errorf("JSON lacks %s:\n%s", key, js)
		}
	}
}

// TestOpenStoreReadOnly: the helper 3.2 fans out through. An indexed store opens with its embedder
// record; every way a store can be unusable is a warning and a nil result; a malformed ref is Usage.
func TestOpenStoreReadOnly(t *testing.T) {
	useFakeEmbedder(t)
	w := newRefWorld(t)
	useNoneModel(t, w.other)
	src := filepath.Join(t.TempDir(), "one.md")
	writeFile(t, src, validFrontMatter("One", "First."))
	if _, stderr, code := run(t, "--root", w.other, "add", src); code != 0 {
		t.Fatalf("add: %d %q", code, stderr)
	}
	t.Chdir(w.loc)
	cmd := newRootCmd(testDeps, os.Stdout, os.Stderr)
	var errBuf strings.Builder
	cmd.SetErr(&errBuf)
	cmd.SetContext(context.Background())

	open := func(ref string) (*openedStore, error) { errBuf.Reset(); return testDeps.openStoreReadOnly(cmd, ref) }

	got, err := open("default@" + w.other)
	if err != nil || got == nil {
		t.Fatalf("open indexed: %v %v", got, err)
	}
	defer got.Store.Close()
	if got.Canonical != "default@"+w.other || !got.Indexed || got.Meta.Model == "" || !got.Store.ReadOnly() || errBuf.Len() != 0 {
		t.Errorf("indexed store: %+v stderr %q", got, errBuf.String())
	}
	if _, err := got.Store.Entries(context.Background()); err != nil {
		t.Errorf("read: %v", err)
	}
	// Through an alias: it resolves, and that store has no database yet, so it warns.
	if viaAlias, err := open("al"); err != nil || viaAlias != nil || !strings.Contains(errBuf.String(), "has no database yet") {
		t.Errorf("alias: %+v %v stderr %q", viaAlias, err, errBuf.String())
	}

	for _, tc := range []struct{ name, ref, warn string }{
		{"no database yet", "adr@" + w.other, "has no database yet"},
		{"missing store", "nope@" + w.other, `no store "nope"`},
		{"missing location", w.other + "/x", "no kb location at"},
		{"unknown word", "nope", `no store "nope"`},
	} {
		if s, err := open(tc.ref); err != nil || s != nil || !strings.Contains(errBuf.String(), "warning: skipping store "+tc.ref) || !strings.Contains(errBuf.String(), tc.warn) {
			t.Errorf("%s: store %v err %v stderr %q", tc.name, s, err, errBuf.String())
		}
	}
	if _, err := open("Bad"); errs.KindOf(err) != errs.Usage {
		t.Errorf("malformed ref: %v", err)
	}

	// Schema problems: an older and a newer database are refused by the open and skipped here.
	orig := testDeps.openStore
	t.Cleanup(func() { testDeps.openStore = orig })
	for _, tc := range []struct {
		name string
		err  error
		want string
	}{
		{"older schema", errs.Wrap(sqlite.ErrSchemaOlder, errs.Conflict, "test", "store database x is older"), "kb --store default@" + w.other + " migrate up"},
		{"newer schema", errs.New(errs.Conflict, "test", "database x is at schema version 9; use a newer kb"), "use a newer kb"},
	} {
		errBuf.Reset()
		testDeps.openStore = func(context.Context, string, bool) (store.Store, error) { return nil, tc.err }
		s, err := open("default@" + w.other)
		if err != nil || s != nil || !strings.Contains(errBuf.String(), tc.want) {
			t.Errorf("%s: %v %v stderr %q", tc.name, s, err, errBuf.String())
		}
		if tc.name == "newer schema" && strings.Contains(errBuf.String(), "migrate up") {
			t.Errorf("a newer schema got the migrate hint: %q", errBuf.String())
		}
	}
	testDeps.openStore = func(context.Context, string, bool) (store.Store, error) {
		return nil, errs.New(errs.Internal, "test", "boom")
	}
	if _, err := open("default@" + w.other); err == nil {
		t.Error("an open failure that is not a schema Conflict must be an error")
	}
}

// TestStoresWithAliasListsItsLocation: a command that needs no store (kb stores) given an alias
// runs in the alias target's location, not the current one.
func TestStoresWithAliasListsItsLocation(t *testing.T) {
	w := newRefWorld(t)
	t.Chdir(w.loc)
	for _, tc := range []struct{ ref, want string }{{"al", w.other}, {"adr", w.loc}} {
		out, stderr, code := run(t, "--store", tc.ref, "stores", "--json")
		var doc struct {
			Location string `json:"location"`
		}
		if err := json.Unmarshal([]byte(out), &doc); err != nil || code != 0 || doc.Location != tc.want {
			t.Errorf("--store %s stores: exit %d location %q (%v) %q; want %s", tc.ref, code, doc.Location, err, stderr, tc.want)
		}
	}
}
