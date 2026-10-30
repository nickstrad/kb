// Package cache is kb's machine-wide embedding cache: a small SQLite file that maps
// (model, dim, sha256(text)) to a vector, so the same text is never embedded twice on one
// machine, whichever store or project asks.
//
// Inputs: a model identity (the embedder's Model(), which must carry every setting that changes
// vectors, as embed_meta does), a dimension, and the exact texts sent to the model (task prefix
// included). Outputs: the cached vectors, or nil for a miss.
//
// Invariants:
//   - A hit has exactly dim floats; a stored blob of any other length reads as a miss and is
//     overwritten by the next Put of that key.
//   - Several processes may open the same file at once: the file is in WAL mode, every
//     connection waits up to busyTimeout for a lock, and writes take the write lock up front.
//   - The schema version lives in PRAGMA user_version. A file with a newer version than this
//     build knows is an error from Open; the caller then runs without a cache.
//   - Nothing is evicted. Clear empties the cache.
//
// The cache is an optimisation: callers (embed.Cached) treat every error from it as "no cache"
// and embed instead.
package cache
