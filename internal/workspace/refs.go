package workspace

import (
	"strings"

	"github.com/nickstrad/kb/internal/errs"
)

// Ref is a parsed store reference. Today it is a local store name or the keyword global; PLAN 3.1
// adds aliases and NAME@LOC. The zero Ref means "no reference given": use the location's default
// store.
type Ref struct {
	Global bool   // the global location's default store
	Name   string // a store of the current location; "" with Global or for the zero Ref
}

// IsZero reports whether no reference was given.
func (r Ref) IsZero() bool { return !r.Global && r.Name == "" }

// ParseRef parses the value of --store. "" is the zero Ref, "global" is Ref{Global: true}, and a
// valid store name is Ref{Name: name}. Anything else is a Usage error; refs with "@" or ":" are
// named as not supported yet, so the message does not look like a typo in a name.
func ParseRef(s string) (Ref, error) {
	switch {
	case s == "":
		return Ref{}, nil
	case s == GlobalName:
		return Ref{Global: true}, nil
	}
	if strings.ContainsAny(s, "@:") {
		return Ref{}, errs.New(errs.Usage, "workspace.ParseRef",
			"store ref %q: NAME@LOCATION refs are not supported yet; use a store name of this location, or global", s)
	}
	if err := ValidName(s); err != nil {
		return Ref{}, err
	}
	return Ref{Name: s}, nil
}

// String is the reference as typed.
func (r Ref) String() string {
	if r.Global {
		return GlobalName
	}
	return r.Name
}
