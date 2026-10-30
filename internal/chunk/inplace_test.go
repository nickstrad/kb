package chunk

import (
	"errors"
	"strings"
	"testing"

	"github.com/nickstrad/kb/internal/config"
	"github.com/nickstrad/kb/internal/entry"
)

func inPlace(title, summary string) entry.Entry {
	return entry.Entry{Path: "docs/x.md", Kind: entry.KindFile, Files: []string{"docs/x.md"}, Layout: config.LayoutFiles,
		Meta: entry.FrontMatter{Title: title, Summary: summary}}
}

// TestSplitInPlace is the decision table for the in-place (files layout) rules of Split: the
// summary chunk with and without a summary, the leading H1 dropped only when it repeats the
// title, and the entries layout still requiring a summary.
func TestSplitInPlace(t *testing.T) {
	tests := []struct {
		name     string
		e        entry.Entry
		body     string
		chunk0   string
		intro    string // text of chunk 1, "" = no chunk 1
		validErr bool
	}{
		{"no summary: chunk 0 is the title", inPlace("Install", ""), "# Install\n\nSteps here.\n", "Install", "Install › (intro)\nSteps here.", false},
		{"summary: title: summary", inPlace("Install", "How to."), "# Install\n\nSteps here.\n", "Install: How to.", "Install › (intro)\nSteps here.", false},
		{"H1 differs from title: kept", inPlace("From FM", ""), "# Other\n\nText.\n", "From FM", "From FM › (intro)\n# Other\n\nText.", false},
		{"H1 matches case-insensitively", inPlace("install", ""), "# INSTALL\nText.\n", "install", "install › (intro)\nText.", false},
		{"only an H1: no intro chunk", inPlace("T", ""), "# T\n", "T", "", false},
		{"entries layout still needs a summary", entry.Entry{Path: "data/x.md", Kind: entry.KindFile, Files: []string{"data/x.md"}, Layout: config.LayoutEntries,
			Meta: entry.FrontMatter{Title: "T"}}, "# T\n", "", "", true},
		{"in place still needs a title", inPlace("", "S"), "x\n", "", "", true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			chunks, err := Split(tt.e, map[string][]byte{tt.e.Path: []byte(tt.body)})
			if tt.validErr {
				var ve *entry.ValidationError
				if !errors.As(err, &ve) {
					t.Fatalf("err = %v, want a *ValidationError", err)
				}
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			if chunks[0].Text != tt.chunk0 || chunks[0].Heading != "" {
				t.Errorf("chunk 0 = %q (heading %q), want %q", chunks[0].Text, chunks[0].Heading, tt.chunk0)
			}
			switch {
			case tt.intro == "" && len(chunks) != 1:
				t.Errorf("got %d chunks, want only chunk 0: %+v", len(chunks), chunks)
			case tt.intro != "" && (len(chunks) < 2 || chunks[1].Text != tt.intro):
				t.Errorf("chunks = %+v, want chunk 1 %q", chunks, tt.intro)
			}
		})
	}
}

// FuzzSplitInPlace: for an in-place entry with no summary, chunk 0 is the title alone and every
// later chunk starts with the "title › " context line; Split never panics or rejects the entry
// for its missing summary.
func FuzzSplitInPlace(f *testing.F) {
	for _, s := range []string{"# T\n\nbody\n", "", "# T\n## A\n\nx\n", "---\ntitle: x\n", "```\n# T\n```\n"} {
		f.Add(s)
	}
	e := inPlace("T", "")
	f.Fuzz(func(t *testing.T, body string) {
		chunks, err := Split(e, map[string][]byte{e.Path: []byte(body)})
		if err != nil {
			var ve *entry.ValidationError
			if errors.As(err, &ve) {
				t.Fatalf("in-place entry rejected: %v", err)
			}
			return // an unclosed front-matter fence: a structural error, as for any entry
		}
		if chunks[0].Text != "T" {
			t.Fatalf("chunk 0 = %q, want the title alone", chunks[0].Text)
		}
		for _, c := range chunks[1:] {
			if !strings.HasPrefix(c.Text, "T › ") {
				t.Fatalf("chunk %d lacks the context line: %q", c.Ord, c.Text)
			}
		}
	})
}
