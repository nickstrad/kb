# The same naming and exclusion rules live in several packages
> Reserved names, never-walked directories and the cache file path are copied between packages ("mirroring …"), so each copy gets its own partial tests and they can drift.

- **Smell:** duplicated rule
- **Status:** watch
- **Where:** `internal/cli/add.go:reservedFileNames`/`reservedDirNames` ("mirroring internal/entry's excludedRootFiles/excludedDirs"); `internal/entry/sources.go:neverWalked` and `internal/config/config.go:reservedSourceDirs` (both `.kb`, `.git`, `node_modules`); `internal/embed/cache/cache.go:DefaultPath` and `internal/cli/setup.go:embedCacheFile`
- **Symptoms in tests:**
  - `internal/entry/entry_test.go` (around line 378) keeps a third copy (`excludedDirsHere`) to know what Discover should skip.
  - `cli/embedcache_test.go` (around line 335) checks that `cache.DefaultPath` equals `embedCacheFile`, a test whose only job is to keep two copies in step. `cache.DefaultPath` still exists (cache.go:81) and has no production caller: its only uses are `cache_test.go:TestDefaultPath` (`t.Setenv` for HOME) and that sync check. This is the Q54 shape (`local.DefaultCacheDir`, deleted in W6).
  - No test checks that `add` refuses exactly what `Discover` would skip, or that config refuses exactly what sources never walk.
- **Why it happens:** each package needed the rule and copied it rather than importing it, sometimes to avoid an import edge.
- **Remedy:** Queue **Q76** (claimed `[~]` by sonnet-q-batch-b, a log now STALE, not yet landed) covers the shared table and deleting `cache.DefaultPath`. Still open after Q76: (3) one property test: anything `add` accepts, `Discover` finds. Smallest step first: delete `cache.DefaultPath` and its sync test. The shared table itself is in PLAN 1.5's area (entry, add), so re-check with the orchestrator before touching `entry` or `add.go`.
- **Cost/benefit:** small. It removes a sync-only test and one `t.Setenv`, and turns "mirrors" comments into one rule.
- **Last reviewed:** 2026-10-30 · W7 pass
