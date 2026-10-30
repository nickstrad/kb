package main

import (
	"fmt"
	"go/ast"
	"go/printer"
	"go/token"
	"regexp"
	"strconv"
	"strings"
)

// Operator names, as accepted by -ops.
const (
	opBoundary = "boundary"    // <  <-> <=, >  <-> >=
	opNegRel   = "negate-rel"  // == <-> !=
	opLogical  = "logical"     // && <-> ||
	opIfNeg    = "if-negate"   // if c  ->  if !(c)
	opConst    = "const"       // integer literal N -> N+1 and N-1
	opArith    = "arith"       // + <-> -, += <-> -=, ++ <-> --
	opErrCheck = "err-check"   // if err != nil {...} -> if false {...}
	opBoolRet  = "bool-return" // return b -> return !(b) where the result is a bool
	opLoopDrop = "loop-drop"   // drop one statement of a for/range body
)

// allOps lists every operator in report order.
var allOps = []string{opBoundary, opNegRel, opLogical, opIfNeg, opConst, opArith, opErrCheck, opBoolRet, opLoopDrop}

// mutation is one reversible edit of a parsed file. apply and undo change the AST in place, so a
// file is parsed once and printed once per mutant.
type mutation struct {
	Op     string
	Pos    token.Position
	Func   string // enclosing function ("Recv.Name" for methods), "" at package level
	Before string
	After  string
	apply  func()
	undo   func()
}

// enumerate returns every mutation of file for the enabled ops, in source order of discovery.
// funcRe, when non-nil, keeps only mutations inside a function whose name matches it; lines, when
// non-nil, keeps only mutations whose line is in the set.
func enumerate(fset *token.FileSet, file *ast.File, ops map[string]bool, funcRe *regexp.Regexp, lines map[int]bool) []*mutation {
	e := &enumerator{fset: fset, ops: ops}
	for _, decl := range file.Decls {
		switch d := decl.(type) {
		case *ast.FuncDecl:
			e.fn = funcName(d)
			e.boolResults = boolResultPositions(d.Type)
			if funcRe != nil && !funcRe.MatchString(e.fn) {
				continue
			}
			if d.Body != nil {
				e.walk(d.Body)
			}
		case *ast.GenDecl:
			if funcRe != nil {
				continue
			}
			e.fn, e.boolResults = "", nil
			e.walk(d)
		}
	}
	if lines == nil {
		return e.out
	}
	kept := e.out[:0]
	for _, m := range e.out {
		if lines[m.Pos.Line] {
			kept = append(kept, m)
		}
	}
	return kept
}

type enumerator struct {
	fset        *token.FileSet
	ops         map[string]bool
	fn          string
	boolResults []bool // per result position of the enclosing func: is it a bool?
	out         []*mutation
}

func (e *enumerator) add(op string, pos token.Pos, before, after string, apply, undo func()) {
	e.out = append(e.out, &mutation{
		Op: op, Pos: e.fset.Position(pos), Func: e.fn,
		Before: before, After: after, apply: apply, undo: undo,
	})
}

// walk visits n. Nested function literals keep the enclosing declaration's name but have their
// own results, so bool-return is not applied inside them.
func (e *enumerator) walk(n ast.Node) {
	ast.Inspect(n, func(n ast.Node) bool {
		switch x := n.(type) {
		case *ast.FuncLit:
			saved := e.boolResults
			e.boolResults = boolResultPositions(x.Type)
			e.walk(x.Body)
			e.boolResults = saved
			return false
		case *ast.BinaryExpr:
			e.binary(x)
		case *ast.AssignStmt:
			e.assignOp(x)
		case *ast.IncDecStmt:
			e.incDec(x)
		case *ast.BasicLit:
			e.intLit(x)
		case *ast.IfStmt:
			e.ifStmt(x)
		case *ast.ReturnStmt:
			e.boolReturn(x)
		case *ast.ForStmt:
			e.loopDrop(x.Body)
		case *ast.RangeStmt:
			e.loopDrop(x.Body)
		}
		return true
	})
}

