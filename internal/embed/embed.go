// Package embed is kb's view of the embedding contract, so the embedders (embed/local, the
// FTS-only None, the providers of the public github.com/nickstrad/kb/embed registry, and the
// test double embed/fake) are interchangeable. The contract itself lives in the public package;
// the names here are aliases of it, so an embedder written against either is the same type.
// What is kb's own lives here: the caching wrapper (Cached) and the shared cache it uses.
//
// Some models are asymmetric: they want stored documents and incoming queries to carry different
// task prefixes. Callers never prepend a prefix themselves; they go through DocText and
// QueryText, which ask the embedder for its prefixes (the Prefixer interface). An embedder that
// does not implement Prefixer gets the text as is.
package embed

import (
	pub "github.com/nickstrad/kb/embed"
)

// DocPrefix and QueryPrefix are nomic-embed-text's task prefixes. Nothing applies them by default
// any more (the local model table carries each model's own); they remain only because tests in
// search and reindex build fixtures with them. Delete them once those tests stop.
const (
	DocPrefix   = "search_document: "
	QueryPrefix = "search_query: "
)

// NoneModel is the model name None reports and embed_meta records for an FTS-only index.
const NoneModel = pub.NoneModel

// ErrUnavailable marks failures that mean the embedder cannot be used at all right now: its
// model is not downloaded or fails to load, its server cannot be reached, its program crashed.
// Callers use errors.Is(err, ErrUnavailable) to fall back to FTS-only search (exit code 2 / mode
// fts-fallback). It does not cover the caller's own ctx being cancelled, or a wrong vector count
// or width, since falling back would not help. It is the public package's sentinel.
var ErrUnavailable = pub.ErrUnavailable

// Embedder turns texts into vectors. Embed returns one vector per input, in order, each of length
// Dim(). Model() and Dim() identify what is stored in the embed_meta table: a mismatch between
// embed_meta and the configured embedder means the index has to be rebuilt.
type Embedder = pub.Embedder

// Prefixer is implemented by an embedder whose model wants a task prefix on documents and
// queries. An empty prefix means the text is sent as is.
type Prefixer = pub.Prefixer

// None is the embedder for [embed] model = "none": the index holds no vectors and search is FTS
// only. Its Dim is 0, which is how the indexer and the store recognise it.
type None = pub.None

// DocText returns text as e should embed it for indexing: with e's document prefix, or as is when
// e has none.
func DocText(e Embedder, text string) string { return pub.DocText(e, text) }

// QueryText returns query as e should embed it for searching: with e's query prefix, or as is
// when e has none.
func QueryText(e Embedder, query string) string { return pub.QueryText(e, query) }

// IsNone reports whether e computes no vectors, so the index is FTS only.
func IsNone(e Embedder) bool { return pub.IsNone(e) }
