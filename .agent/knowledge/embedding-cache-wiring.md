# Embedding cache wiring: where it opens, closes and warns

> `deps.newEmbedder` wraps every non-none embedder (except for doctor) with `embed.Cached`; `execute` closes the cache files and prints late warnings once the command returns.

**Fact.** `deps.embedCachePath` (production: `<CacheBase>/kb/embed-cache.sqlite`, nil in `testDeps`) is opened in `withEmbedCache` (cli/setup.go). An Open error is one stderr warning ("embedding cache not used ... kb model cache --clear") and the plain embedder. Opened caches are tracked in `deps.caches`; `execute` calls `finishEmbedCaches` after cobra returns, which warns if `CachedEmbedder.Err()` is set and closes the files. `reindexRun` replaces `Summary.EmbedCalls` with `Stats().ModelCalls`, so "embed calls: 0" means no model call. `CachedEmbedder` has no `Ping`; `cachedPinger` re-adds it when the inner embedder has one (add, reindex, init look for it). `Open` enables WAL before it reads the version, so a newer-schema file changes a header byte but keeps its `user_version`.

**Why it matters.** A command that builds two embedders gets two cache handles; concurrent `execute` calls on one `deps` would close each other's caches (tests use a `deps` copy with its own `caches`). Wrapping a new command's embedder elsewhere would skip the close.

**How to apply.** Build embedders only through `newEmbedder`. A test that wants the cache sets `testDeps.embedCachePath` and restores it (`useEmbedCache` in cli/embedcache_test.go). `kb model cache --clear` empties a healthy cache and deletes an unopenable one with its -wal/-shm files.

**Seen in:** internal/cli/setup.go, internal/cli/model.go, internal/cli/root.go · 2026-10-30
