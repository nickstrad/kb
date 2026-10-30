package workspace

import (
	"errors"
	"fmt"
	"path/filepath"
	"sort"
	"strings"

	"github.com/nickstrad/kb/internal/assert"
	"github.com/nickstrad/kb/internal/errs"
)

// LocationInfo is what ref resolution needs from a location's kb.toml. The workspace package
// cannot import config (config imports workspace), so the caller supplies a reader.
type LocationInfo struct {
	DefaultStore string
	Aliases      map[string]string // alias name -> store ref
}

// Resolver turns a Ref into the store it names.
//
// Resolution order for a bare NAME: a store of the current location; else an alias of the current
// location; else an alias of the global location. A store that shadows an alias wins, with a
// warning. global, NAME@LOC and PATH name their own location. An alias target is parsed with
// the same grammar, but a bare NAME in it means a store of the location that declares the alias
// (aliases never chain), and a relative path in it is relative to that location's root.
type Resolver struct {
	Env  Env
	Read func(l *Location) (LocationInfo, error) // reads l's kb.toml; NotFound when there is none
	Warn func(msg string)                        // one-line warnings; nil discards them

	// LocalOnly makes a bare NAME mean a store of the current location only: the location was
	// chosen with --root or --global, so an alias (which names its own location) cannot apply.
	LocalOnly bool
}

// HasAlias reports whether name is an alias of cur or of the global location.
func (r Resolver) HasAlias(name string, cur *Location, curInfo LocationInfo) bool {
	_, _, _, found, _ := r.findAlias(name, cur, curInfo)
	return found
}

// ErrUnknownRef is wrapped by the NotFound error for a bare word that names no store and no alias,
// so a caller can tell it from a broken alias or a missing location (errors.Is).
var ErrUnknownRef = errors.New("no such store or alias")

// Location is the location a ref that names its own one (global, NAME@LOC, PATH) points at, with its
// kb.toml. The store itself is not looked up.
func (r Resolver) Location(ref Ref) (*Location, LocationInfo, error) {
	switch {
	case ref.Global:
		return r.locationOf(GlobalName, r.Env.Cwd)
	case ref.At != "":
		return r.locationOf(ref.At, r.Env.Cwd)
	case ref.Path != "":
		return r.locationOf(ref.Path, r.Env.Cwd)
	}
	return nil, LocationInfo{}, errs.New(errs.Internal, "workspace.Location", "ref %q names no location", ref)
}

// Resolved is a store found by Resolve.
type Resolved struct {
	Workspace *Workspace
	Canonical string // NAME@ABSROOT, what the search log records
	Via       string // how it was found, for --debug: "store", "alias x", "global", "NAME@LOC", "path"
}

func (r Resolver) warn(format string, args ...any) {
	if r.Warn != nil {
		r.Warn(fmt.Sprintf(format, args...))
	}
}

func resolved(ws *Workspace, via string) Resolved {
	return Resolved{Workspace: ws, Canonical: Canonical(ws.Store, ws.Root), Via: via}
}

// Resolve finds the store ref names. cur is the current location (nil when discovery found none)
// and curInfo its kb.toml. A zero ref is the default store of cur. Malformed refs were refused by
// ParseRef; here an unknown store, alias or location is NotFound, and the message lists what does
// exist.
func (r Resolver) Resolve(ref Ref, cur *Location, curInfo LocationInfo) (Resolved, error) {
	switch {
	case ref.IsZero():
		if cur == nil {
			return Resolved{}, errs.New(errs.NotFound, "workspace.Resolve", "no current location")
		}
		ws, err := r.defaultStore(cur, curInfo)
		if err != nil {
			return Resolved{}, err
		}
		return resolved(ws, "default_store"), nil
	case ref.Name != "" && !ref.NamesLocation():
		return r.resolveName(ref.Name, cur, curInfo)
	}
	return r.resolveTarget(ref, r.Env.Cwd, "")
}

