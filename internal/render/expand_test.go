package render

import (
	"fmt"
	"math/rand"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
)

// alpha is one entry's chunks: chunk 0, a lede, a section stored as two pieces (a long section is
// split, each piece repeating the context line), and a second file's section of the same name.
func alpha() []Chunk {
	return []Chunk{
		{SourceFile: "alpha.md", Heading: "", Text: "Alpha: tmux notes"},
		{SourceFile: "alpha.md", Heading: "(intro)", Text: "Alpha › (intro)\nTmux keeps sessions alive."},
		{SourceFile: "alpha.md", Heading: "Logs", Text: "Alpha › Logs\nRotate with logrotate."},
		{SourceFile: "alpha.md", Heading: "Logs", Text: "Alpha › Logs\nSet copytruncate."},
		{SourceFile: "alpha.md", Heading: "Panes", Text: "Alpha › Panes\nSplit with %."},
		{SourceFile: "docs/more.md", Heading: "Logs", Text: "Alpha › More › Logs\nOther file."},
	}
}

func TestStripContext(t *testing.T) {
	for _, tc := range []struct{ name, heading, text, want string }{
		{"context line", "Logs", "A › Logs\nbody", "body"},
		{"intro", "(intro)", "A › (intro)\nlede", "lede"},
		{"chunk 0 has no heading", "", "A: summary", "A: summary"},
		{"first line is not the context line", "Logs", "plain first line\nbody", "plain first line\nbody"},
		{"heading only as a prefix", "Logs", "A › Logs and more\nbody", "A › Logs and more\nbody"},
		{"context line alone", "Logs", "A › Logs", "A › Logs"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := StripContext(tc.heading, tc.text); got != tc.want {
				t.Errorf("StripContext = %q, want %q", got, tc.want)
			}
		})
	}
}

func TestSection(t *testing.T) {
	cs := alpha()
	for _, tc := range []struct {
		name string
		at   int
		want string
	}{
		{"first piece gets both pieces", 2, "Rotate with logrotate.\n\nSet copytruncate."},
		{"second piece gets both pieces", 3, "Rotate with logrotate.\n\nSet copytruncate."},
		{"single-chunk section", 4, "Split with %."},
		{"same heading in another file is another section", 5, "Other file."},
		{"chunk 0 stays itself", 0, "Alpha: tmux notes"},
		{"lede", 1, "Tmux keeps sessions alive."},
		{"below range", -1, ""},
		{"above range", 6, ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := Section(cs, tc.at); got != tc.want {
				t.Errorf("Section(%d) = %q, want %q", tc.at, got, tc.want)
			}
		})
	}
	if Section(nil, 0) != "" {
		t.Error("Section of no chunks is not empty")
	}
}

func TestEntry(t *testing.T) {
	want := "Alpha: tmux notes\n\nTmux keeps sessions alive.\n\n## Logs\nRotate with logrotate.\n\nSet copytruncate.\n\n## Panes\nSplit with %.\n\n## Logs\nOther file."
	if got := Entry(alpha()); got != want {
		t.Errorf("Entry =\n%q\nwant\n%q", got, want)
	}
	if got := Entry(nil); got != "" {
		t.Errorf("Entry(nil) = %q", got)
	}
	if got := Entry(alpha()[:1]); got != "Alpha: tmux notes" {
		t.Errorf("Entry(chunk 0 only) = %q", got)
	}
}

// TestDedupeTable: hits sharing a key merge into the first, which lists the absorbed ranks;
// empty keys never merge.
func TestDedupeTable(t *testing.T) {
	h := func(r int) Hit { return Hit{Rank: r, Path: fmt.Sprint("p", r)} }
	for _, tc := range []struct {
		name  string
		keys  []string
		ranks []int
		also  map[int][]int
	}{
		{"all distinct", []string{"a", "b", "c"}, []int{1, 2, 3}, nil},
		{"no keys never merge", []string{"", "", ""}, []int{1, 2, 3}, nil},
		{"two share, the best rank stays", []string{"a", "b", "a"}, []int{1, 2}, map[int][]int{1: {3}}},
		{"three share", []string{"a", "a", "a"}, []int{1}, map[int][]int{1: {2, 3}}},
		{"empty key between shared keys", []string{"a", "", "a"}, []int{1, 2}, map[int][]int{1: {3}}},
		{"two groups", []string{"a", "b", "b", "a"}, []int{1, 2}, map[int][]int{1: {4}, 2: {3}}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var hits []Hit
			for i := range tc.keys {
				hits = append(hits, h(i+1))
			}
			got := Dedupe(hits, tc.keys)
			var ranks []int
			for _, g := range got {
				ranks = append(ranks, g.Rank)
				if !reflect.DeepEqual(g.Also, tc.also[g.Rank]) {
					t.Errorf("rank %d Also = %v, want %v", g.Rank, g.Also, tc.also[g.Rank])
				}
			}
			if !reflect.DeepEqual(ranks, tc.ranks) {
				t.Errorf("ranks = %v, want %v", ranks, tc.ranks)
			}
		})
	}
}

