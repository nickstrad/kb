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
