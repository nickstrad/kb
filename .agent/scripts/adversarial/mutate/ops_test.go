package main

import (
	"go/ast"
	"go/format"
	"go/parser"
	"go/printer"
	"go/token"
	"regexp"
	"strings"
	"testing"
)

const fixture = `package p

const Limit = 10

func cmp(a, b int) bool {
	if a < b && b >= 0 {
		return true
	}
	return a == b
}

func sum(xs []int) (n int, ok bool) {
	for _, x := range xs {
		n += x
		n++
	}
	return n - 1, n > 0
}

func load() error {
	err := do()
	if err != nil {
		return err
	}
	s := "a" + "b"
	_ = s
	return nil
}

func do() error { return nil }
`

// mutants parses the fixture and returns its mutations for ops, plus the file to print.
func mutants(t *testing.T, ops string, funcRe string) (*token.FileSet, *ast.File, []*mutation) {
	t.Helper()
	fset := token.NewFileSet()
	f, err := parser.ParseFile(fset, "p.go", fixture, parser.ParseComments)
	if err != nil {
		t.Fatal(err)
	}
	set, err := parseOps(ops)
	if err != nil {
		t.Fatal(err)
	}
	var re *regexp.Regexp
	if funcRe != "" {
		re = regexp.MustCompile(funcRe)
	}
	return fset, f, enumerate(fset, f, set, re, nil)
}

// render applies m, prints the file gofmt-style, undoes m and checks the undo restored the source.
func render(t *testing.T, fset *token.FileSet, f *ast.File, m *mutation) string {
	t.Helper()
	print := func() string {
		var b strings.Builder
		if err := printer.Fprint(&b, fset, f); err != nil {
			t.Fatal(err)
		}
		out, err := format.Source([]byte(b.String()))
		if err != nil {
			t.Fatalf("mutant does not parse: %v\n%s", err, b.String())
		}
		return string(out)
	}
	orig := print()
	m.apply()
	got := print()
	m.undo()
	if print() != orig {
		t.Fatalf("%s at %v: undo did not restore the source", m.Op, m.Pos)
	}
	return got
}

func TestOperatorRewrites(t *testing.T) {
	tests := []struct {
		name, op, before, after string
		line                    int
		want                    string // a line the mutant must contain
	}{
		{"less to less-equal", opBoundary, "<", "<=", 6, "if a <= b && b >= 0 {"},
		{"greater-equal to greater", opBoundary, ">=", ">", 6, "if a < b && b > 0 {"},
		{"greater to greater-equal", opBoundary, ">", ">=", 17, "return n - 1, n >= 0"},
		{"equal to not-equal", opNegRel, "==", "!=", 9, "return a != b"},
		{"err check flips via negate-rel", opNegRel, "!=", "==", 22, "if err == nil {"},
		{"and to or", opLogical, "&&", "||", 6, "if a < b || b >= 0 {"},
		{"negate if", opIfNeg, "a < b && b >= 0", "!(a < b && b >= 0)", 6, "if !(a < b && b >= 0) {"},
		{"const plus one", opConst, "10", "11", 3, "const Limit = 11"},
		{"const minus one", opConst, "10", "9", 3, "const Limit = 9"},
		{"minus to plus", opArith, "-", "+", 17, "return n + 1, n > 0"},
		{"plus-assign to minus-assign", opArith, "+=", "-=", 14, "n -= x"},
		{"increment to decrement", opArith, "++", "--", 15, "n--"},
		{"drop error check", opErrCheck, "err != nil", "false", 22, "if false {"},
		{"negate bool literal return", opBoolRet, "true", "false", 7, "return false"},
		{"negate bool expression return", opBoolRet, "a == b", "!(a == b)", 9, "return !(a == b)"},
		{"negate bool in second result", opBoolRet, "n > 0", "!(n > 0)", 17, "return n - 1, !(n > 0)"},
		{"drop loop statement", opLoopDrop, "n += x", "(removed)", 14, ""},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			fset, f, ms := mutants(t, tt.op, "")
			var found *mutation
			for _, m := range ms {
				if m.Pos.Line == tt.line && m.Before == tt.before && m.After == tt.after {
					found = m
				}
			}
			if found == nil {
				var got []string
				for _, m := range ms {
					got = append(got, m.Pos.String()+" "+m.Before+" -> "+m.After)
				}
				t.Fatalf("no %s mutant %q -> %q on line %d; have:\n%s", tt.op, tt.before, tt.after, tt.line, strings.Join(got, "\n"))
			}
			out := render(t, fset, f, found)
			if tt.want != "" && !strings.Contains(out, tt.want) {
				t.Fatalf("mutant lacks %q:\n%s", tt.want, out)
			}
			if tt.op == opLoopDrop && (strings.Contains(out, "n += x") || !strings.Contains(out, "n++")) {
				t.Fatalf("drop removed the wrong statement:\n%s", out)
			}
		})
	}
}

