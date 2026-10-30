package workspace

import (
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"github.com/nickstrad/kb/internal/errs"
)

func TestResolveLocation(t *testing.T) {
	base := realTemp(t)
	home := filepath.Join(base, "home")
	loc := filepath.Join(base, "loc") // .kb/kb.toml
	other := filepath.Join(base, "other")
	legacyCfg := filepath.Join(base, "legacycfg") // old marker .kb/config.toml: not a location
	legacyDB := filepath.Join(base, "legacydb")   // old marker .kb/kb.sqlite: not a location
	dirNamed := filepath.Join(base, "dirnamed")   // .kb/kb.toml is a directory
	empty := filepath.Join(base, "empty")
	inner := filepath.Join(loc, "inner") // nested location: nearest wins
	for _, s := range []string{loc, other, inner} {
		mk(t, filepath.Join(s, ".kb", "kb.toml"), false)
	}
	mk(t, filepath.Join(legacyCfg, ".kb", "config.toml"), false)
	mk(t, filepath.Join(legacyDB, ".kb", "kb.sqlite"), false)
	mk(t, filepath.Join(dirNamed, ".kb", "kb.toml"), true)
	mk(t, filepath.Join(dirNamed, "sub"), true)
	mk(t, empty, true)
	deep := filepath.Join(loc, "a", "b", "c", "d", "e", "f")
	mk(t, deep, true)
	mk(t, filepath.Join(inner, "x"), true)
	link := filepath.Join(base, "link")
	if err := os.Symlink(filepath.Join(loc, "a"), link); err != nil {
		t.Fatal(err)
	}
	// .kb/kb.toml as a symlink to a regular file counts.
	viaLink := filepath.Join(base, "vialink")
	mk(t, filepath.Join(base, "real.toml"), false)
	mk(t, filepath.Join(viaLink, ".kb"), true)
	if err := os.Symlink(filepath.Join(base, "real.toml"), filepath.Join(viaLink, ".kb", "kb.toml")); err != nil {
		t.Fatal(err)
	}
	mk(t, filepath.Join(base, "relbase"), true)

	cases := []struct {
		name     string
		rootFlag string
		global   bool
		cwd      string
		xdg      string
		want     string
		kind     errs.Kind // errs.Internal (zero) means success
		msg      string
	}{
		{name: "cwd is the location", cwd: loc, want: loc},
		{name: "nested subdir", cwd: filepath.Join(loc, "a"), want: loc},
		{name: "deep nesting", cwd: deep, want: loc},
		{name: "nearest location wins", cwd: filepath.Join(inner, "x"), want: inner},
		{name: "kb.toml as a symlink to a file", cwd: viaLink, want: viaLink},
		{name: "legacy .kb/config.toml is not a location", cwd: legacyCfg, kind: errs.NotFound, msg: "no kb location found"},
		{name: "legacy .kb/kb.sqlite is not a location", cwd: legacyDB, kind: errs.NotFound, msg: "no kb location found"},
		{name: "kb.toml is a directory: not a location", cwd: filepath.Join(dirNamed, "sub"), kind: errs.NotFound, msg: "no kb location found"},
		{name: "none found up to fs root", cwd: empty, kind: errs.NotFound, msg: `run "kb init" here, or use --global / --root`},
		{name: "relative cwd is not found", cwd: "rel/path", kind: errs.NotFound, msg: "no kb location found in rel/path"},
		{name: "unknown cwd is not found and says so", cwd: "", kind: errs.NotFound, msg: "in the working directory (unknown)"},
		{name: "root flag overrides discovery", rootFlag: other, cwd: deep, want: other},
		{name: "root flag to a nonexistent dir", rootFlag: filepath.Join(base, "nope"), cwd: empty, want: filepath.Join(base, "nope")},
		{name: "root flag relative resolves against cwd", rootFlag: "../relbase/x/../y", cwd: empty, want: filepath.Join(base, "relbase", "y")},
		{name: "root flag relative with no cwd", rootFlag: "x", cwd: "", kind: errs.Usage, msg: "no working directory"},
		{name: "root flag unclean absolute", rootFlag: other + "/./sub/..", cwd: empty, want: other},
		{name: "global with XDG_DATA_HOME", global: true, cwd: empty, xdg: filepath.Join(base, "xdg"), want: filepath.Join(base, "xdg", "kb")},
		{name: "global without XDG_DATA_HOME", global: true, cwd: empty, want: filepath.Join(home, ".local", "share", "kb")},
		{name: "global ignores relative XDG_DATA_HOME", global: true, cwd: empty, xdg: "rel", want: filepath.Join(home, ".local", "share", "kb")},
		{name: "global wins over discovery", global: true, cwd: loc, want: filepath.Join(home, ".local", "share", "kb")},
		{name: "root and global conflict", rootFlag: other, global: true, cwd: empty, kind: errs.Usage, msg: "--root and --global"},
		{name: "symlinked cwd resolves to the physical parent chain", cwd: link, want: loc},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			env := Env{Cwd: tc.cwd, Home: home, XDGDataHome: tc.xdg, CacheBase: filepath.Join(base, "cache")}
			l, err := ResolveLocation(tc.rootFlag, tc.global, env)
			if tc.kind != errs.Internal {
				if err == nil {
					t.Fatalf("want %v error, got location %+v", tc.kind, l)
				}
				if k := errs.KindOf(err); k != tc.kind {
					t.Fatalf("kind %v, want %v (%v)", k, tc.kind, err)
				}
				if !strings.Contains(err.Error(), tc.msg) {
					t.Fatalf("message %q lacks %q", err.Error(), tc.msg)
				}
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			if l.Root != tc.want || l.KBDir != filepath.Join(tc.want, ".kb") || l.FilePath != filepath.Join(tc.want, ".kb", "kb.toml") {
				t.Fatalf("location wrong: %+v, want root %q", l, tc.want)
			}
		})
	}
}

