// Package embed defines the embedding interface the indexer and the searcher use, so the
// embedder implementations (embed/ollama, embed/openai, the FTS-only None, and the deterministic
// test double embed/fake) are interchangeable. embed/provider picks one from the environment.
//
// Some models are asymmetric: nomic-embed-text wants stored documents and incoming queries to
// carry different prefixes. Callers never prepend a prefix themselves; they go through DocText
// and QueryText, which ask the embedder for its prefixes (the Prefixer interface) and fall back
// to the nomic ones for an embedder that does not say.
package embed

import (
	"context"
	"errors"
)

// Prefixes required by nomic-embed-text, the default for an embedder that does not implement
// Prefixer.
const (
	DocPrefix   = "search_document: "
	QueryPrefix = "search_query: "
)

// NoneModel is the model name None reports and embed_meta records for an FTS-only index.
const NoneModel = "none"

// ErrUnavailable marks failures that mean the embedder cannot be reached or used at all:
// connection refused, DNS failure, a deadline the client itself imposed, a 5xx response, or a
// rate limit. Callers use errors.Is(err, ErrUnavailable) to decide whether to fall back to
// FTS-only search (exit code 2 / mode fts-fallback). It does NOT cover the caller's own ctx being
// cancelled, or a 4xx response such as a bad API key or an unknown model, since retrying or
// falling back would not help either.
var ErrUnavailable = errors.New("embedder unavailable")

// Embedder turns texts into vectors. Embed returns one vector per input, in order, each of length
// Dim(). Model() and Dim() identify what is stored in the embed_meta table: a mismatch between
// embed_meta and the configured embedder means the index has to be rebuilt.
type Embedder interface {
	Embed(ctx context.Context, texts []string) ([][]float32, error)
	Model() string
	Dim() int
}

// Prefixer is implemented by an embedder whose model wants a task prefix on documents and
// queries. An empty prefix means the text is sent as is.
type Prefixer interface {
	Prefixes() (doc, query string)
}

// DocText returns text as e should embed it for indexing.
func DocText(e Embedder, text string) string {
	if p, ok := e.(Prefixer); ok {
		doc, _ := p.Prefixes()
		return doc + text
	}
	return DocPrefix + text
}

// QueryText returns query as e should embed it for searching.
func QueryText(e Embedder, query string) string {
	if p, ok := e.(Prefixer); ok {
		_, q := p.Prefixes()
		return q + query
	}
	return QueryPrefix + query
}

// None is the embedder for KB_EMBEDDER=none: the index holds no vectors and search is FTS only.
// Its Dim is 0, which is how the indexer and the store recognise it; Embed is never meant to be
// called and always fails.
type None struct{}

// Embed always fails: an FTS-only index embeds nothing.
func (None) Embed(context.Context, []string) ([][]float32, error) {
	return nil, errors.New("embedder is none (KB_EMBEDDER=none): no vectors are computed")
}

// Model reports NoneModel.
func (None) Model() string { return NoneModel }

// Dim reports 0: no vectors.
func (None) Dim() int { return 0 }

// IsNone reports whether e computes no vectors, so the index is FTS only.
func IsNone(e Embedder) bool { return e == nil || e.Dim() == 0 }
