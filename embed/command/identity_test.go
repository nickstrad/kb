package command

import (
	"fmt"
	"testing"
)

// Regression (review of 17192aa): free-form fields must not be able to imitate another field.
// Every pair of distinct vector-changing settings drawn from hostile values must give a distinct
// Model().
func TestModelIdentityIsCollisionFree(t *testing.T) {
	type key struct {
		argv, model, doc, query string
	}
	argvs := [][]string{{"embedder"}, {"embedder", "--x"}, {`embedder" "--x`}, {"embedder --x"}}
	models := []string{"m", `m doc_prefix="x"`, `m query_prefix="x"`, `m" doc_prefix="x`, "m x"}
	prefixes := []string{"", "x", `x" query_prefix="x`, ` query_prefix="x"`}
	seen := map[string]key{}
	for _, a := range argvs {
		for _, m := range models {
			for _, d := range prefixes {
				for _, q := range prefixes {
					k := key{fmt.Sprintf("%q", a), m, d, q}
					c, err := New(Config{Argv: a, Model: m, Dim: 2, DocPrefix: d, QueryPrefix: q})
					if err != nil {
						t.Fatal(err)
					}
					id := c.Model()
					if prev, ok := seen[id]; ok && prev != k {
						t.Fatalf("distinct settings collide on %q:\n  %+v\n  %+v", id, prev, k)
					}
					seen[id] = k
				}
			}
		}
	}
	// The probe's exact pair, named so a failure reads plainly.
	a, _ := New(Config{Argv: []string{"embedder"}, Model: `m doc_prefix="x"`, Dim: 2})
	b, _ := New(Config{Argv: []string{"embedder"}, Model: "m", Dim: 2, DocPrefix: "x"})
	if a.Model() == b.Model() {
		t.Errorf(`model 'm doc_prefix="x"' collides with model "m" + doc_prefix "x": %q`, a.Model())
	}
}