func TestResolveLocationGlobalWithoutHome(t *testing.T) {
	_, err := ResolveLocation("", true, Env{Cwd: "/"})
	if errs.KindOf(err) != errs.Usage {
		t.Fatalf("want Usage error, got %v", err)
	}
}

// TestTargetLocation: the init target is the working directory itself, never an ancestor.
func TestTargetLocation(t *testing.T) {
	base := realTemp(t)
	parent := filepath.Join(base, "parent")
	mk(t, filepath.Join(parent, ".kb", "kb.toml"), false)
	child := filepath.Join(parent, "child")
	mk(t, child, true)
	link := filepath.Join(base, "link")
	if err := os.Symlink(child, link); err != nil {
		t.Fatal(err)
	}
	cases := []struct {
		name     string
		rootFlag string
		global   bool
		cwd      string
		want     string
		kind     errs.Kind
	}{
		{name: "cwd, not the ancestor location", cwd: child, want: child},
		{name: "symlinked cwd is resolved", cwd: link, want: child},
		{name: "root flag", rootFlag: parent, cwd: child, want: parent},
		{name: "global", global: true, cwd: child, want: filepath.Join(base, "xdg", "kb")},
		{name: "relative cwd with no flag", cwd: "rel", kind: errs.Usage},
		{name: "conflict", rootFlag: parent, global: true, cwd: child, kind: errs.Usage},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			l, err := TargetLocation(tc.rootFlag, tc.global, Env{Cwd: tc.cwd, XDGDataHome: filepath.Join(base, "xdg")})
			if tc.kind != errs.Internal {
				if errs.KindOf(err) != tc.kind {
					t.Fatalf("want %v, got %v", tc.kind, err)
				}
				return
			}
			if err != nil || l.Root != tc.want {
				t.Fatalf("got %+v %v, want root %s", l, err, tc.want)
			}
		})
	}
}

