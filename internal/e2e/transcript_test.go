package e2e

import (
	"reflect"
	"strings"
	"testing"
)

func TestParse(t *testing.T) {
	for _, tc := range []struct {
		name string
		in   string
		want *Transcript
		err  string
	}{
		{"empty", "", &Transcript{}, ""},
		{"prose only", "hello\n\nworld\n", &Transcript{Blocks: []Block{{Prose: []string{"hello", "", "world"}}}}, ""},
		{"command with output", "intro\n\n  $ kb version\n  kb v1\n  [exit 2]\n", &Transcript{Blocks: []Block{
			{Prose: []string{"intro", ""}},
			{IsCmd: true, Cmd: "kb version", Out: []string{"kb v1", "[exit 2]"}},
		}}, ""},
		{"blank line inside output", "  $ kb a\n  one\n\n  two\n\nprose\n", &Transcript{Blocks: []Block{
			{IsCmd: true, Cmd: "kb a", Out: []string{"one", "", "two"}},
			{Prose: []string{"", "prose"}},
		}}, ""},
		{"two-space blank line ends output", "  $ kb a\n  one\n  \n\nprose\n", &Transcript{Blocks: []Block{
			{IsCmd: true, Cmd: "kb a", Out: []string{"one", ""}},
			{Prose: []string{"", "prose"}},
		}}, ""},
		{"elision lines stay as written", "  $ kb a\n  ...\n  ~ x[0-9]+\n  [stderr] ~ w.*\n", &Transcript{Blocks: []Block{
			{IsCmd: true, Cmd: "kb a", Out: []string{"...", "~ x[0-9]+", "[stderr] ~ w.*"}},
		}}, ""},
		{"commands back to back", "  $ cd x\n  $ kb b\n  out\n", &Transcript{Blocks: []Block{
			{IsCmd: true, Cmd: "cd x"}, {IsCmd: true, Cmd: "kb b", Out: []string{"out"}},
		}}, ""},
		{"file sections", "p\n-- a/b.md --\nline1\n\nline2\n-- c.txt --\n-- @d --\nx\n", &Transcript{
			Blocks: []Block{{Prose: []string{"p"}}},
			Files:  []File{{Path: "a/b.md", Lines: []string{"line1", "", "line2"}}, {Path: "c.txt"}, {Path: "@d", Lines: []string{"x"}}},
		}, ""},
		{"indented line with no command", "  stray\n", nil, "line 1"},
		{"odd indentation", "x\n \ty\n", nil, "line 2"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got, err := Parse(tc.in)
			if tc.err != "" {
				if err == nil || !strings.Contains(err.Error(), tc.err) {
					t.Fatalf("err = %v, want it to contain %q", err, tc.err)
				}
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			if !reflect.DeepEqual(got, tc.want) {
				t.Errorf("Parse =\n%#v\nwant\n%#v", got, tc.want)
			}
			back, err := Parse(Format(got))
			if err != nil || !reflect.DeepEqual(back, got) {
				t.Errorf("round trip changed it: %v\n%#v", err, back)
			}
		})
	}
}

func TestFormatIsCanonical(t *testing.T) {
	in := "  $ kb a\n  one\n\n  two\n  \n\nprose\n-- f --\nx\n"
	tr, err := Parse(in)
	if err != nil {
		t.Fatal(err)
	}
	if got := Format(tr); got != in {
		t.Errorf("Format(Parse(x)) = %q, want %q", got, in)
	}
}

func TestSection(t *testing.T) {
	tr, _ := Parse("-- a --\nx\ny\n-- b --\n")
	if s, ok := tr.Section("a"); !ok || s != "x\ny\n" {
		t.Errorf("a = %q, %v", s, ok)
	}
	if s, ok := tr.Section("b"); !ok || s != "" {
		t.Errorf("b = %q, %v", s, ok)
	}
	if _, ok := tr.Section("c"); ok {
		t.Error("c found")
	}
}

func TestMatch(t *testing.T) {
	for _, tc := range []struct {
		name     string
		exp, act []string
		want     bool
	}{
		{"equal", []string{"a", "b"}, []string{"a", "b"}, true},
		{"both empty", nil, nil, true},
		{"missing line", []string{"a", "b"}, []string{"a"}, false},
		{"extra line", []string{"a"}, []string{"a", "b"}, false},
		{"different line", []string{"a"}, []string{"b"}, false},
		{"ellipsis in the middle", []string{"a", "...", "z"}, []string{"a", "x", "y", "z"}, true},
		{"ellipsis matches none", []string{"a", "...", "z"}, []string{"a", "z"}, true},
		{"ellipsis alone matches empty", []string{"..."}, nil, true},
		{"ellipsis at the end", []string{"a", "..."}, []string{"a", "b", "c"}, true},
		{"ellipsis does not skip the anchor", []string{"a", "...", "z"}, []string{"a", "x"}, false},
		{"two ellipses", []string{"...", "m", "..."}, []string{"l", "m", "n"}, true},
		{"regexp line", []string{"id ~ x"}, []string{"id ~ x"}, true}, // not a pattern: no leading "~ "
		{"regexp match", []string{"~ n=[0-9]+"}, []string{"n=42"}, true},
		{"regexp is anchored", []string{"~ n=[0-9]"}, []string{"n=42"}, false},
		{"regexp alternation stays inside the anchor", []string{"~ a|b"}, []string{"ab"}, false},
		{"stderr regexp", []string{"[stderr] ~ warn.*"}, []string{"[stderr] warning: x"}, true},
		{"stderr regexp rejects stdout", []string{"[stderr] ~ warn.*"}, []string{"warning: x"}, false},
		{"bad regexp matches nothing", []string{"~ ("}, []string{"("}, false},
		{"literal tilde text", []string{"~x"}, []string{"~x"}, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := Match(tc.exp, tc.act); got != tc.want {
				t.Errorf("Match(%q, %q) = %v, want %v", tc.exp, tc.act, got, tc.want)
			}
		})
	}
}

func TestMerge(t *testing.T) {
	for _, tc := range []struct {
		name     string
		exp, act []string
		want     []string
	}{
		{"already matching keeps patterns", []string{"a", "...", "~ z+"}, []string{"a", "q", "zz"}, []string{"a", "...", "~ z+"}},
		{"changed literal line", []string{"a", "b", "c"}, []string{"a", "B", "c"}, []string{"a", "B", "c"}},
		{"still matching pattern kept next to a change", []string{"a", "~ n=[0-9]+", "c"}, []string{"A", "n=7", "c"}, []string{"A", "~ n=[0-9]+", "c"}},
		{"ellipsis kept in place", []string{"a", "...", "z"}, []string{"a", "x", "Z"}, []string{"a", "...", "x", "Z"}},
		{"added line", []string{"a"}, []string{"a", "b"}, []string{"a", "b"}},
		{"removed line", []string{"a", "b"}, []string{"a"}, []string{"a"}},
		{"to empty", []string{"a"}, nil, nil},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got := Merge(tc.exp, tc.act)
			if !reflect.DeepEqual(got, tc.want) && !(len(got) == 0 && len(tc.want) == 0) {
				t.Errorf("Merge = %q, want %q", got, tc.want)
			}
			if !Match(got, tc.act) {
				t.Errorf("Merge result %q does not match the actual %q", got, tc.act)
			}
		})
	}
}

func TestDiff(t *testing.T) {
	got := Diff([]string{"a", "b", "~ c+"}, []string{"a", "x", "cc"})
	want := "  a\n- b\n+ x\n  cc\n"
	if got != want {
		t.Errorf("Diff = %q, want %q", got, want)
	}
}
