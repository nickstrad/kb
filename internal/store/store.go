// Package store is the backend-neutral seam between kb's services and its database: the small
// interfaces the services call (Index, Retriever, SearchLog) and the row and value types they
// exchange. It holds no SQL. The SQLite backend lives in internal/store/sqlite; the contract
// every backend must meet is internal/store/storetest.
//
// Errors: every method returns an *errs.Error at the boundary. NotFound, Conflict and Usage mark
// known edge cases (no such search, an embed_meta mismatch, a rank out of range); a driver
// failure is Internal.
package store

import (
	"context"
	"time"
)

// Store is one open knowledge database: everything a command may need from it.
type Store interface {
	Index
	Retriever
	SearchLog
	Path() string
	ReadOnly() bool
	Close() error
}

// Index is the entry and chunk side: what reindex, add, edit, rm and show use.
type Index interface {
	// EmbedMeta returns the recorded embedder; ok is false on a database never indexed.
	EmbedMeta(ctx context.Context) (m EmbedMeta, ok bool, err error)
	// EnsureEmbedMeta records m on a fresh index, or checks it against the record: a different
	// model or dim is a Conflict wrapping *EmbedMetaMismatchError. It creates the vector table at
	// m.Dim lazily.
	EnsureEmbedMeta(ctx context.Context, m EmbedMeta) error
	// ResetIndex empties every entry, chunk, vector and the embed_meta record. It never touches
	// the search log.
	ResetIndex(ctx context.Context) error
	// Entry returns the entry at path, or (nil, nil) when there is none.
	Entry(ctx context.Context, path string) (*EntryRow, error)
	// Entries returns every entry sorted by path.
	Entries(ctx context.Context) ([]EntryRow, error)
	// Chunks returns one entry's chunks in ord order.
	Chunks(ctx context.Context, entryID int64) ([]ChunkRow, error)
	// Embeddings returns the stored vectors of an entry's chunks keyed by text_hash; empty on an
	// index without vectors.
	Embeddings(ctx context.Context, entryID int64) (map[string][]float32, error)
	// ReplaceEntry writes an entry and its complete chunk set in one transaction. Every chunk must
	// carry an embedding of the recorded dim (none on an FTS-only index); otherwise nothing is
	// written and the error is a Conflict.
	ReplaceEntry(ctx context.Context, e EntryInput, chunks []ChunkInput) (entryID int64, err error)
	// DeleteEntry removes an entry and everything indexed for it. A missing entry is not an error.
	DeleteEntry(ctx context.Context, path string) error
}

// Retriever is the backend's retrieval primitives. Fusion, the diversity cap and the AND→OR
// retry stay in internal/search.
type Retriever interface {
	// HasVectors reports whether the index can answer KNN queries at all.
	HasVectors(ctx context.Context) (bool, error)
	// FTS runs one full-text query. A query whose terms all sanitise away runs nothing and
	// returns an empty Match.
	FTS(ctx context.Context, q FTSQuery) (FTSResult, error)
	// KNN returns the q.K nearest chunks by distance, ascending.
	KNN(ctx context.Context, q KNNQuery) ([]VecRow, error)
	// ChunkMeta reads the display data of the given chunks. Ids that no longer exist are absent.
	ChunkMeta(ctx context.Context, ids []int64) (map[int64]ChunkMeta, error)
}

// SearchLog is the search log: searches, their results and candidates, feedback and opens.
type SearchLog interface {
	// LogSearch writes one search with its results and candidates in one transaction.
	LogSearch(ctx context.Context, r SearchRecord) (searchID int64, err error)
	// Feedback records a verdict on one returned hit; it replaces an earlier verdict on the same
	// rank and, when useful, withdraws a whole-search --none. No such search is NotFound; a rank
	// outside 1..n_returned is Usage.
	Feedback(ctx context.Context, searchID int64, rank int, useful bool, note string) error
	// FeedbackNone records that nothing the search returned was useful. A search with a useful
	// verdict is a Conflict.
	FeedbackNone(ctx context.Context, searchID int64, note string) error
	// LastSearch returns the newest search logged by caller, or NotFound.
	LastSearch(ctx context.Context, caller string) (LastSearch, error)
	// RecordOpen attaches a read of entryPath at now to the newest search at most window old
	// that returned it. recorded is false when there is none or the open is already on record.
	RecordOpen(ctx context.Context, entryPath string, now time.Time, window time.Duration) (recorded bool, err error)
	// QueryLog runs read-only analytic SQL in the backend's dialect (kb stats).
	QueryLog(ctx context.Context, sql string, args ...any) (*Table, error)
}