func TestValidName(t *testing.T) {
	cases := []struct {
		name string
		in   string
		ok   bool
	}{
		{"empty (0 chars)", "", false},
		{"1 char", "a", true},
		{"1 digit", "0", true},
		{"32 chars", strings.Repeat("a", 32), true},
		{"33 chars", strings.Repeat("a", 33), false},
		{"leading dash", "-a", false},
		{"leading underscore", "_a", false},
		{"inner dash and underscore", "a-b_c", true},
		{"trailing dash", "a-", true},
		{"uppercase", "A", false},
		{"mixed case", "aB", false},
		{"slash", "a/b", false},
		{"at", "a@b", false},
		{"colon", "a:b", false},
		{"dot", "a.b", false},
		{"dotdot", "..", false},
		{"dot dot slash", "../x", false},
		{"space", "a b", false},
		{"non-ascii", "é", false},
		{"newline suffix", "a\n", false},
		{"reserved global", "global", false},
		{"reserved all", "all", false},
		{"all as a prefix is fine", "all-notes", true},
		{"global prefix is fine", "globalx", true},
		{"default", "default", true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			err := ValidName(tc.in)
			if (err == nil) != tc.ok {
				t.Fatalf("ValidName(%q) = %v, want ok=%v", tc.in, err, tc.ok)
			}
			if err != nil && errs.KindOf(err) != errs.Usage {
				t.Fatalf("kind %v, want Usage", errs.KindOf(err))
			}
		})
	}
	if err := ValidName("global"); err == nil || !strings.Contains(err.Error(), "reserved") {
		t.Fatalf("global should say reserved: %v", err)
	}
}

// FuzzValidName: the TESTING §5.2 property, a valid name makes StoreDir a direct child of
// <KBDir>/stores; and an invalid one never yields a Workspace.
func FuzzValidName(f *testing.F) {
	for _, s := range []string{"", "a", "default", "global", "..", "../x", "a/b", "A", "a@b", "-a", strings.Repeat("a", 32), strings.Repeat("a", 33), "a\x00", "é"} {
		f.Add(s)
	}
	l := NewLocation("/r", "")
	f.Fuzz(func(t *testing.T, name string) {
		w, err := l.StoreAt(name)
		if (ValidName(name) == nil) != (err == nil) {
			t.Fatalf("ValidName and StoreAt disagree on %q: %v", name, err)
		}
		if err != nil {
			if w != nil {
				t.Fatalf("workspace for invalid name %q", name)
			}
			return
		}
		if filepath.Dir(w.StoreDir) != filepath.Join("/r", ".kb", "stores") || filepath.Base(w.StoreDir) != name {
			t.Fatalf("StoreDir %q is not a direct child for %q", w.StoreDir, name)
		}
		if filepath.Dir(w.DBPath) != w.StoreDir || filepath.Dir(w.ConfigPath) != w.StoreDir {
			t.Fatalf("db/config escape the store dir: %+v", w)
		}
	})
}

func TestLocationStore(t *testing.T) {
	base := realTemp(t)
	l := NewLocation(filepath.Join(base, "p"), filepath.Join(base, "cache"))
	mk(t, filepath.Join(l.StoresDir(), "default", "config.toml"), false)
	mk(t, filepath.Join(l.StoresDir(), "adr", "config.toml"), false)

	w, err := l.Store("adr")
	if err != nil {
		t.Fatal(err)
	}
	store := filepath.Join(l.Root, ".kb", "stores", "adr")
	if w.Root != l.Root || w.KBDir != l.KBDir || w.Store != "adr" || w.StoreDir != store || w.Location != l ||
		w.DBPath != filepath.Join(store, "kb.sqlite") || w.ConfigPath != filepath.Join(store, "config.toml") {
		t.Fatalf("workspace wrong: %+v", w)
	}
	if w.CacheDir != filepath.Join(base, "cache", "kb") || w.ModelsDir != filepath.Join(base, "cache", "kb", "models") {
		t.Fatalf("cache paths wrong: %+v", w)
	}

	_, err = l.Store("nope")
	if errs.KindOf(err) != errs.NotFound || !strings.Contains(err.Error(), "stores: adr, default") || !strings.Contains(err.Error(), "kb init --name nope") {
		t.Fatalf("missing store: %v", err)
	}
	if _, err = l.Store("../x"); errs.KindOf(err) != errs.Usage {
		t.Fatalf("invalid name: %v", err)
	}
	if _, err = l.Store("global"); errs.KindOf(err) != errs.Usage {
		t.Fatalf("global: %v", err)
	}
	empty := NewLocation(filepath.Join(base, "q"), "")
	if _, err = empty.Store("default"); errs.KindOf(err) != errs.NotFound || !strings.Contains(err.Error(), "stores: none") {
		t.Fatalf("no stores: %v", err)
	}
	if w, _ := empty.StoreAt("x"); w.CacheDir != "" || w.ModelsDir != "" {
		t.Fatalf("cache paths should be empty: %+v", w)
	}
}

