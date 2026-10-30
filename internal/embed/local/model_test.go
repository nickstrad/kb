//go:build model

// Model tests load the real default model. They need it downloaded (`kb model pull`) and fail
// when it is missing rather than skipping, so a run with -tags model always checks something.
package local

import (
	"context"
	"math"
	"os"
	"testing"

	"github.com/nickstrad/kb/internal/workspace"
)

// userModelsDir is the real model cache kb model pull writes to: the workspace rule
// (workspace.ModelsDirOf) applied to the user cache directory.
func userModelsDir(t *testing.T) string {
	t.Helper()
	base, err := os.UserCacheDir()
	if err != nil {
		t.Fatal(err)
	}
	return workspace.ModelsDirOf(base)
}

func TestDefaultModelEmbeds(t *testing.T) {
	cache := userModelsDir(t)
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
