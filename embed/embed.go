// Package embed is kb's public embedding plugin surface: the Embedder contract every provider
// implements, and a Registry that picks and configures a provider by name. Any Go module can
// import it; nothing in it is specific to kb, and nothing in it loads model weights.
//
// Providers that ship with it live in subpackages: embed/httpembed (the "http" provider, a
// generic HTTP embeddings API) and embed/command (the "command" provider, a user-supplied
// program speaking JSON on stdin/stdout). NoneProvider (FTS only) is in this package, and
// embed/builtin registers all three. kb adds its in-process "local" provider (internal) on top.
//
// Some models are asymmetric: they want stored documents and incoming queries to carry different
// task prefixes. Callers never prepend a prefix themselves; they go through DocText and
// QueryText, which ask the embedder for its prefixes (the Prefixer interface). An embedder that
// does not implement Prefixer gets the text as is.
package embed

import (
	"context"
	"errors"
)

// NoneModel is the model name None reports and embed_meta records for an FTS-only index.
const NoneModel = "none"

// ErrUnavailable marks failures that mean the embedder cannot be used at all right now: a model
// that is not downloaded or fails to load, a server that cannot be reached or answers 5xx/408/429,
// a deadline the client itself imposed, or a program that crashes. Callers use
// errors.Is(err, ErrUnavailable) to fall back to FTS-only search. It does not cover the caller's
// own ctx being cancelled, a configuration mistake (bad key, unknown model, missing program), or a
// wrong vector count or width, since falling back would not help.
var ErrUnavailable = errors.New("embedder unavailable")

// Embedder turns texts into vectors. Embed returns one vector per input, in order, each of length
// Dim(). Model() and Dim() identify the vectors: a program stores them next to its index and
// refuses to mix vectors when they change, so Model() must name every setting that changes the
// vectors (for a remote API: the endpoint and the model; for a program: the program and its
// arguments).
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

// Pinger is implemented by an embedder that can check, cheaply and before any destructive work,
// that it is reachable and correctly configured.
type Pinger interface {
	Ping(ctx context.Context) error
}

// DocText returns text as e should embed it for indexing: with e's document prefix, or as is when
// e has none.
func DocText(e Embedder, text string) string {
	if p, ok := e.(Prefixer); ok {
		doc, _ := p.Prefixes()
		return doc + text
	}
	return text
}

// QueryText returns query as e should embed it for searching: with e's query prefix, or as is
// when e has none.
func QueryText(e Embedder, query string) string {
	if p, ok := e.(Prefixer); ok {
		_, q := p.Prefixes()
		return q + query
	}
	return query
}

// None is the embedder of the "none" provider: the index holds no vectors and search is FTS only.
// Its Dim is 0, which is how indexers and stores recognise it; Embed is never meant to be called
// and always fails.
type None struct{}

// Embed always fails: an FTS-only index embeds nothing.
func (None) Embed(context.Context, []string) ([][]float32, error) {
	return nil, errors.New("embedder is none: no vectors are computed")
}

// Model reports NoneModel.
func (None) Model() string { return NoneModel }

// Dim reports 0: no vectors.
func (None) Dim() int { return 0 }

// IsNone reports whether e computes no vectors, so the index is FTS only.
func IsNone(e Embedder) bool { return e == nil || e.Dim() == 0 }
