package entry

import (
	"errors"
	"reflect"
	"strings"
	"testing"
)

func TestParseFrontMatterLists(t *testing.T) {
	const head = "---\ntitle: T\nsummary: S\n"
	for _, tc := range []struct {
		name string
		fm   string
		get  func(FrontMatter) []string
		want []string
	}{
		{"related inline", "related: [a.md, \"b c.md\", 'd.md']\n", func(f FrontMatter) []string { return f.Related }, []string{"a.md", "b c.md", "d.md"}},
		{"related block", "related:\n  - a.md\n  - \"[[b]]\"\n", func(f FrontMatter) []string { return f.Related }, []string{"a.md", "[[b]]"}},
		{"supersedes inline", "supersedes: [old.md]\n", func(f FrontMatter) []string { return f.Supersedes }, []string{"old.md"}},
		{"depends_on block", "depends_on:\n- x.md\n- y.md\n", func(f FrontMatter) []string { return f.DependsOn }, []string{"x.md", "y.md"}},
		{"aliases inline", "aliases: [Dock, docker setup]\n", func(f FrontMatter) []string { return f.Aliases }, []string{"Dock", "docker setup"}},
		{"aliases block quoted comma", "aliases:\n  - \"a, b\"\n", func(f FrontMatter) []string { return f.Aliases }, []string{"a, b"}},
		{"empty inline", "related: []\n", func(f FrontMatter) []string { return f.Related }, nil},
		{"inline drops empty items", "related: [a.md, , \"\"]\n", func(f FrontMatter) []string { return f.Related }, []string{"a.md"}},
		{"block drops empty items", "aliases:\n  - \"\"\n  - a\n", func(f FrontMatter) []string { return f.Aliases }, []string{"a"}},
		{"tags block keeps empty items", "tags:\n  - \"\"\n  - a\n", func(f FrontMatter) []string { return f.Tags }, []string{"", "a"}},
		{"repeated key replaces inline", "related: [a.md]\nrelated: [b.md]\n", func(f FrontMatter) []string { return f.Related }, []string{"b.md"}},
		{"block items after another key go to that key", "related:\n  - a.md\naliases:\n  - z\n", func(f FrontMatter) []string { return f.Related }, []string{"a.md"}},
		{"block items under scalar key are ignored", "updated: x\n  - a.md\n", func(f FrontMatter) []string { return f.Related }, nil},
		{"unknown key list is ignored", "other:\n  - a.md\n", func(f FrontMatter) []string { return f.Related }, nil},
		{"absent", "", func(f FrontMatter) []string { return f.Aliases }, nil},
	} {
		t.Run(tc.name, func(t *testing.T) {
			fm, _, err := ParseFrontMatter([]byte(head + tc.fm + "---\nbody"))
			if err != nil {
				t.Fatal(err)
			}
			if got := tc.get(fm); !reflect.DeepEqual(got, tc.want) {
				t.Fatalf("got %#v, want %#v", got, tc.want)
			}
		})
	}
}

func TestAliasLimits(t *testing.T) {
	const head = "---\ntitle: T\nsummary: S\n"
	many := func(n int) string { return "aliases: [" + strings.TrimSuffix(strings.Repeat("a, ", n), ", ") + "]\n" }
	for _, tc := range []struct {
		name    string
		fm      string
		invalid bool
	}{
		{"at MaxAliases", many(MaxAliases), false},
		{"MaxAliases+1", many(MaxAliases + 1), true},
		{"at MaxAliasRunes", "aliases: [" + strings.Repeat("é", MaxAliasRunes) + "]\n", false},
		{"MaxAliasRunes+1", "aliases: [" + strings.Repeat("é", MaxAliasRunes+1) + "]\n", true},
		{"control char", "aliases:\n  - \"a\x01b\"\n", true},
		{"tab", "aliases:\n  - \"a\tb\"\n", true},
		{"invalid UTF-8", "aliases: [a\xffb]\n", true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			_, _, err := ParseFrontMatter([]byte(head + tc.fm + "---\n"))
			var ve *ValidationError
			if got := errors.As(err, &ve); got != tc.invalid || (err != nil) != tc.invalid {
				t.Fatalf("err = %v, want invalid=%v", err, tc.invalid)
			}
		})
	}
}

// TestParseFrontMatterBlockStructure pins two structure rules the W4 mutation pass found
// unpinned: blank and comment lines between keys are skipped (they must not read as a
// continuation of title or summary), and an empty "---\n---" block is closed, so it is a
// validation error (no title), not an unclosed fence.
func TestParseFrontMatterBlockStructure(t *testing.T) {
	for _, tc := range []struct {
		name       string
		src        string
		validation bool // want *ValidationError; false means nil error
	}{
		{"blank line after title", "---\ntitle: T\n\nsummary: S\n---\n", false},
		{"comment line after summary", "---\ntitle: T\nsummary: S\n# note\n---\n", false},
		{"empty block is closed", "---\n---\nbody\n", true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			_, _, err := ParseFrontMatter([]byte(tc.src))
			var ve *ValidationError
			if got := errors.As(err, &ve); got != tc.validation || (err != nil) != tc.validation {
				t.Fatalf("err = %v, want validation error %v", err, tc.validation)
			}
		})
	}
}
