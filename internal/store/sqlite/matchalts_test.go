package sqlite

import (
	"testing"

	"github.com/nickstrad/kb/internal/store"
)

// TestMatchExprAlts is the rendering table of synonym alternatives (PLAN 5.5).
func TestMatchExprAlts(t *testing.T) {
	for _, tc := range []struct {
		name  string
		terms []string
		op    store.FTSOp
		alts  map[string][]string
		want  string
	}{
		{"nil alts is MatchExpr", []string{"a", "b"}, store.OpAnd, nil, `"a" AND "b"`},
		{"one expanded term", []string{"k8s", "pods"}, store.OpAnd, map[string][]string{"k8s": {"kubernetes"}}, `("k8s" OR "kubernetes") AND "pods"`},
		{"OR op", []string{"k8s", "pods"}, store.OpOr, map[string][]string{"k8s": {"kubernetes"}}, `("k8s" OR "kubernetes") OR "pods"`},
		{"phrase alternative", []string{"vm"}, store.OpAnd, map[string][]string{"vm": {"virtual machine"}}, `("vm" OR "virtual machine")`},
		{"key is exact: other case not expanded", []string{"K8S"}, store.OpAnd, map[string][]string{"k8s": {"kubernetes"}}, `"K8S"`},
		{"alternative that sanitises away is dropped", []string{"a"}, store.OpAnd, map[string][]string{"a": {"or", "***", `"`}}, `"a"`},
		{"duplicate alternative collapses", []string{"a"}, store.OpAnd, map[string][]string{"a": {"b", "b", "a"}}, `("a" OR "b")`},
		{"term that sanitises away stays dropped (it must not narrow the query)", []string{"a", "or"}, store.OpAnd, map[string][]string{"or": {"either"}}, `"a"`},
		{"quote in an alternative cannot break out", []string{"a"}, store.OpAnd, map[string][]string{"a": {`x" OR "y`}}, `("a" OR "x  OR  y")`},
		{"nothing survives", []string{"or"}, store.OpAnd, map[string][]string{"or": {"and"}}, ``},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := MatchExprAlts(tc.terms, tc.op, tc.alts); got != tc.want {
				t.Errorf("MatchExprAlts = %s, want %s", got, tc.want)
			}
		})
	}
}
