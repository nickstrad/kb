package workspace

import (
	"errors"
	"path/filepath"
	"strings"
	"testing"

	"github.com/nickstrad/kb/internal/errs"
)

// refFixture is a small world: a current location (proj) with stores default, adr and docs and a
// set of aliases, a global location, another location (other), the home's knowledge location, a
// location under the working directory, and a directory that is no location.
type refFixture struct {
	base, home, cwd                     string
	proj, other, know, global, sub, bad string
	infos                               map[string]LocationInfo
	warnings                            []string
}

func newRefFixture(t *testing.T) *refFixture {
	t.Helper()
	f := &refFixture{base: realTemp(t), infos: map[string]LocationInfo{}}
	f.home = filepath.Join(f.base, "home")
	f.cwd = filepath.Join(f.base, "work")
	f.proj = filepath.Join(f.base, "proj")
	f.other = filepath.Join(f.base, "other")
	f.know = filepath.Join(f.home, "knowledge")
	f.global = filepath.Join(f.home, ".local", "share", "kb")
	f.sub = filepath.Join(f.cwd, "sub")
	f.bad = filepath.Join(f.base, "nokb")
	mk(t, f.cwd, true)
	mk(t, f.bad, true)
	mkLoc := func(root, def string, aliases map[string]string, stores ...string) {
		mk(t, filepath.Join(root, ".kb", "kb.toml"), false)
		for _, s := range stores {
			mk(t, filepath.Join(root, ".kb", "stores", s, "config.toml"), false)
		}
		f.infos[root] = LocationInfo{DefaultStore: def, Aliases: aliases}
	}
	mkLoc(f.proj, "default", map[string]string{
		"notes": "~/knowledge",    // a path: that location's default store
		"ext":   "adr@../other",   // NAME@LOC, relative to the declaring location's root
		"adr":   "default",        // shadowed by the store adr
		"dup":   "adr",            // a bare name: a store of the declaring location
		"bad":   "ghost@/nowhere", // no such location
		"rel":   "../other",       // a relative path
		"gl":    "global",
		"chain": "notes",          // a bare name is a store of the declaring location, never an alias
		"nostr": "ghost@../other", // the location exists, the store does not
		"junk":  "a b",            // not a valid ref
	}, "default", "adr", "docs")
	mkLoc(f.other, "default", nil, "default", "adr")
	mkLoc(f.know, "main", nil, "main")
	mkLoc(f.global, "default", map[string]string{
		"gal":  "hub",     // a store of the global location
		"dup":  "default", // hidden by proj's dup
		"proj": f.proj,    // an absolute path
		"docs": "default", // shadowed by proj's store docs
	}, "default", "hub")
	mkLoc(f.sub, "default", nil, "default")
	return f
}

func (f *refFixture) resolver(localOnly bool) Resolver {
	return Resolver{
		Env: Env{Cwd: f.cwd, Home: f.home},
		Read: func(l *Location) (LocationInfo, error) {
			if info, ok := f.infos[l.Root]; ok {
				return info, nil
			}
			return LocationInfo{}, errs.New(errs.NotFound, "test.Read", "no kb location file at %s", l.FilePath)
		},
		Warn:      func(m string) { f.warnings = append(f.warnings, m) },
		LocalOnly: localOnly,
	}
}

