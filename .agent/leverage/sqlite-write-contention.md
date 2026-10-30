# SQLite write contention
> Several kb processes (agents, editors, a reindex) write one store at once; locking mistakes lose data or stall commands.

- **Category:** contention
- **Status:** validated (2026-10-29)
- **Where:** `internal/store` (DSN: WAL, `_busy_timeout=5000`, `_txlock=immediate`), `internal/reindex` (`All` discovers, then prunes), `internal/search/log.go` (a search log write on every search), `internal/embed/cache`.
- **Why it matters:** many agents share one store, so concurrent add/search/reindex is the normal case. Before 0.5b, deferred transactions skipped the busy timeout and lost entries.
- **Evidence:** the adversarial harness `concurrent-fresh-db` found "database is locked" (fixed in 0.5b with IMMEDIATE transactions; knowledge: `sqlite-deferred-tx-locked.md`). The reindex-prune race (Q22: `reindex --all` deleted an entry a concurrent `kb add` committed after its Discover) is fixed in the W2 pass: `All` discovers again after `ListEntries` and prunes only rows neither discovery found (knowledge: `reindex-prune-rediscover.md`). `concurrent-fresh-db` then passed 7/7 runs (it failed about 1 in 3 before).
- **Coverage today:** `TestConcurrentWritersFromTwoHandles` (store, `-race`); `TestAllKeepsEntryAddedDuringRun` (reindex, deterministic: the embedder plays the concurrent add after Discover); harness `concurrent-fresh-db` (one shot per run; repeats are manual until H13); cache concurrency tests.
- **Next checks:** `-repeat N` for racy scenarios (H13) and a 20× run per wave; `kb add` vs `kb rm` of the same entry and `reindex --all` while a slow search holds a read transaction (H13); p95 search latency while a reindex runs (does the search-log write wait behind a long reindex transaction?); cross-store fan-out (Phase 3) opening many stores; the shared embedding cache across processes once wired (H19).
- **Last reviewed:** 2026-10-29 · W2 wave-end pass
