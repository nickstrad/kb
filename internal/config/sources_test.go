package config

import (
	"strings"
	"testing"

	"github.com/nickstrad/kb/internal/errs"
)

// TestValidateSources is the [[sources]] decision table: one row per accept or refuse outcome of
// validateSources, each refusal naming the key and the problem.
func TestValidateSources(t *testing.T) {
	entries := func(p string) Source { return Source{Path: p, Layout: LayoutEntries} }
	files := func(g string, ex ...string) Source { return Source{Glob: g, Layout: LayoutFiles, Exclude: ex} }
	tests := []struct {
		name    string
		sources []Source
		want    string // "" = valid; else a substring of the message
	}{
		{"none", nil, ""},
		{"default entries", []Source{entries("data")}, ""},
		{"named store entries", []Source{entries("data-adr")}, ""},
		{"nested entries dir", []Source{entries("kb/data")}, ""},
		{"files with exclude", []Source{files("docs/**/*.md", "docs/archive/**")}, ""},
		{"entries and files", []Source{entries("data"), files("docs/*.md")}, ""},
		{"sibling entries dirs", []Source{entries("data"), entries("data-adr")}, ""},
		{"two different globs", []Source{files("docs/*.md"), files("adr/*.md")}, ""},

		{"layout missing", []Source{{Path: "data"}}, `sources[1].layout: must be "entries" or "files" (got "")`},
		{"layout unknown", []Source{{Path: "data", Layout: "tree"}}, `sources[1].layout`},
		{"entries without path", []Source{{Layout: LayoutEntries}}, "sources[1].path: the entries layout needs a directory"},
		{"entries with glob", []Source{{Path: "data", Glob: "*.md", Layout: LayoutEntries}}, "sources[1].glob"},
		{"entries with exclude", []Source{{Path: "data", Layout: LayoutEntries, Exclude: []string{"x"}}}, "sources[1].exclude: only the files layout"},
		{"entries absolute", []Source{entries("/data")}, "is absolute"},
		{"entries dot", []Source{entries(".")}, "clean directory path"},
		{"entries parent", []Source{entries("../data")}, "clean directory path"},
		{"entries unclean", []Source{entries("data/")}, "clean directory path"},
		{"entries inside .kb", []Source{entries(".kb/stores")}, "inside .kb"},
		{"entries .git", []Source{entries(".git")}, "inside .git"},
		{"entries node_modules", []Source{entries("node_modules/x")}, "inside node_modules"},
		{"entries control char", []Source{entries("da\x1bta")}, "control character"},
		{"files without glob", []Source{{Layout: LayoutFiles}}, "sources[1].glob: the files layout needs a glob"},
		{"files with path", []Source{{Path: "docs", Glob: "*.md", Layout: LayoutFiles}}, "sources[1].path"},
		{"files bad glob", []Source{files("docs/[.md")}, "sources[1].glob"},
		{"files absolute glob", []Source{files("/etc/*.md")}, "is absolute"},
		{"files escaping glob", []Source{files("../*.md")}, `".." segment`},
		{"files bad exclude", []Source{files("*.md", "a/../b")}, "sources[1].exclude"},
		{"files control char", []Source{files("a\n.md")}, "control character"},
		{"second source is named", []Source{entries("data"), {Layout: "x"}}, "sources[2].layout"},
		{"same entries dir twice", []Source{entries("data"), entries("data")}, "sources[2].path: overlaps sources[1]"},
		{"nested entries dirs", []Source{entries("data"), entries("data/sub")}, "sources[2].path: overlaps sources[1]"},
		{"nested entries dirs, inner first", []Source{entries("data/sub"), entries("data")}, "overlaps"},
		{"same glob twice", []Source{files("*.md"), files("*.md")}, "sources[2].glob: repeats sources[1]"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			c := Default()
			c.Sources = tt.sources
			err := c.Validate("cfg.toml")
			if tt.want == "" {
				if err != nil {
					t.Fatalf("Validate = %v, want nil", err)
				}
				return
			}
			if err == nil {
				t.Fatalf("Validate accepted %+v", tt.sources)
			}
			if errs.KindOf(err) != errs.Usage || !strings.Contains(err.Error(), tt.want) || !strings.HasPrefix(err.Error(), "cfg.toml: ") {
				t.Errorf("Validate = %v (kind %v), want Usage naming cfg.toml and containing %q", err, errs.KindOf(err), tt.want)
			}
		})
	}
}

// TestParseSources: a [[sources]] table written the way kb init writes it parses, and an
// unknown key inside one is refused like any other unknown key.
func TestParseSources(t *testing.T) {
	c, err := Parse([]byte("[[sources]]\npath = \"data\"\nlayout = \"entries\"\n\n[[sources]]\nglob = \"docs/**/*.md\"\nlayout = \"files\"\nexclude = [\"docs/archive/**\"]\n"), "c.toml")
	if err != nil {
		t.Fatal(err)
	}
	want := []Source{{Path: "data", Layout: LayoutEntries, Exclude: []string{}}, {Glob: "docs/**/*.md", Layout: LayoutFiles, Exclude: []string{"docs/archive/**"}}}
	if len(c.Sources) != 2 || c.Sources[0].Path != want[0].Path || c.Sources[1].Glob != want[1].Glob || c.Sources[1].Exclude[0] != "docs/archive/**" {
		t.Fatalf("Sources = %+v, want %+v", c.Sources, want)
	}
	if _, err := Parse([]byte("[[sources]]\npath = \"data\"\nlayout = \"entries\"\nrecursive = true\n"), "c.toml"); err == nil || !strings.Contains(err.Error(), `"sources.recursive"`) {
		t.Errorf("unknown source key: %v", err)
	}
}

func TestSourceString(t *testing.T) {
	if got := (Source{Path: "data", Layout: LayoutEntries}).String(); got != `"data" (entries)` {
		t.Errorf("entries: %s", got)
	}
	if got := (Source{Glob: "*.md", Layout: LayoutFiles}).String(); got != `"*.md" (files)` {
		t.Errorf("files: %s", got)
	}
}