// TestResolve is the decision table for ref resolution: every grammar branch, with the store
// existing and missing, shadowing, ~ and relative paths.
func TestResolve(t *testing.T) {
	f := newRefFixture(t)
	canon := func(store, root string) string { return store + "@" + root }
	cases := []struct {
		name  string
		ref   string
		cur   string // "" = no current location
		want  string // canonical, or ""
		kind  errs.Kind
		msg   string
		warn  string
		local bool // LocalOnly: --root/--global chose the location
		via   string
	}{
		{name: "zero ref is default_store", ref: "", cur: f.proj, want: canon("default", f.proj), via: "default_store"},
		{name: "zero ref without location", ref: "", cur: "", kind: errs.NotFound, msg: "no current location"},
		{name: "local store", ref: "adr", cur: f.proj, want: canon("adr", f.proj), warn: `store "adr" of ` + f.proj + ` shadows the alias "adr"`, via: "store"},
		{name: "local store, no alias", ref: "default", cur: f.proj, want: canon("default", f.proj), via: "store"},
		{name: "alias to a path", ref: "notes", cur: f.proj, want: canon("main", f.know), via: "alias notes"},
		{name: "alias NAME@LOC relative to its declaring root", ref: "ext", cur: f.proj, want: canon("adr", f.other)},
		{name: "alias with a bare name is a store of the declaring location", ref: "dup", cur: f.proj, want: canon("adr", f.proj)},
		{name: "alias relative path", ref: "rel", cur: f.proj, want: canon("default", f.other)},
		{name: "alias to global", ref: "gl", cur: f.proj, want: canon("default", f.global)},
		{name: "alias that points at a missing location", ref: "bad", cur: f.proj, kind: errs.NotFound, msg: `alias "bad"`},
		{name: "alias whose store is missing", ref: "nostr", cur: f.proj, kind: errs.NotFound, msg: `no store "ghost"`},
		{name: "alias never chains", ref: "chain", cur: f.proj, kind: errs.NotFound, msg: `no store "notes"`},
		{name: "alias with a malformed target", ref: "junk", cur: f.proj, kind: errs.Usage, msg: `alias "junk"`},
		{name: "global alias from another location", ref: "gal", cur: f.proj, want: canon("hub", f.global), via: "alias gal"},
		{name: "global alias to an absolute path", ref: "proj", cur: f.other, want: canon("default", f.proj)},
		{name: "location alias hides the global one", ref: "dup", cur: f.proj, want: canon("adr", f.proj)},
		{name: "global alias with no current location", ref: "gal", cur: "", want: canon("hub", f.global)},
		{name: "global alias, shadowed by a local store", ref: "docs", cur: f.proj, want: canon("docs", f.proj), warn: `shadows the alias "docs"`},
		{name: "global alias seen from the global location is its own", ref: "gal", cur: f.global, want: canon("hub", f.global)},
		{name: "unknown word", ref: "nope", cur: f.proj, kind: errs.NotFound, msg: `no store "nope" in ` + f.proj},
		{name: "unknown word lists stores and aliases", ref: "nope", cur: f.proj, kind: errs.NotFound, msg: "stores: adr, default, docs; aliases: adr, bad, chain"},
		{name: "unknown word near a store", ref: "adrs", cur: f.proj, kind: errs.NotFound, msg: `did you mean "adr"?`},
		{name: "unknown word near an alias", ref: "note", cur: f.proj, kind: errs.NotFound, msg: `did you mean "notes"?`},
		{name: "unknown word with no location", ref: "nope", cur: "", kind: errs.NotFound, msg: "no kb location here"},
		{name: "unknown word with no location lists the global aliases", ref: "nope", cur: "", kind: errs.NotFound, msg: "(stores: none; aliases: docs, dup, gal, proj)"},
		{name: "unknown word two edits from a store", ref: "adrxy", cur: f.proj, kind: errs.NotFound, msg: `did you mean "adr"?`},
		{name: "local-only message lists no aliases", ref: "nope", cur: f.proj, local: true, kind: errs.NotFound, msg: "stores: adr, default, docs; aliases: none"},
		{name: "local-only ignores aliases", ref: "notes", cur: f.proj, local: true, kind: errs.NotFound, msg: `no store "notes"`},
		{name: "local-only keeps stores, no shadow warning", ref: "adr", cur: f.proj, local: true, want: canon("adr", f.proj)},
		{name: "global", ref: "global", cur: f.proj, want: canon("default", f.global), via: "global"},
		{name: "global with no current location", ref: "global", cur: "", want: canon("default", f.global)},
		{name: "NAME@global", ref: "hub@global", cur: f.proj, want: canon("hub", f.global)},
		{name: "NAME@global, store missing", ref: "nope@global", cur: f.proj, kind: errs.NotFound, msg: `no store "nope"`},
		{name: "NAME@abs", ref: "adr@" + f.other, cur: f.proj, want: canon("adr", f.other), via: "NAME@LOC"},
		{name: "NAME@abs trailing slash", ref: "adr@" + f.other + "/", cur: f.proj, want: canon("adr", f.other)},
		{name: "NAME@abs with dot segments", ref: "adr@" + f.other + "/x/..", cur: f.proj, want: canon("adr", f.other)},
		{name: "NAME@~/path", ref: "main@~/knowledge", cur: f.proj, want: canon("main", f.know)},
		{name: "NAME@~", ref: "x@~", cur: f.proj, kind: errs.NotFound, msg: "no kb location at " + f.home},
		{name: "NAME@relative is under the cwd", ref: "default@sub", cur: f.proj, want: canon("default", f.sub)},
		{name: "NAME@../relative", ref: "adr@../other", cur: f.proj, want: canon("adr", f.other)},
		{name: "NAME@LOC, location missing", ref: "adr@" + f.bad, cur: f.proj, kind: errs.NotFound, msg: "no kb location at " + f.bad},
		{name: "NAME@LOC, store missing", ref: "ghost@" + f.other, cur: f.proj, kind: errs.NotFound, msg: `no store "ghost"`},
		{name: "NAME@LOC, store missing lists the stores", ref: "ghost@" + f.other, cur: f.proj, kind: errs.NotFound, msg: "stores: adr, default"},
		{name: "path absolute", ref: f.other, cur: f.proj, want: canon("default", f.other), via: "path"},
		{name: "path with trailing slash", ref: f.other + "/", cur: f.proj, want: canon("default", f.other)},
		{name: "path ~/", ref: "~/knowledge", cur: f.proj, want: canon("main", f.know)},
		{name: "path relative ./", ref: "./sub", cur: f.proj, want: canon("default", f.sub)},
		{name: "path relative ../", ref: "../proj", cur: f.proj, want: canon("default", f.proj)},
		{name: "path to a directory with no location", ref: f.bad, cur: f.proj, kind: errs.NotFound, msg: "no kb location at " + f.bad},
		{name: "path to a missing directory", ref: f.base + "/absent", cur: f.proj, kind: errs.NotFound, msg: "no kb location at"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			f.warnings = nil
			var cur *Location
			var info LocationInfo
			if tc.cur != "" {
				cur, info = NewLocation(tc.cur, ""), f.infos[tc.cur]
			}
			ref, err := ParseRef(tc.ref)
			if err != nil {
				t.Fatalf("ParseRef: %v", err)
			}
			res, err := f.resolver(tc.local).Resolve(ref, cur, info)
			if tc.want == "" {
				if err == nil {
					t.Fatalf("Resolve(%q) = %s, want %v containing %q", tc.ref, res.Canonical, tc.kind, tc.msg)
				}
				if errs.KindOf(err) != tc.kind || !strings.Contains(err.Error(), tc.msg) {
					t.Fatalf("Resolve(%q): %v (kind %v), want %v containing %q", tc.ref, err, errs.KindOf(err), tc.kind, tc.msg)
				}
				return
			}
			if err != nil {
				t.Fatalf("Resolve(%q): %v", tc.ref, err)
			}
			if res.Canonical != tc.want {
				t.Errorf("canonical = %q, want %q", res.Canonical, tc.want)
			}
			if tc.via != "" && res.Via != tc.via {
				t.Errorf("via = %q, want %q", res.Via, tc.via)
			}
			// The canonical form parses back to the same store.
			back, err := ParseRef(res.Canonical)
			if err != nil || back != (Ref{Name: res.Workspace.Store, At: res.Workspace.Root}) {
				t.Errorf("ParseRef(%q) = %+v, %v", res.Canonical, back, err)
			}
			switch {
			case tc.warn == "" && len(f.warnings) != 0:
				t.Errorf("unexpected warnings: %q", f.warnings)
			case tc.warn != "" && (len(f.warnings) != 1 || !strings.Contains(f.warnings[0], tc.warn)):
				t.Errorf("warnings = %q, want one containing %q", f.warnings, tc.warn)
			}
		})
	}
}