// resolveName is the bare-word rule: local store, then alias.
func (r Resolver) resolveName(name string, cur *Location, curInfo LocationInfo) (Resolved, error) {
	var local *Workspace
	if cur != nil {
		local, _ = cur.Store(name) // nil when there is no such store
	}
	var target, scope string
	var decl *Location
	var found bool
	var globalErr error
	if !r.LocalOnly {
		target, decl, scope, found, globalErr = r.findAlias(name, cur, curInfo)
	}
	switch {
	case local != nil && found:
		r.warn("warning: store %q of %s shadows the alias %q (%s = %q); using the store", name, cur.Root, name, scope, target)
		return resolved(local, "store"), nil
	case local != nil:
		return resolved(local, "store"), nil
	case found:
		ref, err := ParseRef(target)
		var res Resolved
		if err == nil {
			res, err = r.resolveTarget(ref, decl.Root, name)
		}
		if err != nil {
			return Resolved{}, errs.Wrap(err, errs.KindOf(err), "workspace.Resolve", "alias %q (%s, %q in %s): %v", name, scope, target, decl.FilePath, err)
		}
		res.Via = "alias " + name
		return res, nil
	}
	return Resolved{}, r.notFound(name, cur, curInfo, globalErr)
}

// findAlias looks for name in the aliases of cur, then of the global location. decl is the
// location that declares it. globalErr is why the global location's aliases could not be read
// (nil for a missing global location, which is normal).
func (r Resolver) findAlias(name string, cur *Location, curInfo LocationInfo) (target string, decl *Location, scope string, found bool, globalErr error) {
	if cur != nil {
		if t, ok := curInfo.Aliases[name]; ok {
			return t, cur, "this location", true, nil
		}
	}
	g, info, err := r.globalLocation()
	if err != nil || g == nil {
		return "", nil, "", false, err
	}
	if cur != nil && g.Root == cur.Root {
		return "", nil, "", false, nil // already searched as the current location
	}
	if t, ok := info.Aliases[name]; ok {
		return t, g, "global location", true, nil
	}
	return "", nil, "", false, nil
}

// globalLocation reads the global location. A missing one is (nil, _, nil): aliases there are
// optional. Other failures (a broken kb.toml) are returned so a not-found message can mention them.
func (r Resolver) globalLocation() (*Location, LocationInfo, error) {
	g, err := ResolveLocation("", true, r.Env)
	if err != nil {
		return nil, LocationInfo{}, nil // no home directory: no global location
	}
	info, err := r.Read(g)
	if err != nil {
		if errs.KindOf(err) == errs.NotFound {
			return nil, LocationInfo{}, nil
		}
		return nil, LocationInfo{}, err
	}
	return g, info, nil
}

// resolveTarget resolves a ref that is not a bare word to be looked up in the current location:
// global, NAME@LOC, PATH, or (inside an alias) a bare NAME of the declaring location. base is what
// relative paths are relative to (the working directory, or the declaring location's root).
// alias is the alias being resolved, "" for a typed ref.
func (r Resolver) resolveTarget(ref Ref, base, alias string) (Resolved, error) {
	switch {
	case ref.Global:
		g, info, err := r.requireGlobal()
		if err != nil {
			return Resolved{}, err
		}
		ws, err := r.defaultStore(g, info)
		if err != nil {
			return Resolved{}, err
		}
		return resolved(ws, "global"), nil
	case ref.At != "":
		loc, _, err := r.locationOf(ref.At, base)
		if err != nil {
			return Resolved{}, err
		}
		ws, err := loc.Store(ref.Name)
		if err != nil {
			return Resolved{}, err
		}
		return resolved(ws, "NAME@LOC"), nil
	case ref.Path != "":
		loc, info, err := r.locationOf(ref.Path, base)
		if err != nil {
			return Resolved{}, err
		}
		ws, err := r.defaultStore(loc, info)
		if err != nil {
			return Resolved{}, err
		}
		return resolved(ws, "path"), nil
	}
	// A bare NAME inside an alias target: a store of the declaring location (base is its root).
	assert.Pre(alias != "", "workspace: resolveTarget got a bare name outside an alias")
	loc := NewLocation(base, r.Env.CacheBase)
	ws, err := loc.Store(ref.Name)
	if err != nil {
		return Resolved{}, err
	}
	return resolved(ws, "store"), nil
}

