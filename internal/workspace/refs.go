package workspace

import (
	"strings"

	"github.com/nickstrad/kb/internal/errs"
)

// MaxRefLen bounds one store ref, so a hostile alias or flag cannot make kb build a huge path. It
// is the same bound as config.MaxAliasTargetLen.
const MaxRefLen = 4096

// Ref is a parsed store reference (docs/design/named-stores.md §3). The grammar is syntactic:
//
//	""                zero Ref: the location's default store
//	global            the global location's default store
//	NAME              a store of the current location, or an alias (Resolver decides which)
//	NAME@LOC          a named store of another location; LOC is "global" or a path
//	PATH              the default store of the location at PATH
//
// A PATH is "~", "~/...", "/...", "./...", "../...", "." or "..": a bare "docs/x" is refused as
// ambiguous, so a typo in a store name never turns into a path. After "@" any path is allowed,
// because there is no ambiguity there; write ./global for a directory called global.
// Ref keeps the strings as typed; "~" and relative paths are expanded by the Resolver, which
// knows the home and working directory. String gives the ref back as typed.
type Ref struct {
	Global bool   // the keyword global
	Name   string // NAME, or the store of NAME@LOC
	At     string // LOC of NAME@LOC, as typed ("global" or a path); "" otherwise
	Path   string // a path-only ref, as typed
}

// IsZero reports whether no reference was given.
func (r Ref) IsZero() bool { return r == Ref{} }

// NamesLocation reports whether the ref states its own location (global, NAME@LOC, PATH) rather
// than being a bare word that applies within the current one.
func (r Ref) NamesLocation() bool { return r.Global || r.At != "" || r.Path != "" }

// ParseRef parses one store ref. Every failure is a Usage error that says what is wrong with the
// text; whether a store or alias of that name exists is the Resolver's question.
func ParseRef(s string) (Ref, error) {
	const op = "workspace.ParseRef"
	switch {
	case s == "":
		return Ref{}, nil
	case s == GlobalName:
		return Ref{Global: true}, nil
	case len(s) > MaxRefLen:
		return Ref{}, errs.New(errs.Usage, op, "store ref is %d bytes; at most %d", len(s), MaxRefLen)
	case hasControl(s):
		return Ref{}, errs.New(errs.Usage, op, "store ref %q has a control character", s)
	}
	if looksLikePath(s) {
		return Ref{Path: s}, nil
	}
	if strings.HasPrefix(s, "~") {
		return Ref{}, errs.New(errs.Usage, op, "store ref %q: only ~ and ~/... are supported, not ~user", s)
	}
	if i := strings.IndexByte(s, '@'); i >= 0 {
		name, loc := s[:i], s[i+1:]
		if err := ValidName(name); err != nil {
			return Ref{}, errs.Wrap(err, errs.Usage, op, "store ref %q: %v", s, err)
		}
		switch {
		case loc == "":
			return Ref{}, errs.New(errs.Usage, op, "store ref %q: nothing after @; write NAME@global or NAME@PATH", s)
		case strings.HasPrefix(loc, "~") && !looksLikePath(loc):
			return Ref{}, errs.New(errs.Usage, op, "store ref %q: only ~ and ~/... are supported, not ~user", s)
		}
		return Ref{Name: name, At: loc}, nil
	}
	if strings.Contains(s, "/") {
		return Ref{}, errs.New(errs.Usage, op,
			"store ref %q is neither a store name nor a path: a path starts with /, ~/, ./ or ../ (write ./%s)", s, s)
	}
	if strings.Contains(s, ":") {
		return Ref{}, errs.New(errs.Usage, op,
			"store ref %q: \":\" is not part of a ref (it separates a store from an entry path, as in kb show STORE:path)", s)
	}
	if err := ValidName(s); err != nil {
		return Ref{}, err
	}
	return Ref{Name: s}, nil
}

// String is the ref as typed.
func (r Ref) String() string {
	switch {
	case r.Global:
		return GlobalName
	case r.Path != "":
		return r.Path
	case r.At != "":
		return r.Name + "@" + r.At
	}
	return r.Name
}

// Canonical is the form kb records for a resolved store: NAME@ABSROOT. ParseRef of it gives
// Ref{Name: name, At: root} again.
func Canonical(name, root string) string { return name + "@" + root }

func looksLikePath(s string) bool {
	switch s {
	case ".", "..", "~":
		return true
	}
	return strings.HasPrefix(s, "/") || strings.HasPrefix(s, "~/") || strings.HasPrefix(s, "./") || strings.HasPrefix(s, "../")
}

func hasControl(s string) bool {
	for _, r := range s {
		if r < 0x20 || r == 0x7f {
			return true
		}
	}
	return false
}
