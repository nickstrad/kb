package store

import (
	"errors"
	"strings"
	"testing"
)

func TestEmbedMetaMismatchError(t *testing.T) {
	err := error(&EmbedMetaMismatchError{StoredModel: "a", StoredDim: 4, WantModel: "b", WantDim: 8})
	if !errors.Is(err, ErrEmbedMetaMismatch) {
		t.Error("errors.Is(ErrEmbedMetaMismatch) is false")
	}
	for _, want := range []string{"a (4 dims)", "b (8 dims)", "kb reindex --all"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("%q lacks %q", err.Error(), want)
		}
	}
}

// TestDefaultBM25 pins the shipped column weights: a title hit counts three times a body hit, a
// heading hit twice. Changing them changes every ranking, so it needs a reviewed golden update.
func TestDefaultBM25(t *testing.T) {
	if got, want := DefaultBM25(), (BM25Weights{Text: 1, Heading: 2, Title: 3}); got != want {
		t.Errorf("DefaultBM25 = %+v, want %+v", got, want)
	}
}