func TestLocationStoreListing(t *testing.T) {
	base := realTemp(t)
	one := NewLocation(filepath.Join(base, "one"), "")
	mk(t, filepath.Join(one.StoresDir(), "solo", "config.toml"), false)
	if _, err := one.Store("x"); err == nil || !strings.Contains(err.Error(), "(stores: solo)") {
		t.Fatalf("one store should be listed: %v", err)
	}
	bad := NewLocation(filepath.Join(base, "bad"), "")
	mk(t, bad.KBDir, true)
	mk(t, bad.StoresDir(), false) // stores is a file: listing fails, and Store reports that
	if _, err := bad.Store("x"); errs.KindOf(err) != errs.Usage || !strings.Contains(err.Error(), "cannot read") {
		t.Fatalf("unreadable stores dir: %v", err)
	}
}

func TestStores(t *testing.T) {
	base := realTemp(t)
	l := NewLocation(filepath.Join(base, "p"), "")
	if names, skipped, err := l.Stores(); err != nil || names != nil || skipped != nil {
		t.Fatalf("no stores dir: %v %v %v", names, skipped, err)
	}
	sd := l.StoresDir()
	mk(t, filepath.Join(sd, "b", "config.toml"), false)
	mk(t, filepath.Join(sd, "a", "config.toml"), false)
	mk(t, filepath.Join(sd, "nocfg", "kb.sqlite"), false)      // dir without config
	mk(t, filepath.Join(sd, "cfgdir", "config.toml"), true)    // config.toml is a directory
	mk(t, filepath.Join(sd, "Upper", "config.toml"), false)    // invalid name
	mk(t, filepath.Join(sd, "global", "config.toml"), false)   // reserved
	mk(t, filepath.Join(sd, ".hidden", "config.toml"), false)  // invalid name
	mk(t, filepath.Join(sd, "afile"), false)                   // a file named like a store
	mk(t, filepath.Join(sd, "BIGFILE"), false)                 // a file with an invalid name: ignored, not reported
	mk(t, filepath.Join(base, "target", "config.toml"), false) // symlinked store dir
	mk(t, filepath.Join(base, "realcfg"), false)               // symlinked config target
	if err := os.Symlink(filepath.Join(base, "target"), filepath.Join(sd, "linkdir")); err != nil {
		t.Fatal(err)
	}
	mk(t, filepath.Join(sd, "linkcfg"), true)
	if err := os.Symlink(filepath.Join(base, "realcfg"), filepath.Join(sd, "linkcfg", "config.toml")); err != nil {
		t.Fatal(err)
	}
	names, skipped, err := l.Stores()
	if err != nil {
		t.Fatal(err)
	}
	if want := []string{"a", "b", "linkcfg", "linkdir"}; !reflect.DeepEqual(names, want) {
		t.Fatalf("names %v, want %v", names, want)
	}
	if want := []string{".hidden", "Upper", "global"}; !reflect.DeepEqual(skipped, want) {
		t.Fatalf("skipped %v, want %v", skipped, want)
	}
}