func TestResolveNotFoundIsUnknownRef(t *testing.T) {
	f := newRefFixture(t)
	cur := NewLocation(f.proj, "")
	r := f.resolver(false)
	_, err := r.Resolve(Ref{Name: "nope"}, cur, f.infos[f.proj])
	if !errors.Is(err, ErrUnknownRef) {
		t.Errorf("unknown word: %v is not ErrUnknownRef", err)
	}
	// A broken alias is not "unknown": the word was found.
	_, err = r.Resolve(Ref{Name: "bad"}, cur, f.infos[f.proj])
	if err == nil || errors.Is(err, ErrUnknownRef) {
		t.Errorf("broken alias: %v", err)
	}
}

// TestResolveGlobalProblems: the global location missing or broken.
func TestResolveGlobalProblems(t *testing.T) {
	f := newRefFixture(t)
	cur := NewLocation(f.proj, "")
	info := f.infos[f.proj]
	delete(f.infos, f.global) // the global location has no kb.toml
	r := f.resolver(false)
	_, err := r.Resolve(Ref{Global: true}, cur, info)
	if errs.KindOf(err) != errs.NotFound || !strings.Contains(err.Error(), "kb init --global") {
		t.Errorf("global missing: %v", err)
	}
	// An alias lookup just skips a missing global location.
	_, err = r.Resolve(Ref{Name: "gal"}, cur, info)
	if !errors.Is(err, ErrUnknownRef) {
		t.Errorf("alias with global missing: %v", err)
	}
	// An unreadable global kb.toml is mentioned by a not-found message.
	r.Read = func(l *Location) (LocationInfo, error) {
		if l.Root == f.global {
			return LocationInfo{}, errs.New(errs.Usage, "test", "%s: bad toml", l.FilePath)
		}
		return f.infos[l.Root], nil
	}
	_, err = r.Resolve(Ref{Name: "gal"}, cur, info)
	if !errors.Is(err, ErrUnknownRef) || !strings.Contains(err.Error(), "global location's kb.toml is unreadable") {
		t.Errorf("alias with global broken: %v", err)
	}
	// No home and no XDG: there is no global location, and `global` says so.
	r2 := f.resolver(false)
	r2.Env.Home = ""
	if _, err := r2.Resolve(Ref{Global: true}, cur, info); errs.KindOf(err) != errs.Usage {
		t.Errorf("global with no home: %v", err)
	}
	if _, err := r2.Resolve(Ref{Path: "~/x"}, cur, info); errs.KindOf(err) != errs.Usage || !strings.Contains(err.Error(), "no home directory") {
		t.Errorf("~ with no home: %v", err)
	}
	r3 := f.resolver(false)
	r3.Env.Cwd = ""
	if _, err := r3.Resolve(Ref{Path: "./x"}, cur, info); errs.KindOf(err) != errs.Usage {
		t.Errorf("relative with no cwd: %v", err)
	}
}