var swaps = map[string]map[token.Token]token.Token{
	opBoundary: {token.LSS: token.LEQ, token.LEQ: token.LSS, token.GTR: token.GEQ, token.GEQ: token.GTR},
	opNegRel:   {token.EQL: token.NEQ, token.NEQ: token.EQL},
	opLogical:  {token.LAND: token.LOR, token.LOR: token.LAND},
	opArith:    {token.ADD: token.SUB, token.SUB: token.ADD},
}

func (e *enumerator) binary(x *ast.BinaryExpr) {
	for _, op := range []string{opBoundary, opNegRel, opLogical, opArith} {
		to, ok := swaps[op][x.Op]
		if !ok || !e.ops[op] {
			continue
		}
		if op == opArith && (isStringLit(x.X) || isStringLit(x.Y)) {
			continue // string concatenation: "-" would not compile
		}
		from := x.Op
		e.add(op, x.OpPos, from.String(), to.String(), func() { x.Op = to }, func() { x.Op = from })
	}
}

func (e *enumerator) assignOp(x *ast.AssignStmt) {
	if !e.ops[opArith] {
		return
	}
	var to token.Token
	switch x.Tok {
	case token.ADD_ASSIGN:
		to = token.SUB_ASSIGN
	case token.SUB_ASSIGN:
		to = token.ADD_ASSIGN
	default:
		return
	}
	if len(x.Rhs) == 1 && isStringLit(x.Rhs[0]) {
		return
	}
	from := x.Tok
	e.add(opArith, x.TokPos, from.String(), to.String(), func() { x.Tok = to }, func() { x.Tok = from })
}

func (e *enumerator) incDec(x *ast.IncDecStmt) {
	if !e.ops[opArith] {
		return
	}
	from := x.Tok
	to := token.DEC
	if from == token.DEC {
		to = token.INC
	}
	e.add(opArith, x.TokPos, from.String(), to.String(), func() { x.Tok = to }, func() { x.Tok = from })
}

func (e *enumerator) intLit(x *ast.BasicLit) {
	if !e.ops[opConst] || x.Kind != token.INT {
		return
	}
	v, err := strconv.ParseInt(strings.ReplaceAll(x.Value, "_", ""), 0, 64)
	if err != nil {
		return
	}
	from := x.Value
	for _, d := range []int64{1, -1} {
		if v+d < 0 {
			continue // a negative literal may not compile (array length, shift count)
		}
		to := strconv.FormatInt(v+d, 10)
		e.add(opConst, x.ValuePos, from, to, func() { x.Value = to }, func() { x.Value = from })
	}
}

func (e *enumerator) ifStmt(x *ast.IfStmt) {
	cond := x.Cond
	if e.ops[opErrCheck] && isErrNotNil(cond) {
		f := ast.NewIdent("false")
		f.NamePos = cond.Pos()
		e.add(opErrCheck, cond.Pos(), exprString(e.fset, cond), "false",
			func() { x.Cond = f }, func() { x.Cond = cond })
	}
	if e.ops[opIfNeg] {
		neg := negate(cond)
		e.add(opIfNeg, cond.Pos(), exprString(e.fset, cond), exprString(e.fset, neg),
			func() { x.Cond = neg }, func() { x.Cond = cond })
	}
}

func (e *enumerator) boolReturn(x *ast.ReturnStmt) {
	if !e.ops[opBoolRet] || len(x.Results) != len(e.boolResults) {
		return
	}
	for i, isBool := range e.boolResults {
		if !isBool {
			continue
		}
		orig := x.Results[i]
		neg := negate(orig)
		i := i
		e.add(opBoolRet, orig.Pos(), exprString(e.fset, orig), exprString(e.fset, neg),
			func() { x.Results[i] = neg }, func() { x.Results[i] = orig })
	}
}

