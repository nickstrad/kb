# Shared embedding cache file
> One SQLite file under the user cache directory is shared by every kb process and project on the machine.

- **Category:** contention · data integrity
- **Status:** watch
- **Where:** `internal/embed/cache`, `internal/embed/cached.go`.
- **Why it matters:** it is the only state shared across projects. A wrong key mixes vector spaces, and a stuck lock stalls every project's indexing.
- **Evidence:** the key is `(model, dim, sha256(prefixed text))`; it degrades to off after the first error; IMMEDIATE transactions; a newer schema is refused untouched. `Model()` must change if int8 ships, or vectors mix. W2 pass mutation (`-diff main`): 79.6%, survivors are `Open`/`init` error paths (directory is a file, schema create) and WAL-retry timing constants; the duplicate-text copy in `Cached.Embed` survivors are equivalent.
- **Coverage today:** 92.5% coverage; `-race` tests with multiple handles; fuzz equivalence with the uncached embedder.
- **W6 (1.8a):** the cache schema is a `migrate.Steps` (`cache/0001-baseline`) applied on open in one transaction; `cache.Artefact` exposes status/backup/verify to `kb migrate` (`TestCacheArtefact`, `TestCacheWrapKinds`). Its damage messages name `kb model cache --clear`, which 3.3b must add.
- **Next checks:** a two-process cache scenario that actually writes vectors (needs an offline fake embedder in the harness binary, or `-tags model` in a manual run): `cache-two-stores` today probes the file, not reuse or write contention (narrow H19 to that); add `+int8` to `local.Model()` before int8 ships; cache size growth over a month of use (`kb model cache --stats` now reports it); whether stale model rows need pruning when the default model changes (`--clear` is all-or-nothing).
- **W7 (3.3b):** wired in `cli/setup.go` (`withEmbedCache`); open errors, a newer or garbage file and a mid-run failure each warn and fall back (`TestEmbedCacheDegrades`, `TestEmbedCacheGoesOffMidRun`); two stores share one file (`TestEmbedCacheSharedAcrossStores`, `-race` `TestEmbedCacheConcurrentReindex`); `kb model cache --stats|--clear`; harness `cache-two-stores` (no model offline, so it probes the file, not reuse). `+int8` is still not in `local.Model()`: add it before int8 ships.
- **W7 wave-end pass:** the `Open` error paths are now user-reachable and table-tested through the CLI (`TestEmbedCacheDegrades`: open error, newer, garbage; `TestEmbedCacheGoesOffMidRun`); `TestEmbedCacheSharedAcrossStores` and `-race` `TestEmbedCacheConcurrentReindex` pass. The cache schema migrates on open (it is a per-machine derived file, exempt from the `kb migrate up` refusal).
- **Last reviewed:** 2026-10-30 · W7 wave-end pass
