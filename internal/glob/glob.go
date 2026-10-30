// Package glob matches slash-separated, root-relative paths against the patterns a store's
// [[sources]] tables use ("docs/**/*.md", "README.md", "adr/[0-9]*.md").
//
// A pattern is split on "/" into segments. The segment "**" matches zero or more whole path
// segments; every other segment is a path.Match pattern, so "*" and "?" never cross a "/". kb uses
// its own matcher rather than a dependency because these few rules are all a source glob needs.
//
// Invariants: Match never panics and runs in O(len(pattern segments) × len(path segments)) for any
// input (no backtracking blow-up with many "**"); Valid accepts exactly the patterns Match can
// judge; Base(p) is a literal directory prefix of every path p matches; CouldMatchUnder never
// prunes a directory that holds a match.
package glob

import (
	"fmt"
	"path"
	"strings"
)

// Valid reports why pattern cannot be a source glob, or nil. A pattern is relative and clean:
// not empty, no leading "/", no empty, "." or ".." segment, and every segment other than "**" is
// a well-formed path.Match pattern.
func Valid(pattern string) error {
	if pattern == "" {
		return fmt.Errorf("is empty")
	}
	if strings.HasPrefix(pattern, "/") {
		return fmt.Errorf("%q is absolute; source globs are relative to the location root", pattern)
	}
	for _, seg := range strings.Split(pattern, "/") {
		switch seg {
		case "":
			return fmt.Errorf("%q has an empty path segment", pattern)
		case ".", "..":
			return fmt.Errorf("%q has a %q segment; source globs stay inside the location root", pattern, seg)
		case "**":
			continue
		}
		if _, err := path.Match(seg, ""); err != nil {
			return fmt.Errorf("%q is not a valid pattern: %w", pattern, err)
		}
	}
	return nil
}

// Match reports whether the slash-separated relative path name matches pattern. An invalid
// pattern matches nothing.
func Match(pattern, name string) bool {
	if Valid(pattern) != nil || name == "" {
		return false
	}
	ps := strings.Split(pattern, "/")
	ns := strings.Split(name, "/")
	// ok[i][j]: ps[i:] matches ns[j:]. Filled from the end, so each cell is computed once.
	ok := make([][]bool, len(ps)+1)
	for i := range ok {
		ok[i] = make([]bool, len(ns)+1)
	}
	ok[len(ps)][len(ns)] = true
	for i := len(ps) - 1; i >= 0; i-- {
		for j := len(ns); j >= 0; j-- {
			if ps[i] == "**" {
				// zero segments, or one segment and stay on "**".
				ok[i][j] = ok[i+1][j] || (j < len(ns) && ok[i][j+1])
				continue
			}
			if j == len(ns) {
				continue
			}
			m, _ := path.Match(ps[i], ns[j]) // Valid above: no error is possible
			ok[i][j] = m && ok[i+1][j+1]
		}
	}
	return ok[0][0]
}

// Base returns the directory a walk for pattern can start from: its leading segments that hold
// no pattern syntax, excluding the last segment. "docs/**/*.md" → "docs", "README.md" → "",
// "a/b/c.md" → "a/b", "**/x.md" → "".
func Base(pattern string) string {
	segs := strings.Split(pattern, "/")
	var lit []string
	for _, s := range segs[:len(segs)-1] {
		if s == "**" || strings.ContainsAny(s, `*?[\`) {
			break
		}
		lit = append(lit, s)
	}
	return strings.Join(lit, "/")
}

// CouldMatchUnder reports whether some path below the directory dir (slash-separated, relative)
// can match pattern, so a walk can skip a directory that cannot. It is exact in one direction:
// when Match(pattern, dir+"/"+x) holds for any x, CouldMatchUnder(pattern, dir) is true.
func CouldMatchUnder(pattern, dir string) bool {
	if Valid(pattern) != nil {
		return false
	}
	ps := strings.Split(pattern, "/")
	// states holds the pattern positions reachable after the segments consumed so far.
	states := closure(ps, map[int]bool{0: true})
	if dir != "" {
		for _, seg := range strings.Split(dir, "/") {
			next := map[int]bool{}
			for i := range states {
				switch {
				case i >= len(ps):
				case ps[i] == "**":
					next[i] = true
				default:
					if m, _ := path.Match(ps[i], seg); m {
						next[i+1] = true
					}
				}
			}
			states = closure(ps, next)
			if len(states) == 0 {
				return false
			}
		}
	}
	for i := range states {
		if i < len(ps) {
			return true
		}
	}
	return false
}

// closure adds, for every reachable position on a "**", the position after it ("**" matching
// zero segments).
func closure(ps []string, states map[int]bool) map[int]bool {
	for i := 0; i < len(ps); i++ {
		if states[i] && ps[i] == "**" {
			states[i+1] = true
		}
	}
	return states
}