// requireGlobal is the global location, which must exist for an explicit global ref.
func (r Resolver) requireGlobal() (*Location, LocationInfo, error) {
	g, err := ResolveLocation("", true, r.Env)
	if err != nil {
		return nil, LocationInfo{}, err
	}
	info, err := r.Read(g)
	if err != nil {
		if errs.KindOf(err) == errs.NotFound {
			return nil, LocationInfo{}, errs.Wrap(err, errs.NotFound, "workspace.Resolve",
				"no global location at %s; create it with kb init --global", g.Root)
		}
		return nil, LocationInfo{}, err
	}
	return g, info, nil
}

// locationOf opens the location named by loc ("global", or a path as typed) and reads its kb.toml.
func (r Resolver) locationOf(loc, base string) (*Location, LocationInfo, error) {
	if loc == GlobalName {
		return r.requireGlobal()
	}
	root, err := ExpandPath(loc, base, r.Env.Home)
	if err != nil {
		return nil, LocationInfo{}, err
	}
	l := NewLocation(root, r.Env.CacheBase)
	info, err := r.Read(l)
	if err != nil {
		if errs.KindOf(err) == errs.NotFound {
			return nil, LocationInfo{}, errs.Wrap(err, errs.NotFound, "workspace.Resolve",
				"no kb location at %s (no .kb/kb.toml); run kb init --root %s", root, root)
		}
		return nil, LocationInfo{}, err
	}
	return l, info, nil
}

// defaultStore is the store named by info.DefaultStore. A name with no store is NotFound naming kb.toml.
func (r Resolver) defaultStore(l *Location, info LocationInfo) (*Workspace, error) {
	ws, err := l.Store(info.DefaultStore)
	if err != nil {
		if errs.KindOf(err) == errs.NotFound {
			return nil, errs.Wrap(err, errs.NotFound, "workspace.Resolve",
				"default_store %q in %s names no store in %s; edit default_store or create it with kb init --name %s",
				info.DefaultStore, l.FilePath, l.Root, info.DefaultStore)
		}
		return nil, err
	}
	return ws, nil
}

// ExpandPath makes p absolute and clean: "~" and "~/x" are under home, a relative path is under
// base. Both failures are Usage errors. A trailing slash is dropped (Clean).
func ExpandPath(p, base, home string) (string, error) {
	const op = "workspace.ExpandPath"
	switch {
	case p == "~" || strings.HasPrefix(p, "~/"):
		if !filepath.IsAbs(home) {
			return "", errs.New(errs.Usage, op, "cannot expand %q: no home directory", p)
		}
		p = filepath.Join(home, p[1:])
	case !filepath.IsAbs(p):
		if !filepath.IsAbs(base) {
			return "", errs.New(errs.Usage, op, "cannot make %q absolute: no base directory", p)
		}
		p = filepath.Join(base, p)
	}
	p = filepath.Clean(p)
	assert.Post(filepath.IsAbs(p), "workspace: ExpandPath returned %q", p)
	return p, nil
}

