package sqlite

import (
	"context"
	"fmt"
	"maps"
	"slices"
	"testing"

	"github.com/nickstrad/kb/internal/store"
)

// vocabStore indexes one entry whose chunks hold the vocabulary the vocab tests read.
func vocabStore(t *testing.T) *Store {
	t.Helper()
	s, emb := newTestStore(t)
	if _, err := s.ReplaceEntry(context.Background(), sampleEntry(), chunkInputs(t, emb,
		"kubernetes pods; see kb.toml.",
		"kubernetes kb.toml and Kubernetes again",
		"café zebracorn: the kubernetes",
		"zebracorn",
	)); err != nil {
		t.Fatal(err)
	}
	return s
}

// TestVocabTerms: trailing `.`/`:` merge into the bare term, counts are per chunk (case folds,
// diacritics stay), and the document and length bounds hold at their edges.
func TestVocabTerms(t *testing.T) {
	s := vocabStore(t)
	read := func(q store.VocabQuery) map[string]int {
		t.Helper()
		got, err := s.VocabTerms(context.Background(), q)
		if err != nil {
			t.Fatal(err)
		}
		out := map[string]int{}
		for i, v := range got {
			if i > 0 && got[i-1].Term >= v.Term {
				t.Errorf("not sorted: %q before %q", got[i-1].Term, v.Term)
			}
			out[v.Term] = v.Doc
		}
		return out
	}
	all := read(store.VocabQuery{})
	for term, want := range map[string]int{"kubernetes": 3, "kb.toml": 2, "zebracorn": 2, "café": 1, "pods": 1, "section": 4} {
		if all[term] != want {
			t.Errorf("doc(%q) = %d, want %d (all: %v)", term, all[term], want, all)
		}
	}
	for _, gone := range []string{"kb.toml.", "zebracorn:", "Kubernetes"} {
		if _, ok := all[gone]; ok {
			t.Errorf("%q should have merged into its trimmed form", gone)
		}
	}
	for _, tc := range []struct {
		q    store.VocabQuery
		want []string
	}{
		{store.VocabQuery{MinDoc: 2, MinLen: 9, MaxLen: 10}, []string{"kubernetes", "zebracorn"}},
		{store.VocabQuery{MinDoc: 3, MinLen: 9, MaxLen: 10}, []string{"kubernetes"}},
		{store.VocabQuery{MinDoc: 2, MinLen: 10, MaxLen: 10}, []string{"kubernetes"}},
		{store.VocabQuery{MinDoc: 1, MinLen: 4, MaxLen: 4}, []string{"café", "pods"}}, // length in characters, not bytes
		{store.VocabQuery{MinDoc: 5}, nil},
	} {
		got := read(tc.q)
		if keys := slices.Sorted(maps.Keys(got)); !slices.Equal(keys, tc.want) {
			t.Errorf("%+v: got %v, want %v", tc.q, keys, tc.want)
		}
	}
}

// TestDocFreq: looked-up terms with their variants added, absent terms absent, batches beyond
// docFreqBatch and duplicates.
func TestDocFreq(t *testing.T) {
	s := vocabStore(t)
	terms := []string{"kubernetes", "kb.toml", "nope", "kubernetes"}
	for i := 0; i < docFreqBatch; i++ {
		terms = append(terms, fmt.Sprintf("filler%d", i))
	}
	// Last, so its last variant is the statement's last placeholder: zebracorn's second chunk
	// holds only `zebracorn:`.
	terms = append(terms, "café", "zebracorn")
	// A duplicate far past the first batch's end: with per-batch dedupe it was counted twice.
	terms = append(terms, "kubernetes")
	got, err := s.DocFreq(context.Background(), terms)
	if err != nil {
		t.Fatal(err)
	}
	want := map[string]int{"kubernetes": 3, "kb.toml": 2, "zebracorn": 2, "café": 1}
	if !maps.Equal(got, want) {
		t.Errorf("DocFreq = %v, want %v", got, want)
	}
	if got, err := s.DocFreq(context.Background(), nil); err != nil || len(got) != 0 {
		t.Errorf("DocFreq(nil) = %v, %v", got, err)
	}
}

// TestVocabClosed: both reads fail cleanly on a closed store.
func TestVocabClosed(t *testing.T) {
	s := vocabStore(t)
	s.Close()
	if _, err := s.VocabTerms(context.Background(), store.VocabQuery{}); err == nil {
		t.Errorf("VocabTerms on a closed store: %v", err)
	}
	if _, err := s.DocFreq(context.Background(), []string{"x"}); err == nil {
		t.Errorf("DocFreq on a closed store: %v", err)
	}
}