func TestListAliases(t *testing.T) {
	f := newRefFixture(t)
	cur := NewLocation(f.proj, "")
	rows := f.resolver(false).ListAliases(cur, f.infos[f.proj])
	byKey := map[string]AliasStatus{}
	for _, r := range rows {
		byKey[r.Scope+"/"+r.Name] = r
	}
	check := func(key, resolved string, broken bool) AliasStatus {
		t.Helper()
		r, ok := byKey[key]
		if !ok {
			t.Fatalf("no row %s in %+v", key, rows)
		}
		if (r.Problem != "") != broken || r.Resolved != resolved {
			t.Errorf("%s: resolved %q problem %q; want resolved %q broken %v", key, r.Resolved, r.Problem, resolved, broken)
		}
		return r
	}
	check("location/notes", "main@"+f.know, false)
	check("location/ext", "adr@"+f.other, false)
	check("location/bad", "", true)
	check("location/junk", "", true)
	check("global/gal", "hub@"+f.global, false)
	if r := check("location/adr", "default@"+f.proj, false); !r.Shadowed {
		t.Errorf("alias adr should be shadowed by the store adr: %+v", r)
	}
	if r := check("global/dup", "default@"+f.global, false); !r.Overridden {
		t.Errorf("global dup should be overridden: %+v", r)
	}
	if r := check("global/docs", "default@"+f.global, false); !r.Shadowed {
		t.Errorf("global docs should be shadowed by the store docs: %+v", r)
	}
	if r := check("location/notes", "main@"+f.know, false); r.Shadowed || r.Overridden {
		t.Errorf("notes should be plain: %+v", r)
	}
	// Rows come out location first, then global, each sorted by name.
	for i := 1; i < len(rows); i++ {
		a, b := rows[i-1], rows[i]
		if a.Scope == "global" && b.Scope == "location" || a.Scope == b.Scope && a.Name > b.Name {
			t.Errorf("row order: %s/%s before %s/%s", a.Scope, a.Name, b.Scope, b.Name)
		}
	}
	// With no current location only the global aliases are listed.
	if rows := f.resolver(false).ListAliases(nil, LocationInfo{}); len(rows) != 4 || rows[0].Scope != "global" {
		t.Errorf("no current location: %+v", rows)
	}
	// An empty target is a broken row with a Usage problem, not a row that resolves to nothing.
	empty := f.resolver(false).ListAliases(cur, LocationInfo{Aliases: map[string]string{"empty": ""}})
	if len(empty) == 0 || empty[0].Name != "empty" || empty[0].Resolved != "" || !strings.Contains(empty[0].Problem, "empty target") {
		t.Errorf("empty target row: %+v", empty)
	}
	// When cur is the global location, its aliases are listed once, as its own (location) scope.
	gcur := NewLocation(f.global, "")
	own := f.resolver(false).ListAliases(gcur, f.infos[f.global])
	if len(own) != 4 {
		t.Fatalf("aliases of the global location listed %d times, want 4: %+v", len(own), own)
	}
	for _, r := range own {
		if r.Scope != "location" {
			t.Errorf("global location alias %q has scope %q, want location", r.Name, r.Scope)
		}
	}
}

