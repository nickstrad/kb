package workspace

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/nickstrad/kb/internal/errs"
)

// realTemp is a temp dir with symlinks resolved (macOS /var is a link to /private/var).
func realTemp(t *testing.T) string {
	t.Helper()
	d, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	return d
}

func mk(t *testing.T, path string, isDir bool) {
	t.Helper()
	if isDir {
		if err := os.MkdirAll(path, 0o755); err != nil {
			t.Fatal(err)
		}
		return
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, nil, 0o644); err != nil {
		t.Fatal(err)
	}
}

func TestResolve(t *testing.T) {
	base := realTemp(t)
	home := filepath.Join(base, "home")
	cfgStore := filepath.Join(base, "cfg")      // .kb/config.toml
	legacy := filepath.Join(base, "legacy")     // .kb/kb.sqlite only
	both := filepath.Join(base, "both")         // both files
	dirNamed := filepath.Join(base, "dirnamed") // .kb/config.toml is a directory
	other := filepath.Join(base, "other")
	empty := filepath.Join(base, "empty")
	for _, s := range []string{cfgStore, both, other} {
		mk(t, filepath.Join(s, ".kb", "config.toml"), false)
	}
	mk(t, filepath.Join(legacy, ".kb", "kb.sqlite"), false)
	mk(t, filepath.Join(both, ".kb", "kb.sqlite"), false)
	mk(t, filepath.Join(dirNamed, ".kb", "config.toml"), true)
	mk(t, empty, true)
	deep := filepath.Join(cfgStore, "a", "b", "c", "d", "e", "f")
	mk(t, deep, true)
	mk(t, filepath.Join(legacy, "sub"), true)
	mk(t, filepath.Join(dirNamed, "sub"), true)
	link := filepath.Join(base, "link")
	if err := os.Symlink(filepath.Join(cfgStore, "a"), link); err != nil {
		t.Fatal(err)
	}
	rel := filepath.Join(base, "relbase")
	mk(t, rel, true)

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
		{name: "cwd is root (config)", cwd: cfgStore, want: cfgStore},
		{name: "nested subdir", cwd: filepath.Join(cfgStore, "a"), want: cfgStore},
		{name: "deep nesting", cwd: deep, want: cfgStore},
		{name: "legacy sqlite only", cwd: filepath.Join(legacy, "sub"), want: legacy},
		{name: "config only", cwd: cfgStore, want: cfgStore},
		{name: "both files", cwd: both, want: both},
		{name: "config.toml is a directory: not a store", cwd: filepath.Join(dirNamed, "sub"), kind: errs.NotFound, msg: "no kb store found"},
		{name: "none found up to fs root", cwd: empty, kind: errs.NotFound, msg: `run "kb init" here, or use --global / --root`},
		{name: "relative cwd is not found", cwd: "rel/path", kind: errs.NotFound, msg: "no kb store found in rel/path"},
		{name: "unknown cwd is not found and says so", cwd: "", kind: errs.NotFound, msg: "no kb store found in the working directory (unknown)"},
		{name: "root flag overrides discovery inside another store", rootFlag: other, cwd: deep, want: other},
		{name: "root flag to a nonexistent dir", rootFlag: filepath.Join(base, "nope"), cwd: empty, want: filepath.Join(base, "nope")},
		{name: "root flag relative resolves against cwd", rootFlag: "../relbase/x/../y", cwd: empty, want: filepath.Join(base, "relbase", "y")},
		{name: "root flag relative with no cwd", rootFlag: "x", cwd: "", kind: errs.Usage, msg: "no working directory"},
		{name: "root flag unclean absolute", rootFlag: other + "/./sub/..", cwd: empty, want: other},
		{name: "global with XDG_DATA_HOME", global: true, cwd: empty, xdg: filepath.Join(base, "xdg"), want: filepath.Join(base, "xdg", "kb")},
		{name: "global without XDG_DATA_HOME", global: true, cwd: empty, want: filepath.Join(home, ".local", "share", "kb")},
		{name: "global ignores relative XDG_DATA_HOME", global: true, cwd: empty, xdg: "rel", want: filepath.Join(home, ".local", "share", "kb")},
		{name: "global wins over discovery", global: true, cwd: cfgStore, want: filepath.Join(home, ".local", "share", "kb")},
		{name: "root and global conflict", rootFlag: other, global: true, cwd: empty, kind: errs.Usage, msg: "--root and --global"},
		{name: "symlinked cwd resolves to the physical parent chain", cwd: link, want: cfgStore},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			env := Env{Cwd: tc.cwd, Home: home, XDGDataHome: tc.xdg, CacheBase: filepath.Join(base, "cache")}
			w, err := Resolve(tc.rootFlag, tc.global, env)
			if tc.kind != errs.Internal {
				if err == nil {
					t.Fatalf("want %v error, got workspace %+v", tc.kind, w)
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
			if w.Root != tc.want {
				t.Fatalf("root %q, want %q", w.Root, tc.want)
			}
			if w.KBDir != filepath.Join(tc.want, ".kb") || w.DBPath != filepath.Join(tc.want, ".kb", "kb.sqlite") ||
				w.ConfigPath != filepath.Join(tc.want, ".kb", "config.toml") {
				t.Fatalf("derived paths wrong: %+v", w)
			}
			if w.CacheDir != filepath.Join(base, "cache", "kb") || w.ModelsDir != filepath.Join(base, "cache", "kb", "models") {
				t.Fatalf("cache paths wrong: %+v", w)
			}
		})
	}
}

