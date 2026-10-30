// Package embedtest is the contract suite every embed.Embedder must pass. Each implementation
// calls Run from its own tests; behaviour specific to one implementation (model table, cache
// layout) stays in that implementation's package.
package embedtest

import (
	"context"
	"errors"
	"testing"

	"github.com/nickstrad/kb/internal/embed"
)

// Caps says which optional guarantees an implementation makes.
type Caps struct {
	Deterministic bool // equal input gives equal vectors
	FTSOnly       bool // Dim()==0 and Embed always fails (embed.None)
}

// Factory returns a fresh embedder ready to use. It registers its own cleanup.
type Factory func(t *testing.T) embed.Embedder

// Run checks the embed.Embedder contract: a non-empty Model; one vector per input, in order, each
// of length Dim (including for one input and for an empty text); an empty input gives an empty
// result; a cancelled ctx is an error that is not
// embed.ErrUnavailable; equal inputs give equal vectors when Deterministic; and for FTSOnly,
// Dim()==0 with Embed always failing. Input order is checked only when Deterministic: a
// non-deterministic embedder's vectors cannot be compared across calls, so order is unchecked
// for it.
func Run(t *testing.T, newEmbedder Factory, caps Caps) {
	t.Helper()

	t.Run("model is named", func(t *testing.T) {
		if newEmbedder(t).Model() == "" {
			t.Fatal("Model() is empty; embed_meta could not record it")
		}
	})

	if caps.FTSOnly {
		t.Run("fts only: no dims, embed fails", func(t *testing.T) {
			e := newEmbedder(t)
			if !embed.IsNone(e) {
				t.Fatal("FTS-only embedder is not embed.None (embed.IsNone is false)")
			}
			if e.Dim() != 0 {
				t.Fatalf("Dim() = %d, want 0", e.Dim())
			}
			if _, err := e.Embed(context.Background(), []string{"x"}); err == nil {
				t.Fatal("Embed succeeded on an FTS-only embedder")
			}
		})
		return
	}

	t.Run("one vector per input, in order, of length Dim", func(t *testing.T) {
		e := newEmbedder(t)
		if e.Dim() <= 0 {
			t.Fatalf("Dim() = %d, want > 0", e.Dim())
		}
		texts := []string{"alpha", "beta gamma", "delta"}
		vecs, err := e.Embed(context.Background(), texts)
		if err != nil {
			t.Fatalf("Embed: %v", err)
		}
		if len(vecs) != len(texts) {
			t.Fatalf("got %d vectors for %d texts", len(vecs), len(texts))
		}
		for i, v := range vecs {
			if len(v) != e.Dim() {
				t.Fatalf("vector %d has %d dims, Dim() = %d", i, len(v), e.Dim())
			}
		}
		// Order: each text alone must give the vector at its position in the batch.
		single, err := e.Embed(context.Background(), texts[1:2])
		if err != nil {
			t.Fatalf("Embed single: %v", err)
		}
		if caps.Deterministic && !approxEqual(single[0], vecs[1]) {
			t.Fatal("vector for texts[1] differs alone and in a batch: output is not in input order")
		}
	})

	t.Run("empty input gives empty result", func(t *testing.T) {
		for name, in := range map[string][]string{"nil": nil, "empty slice": {}} {
			vecs, err := newEmbedder(t).Embed(context.Background(), in)
			if err != nil {
				t.Fatalf("Embed(%s): %v", name, err)
			}
			if len(vecs) != 0 {
				t.Fatalf("Embed(%s) gave %d vectors", name, len(vecs))
			}
		}
	})

	t.Run("one input and an empty text", func(t *testing.T) {
		e := newEmbedder(t)
		for _, text := range []string{"x", ""} {
			vecs, err := e.Embed(context.Background(), []string{text})
			if err != nil {
				t.Fatalf("Embed(%q): %v", text, err)
			}
			if len(vecs) != 1 || len(vecs[0]) != e.Dim() {
				t.Fatalf("Embed(%q) gave %d vectors, want 1 of %d dims", text, len(vecs), e.Dim())
			}
		}
	})

	t.Run("cancelled ctx is an error, not unavailable", func(t *testing.T) {
		ctx, cancel := context.WithCancel(context.Background())
		cancel()
		_, err := newEmbedder(t).Embed(ctx, []string{"x"})
		if err == nil {
			t.Fatal("Embed with a cancelled ctx succeeded")
		}
		if errors.Is(err, embed.ErrUnavailable) {
			t.Fatalf("cancelled ctx reported as ErrUnavailable (would trigger an fts fallback): %v", err)
		}
		if !errors.Is(err, context.Canceled) {
			t.Fatalf("error does not wrap context.Canceled: %v", err)
		}
	})

	if caps.Deterministic {
		t.Run("equal inputs give equal vectors", func(t *testing.T) {
			e := newEmbedder(t)
			a, err := e.Embed(context.Background(), []string{"same text", "other", "same text"})
			if err != nil {
				t.Fatalf("Embed: %v", err)
			}
			b, err := newEmbedder(t).Embed(context.Background(), []string{"same text"})
			if err != nil {
				t.Fatalf("Embed: %v", err)
			}
			if !approxEqual(a[0], a[2]) || !approxEqual(a[0], b[0]) {
				t.Fatal("equal inputs gave different vectors")
			}
		})
	}
}

// approxEqual compares vectors with a small tolerance: a real model may batch-pad, which moves
// floats in the last bits.
func approxEqual(a, b []float32) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		d := a[i] - b[i]
		if d > 1e-4 || d < -1e-4 {
			return false
		}
	}
	return true
}
