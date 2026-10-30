package glob

import (
	"path"
	"strings"
	"testing"
)

func TestMatch(t *testing.T) {
	tests := []struct {
		pattern, name string
		want          bool
	}{
		{"README.md", "README.md", true},
		{"README.md", "docs/README.md", false},
		{"docs/*.md", "docs/a.md", true},
		{"docs/*.md", "docs/sub/a.md", false}, // * does not cross /
		{"docs/**/*.md", "docs/a.md", true},   // ** matches zero segments
		{"docs/**/*.md", "docs/x/y/a.md", true},
		{"docs/**/*.md", "other/a.md", false},
		{"**/*.md", "a.md", true},
		{"**/*.md", "a/b/c.md", true},
		{"**", "a/b/c", true},
		{"docs/**", "docs", true}, // ** after a literal matches the directory itself too
		{"docs/archive/**", "docs/archive/old.md", true},
		{"docs/archive/**", "docs/archived.md", false},
		{"a/**/b/**/c.md", "a/b/c.md", true},
		{"a/**/b/**/c.md", "a/x/b/y/z/c.md", true},
		{"a/**/b/**/c.md", "a/x/c.md", false},
		{"adr/[0-9]*.md", "adr/0001-x.md", true},
		{"adr/[0-9]*.md", "adr/x.md", false},
		{"?.md", "a.md", true},
		{"?.md", "ab.md", false},
		{"*.md", ".hidden.md", true}, // path.Match: * matches a leading dot
		{"[", "[", false},            // invalid pattern matches nothing
		{"docs/*.md", "", false},
	}
	for _, tt := range tests {
		if got := Match(tt.pattern, tt.name); got != tt.want {
			t.Errorf("Match(%q, %q) = %v, want %v", tt.pattern, tt.name, got, tt.want)
		}
	}
}

func TestValid(t *testing.T) {
	tests := []struct {
		pattern string
		problem string // "" = valid
	}{
		{"docs/**/*.md", ""},
		{"README.md", ""},
		{"a/[0-9]*.md", ""},
		{"", "empty"},
		{"/etc/*.md", "absolute"},
		{"a//b.md", "empty path segment"},
		{"./a.md", `"." segment`},
		{"../a.md", `".." segment`},
		{"a/../b.md", `".." segment`},
		{"a/[.md", "not a valid pattern"},
		{`a\`, "not a valid pattern"},
	}
	for _, tt := range tests {
		err := Valid(tt.pattern)
		switch {
		case tt.problem == "" && err != nil:
			t.Errorf("Valid(%q) = %v, want nil", tt.pattern, err)
		case tt.problem != "" && (err == nil || !strings.Contains(err.Error(), tt.problem)):
			t.Errorf("Valid(%q) = %v, want an error containing %q", tt.pattern, err, tt.problem)
		}
	}
}

func TestBase(t *testing.T) {
	for pattern, want := range map[string]string{
		"docs/**/*.md": "docs",
		"README.md":    "",
		"a/b/c.md":     "a/b",
		"**/x.md":      "",
		"a/*/b/c.md":   "a",
		"a/[x]/c.md":   "a",
	} {
		if got := Base(pattern); got != want {
			t.Errorf("Base(%q) = %q, want %q", pattern, got, want)
		}
	}
}

func TestCouldMatchUnder(t *testing.T) {
	tests := []struct {
		pattern, dir string
		want         bool
	}{
		{"README.md", "", true},
		{"README.md", "docs", false},
		{"docs/*.md", "docs", true},
		{"docs/*.md", "docs/sub", false},
		{"docs/*.md", "src", false},
		{"docs/**/*.md", "docs/a/b", true},
		{"docs/**/*.md", "src/a", false},
		{"**/*.md", "a/b/c", true},
		{"a/**", "a", true},
		{"[", "", false},
	}
	for _, tt := range tests {
		if got := CouldMatchUnder(tt.pattern, tt.dir); got != tt.want {
			t.Errorf("CouldMatchUnder(%q, %q) = %v, want %v", tt.pattern, tt.dir, got, tt.want)
		}
	}
}

// FuzzMatch: Match never panics, a pattern with no "**" agrees with a segment-wise path.Match
// oracle, every path a valid pattern matches lies under Base, and a literal pattern matches
// itself.
func FuzzMatch(f *testing.F) {
	for _, s := range [][2]string{
		{"docs/**/*.md", "docs/a/b.md"}, {"**", "x"}, {"a/*", "a/b"}, {"[", "["},
		{"**/**/**/**/**/**/**/**/**/**/a", strings.Repeat("x/", 60) + "b"},
	} {
		f.Add(s[0], s[1])
	}
	f.Fuzz(func(t *testing.T, pattern, name string) {
		got := Match(pattern, name)
		if Valid(pattern) != nil {
			if got {
				t.Fatalf("invalid pattern %q matched %q", pattern, name)
			}
			return
		}
		if got {
			if b := Base(pattern); b != "" && name != b && !strings.HasPrefix(name, b+"/") {
				t.Fatalf("Match(%q, %q) but the name is not under Base %q", pattern, name, b)
			}
		}
		if !strings.Contains(pattern, "**") {
			ps, ns := strings.Split(pattern, "/"), strings.Split(name, "/")
			want := name != "" && len(ps) == len(ns)
			for i := 0; want && i < len(ps); i++ {
				m, _ := path.Match(ps[i], ns[i])
				want = m
			}
			if got != want {
				t.Fatalf("Match(%q, %q) = %v, segment-wise path.Match says %v", pattern, name, got, want)
			}
		}
		// CouldMatchUnder never prunes a directory that holds a match.
		if got {
			if i := strings.LastIndex(name, "/"); i > 0 && !CouldMatchUnder(pattern, name[:i]) {
				t.Fatalf("Match(%q, %q) but CouldMatchUnder(%q) is false", pattern, name, name[:i])
			}
		}
		if !strings.ContainsAny(pattern, `*?[\`) && !Match(pattern, pattern) {
			t.Fatalf("literal pattern %q does not match itself", pattern)
		}
	})
}