func TestStoresUnreadable(t *testing.T) {
	base := realTemp(t)
	l := NewLocation(filepath.Join(base, "p"), "")
	mk(t, filepath.Join(l.KBDir), true)
	mk(t, l.StoresDir(), false) // stores is a file: ReadDir fails with ENOTDIR
	if _, _, err := l.Stores(); errs.KindOf(err) != errs.Usage {
		t.Fatalf("want Usage, got %v", err)
	}
}

// TestLocationMetamorphic: from any directory under a location discovery returns the same root.
func TestLocationMetamorphic(t *testing.T) {
	base := realTemp(t)
	root := filepath.Join(base, "loc")
	mk(t, filepath.Join(root, ".kb", "kb.toml"), false)
	outside := filepath.Join(base, "outside")
	mk(t, outside, true)
	for _, rel := range []string{".", "a", "a/b", "data", ".kb", ".kb/stores/default", "with space/x"} {
		dir := filepath.Join(root, rel)
		mk(t, dir, true)
		l, err := ResolveLocation("", false, Env{Cwd: dir})
		if err != nil || l.Root != root {
			t.Errorf("from %s: %v %v, want %s", rel, l, err, root)
		}
	}
	if _, err := ResolveLocation("", false, Env{Cwd: outside}); errs.KindOf(err) != errs.NotFound {
		t.Errorf("outside: want NotFound, got %v", err)
	}
}

// TestParseRef is the grammar of docs/design/named-stores.md §3, one row per form and boundary.
func TestParseRef(t *testing.T) {
	long := strings.Repeat("a", MaxNameLen)
	cases := []struct {
		in      string
		want    Ref
		wantErr bool
		msg     string // in the error
	}{
		{in: "", want: Ref{}},
		{in: "global", want: Ref{Global: true}},
		{in: "default", want: Ref{Name: "default"}},
		{in: "adr", want: Ref{Name: "adr"}},
		{in: "a", want: Ref{Name: "a"}},
		{in: long, want: Ref{Name: long}},
		{in: long + "a", wantErr: true, msg: "invalid store name"},
		{in: "Adr", wantErr: true, msg: "invalid store name"},
		{in: "-adr", wantErr: true, msg: "invalid store name"},
		{in: "a b", wantErr: true, msg: "invalid store name"},
		{in: "adr@/p", want: Ref{Name: "adr", At: "/p"}},
		{in: "adr@/p/", want: Ref{Name: "adr", At: "/p/"}},
		{in: "adr@global", want: Ref{Name: "adr", At: "global"}},
		{in: "adr@~/proj", want: Ref{Name: "adr", At: "~/proj"}},
		{in: "adr@~", want: Ref{Name: "adr", At: "~"}},
		{in: "adr@rel/dir", want: Ref{Name: "adr", At: "rel/dir"}},
		{in: "adr@proj", want: Ref{Name: "adr", At: "proj"}},
		{in: "adr@/p@q", want: Ref{Name: "adr", At: "/p@q"}},
		{in: "adr@", wantErr: true, msg: "nothing after @"},
		{in: "@/p", wantErr: true, msg: "invalid store name"},
		{in: "global@/p", wantErr: true, msg: "reserved"},
		{in: "all@/p", wantErr: true, msg: "reserved"},
		{in: "Adr@/p", wantErr: true, msg: "invalid store name"},
		{in: "adr@~bob/x", wantErr: true, msg: "not ~user"},
		{in: "c:proj", wantErr: true, msg: `":" is not part of a ref`},
		{in: "adr:notes/x.md", wantErr: true, msg: "neither a store name nor a path"},
		{in: "~", want: Ref{Path: "~"}},
		{in: "~/knowledge", want: Ref{Path: "~/knowledge"}},
		{in: "~/knowledge/", want: Ref{Path: "~/knowledge/"}},
		{in: "~/me@x", want: Ref{Path: "~/me@x"}}, // a path wins over NAME@LOC
		{in: "/abs/p", want: Ref{Path: "/abs/p"}},
		{in: "/", want: Ref{Path: "/"}},
		{in: "./rel", want: Ref{Path: "./rel"}},
		{in: "../x", want: Ref{Path: "../x"}},
		{in: ".", want: Ref{Path: "."}},
		{in: "..", want: Ref{Path: ".."}},
		{in: "~bob/x", wantErr: true, msg: "not ~user"},
		{in: "~bob", wantErr: true, msg: "not ~user"},
		{in: "docs/x", wantErr: true, msg: "neither a store name nor a path"},
		{in: "x\x00y", wantErr: true, msg: "control character"},
		{in: "/p\n", wantErr: true, msg: "control character"},
		{in: "/" + strings.Repeat("a", MaxRefLen-1), want: Ref{Path: "/" + strings.Repeat("a", MaxRefLen-1)}}, // exactly MaxRefLen
		{in: "/" + strings.Repeat("a", MaxRefLen), wantErr: true, msg: "at most"},                             // one over
		{in: "a\x1f", wantErr: true, msg: "control character"},
		{in: "a\x7f", wantErr: true, msg: "control character"},
		{in: "a\x20b", wantErr: true, msg: "invalid store name"}, // 0x20 is not a control character
	}
	for _, tc := range cases {
		got, err := ParseRef(tc.in)
		if (err != nil) != tc.wantErr || got != tc.want {
			t.Errorf("ParseRef(%q) = %+v, %v; want %+v err=%v", tc.in, got, err, tc.want, tc.wantErr)
		}
		if err == nil && got.String() != tc.in {
			t.Errorf("String of %q = %q", tc.in, got.String())
		}
		if err != nil && (errs.KindOf(err) != errs.Usage || !strings.Contains(err.Error(), tc.msg)) {
			t.Errorf("ParseRef(%q): %v (kind %v), want Usage containing %q", tc.in, err, errs.KindOf(err), tc.msg)
		}
		if err == nil {
			if got.IsZero() != (tc.in == "") {
				t.Errorf("IsZero(%q) = %v", tc.in, got.IsZero())
			}
			wantLoc := got.Global || got.At != "" || got.Path != ""
			if got.NamesLocation() != wantLoc {
				t.Errorf("NamesLocation(%q) = %v", tc.in, got.NamesLocation())
			}
		}
	}
}