func TestHeaderAlso(t *testing.T) {
	h := Hit{Rank: 1, Path: "a.md", Heading: "H", Also: []int{3, 5}}
	if got, want := header(h), "[1] a.md › H · also [3] [5]"; got != want {
		t.Errorf("header = %q, want %q", got, want)
	}
}

// expandedFixture is two hits of one entry (alpha, ranks 1 and 3) and one other (rank 2), with
// the expansion the CLI would attach for mode.
func expandedFixture(mode string) []Hit {
	cs := alpha()
	hits := []Hit{
		{Rank: 1, Path: "data/alpha.md", Heading: "Logs", Updated: "2026-03-01", Summary: "Alpha tmux notes.", Text: cs[2].Text},
		{Rank: 2, Store: "ops", Path: "data/beta.md", Heading: "Run", Summary: "Beta runbook.", Text: "Beta › Run\nRestart the service."},
		{Rank: 3, Path: "data/alpha.md", Heading: "Panes", Updated: "2026-03-01", Summary: "Alpha tmux notes.", Text: cs[4].Text},
	}
	var keys []string
	switch mode {
	case ExpandSection:
		hits[0].Expanded = Section(cs, 2)
		hits[2].Expanded = Section(cs, 4)
		keys = []string{"alpha/Logs", "", "alpha/Panes"}
	case ExpandEntry:
		hits[0].Expanded = Entry(cs)
		hits[2].Expanded = Entry(cs)
		keys = []string{"alpha", "", "alpha"}
	default:
		return hits
	}
	return Dedupe(hits, keys)
}

func TestRenderExpandGolden(t *testing.T) {
	for _, mode := range []string{ExpandNone, ExpandSection, ExpandEntry} {
		for _, b := range []int{0, 12, 30, 60, 150} {
			t.Run(fmt.Sprintf("%s_%d", mode, b), func(t *testing.T) {
				res := Render(expandedFixture(mode), b)
				got := fmt.Sprintf("levels=%v tokens=%d\n%s\n", res.Levels, res.Tokens, res.Text)
				path := filepath.Join("testdata", fmt.Sprintf("expand_%s_%d.golden", mode, b))
				if *update {
					if err := os.WriteFile(path, []byte(got), 0o644); err != nil {
						t.Fatal(err)
					}
				}
				want, err := os.ReadFile(path)
				if err != nil {
					t.Fatalf("read golden (run with -update): %v", err)
				}
				if got != string(want) {
					t.Errorf("output changed:\n--- got\n%s--- want\n%s", got, want)
				}
			})
		}
	}
}

// TestRenderExpandBoundaries: the exact-fit budget shows everything at full text with no tail;
// one token less condenses something and still fits; the output never exceeds the budget.
func TestRenderExpandBoundaries(t *testing.T) {
	for _, mode := range []string{ExpandSection, ExpandEntry} {
		t.Run(mode, func(t *testing.T) {
			hits := expandedFixture(mode)
			full := Render(hits, 0)
			if strings.Contains(full.Text, "raise --budget") {
				t.Fatalf("unlimited output has a tail:\n%s", full.Text)
			}
			exact := Render(hits, full.Tokens)
			if exact.Text != full.Text || exact.Tokens > full.Tokens {
				t.Errorf("budget %d (exact fit) differs from unlimited:\n%s", full.Tokens, exact.Text)
			}
			under := Render(hits, full.Tokens-1)
			if under.Tokens > full.Tokens-1 || !strings.Contains(under.Text, "raise --budget") {
				t.Errorf("budget %d: %d tokens, text %q; want a condensed render within budget", full.Tokens-1, under.Tokens, under.Text)
			}
			for b := 1; b <= full.Tokens; b++ {
				if got := Render(hits, b).Tokens; got > b {
					t.Errorf("budget %d: %d tokens", b, got)
				}
			}
		})
	}
}

// TestRenderExpandProperty: with an unlimited budget the expanded output contains each hit's
// whole (untrimmed) text, which the unexpanded output may cut at TextLimit; with a budget it
// stays within it.
func TestRenderExpandProperty(t *testing.T) {
	r := rand.New(rand.NewSource(7))
	for i := 0; i < 300; i++ {
		n := 1 + r.Intn(5)
		hits := make([]Hit, n)
		for j := range hits {
			body := strings.Repeat("word ", r.Intn(300))
			hits[j] = Hit{Rank: j + 1, Path: fmt.Sprint("data/e", j, ".md"), Heading: "H", Summary: "sum", Text: "short " + body}
			if r.Intn(2) == 0 {
				hits[j].Expanded = hits[j].Text + "\n\nmore " + strings.Repeat("tail ", r.Intn(300))
			}
		}
		un := Render(hits, 0).Text
		for _, h := range hits {
			if h.Expanded != "" && !strings.Contains(un, h.Expanded) {
				t.Fatalf("iteration %d: unlimited output lacks the expanded text of rank %d", i, h.Rank)
			}
			if h.Expanded != "" && !strings.Contains(un, Trim(h.Text, TextLimit)[:5]) {
				t.Fatalf("iteration %d: output lacks the hit text", i)
			}
		}
		b := 1 + r.Intn(2000)
		if got := Render(hits, b).Tokens; got > b {
			t.Fatalf("iteration %d: %d tokens over budget %d", i, got, b)
		}
	}
}
