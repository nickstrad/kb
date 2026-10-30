package sqlite

import (
	"context"
	"fmt"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/nickstrad/kb/internal/assert"
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

// TestMatchPhraseAndNear is the rendering table of the phrase and NEAR tiers (PLAN 4.6): one
// quoted phrase of every surviving field's words, or a NEAR group of one phrase per field; empty
// unless two fields survive (and, for NEAR, the window is at least 1).
func TestMatchPhraseAndNear(t *testing.T) {
	for _, tc := range []struct {
		name         string
		terms        []string
		phrase, near string
	}{
		{"two words", []string{"red", "apple"}, `"red apple"`, `NEAR("red" "apple", 8)`},
		{"three words", []string{"a", "b", "c"}, `"a b c"`, `NEAR("a" "b" "c", 8)`},
		{"terms are split on whitespace", []string{"red  apple", "pie"}, `"red apple pie"`, `NEAR("red" "apple" "pie", 8)`},
		{"one word has no tier", []string{"red"}, ``, ``},
		{"none", nil, ``, ``},
		{"a hyphenated field is one group of two words", []string{"draw-visual", "mode"}, `"draw-visual mode"`, `NEAR("draw-visual" "mode", 8)`},
		{"one hyphenated field alone has no tier", []string{"draw-visual"}, ``, ``},
		{"a field that sanitises away is dropped", []string{"red", "&&", "apple"}, `"red apple"`, `NEAR("red" "apple", 8)`},
		{"a bare operator word is dropped", []string{"red", "AND", "apple"}, `"red apple"`, `NEAR("red" "apple", 8)`},
		{"dropping leaves one field: no tier", []string{"red", "or"}, ``, ``},
		{"the prefix star is deleted", []string{"re*d", "apple"}, `"red apple"`, `NEAR("red" "apple", 8)`},
		{"a quote cannot break out (the operator word is its own field and dropped)", []string{`a" OR "b`, "c"}, `"a b c"`, `NEAR("a" "b" "c", 8)`},
		{"a quote inside a field becomes a space", []string{`a"b`, "c"}, `"a b c"`, `NEAR("a b" "c", 8)`},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := MatchPhrase(tc.terms); got != tc.phrase {
				t.Errorf("MatchPhrase = %s, want %s", got, tc.phrase)
			}
			if got := MatchNear(tc.terms, store.NearDistance); got != tc.near {
				t.Errorf("MatchNear = %s, want %s", got, tc.near)
			}
		})
	}
	if got := MatchNear([]string{"a", "b"}, 3); got != `NEAR("a" "b", 3)` {
		t.Errorf("MatchNear with window 3 = %s", got)
	}
	for _, bad := range []int{0, -1} {
		if got := MatchNear([]string{"a", "b"}, bad); got != "" {
			t.Errorf("MatchNear with window %d = %s, want none", bad, got)
		}
	}
}

// TestFTSPhraseAndNearOps: the ops run against the real table. The phrase op needs adjacent words
// in order; NEAR accepts either order within the window and respects Near; both ignore Alts; a
// single term or a query that sanitises to under two fields runs nothing but reports no match.
func TestFTSPhraseAndNearOps(t *testing.T) {
	ctx := context.Background()
	s, err := Open(ctx, filepath.Join(t.TempDir(), "kb.sqlite"), Options{})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { s.Close() })
	if err := s.EnsureEmbedMeta(ctx, store.EmbedMeta{Model: store.FTSOnlyModel}); err != nil {
		t.Fatal(err)
	}
	texts := []string{
		1: "a red apple pie",                                     // adjacent, in order
		2: "an apple and a red pear",                             // reversed, 4 tokens apart
		3: "red " + strings.Repeat("x ", 12) + "apple",           // 12 tokens apart
		4: "a green pear tart",                                   // neither word
		5: "red velvet and an apple turnover, then a kubernetes", // 3 tokens apart, in order
	}
	for i, text := range texts[1:] {
		path := fmt.Sprintf("p%d.md", i+1)
		if _, err := s.ReplaceEntry(ctx, store.EntryInput{Path: path, Kind: "file", Title: "T", Summary: "S"},
			[]store.ChunkInput{{Ord: 0, SourceFile: path, Text: text, TextHash: path}}); err != nil {
			t.Fatal(err)
		}
	}
	ids := func(op store.FTSOp, near int, terms ...string) ([]int64, string) {
		res, err := s.FTS(ctx, store.FTSQuery{Terms: terms, Op: op, Near: near, Limit: 10})
		if err != nil {
			t.Fatalf("FTS %v %v: %v", op, terms, err)
		}
		var out []int64
		for _, r := range res.Rows {
			out = append(out, r.ChunkID)
		}
		slices.Sort(out)
		return out, res.Match
	}
	for _, tc := range []struct {
		name  string
		op    store.FTSOp
		near  int
		terms []string
		want  []int64
	}{
		{"phrase: adjacent and in order only", store.OpPhrase, 0, []string{"red", "apple"}, []int64{1}},
		{"near: either order within the default window", store.OpNear, 0, []string{"red", "apple"}, []int64{1, 2, 5}},
		{"near: window 1 is adjacency (in either order)", store.OpNear, 1, []string{"red", "apple"}, []int64{1}},
		{"near: a wide window reaches the far chunk", store.OpNear, 20, []string{"red", "apple"}, []int64{1, 2, 3, 5}},
		{"and: the far chunk too", store.OpAnd, 0, []string{"red", "apple"}, []int64{1, 2, 3, 5}},
		{"phrase of one field: nothing runs", store.OpPhrase, 0, []string{"red"}, nil},
		{"near of one field: nothing runs", store.OpNear, 0, []string{"red"}, nil},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got, _ := ids(tc.op, tc.near, tc.terms...)
			if !slices.Equal(got, tc.want) {
				t.Errorf("rows = %v, want %v", got, tc.want)
			}
		})
	}
	// The alternatives of a synonym do not widen a phrase or NEAR query.
	res, err := s.FTS(ctx, store.FTSQuery{Terms: []string{"red", "apple"}, Op: store.OpPhrase, Limit: 10,
		Alts: map[string][]string{"apple": {"pear"}}})
	if err != nil || res.Match != `"red apple"` {
		t.Errorf("phrase with Alts: match %q err %v, want the literal phrase", res.Match, err)
	}
	// A negative window is a caller bug.
	func() {
		defer func() {
			if recover() == nil && assert.Enabled {
				t.Error("a negative Near did not panic under assertions")
			}
		}()
		_, _ = s.FTS(ctx, store.FTSQuery{Terms: []string{"a", "b"}, Op: store.OpNear, Near: -1, Limit: 1})
	}()
}
