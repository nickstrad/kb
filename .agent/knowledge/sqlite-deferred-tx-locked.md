# Deferred transactions bypass busy_timeout: concurrent kb writers get "database is locked"

> `BeginTx(ctx, nil)` starts a DEFERRED transaction. In WAL mode a writer whose read snapshot has gone stale gets SQLITE_BUSY at once, without the 5 s busy_timeout.

**Fact.** `store/entries.go` opens write transactions with `s.db.BeginTx(ctx, nil)`, which is a
DEFERRED transaction in mattn/go-sqlite3. It reads first and takes the write lock at the first
write. If another process committed in between, SQLite returns SQLITE_BUSY (BUSY_SNAPSHOT) at
once and does not call the busy handler, because waiting could not help. Six parallel
`kb add --embedder none` runs on a fresh root failed 2–4 of 6 with `database is locked` within
about 30 ms (harness `concurrent-fresh-db`, 2026-10-29). `migrate` does not have the problem
because it uses `BEGIN IMMEDIATE`.

**Why it matters.** Several agents calling `kb add` or `kb reindex` at the same time is kb's
normal use. `kb add` rolls back cleanly, but the entry is not added.

**How to apply.** Take the write lock up front for every write transaction: add `_txlock=immediate`
to the DSN in `store.Open` (mattn supports it), or `BEGIN IMMEDIATE` on a pinned conn. The
regression test belongs in store: two `Store` handles on one file, concurrent `ReplaceEntryChunks`.
Re-run `go run ./.agent/scripts/adversarial/harness -run concurrent` to confirm.

**Status (2026-10-29, 0.5b):** fixed with `_txlock=immediate` in the store DSN; regression test
`TestConcurrentWritersFromTwoHandles` (internal/store/concurrent_test.go) fails without it.
Any new handle that writes (e.g. the embed cache's own DSN) needs the same flag. The harness
`concurrent-fresh-db` no longer sees "locked"; its remaining flake is the reindex prune race (Q22).

**Seen in:** internal/store/entries.go:175,333,419; internal/store/store.go (DSN) · 2026-10-29

**Related quirks (3.3a, 2026-10-29):**
- A read-only SQLite file that's already in WAL mode still opens, and its schema transaction
  succeeds. Writes fail only at the first write, so a read-only cache degrades then, not at `Open`.
- `url.URL{Scheme:"file", Path:":memory:"}` builds a URI that mattn/go-sqlite3 rejects, so
  in-memory databases can't exercise a file-URI code path such as the WAL switch. Use a file in
  `t.TempDir()`.
- The embedding cache uses `_txlock=immediate` in its DSN for exactly this reason.
