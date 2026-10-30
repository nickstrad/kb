# Prune only what is gone at prune time

> `reindex --all` must not prune by its start-of-run Discover: a concurrent `kb add` commits rows the run never discovered. Discover again after ListEntries.

**Fact.** `reindex.All` discovers entries, reindexes them, then lists DB rows and deletes those
not on disk. A `kb add` that copies its file after that Discover and commits its row before
`ListEntries` looked "not on disk" and was pruned (Q22; harness `concurrent-fresh-db` failed about
1 run in 3). Every writer puts the file on disk *before* committing the row, so any row
`ListEntries` returns had its file present before `ListEntries` ran. A second Discover after
`ListEntries` therefore sees it. `All` prunes only rows that neither discovery found, and runs
the second Discover only when there are prune candidates.

**Why it matters.** The race silently deletes a just-added entry from the index; nothing reports
it until a search misses it. Any future "derive the index from disk, then delete the rest" loop
(in-place sources in 1.5, cross-store sync) has the same shape.

**How to apply.** Keep the order "write the file, then commit the row" in every writer. In any
prune loop, re-check existence after reading the rows to delete. To test such a race
deterministically, let a fake embedder (called after Discover, before the prune) play the
concurrent writer: `TestAllKeepsEntryAddedDuringRun`.

**Seen in:** internal/reindex/reindex.go (All, discover), internal/reindex/prune_test.go · 2026-10-29
