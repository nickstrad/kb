// Command boundaries lists the numeric limits a package declares (integer and float constants,
// and package-level variables initialised from a numeric literal) and reports, for each, whether
// the package's tests name it at, one below and one above the limit — the docs/TESTING.md §2.1
// rule "Every boundary is tested". It is a lead generator: a limit with no "below"/"above" use may
// still be covered by a fuzz seed or a literal, so read the tests before filing a finding.
//
// Usage: go run ./.agent/scripts/adversarial/boundaries [-json] <pkg dir>...
package main

import (
	"encoding/json"
	"flag"
	"fmt"
	"go/ast"
	"go/parser"
	"go/token"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
)

type limit struct {
	Pkg   string `json:"pkg"`
	Name  string `json:"name"`
	Value string `json:"value"`
	Pos   string `json:"pos"`
	At    int    `json:"at"`    // test lines naming the limit
	Below int    `json:"below"` // ... as Name-1 (or Name - 1)
	Above int    `json:"above"` // ... as Name+1 (or Name + 1)
}

func main() {
	asJSON := flag.Bool("json", false, "print JSON")
	flag.Parse()
	if flag.NArg() == 0 {
		fmt.Fprintln(os.Stderr, "usage: boundaries [-json] <pkg dir>...")
		os.Exit(2)
	}
	var all []limit
	for _, dir := range flag.Args() {
		ls, err := scan(dir)
		if err != nil {
			fmt.Fprintf(os.Stderr, "boundaries: %v\n", err)
			os.Exit(2)
		}
		all = append(all, ls...)
	}
	if *asJSON {
		enc := json.NewEncoder(os.Stdout)
		enc.SetIndent("", "  ")
		_ = enc.Encode(all) // nowhere better to report a stdout failure
		return
	}
	fmt.Printf("%-6s %-5s %-5s %-5s  %-28s %-10s %s\n", "STATUS", "AT", "BELOW", "ABOVE", "LIMIT", "VALUE", "WHERE")
	for _, l := range all {
		fmt.Printf("%-6s %-5d %-5d %-5d  %-28s %-10s %s\n", status(l), l.At, l.Below, l.Above, l.Name, clip(l.Value, 10), l.Pos)
	}
}

// status: ok (named at, below and above), part (named, but not on both sides), none (never named).
func status(l limit) string {
	switch {
	case l.At == 0:
		return "none"
	case l.Below == 0 || l.Above == 0:
		return "part"
	}
	return "ok"
}

// scan parses dir's non-test files for limits and counts their uses in dir's _test.go files.
func scan(dir string) ([]limit, error) {
	fset := token.NewFileSet()
	matches, err := filepath.Glob(filepath.Join(dir, "*.go"))
	if err != nil {
		return nil, err
	}
	var limits []limit
	var tests []string
	for _, p := range matches {
		if strings.HasSuffix(p, "_test.go") {
			b, err := os.ReadFile(p)
			if err != nil {
				return nil, err
			}
			tests = append(tests, string(b))
			continue
		}
		f, err := parser.ParseFile(fset, p, nil, parser.SkipObjectResolution)
		if err != nil {
			return nil, err
		}
		limits = append(limits, fileLimits(fset, f, dir)...)
	}
	for i := range limits {
		countUses(&limits[i], tests)
	}
	sort.Slice(limits, func(i, j int) bool { return limits[i].Pos < limits[j].Pos })
	return limits, nil
}

// fileLimits returns the numeric constants and literal-initialised package variables in f.
func fileLimits(fset *token.FileSet, f *ast.File, dir string) []limit {
	var out []limit
	for _, d := range f.Decls {
		g, ok := d.(*ast.GenDecl)
		if !ok || (g.Tok != token.CONST && g.Tok != token.VAR) {
			continue
		}
		for _, spec := range g.Specs {
			vs := spec.(*ast.ValueSpec)
			for i, n := range vs.Names {
				if n.Name == "_" || i >= len(vs.Values) {
					continue
				}
				if v, ok := numeric(vs.Values[i]); ok {
					p := fset.Position(n.Pos())
					out = append(out, limit{Pkg: dir, Name: n.Name, Value: v,
						Pos: fmt.Sprintf("%s:%d", p.Filename, p.Line)})
				}
			}
		}
	}
	return out
}

// numeric reports whether e is a numeric literal expression (literals, unary minus, arithmetic of
// literals, time.Second-style products), returning its source-ish text.
func numeric(e ast.Expr) (string, bool) {
	switch x := e.(type) {
	case *ast.BasicLit:
		return x.Value, x.Kind == token.INT || x.Kind == token.FLOAT
	case *ast.UnaryExpr:
		s, ok := numeric(x.X)
		return x.Op.String() + s, ok
	case *ast.ParenExpr:
		return numeric(x.X)
	case *ast.BinaryExpr:
		a, aok := numeric(x.X)
		b, bok := numeric(x.Y)
		if !aok && !bok { // one side may be a unit such as time.Second
			return "", false
		}
		if !aok {
			a = exprName(x.X)
		}
		if !bok {
			b = exprName(x.Y)
		}
		return a + x.Op.String() + b, true
	}
	return "", false
}

func exprName(e ast.Expr) string {
	switch x := e.(type) {
	case *ast.Ident:
		return x.Name
	case *ast.SelectorExpr:
		return exprName(x.X) + "." + x.Sel.Name
	}
	return "?"
}

func countUses(l *limit, tests []string) {
	name := regexp.QuoteMeta(l.Name)
	at := regexp.MustCompile(`\b` + name + `\b`)
	below := regexp.MustCompile(`\b` + name + `\s*-\s*1\b`)
	above := regexp.MustCompile(`\b` + name + `\s*\+\s*1\b`)
	for _, src := range tests {
		for _, line := range strings.Split(src, "\n") {
			if at.MatchString(line) {
				l.At++
			}
			if below.MatchString(line) {
				l.Below++
			}
			if above.MatchString(line) {
				l.Above++
			}
		}
	}
}

func clip(s string, n int) string {
	if len(s) > n {
		return s[:n-1] + "…"
	}
	return s
}
