# The same naming and exclusion rules live in several packages
> Reserved names, never-walked directories and the cache file path are copied between packages ("mirroring …"), so each copy gets its own partial tests and they can drift.

- **Smell:** duplicated rule
- **Status:** watch
- **Where:** `internal/cli/add.go:reservedFileNames`/`reservedDirNames` ("mirroring internal/entry's excludedRootFiles/excludedDirs"); `internal/entry/sources.go:neverWalked` and `internal/config/config.go:reservedSourceDirs` (both `.kb`, `.git`, `node_modules`); `internal/embed/cache/cache.go:DefaultPath` and `internal/cli/setup.go:embedCacheFile`
- **Symptoms in tests:**
  - `entry_test.go` keeps a third copy (`excludedDirsHere`) to know what Discover should skip.
  - `cli/embedcache_test.go` (around line 335) checks that `cache.DefaultPath` equals `embedCacheFile`, a test whose only job is to keep two copies in step. `DefaultPath` has no production caller, and `cache_test.go:TestDefaultPath` uses `t.Setenv` (HOME) to test it. This is the Q54 shape (`local.DefaultCacheDir`, deleted in W6).
  - No test checks that `add` refuses exactly what `Discover` would skip, or that config refuses exactly what sources never walk.
- **Why it happens:** each package needed the rule and copied it rather than importing it, sometimes to avoid an import edge.
- **Remedy:** (1) Delete `cache.DefaultPath` (cli owns the path rule through the `workspace` cache base). (2) `entry` exports the one exclusion predicate (`entry.Reserved(name)` / `entry.NeverWalked(name)`), and `add` and `config` call it (entry imports config, so the shared table lives in config or a leaf package, and entry and add call it). (3) Add one property test: anything `add` accepts, `Discover` finds. → **1.5** owns `entry` and `add.go` in W7 (in progress, so re-check). Otherwise queue (1) as a low finding and (2)–(3) for the W7 wave-end pass.
- **Cost/benefit:** small. It removes a sync-only test and one `t.Setenv`, and turns "mirrors" comments into one rule.
- **Last reviewed:** 2026-10-30 · testability first pass (1.5 was editing entry/add.go; re-check)
