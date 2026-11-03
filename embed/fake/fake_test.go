package fake

import (
	"context"
	"math"
	"testing"
)

// TestEmbedIsDeterministicAndNormalised is the whole contract the test double has to keep.
func TestEmbedIsDeterministicAndNormalised(t *testing.T) {
	e := New("fake-768", 768)
	if e.Model() != "fake-768" || e.Dim() != 768 {
		t.Fatalf("model/dim = %q / %d", e.Model(), e.Dim())
	}

	vectors, err := e.Embed(context.Background(), []string{"alpha", "beta", "alpha"})
	if err != nil {
		t.Fatalf("Embed: %v", err)
	}
	if len(vectors) != 3 {
		t.Fatalf("got %d vectors, want 3", len(vectors))
	}
	for i, v := range vectors {
		if len(v) != 768 {
			t.Fatalf("vector %d has length %d, want 768", i, len(v))
		}
		var sum float64
		for _, f := range v {
			sum += float64(f) * float64(f)
		}
		if math.Abs(math.Sqrt(sum)-1) > 1e-5 {
			t.Errorf("vector %d has norm %v, want 1", i, math.Sqrt(sum))
		}
	}
	for i := range vectors[0] {
		if vectors[0][i] != vectors[2][i] {
			t.Fatalf("same text produced different vectors at index %d", i)
		}
	}
	same := true
	for i := range vectors[0] {
		if vectors[0][i] != vectors[1][i] {
			same = false
			break
		}
	}
	if same {
		t.Error("different texts produced identical vectors")
	}
}