// FuzzDiscoveryUnderLocation: from any plain subdirectory of a location, discovery returns that
// location's root.
func FuzzDiscoveryUnderLocation(f *testing.F) {
	for _, s := range []string{"", "a", "a/b/c", "..x/y", "data/ä"} {
		f.Add(s)
	}
	base, err := filepath.EvalSymlinks(f.TempDir())
	if err != nil {
		f.Fatal(err)
	}
	root := filepath.Join(base, "loc")
	_ = os.MkdirAll(filepath.Join(root, ".kb"), 0o755)
	_ = os.WriteFile(filepath.Join(root, ".kb", "kb.toml"), nil, 0o644)
	f.Fuzz(func(t *testing.T, rel string) {
		if len(rel) > 200 || strings.ContainsRune(rel, 0) {
			t.Skip()
		}
		parts := []string{}
		for _, p := range strings.Split(rel, "/") {
			if p == "" || p == "." || p == ".." || len(p) > 50 {
				continue
			}
			parts = append(parts, p)
		}
		dir := filepath.Join(append([]string{root}, parts...)...)
		if err := os.MkdirAll(dir, 0o755); err != nil {
			t.Skip()
		}
		l, err := ResolveLocation("", false, Env{Cwd: dir})
		if err != nil || l.Root != root {
			t.Fatalf("from %q: %v %v", dir, l, err)
		}
	})
}

func TestModelsDirOf(t *testing.T) {
	if got := ModelsDirOf("/c"); got != "/c/kb/models" {
		t.Errorf("ModelsDirOf(/c) = %q", got)
	}
	if got := ModelsDirOf(""); got != "" {
		t.Errorf("ModelsDirOf(\"\") = %q, want empty", got)
	}
}