func TestExpandPath(t *testing.T) {
	for _, tc := range []struct{ in, want string }{
		{"/a/b", "/a/b"}, {"/a/b/", "/a/b"}, {"/a//b/../c", "/a/c"}, {"~", "/h"}, {"~/x/", "/h/x"},
		{"x", "/base/x"}, {"./x/y/", "/base/x/y"}, {"../x", "/x"}, {".", "/base"}, {"/", "/"},
	} {
		got, err := ExpandPath(tc.in, "/base", "/h")
		if err != nil || got != tc.want {
			t.Errorf("ExpandPath(%q) = %q, %v; want %q", tc.in, got, err, tc.want)
		}
	}
	for _, tc := range []struct{ in, base, home string }{{"~", "/b", ""}, {"x", "", "/h"}, {"~/x", "/b", "rel"}} {
		if _, err := ExpandPath(tc.in, tc.base, tc.home); errs.KindOf(err) != errs.Usage {
			t.Errorf("ExpandPath(%q, %q, %q) = %v, want Usage", tc.in, tc.base, tc.home, err)
		}
	}
}

func TestEditDistanceAndNearest(t *testing.T) {
	for _, tc := range []struct {
		a, b string
		want int
	}{{"", "", 0}, {"a", "", 1}, {"", "ab", 2}, {"adr", "adrs", 1}, {"kitten", "sitting", 3}, {"abc", "abc", 0}, {"ab", "ba", 2}} {
		if got := editDistance(tc.a, tc.b); got != tc.want {
			t.Errorf("editDistance(%q,%q) = %d, want %d", tc.a, tc.b, got, tc.want)
		}
	}
	if got := nearest("adrs", []string{"zzz", "adr"}); got != "adr" {
		t.Errorf("nearest = %q", got)
	}
	if got := nearest("a", []string{"zzzz"}); got != "" {
		t.Errorf("nearest of a far word = %q", got)
	}
	if got := nearest("adr", []string{"adr"}); got != "" {
		t.Errorf("nearest must not suggest the word itself: %q", got)
	}
	// Distance 2 is accepted for a three-rune word (limit len/3+1 = 2) but not for a two-rune one.
	if got := nearest("abc", []string{"axy"}); got != "axy" {
		t.Errorf("nearest at distance 2 of a 3-rune word = %q, want axy", got)
	}
	if got := nearest("ab", []string{"cd"}); got != "" {
		t.Errorf("nearest at distance 2 of a 2-rune word = %q, want none", got)
	}
}

// FuzzParseRef: ParseRef never panics; a ref it accepts is exactly one form, renders back to the
// text it came from and parses to the same Ref; and the canonical form NAME@ABSROOT of any valid name
// and clean absolute root parses back to that pair.
func FuzzParseRef(f *testing.F) {
	for _, s := range []string{"", "global", "adr", "adr@/p", "adr@global", "~/x", "~", "../x", "adr@", "@", "a@b@c", "c:x", "~u/x",
		"x\x00", "/p/\n", "a/b", strings.Repeat("a", 33), "ADR", "adr@~/a b/c", "/", ".", "日本"} {
		f.Add(s)
	}
	f.Fuzz(func(t *testing.T, s string) {
		ref, err := ParseRef(s)
		if err != nil {
			if errs.KindOf(err) != errs.Usage {
				t.Fatalf("ParseRef(%q): %v is not a Usage error", s, err)
			}
			return
		}
		if ref.String() != s {
			t.Fatalf("ParseRef(%q).String() = %q", s, ref.String())
		}
		if back, err := ParseRef(ref.String()); err != nil || back != ref {
			t.Fatalf("re-parse of %q = %+v, %v; want %+v", s, back, err, ref)
		}
		forms := 0
		for _, set := range []bool{ref.Global, ref.Path != "", ref.At != "", ref.Name != "" && ref.At == ""} {
			if set {
				forms++
			}
		}
		if forms+btoi(ref.IsZero()) != 1 {
			t.Fatalf("ParseRef(%q) = %+v is not exactly one form", s, ref)
		}
		if ref.Name != "" && ValidName(ref.Name) != nil {
			t.Fatalf("ParseRef(%q) accepted the name %q", s, ref.Name)
		}
		// The canonical form of this ref's name at a clean absolute root.
		if ref.Name != "" {
			root := "/" + strings.Trim(filepath.Clean("/"+ref.At), "/")
			if c, err := ParseRef(Canonical(ref.Name, root)); err != nil || c != (Ref{Name: ref.Name, At: root}) {
				t.Fatalf("canonical %q parsed to %+v, %v", Canonical(ref.Name, root), c, err)
			}
		}
	})
}