// loopDrop removes one statement at a time from a loop body. Declarations and := are skipped:
// dropping them leaves an undefined or unused name, which only yields a compile error.
func (e *enumerator) loopDrop(body *ast.BlockStmt) {
	if !e.ops[opLoopDrop] || body == nil {
		return
	}
	for i, s := range body.List {
		switch st := s.(type) {
		case *ast.DeclStmt, *ast.LabeledStmt:
			continue
		case *ast.AssignStmt:
			if st.Tok == token.DEFINE {
				continue
			}
		}
		orig := body.List
		dropped := make([]ast.Stmt, 0, len(orig)-1)
		dropped = append(dropped, orig[:i]...)
		dropped = append(dropped, orig[i+1:]...)
		e.add(opLoopDrop, s.Pos(), firstLine(exprString(e.fset, s)), "(removed)",
			func() { body.List = dropped }, func() { body.List = orig })
	}
}

// negate returns !(c), or c's operand when c is already a negation.
func negate(c ast.Expr) ast.Expr {
	if u, ok := c.(*ast.UnaryExpr); ok && u.Op == token.NOT {
		return u.X
	}
	if id, ok := c.(*ast.Ident); ok && (id.Name == "true" || id.Name == "false") {
		flip := "true"
		if id.Name == "true" {
			flip = "false"
		}
		return &ast.Ident{NamePos: id.NamePos, Name: flip}
	}
	switch c.(type) {
	case *ast.Ident, *ast.CallExpr, *ast.ParenExpr, *ast.SelectorExpr, *ast.IndexExpr:
		return &ast.UnaryExpr{OpPos: c.Pos(), Op: token.NOT, X: c}
	}
	return &ast.UnaryExpr{OpPos: c.Pos(), Op: token.NOT, X: &ast.ParenExpr{Lparen: c.Pos(), X: c, Rparen: c.End()}}
}

func isErrNotNil(c ast.Expr) bool {
	b, ok := c.(*ast.BinaryExpr)
	if !ok || b.Op != token.NEQ {
		return false
	}
	l, lok := b.X.(*ast.Ident)
	r, rok := b.Y.(*ast.Ident)
	return lok && rok && r.Name == "nil" && (l.Name == "err" || strings.HasSuffix(l.Name, "Err"))
}

func isStringLit(x ast.Expr) bool {
	b, ok := x.(*ast.BasicLit)
	return ok && b.Kind == token.STRING
}

// boolResultPositions reports, per result value of ft, whether its declared type is bool.
func boolResultPositions(ft *ast.FuncType) []bool {
	if ft == nil || ft.Results == nil {
		return nil
	}
	var out []bool
	for _, f := range ft.Results.List {
		id, ok := f.Type.(*ast.Ident)
		isBool := ok && id.Name == "bool"
		n := len(f.Names)
		if n == 0 {
			n = 1
		}
		for range n {
			out = append(out, isBool)
		}
	}
	return out
}

func funcName(d *ast.FuncDecl) string {
	if d.Recv == nil || len(d.Recv.List) == 0 {
		return d.Name.Name
	}
	t := d.Recv.List[0].Type
	if s, ok := t.(*ast.StarExpr); ok {
		t = s.X
	}
	if ix, ok := t.(*ast.IndexExpr); ok { // generic receiver
		t = ix.X
	}
	if id, ok := t.(*ast.Ident); ok {
		return id.Name + "." + d.Name.Name
	}
	return d.Name.Name
}

func exprString(fset *token.FileSet, n ast.Node) string {
	var b strings.Builder
	if err := printer.Fprint(&b, fset, n); err != nil {
		return fmt.Sprintf("<%T>", n)
	}
	return b.String()
}

func firstLine(s string) string {
	if i := strings.IndexByte(s, '\n'); i >= 0 {
		return s[:i] + " …"
	}
	return s
}

// parseOps turns a comma-separated -ops value into a set; "" or "all" means every operator.
func parseOps(s string) (map[string]bool, error) {
	set := map[string]bool{}
	if s == "" || s == "all" {
		for _, o := range allOps {
			set[o] = true
		}
		return set, nil
	}
	known := map[string]bool{}
	for _, o := range allOps {
		known[o] = true
	}
	for _, o := range strings.Split(s, ",") {
		o = strings.TrimSpace(o)
		if !known[o] {
			return nil, fmt.Errorf("unknown operator %q (want one of %s)", o, strings.Join(allOps, ", "))
		}
		set[o] = true
	}
	return set, nil
}
