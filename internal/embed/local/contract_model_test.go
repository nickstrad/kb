//go:build model

package local

import (
	"testing"

	"github.com/nickstrad/kb/internal/embed"
	"github.com/nickstrad/kb/internal/embed/embedtest"
)

// TestContract runs the embedder contract against the real default model. Like the other model
// tests it fails, rather than skips, when the model is not downloaded.
func TestContract(t *testing.T) {
	cache := userModelsDir(t)
	embedtest.Run(t, func(t *testing.T) embed.Embedder {
		e := New(models[DefaultModel], cache)
		if !e.Cached() {
			t.Fatalf("model %s not in %s; run `kb model pull` first", DefaultModel, cache)
		}
		t.Cleanup(func() { e.Close() })
		return e
	}, embedtest.Caps{Deterministic: true})
}