func btoi(b bool) int {
	if b {
		return 1
	}
	return 0
}

// TestResolveDefaultStoreMissing: a default_store that names no store is NotFound naming kb.toml's
// default_store, for every ref form that uses it: the zero ref, global and a PATH.
func TestResolveDefaultStoreMissing(t *testing.T) {
	f := newRefFixture(t)
	for _, root := range []string{f.proj, f.global, f.other} {
		info := f.infos[root]
		info.DefaultStore = "ghost"
		f.infos[root] = info
	}
	r := f.resolver(false)
	cur := NewLocation(f.proj, "")
	for _, tc := range []struct {
		name string
		ref  Ref
	}{
		{"zero ref", Ref{}},
		{"global", Ref{Global: true}},
		{"path", Ref{Path: f.other}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			res, err := r.Resolve(tc.ref, cur, f.infos[f.proj])
			if errs.KindOf(err) != errs.NotFound || !strings.Contains(err.Error(), `default_store "ghost"`) {
				t.Fatalf("Resolve = %+v, %v; want NotFound naming default_store \"ghost\"", res, err)
			}
		})
	}
}

// TestResolveFromGlobalLocation: with the global location as the current one, an unknown word is
// an ordinary unknown ref (the global aliases are not searched twice), and a global alias is found.
func TestResolveFromGlobalLocation(t *testing.T) {
	f := newRefFixture(t)
	r := f.resolver(false)
	g := NewLocation(f.global, "")
	if _, err := r.Resolve(Ref{Name: "nope"}, g, f.infos[f.global]); errs.KindOf(err) != errs.NotFound || !errors.Is(err, ErrUnknownRef) {
		t.Errorf("unknown word from the global location: %v", err)
	}
	_, _, _, found, gerr := r.findAlias("nope", g, f.infos[f.global])
	if found || gerr != nil {
		t.Errorf("findAlias from the global location = found %v, %v", found, gerr)
	}
}

// TestResolverLocation: the location a self-locating ref names; a ref that names no location is a
// bug (Internal).
func TestResolverLocation(t *testing.T) {
	f := newRefFixture(t)
	r := f.resolver(false)
	for _, tc := range []struct {
		name string
		ref  Ref
		root string
	}{
		{"global", Ref{Global: true}, f.global},
		{"NAME@LOC", Ref{Name: "adr", At: f.other}, f.other},
		{"PATH", Ref{Path: f.proj}, f.proj},
	} {
		loc, info, err := r.Location(tc.ref)
		if err != nil || loc.Root != tc.root || info.DefaultStore != f.infos[tc.root].DefaultStore {
			t.Errorf("%s: Location = %v, %+v, %v; want %s", tc.name, loc, info, err, tc.root)
		}
	}
	if _, _, err := r.Location(Ref{}); errs.KindOf(err) != errs.Internal {
		t.Errorf("zero ref: %v, want Internal", err)
	}
	if _, _, err := r.Location(Ref{Name: "adr"}); errs.KindOf(err) != errs.Internal {
		t.Errorf("bare name: %v, want Internal", err)
	}
}

// TestHasAlias: an alias of the current location, one of the global location, and neither.
func TestHasAlias(t *testing.T) {
	f := newRefFixture(t)
	r := f.resolver(false)
	cur := NewLocation(f.proj, "")
	for _, tc := range []struct {
		name string
		want bool
	}{
		{"notes", true}, // this location
		{"gal", true},   // global location
		{"nope", false},
		{"default", false}, // a store, not an alias
	} {
		if got := r.HasAlias(tc.name, cur, f.infos[f.proj]); got != tc.want {
			t.Errorf("HasAlias(%q) = %v, want %v", tc.name, got, tc.want)
		}
	}
	if !r.HasAlias("gal", nil, LocationInfo{}) {
		t.Error("HasAlias with no current location must still see global aliases")
	}
}
