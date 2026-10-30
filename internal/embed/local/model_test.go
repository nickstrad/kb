//go:build model

// Model tests load the real default model. They need it downloaded (`kb model pull`) and fail
// when it is missing rather than skipping, so a run with -tags model always checks something.
package local

import (
	"context"
	"math"
	"testing"
)

func TestDefaultModelEmbeds(t *testing.T) {
	cache, err := DefaultCacheDir()
	if err != nil {
		t.Fatal(err)
	}
	e := New(models[DefaultModel], cache)
	defer e.Close()
	if !e.Cached() {
		t.Fatalf("model %s not in %s; run `kb model pull` first", DefaultModel, cache)
	}

	_, qp := e.Prefixes()
	vecs, err := e.Embed(context.Background(), []string{
		qp + "which port does postgres listen on locally",
		"Postgres listens on port 5433 on the dev box because 5432 is taken.",
		"tmux servers die at logout unless lingering is enabled.",
	})
	if err != nil {
		t.Fatal(err)
	}
	for i, v := range vecs {
		if len(v) != 384 {
			t.Fatalf("vector %d has %d dims", i, len(v))
		}
		var n float64
		for _, x := range v {
			n += float64(x) * float64(x)
		}
		if math.Abs(n-1) > 1e-3 {
			t.Fatalf("vector %d has squared norm %f, want 1", i, n)
		}
	}
	if dot(vecs[0], vecs[1]) <= dot(vecs[0], vecs[2]) {
		t.Fatal("query is closer to the unrelated passage than to its answer")
	}
}

func dot(a, b []float32) float64 {
	var s float64
	for i := range a {
		s += float64(a[i]) * float64(b[i])
	}
	return s
}