func TestResolveGlobalWithoutHome(t *testing.T) {
	_, err := Resolve("", true, Env{Cwd: "/"})
	if errs.KindOf(err) != errs.Usage {
		t.Fatalf("want Usage error, got %v", err)
	}
}

func TestNewWithoutCacheBase(t *testing.T) {
	w := New("/r", "")
	if w.CacheDir != "" || w.ModelsDir != "" {
		t.Fatalf("cache paths should be empty: %+v", w)
	}
}

// TestDiscoveryMetamorphic is the TESTING §5.2 property: from any directory under a store,
// discovery returns the same root; from outside, NotFound.
func TestDiscoveryMetamorphic(t *testing.T) {
	base := realTemp(t)
	store := filepath.Join(base, "store")
	mk(t, filepath.Join(store, ".kb", "config.toml"), false)
	outside := filepath.Join(base, "outside")
	mk(t, outside, true)
	for _, rel := range []string{".", "a", "a/b", "data", "data/topic", ".kb", "a/b/c/d/e/f/g/h", "with space/x"} {
		dir := filepath.Join(store, rel)
		mk(t, dir, true)
		w, err := Resolve("", false, Env{Cwd: dir})
		if err != nil || w.Root != store {
			t.Errorf("from %s: root %v err %v, want %s", rel, w, err, store)
		}
	}
	if _, err := Resolve("", false, Env{Cwd: outside}); errs.KindOf(err) != errs.NotFound {
		t.Errorf("outside: want NotFound, got %v", err)
	}
}

func FuzzDiscoveryUnderStore(f *testing.F) {
	for _, s := range []string{"", "a", "a/b/c", "..x/y", "data/ä"} {
		f.Add(s)
	}
	base, err := filepath.EvalSymlinks(f.TempDir())
	if err != nil {
		f.Fatal(err)
	}
	store := filepath.Join(base, "store")
	mk2 := func(p string) { _ = os.MkdirAll(p, 0o755) }
	mk2(filepath.Join(store, ".kb"))
	_ = os.WriteFile(filepath.Join(store, ".kb", "config.toml"), nil, 0o644)
	f.Fuzz(func(t *testing.T, rel string) {
		// Keep to plain relative components: no NUL, no "..", no absolute paths, bounded size.
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
		dir := filepath.Join(append([]string{store}, parts...)...)
		if err := os.MkdirAll(dir, 0o755); err != nil {
			t.Skip()
		}
		w, err := Resolve("", false, Env{Cwd: dir})
		if err != nil || w.Root != store {
			t.Fatalf("from %q: %v %v", dir, w, err)
		}
	})
}