// notFound explains an unknown bare word: what exists, and the nearest name.
func (r Resolver) notFound(name string, cur *Location, curInfo LocationInfo, globalErr error) error {
	var stores, aliases []string
	where := "no kb location here"
	if cur != nil {
		stores, _, _ = cur.Stores()
		where = "in " + cur.Root
		if !r.LocalOnly {
			aliases = sortedKeys(curInfo.Aliases)
		}
	}
	if g, info, err := r.globalLocation(); !r.LocalOnly && err == nil && g != nil && (cur == nil || g.Root != cur.Root) {
		aliases = append(aliases, sortedKeys(info.Aliases)...)
	}
	list := func(s []string) string {
		if len(s) == 0 {
			return "none"
		}
		return strings.Join(s, ", ")
	}
	msg := fmt.Sprintf("no store %q %s and no alias of that name (stores: %s; aliases: %s)", name, where, list(stores), list(aliases))
	if near := nearest(name, append(append([]string{}, stores...), aliases...)); near != "" {
		msg += fmt.Sprintf("; did you mean %q?", near)
	}
	if cur != nil {
		msg += fmt.Sprintf("; create a store with kb init --name %s", name)
	}
	if globalErr != nil {
		msg += fmt.Sprintf("; the global location's kb.toml is unreadable: %v", globalErr)
	}
	return errs.Wrap(ErrUnknownRef, errs.NotFound, "workspace.Resolve", "%s", msg)
}

func sortedKeys(m map[string]string) []string {
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	return keys
}

// nearest is the candidate within edit distance 2 of name (at most a third of its length), or "".
func nearest(name string, cands []string) string {
	best, bestD := "", 3
	for _, c := range cands {
		if d := editDistance(name, c); d < bestD && d <= len(name)/3+1 && c != name {
			best, bestD = c, d
		}
	}
	return best
}

func editDistance(a, b string) int {
	prev := make([]int, len(b)+1)
	for j := range prev {
		prev[j] = j
	}
	for i := 1; i <= len(a); i++ {
		cur := make([]int, len(b)+1)
		cur[0] = i
		for j := 1; j <= len(b); j++ {
			cost := 1
			if a[i-1] == b[j-1] {
				cost = 0
			}
			cur[j] = min(prev[j]+1, cur[j-1]+1, prev[j-1]+cost)
		}
		prev = cur
	}
	return prev[len(b)]
}

// AliasStatus is one alias for `kb stores`: where it is declared, what it points at, and whether
// that store exists.
type AliasStatus struct {
	Name       string
	Target     string // the ref as written
	Scope      string // "location" or "global"
	Resolved   string // NAME@ABSROOT, "" when it does not resolve
	Problem    string // why it does not resolve
	Shadowed   bool   // a store of the current location has this name and wins
	Overridden bool   // a global alias hidden by a same-named alias of the current location
}

// ListAliases resolves every alias of cur, then of the global location (when it is another
// location), sorted by scope then name. A broken alias is a row with Problem, never an error.
func (r Resolver) ListAliases(cur *Location, curInfo LocationInfo) []AliasStatus {
	var rows []AliasStatus
	add := func(decl *Location, info LocationInfo, scope string) {
		for _, n := range sortedKeys(info.Aliases) {
			row := AliasStatus{Name: n, Target: info.Aliases[n], Scope: scope}
			if cur != nil {
				if _, err := cur.Store(n); err == nil {
					row.Shadowed = true
				}
				if scope == "global" {
					_, row.Overridden = curInfo.Aliases[n]
				}
			}
			ref, err := ParseRef(row.Target)
			if err == nil && ref.IsZero() {
				err = errs.New(errs.Usage, "workspace.ListAliases", "empty target")
			}
			if err == nil {
				var res Resolved
				if res, err = r.resolveTarget(ref, decl.Root, n); err == nil {
					row.Resolved = res.Canonical
				}
			}
			if err != nil {
				row.Problem = err.Error()
			}
			rows = append(rows, row)
		}
	}
	if cur != nil {
		add(cur, curInfo, "location")
	}
	if g, info, err := r.globalLocation(); err == nil && g != nil && (cur == nil || g.Root != cur.Root) {
		add(g, info, "global")
	}
	return rows
}
