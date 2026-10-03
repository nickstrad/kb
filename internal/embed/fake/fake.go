// Package fake is a deterministic Embedder for tests: no network, no Ollama, same vector for the
// same text on every run. The vectors carry no semantics, so tests may assert on identity and on
// plumbing, never on relevance.
package fake

import (
	"context"
	"crypto/sha256"
	"encoding/binary"
	"fmt"
	"math"
)

// Embedder implements embed.Embedder by hashing each text into a unit vector.
type Embedder struct {
	model string
	dim   int
}

// New returns a fake embedder reporting the given model name and dimension. dim must be positive.
func New(model string, dim int) *Embedder {
	if dim <= 0 {
		panic("fake: dim must be positive")
	}
	return &Embedder{model: model, dim: dim}
}

// Model reports the model name this fake claims to be.
func (e *Embedder) Model() string { return e.model }

// Dim reports the vector length.
func (e *Embedder) Dim() int { return e.dim }

// Embed returns one deterministic unit vector per text. The context is honoured only in that a
// cancelled context fails immediately; no I/O happens.
func (e *Embedder) Embed(ctx context.Context, texts []string) ([][]float32, error) {
	if err := ctx.Err(); err != nil {
		return nil, fmt.Errorf("fake embed: %w", err)
	}
	out := make([][]float32, len(texts))
	for i, text := range texts {
		out[i] = e.vector(text)
	}
	return out, nil
}

// vector expands sha256(text) into dim floats with a counter, then normalises to unit length so
// cosine distance behaves.
func (e *Embedder) vector(text string) []float32 {
	v := make([]float32, e.dim)
	var block [32]byte
	var counter uint32
	used := len(block)
	var sum float64
	for i := range v {
		if used+4 > len(block) {
			h := sha256.New()
			h.Write([]byte(text))
			var ctr [4]byte
			binary.BigEndian.PutUint32(ctr[:], counter)
			h.Write(ctr[:])
			copy(block[:], h.Sum(nil))
			counter++
			used = 0
		}
		// Map four hash bytes into [-1, 1).
		u := binary.BigEndian.Uint32(block[used : used+4])
		used += 4
		f := float64(u)/float64(1<<31) - 1
		v[i] = float32(f)
		sum += f * f
	}
	norm := math.Sqrt(sum)
	if norm == 0 {
		v[0] = 1
		return v
	}
	for i := range v {
		v[i] = float32(float64(v[i]) / norm)
	}
	return v
}
