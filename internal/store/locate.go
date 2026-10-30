package store

import "context"

// ChunkRef names a chunk by where the search log recorded it: the entry path and heading, never
// the chunk id, which a reindex reuses. AnyHeading ignores Heading and names the entry's first
// chunk (an open records an entry, not a heading).
type ChunkRef struct {
	Path       string
	Heading    string
	AnyHeading bool
}

// ChunkLocator is an optional capability of a Retriever (like Vocab): it maps ChunkRefs from the
// search log back to chunks of the current index, for the query-memory list (PLAN 6.3).
type ChunkLocator interface {
	// LocateChunks returns, for every ref that still resolves, the id of its first chunk by
	// position (lowest ord) whose entry carries tag (normalised as the search filter does; ""
	// means any). A ref whose entry or heading is gone, or whose entry lacks the tag, is absent.
	LocateChunks(ctx context.Context, refs []ChunkRef, tag string) (map[ChunkRef]int64, error)
}
