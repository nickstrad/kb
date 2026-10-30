# Shared embedding cache file
> One SQLite file under the user cache directory is shared by every kb process and project on the machine.

- **Category:** contention · data integrity
- **Status:** watch (package built in 3.3a; not wired until 3.3b)
- **Where:** `internal/embed/cache`, `internal/embed/cached.go`.
- **Why it matters:** it is the only state shared across projects. A wrong key mixes vector spaces, and a stuck lock stalls every project's indexing.
- **Evidence:** the key is `(model, dim, sha256(prefixed text))`; it degrades to off after the first error; IMMEDIATE transactions; a newer schema is refused untouched. `Model()` must change if int8 ships, or vectors mix. W2 pass mutation (`-diff main`): 79.6%, survivors are `Open`/`init` error paths (directory is a file, schema create) and WAL-retry timing constants; the duplicate-text copy in `Cached.Embed` survivors are equivalent.
- **Coverage today:** 92.5% coverage; `-race` tests with multiple handles; fuzz equivalence with the uncached embedder.
- **Next checks:** before 3.3b wiring, an `Open` error-path table (PLAN 3.3 carried note), since wiring makes those paths user-reachable; after 3.3b, harness scenarios with two projects reindexing at once and a garbage/newer cache file (H19); cache size growth over a month of use; whether stale model rows need pruning when the default model changes.
- **Last reviewed:** 2026-10-29 · W2 wave-end pass