func TestOperatorExclusions(t *testing.T) {
	_, _, ms := mutants(t, "all", "")
	for _, m := range ms {
		if m.Op == opArith && m.Pos.Line == 25 {
			t.Errorf("string concatenation mutated: %s -> %s", m.Before, m.After)
		}
		if m.Op == opLoopDrop && strings.Contains(m.Before, ":=") {
			t.Errorf("loop-drop removed a declaration: %s", m.Before)
		}
		if m.Op == opBoolRet && m.Func == "load" {
			t.Errorf("bool-return applied to a non-bool result in %s", m.Func)
		}
	}
}

func TestFuncFilter(t *testing.T) {
	_, _, ms := mutants(t, "all", "^sum$")
	if len(ms) == 0 {
		t.Fatal("no mutants in sum")
	}
	for _, m := range ms {
		if m.Func != "sum" {
			t.Errorf("mutant outside sum: %s at %v", m.Func, m.Pos)
		}
	}
}

func TestParseOps(t *testing.T) {
	if s, err := parseOps("all"); err != nil || len(s) != len(allOps) {
		t.Fatalf("all = %v, %v", s, err)
	}
	if _, err := parseOps("boundary,nope"); err == nil {
		t.Fatal("unknown operator accepted")
	}
}

func TestSampleKeepsSpread(t *testing.T) {
	var all []result
	for i := range 10 {
		all = append(all, result{Line: i})
	}
	got := sample(all, 5)
	if len(got) != 5 || got[0].Line != 0 || got[4].Line != 8 {
		t.Fatalf("sample = %v", got)
	}
	if len(sample(all, 0)) != 10 {
		t.Fatal("max 0 should keep all")
	}
}

func TestInsideBox(t *testing.T) {
	box := "/r/.agent/sandbox/x"
	if insideBox(box, box+"/internal/a.go") != nil {
		t.Fatal("path inside rejected")
	}
	for _, p := range []string{"/r/internal/a.go", box + "/../y/a.go", box + "x/a.go"} {
		if insideBox(box, p) == nil {
			t.Errorf("%s accepted", p)
		}
	}
}

// TestPickMutantUsesSameFilter is the regression test for -diff mode: the sandbox used to
// re-enumerate without the line filter, so a filtered index named a different mutant and every
// -diff result was mislabelled. pickMutant now refuses a mismatch, and the filtered re-enumeration
// finds the mutant it was asked for.
func TestPickMutantUsesSameFilter(t *testing.T) {
	fset := token.NewFileSet()
	f, err := parser.ParseFile(fset, "p.go", fixture, parser.ParseComments)
	if err != nil {
		t.Fatal(err)
	}
	set, _ := parseOps("all")
	lines := map[int]bool{17: true}
	filtered := enumerate(fset, f, set, nil, lines)
	unfiltered := enumerate(fset, f, set, nil, nil)
	if len(filtered) == 0 || len(filtered) == len(unfiltered) {
		t.Fatalf("fixture must have mutants on line 17 and elsewhere: %d filtered, %d total", len(filtered), len(unfiltered))
	}
	for i, m := range filtered {
		r := &result{File: "p.go", Line: m.Pos.Line, Op: m.Op, Before: m.Before, After: m.After, fileIdx: i}
		if got, err := pickMutant(filtered, r); err != nil || got != m {
			t.Errorf("filtered #%d: got %v, %v; want the same mutant", i, got, err)
		}
	}
	r := &result{File: "p.go", Line: filtered[0].Pos.Line, Op: filtered[0].Op, Before: filtered[0].Before, After: filtered[0].After, fileIdx: 0}
	if unfiltered[0].Pos.Line == 17 {
		t.Skip("fixture's first mutant is on line 17; the mismatch case needs another line")
	}
	if _, err := pickMutant(unfiltered, r); err == nil {
		t.Error("pickMutant accepted a mutant from an enumeration with a different filter")
	}
}
