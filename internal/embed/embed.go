// Package embed defines the embedding interface the indexer and the searcher use, so the Ollama
// client (P3.1) and the deterministic test double (embed/fake) are interchangeable.
//
// nomic-embed-text is asymmetric: stored documents and incoming queries carry different prefixes.
// Every caller must prepend DocPrefix when embedding a chunk and QueryPrefix when embedding a
// search query, or the distances come out subtly wrong.
package embed

import "context"

// Prefixes required by nomic-embed-text.
const (
	DocPrefix   = "search_document: "
	QueryPrefix = "search_query: "
)

// Embedder turns texts into vectors. Embed returns one vector per input, in order, each of length
// Dim(). Model() and Dim() identify what is stored in the embed_meta table: a mismatch between
// embed_meta and the configured embedder means the index has to be rebuilt.
type Embedder interface {
	Embed(ctx context.Context, texts []string) ([][]float32, error)
	Model() string
	Dim() int
}
