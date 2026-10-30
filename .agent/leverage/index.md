# Leverage index

Rules: README.md and AGENTS.md "Leverage points". At most about 10 active entries.

- [sqlite-write-contention](sqlite-write-contention.md) — validated (2026-10-30) · several kb processes writing one store: locking, busy waits and reindex/add races
- [schema-migrations](schema-migrations.md) — watch · forward-only migrations (internal/migrate + adapters) must preserve the search log; lock/backup/apply and declared effects must be right
- [embedding-model-lifecycle](embedding-model-lifecycle.md) — watch · model load (~0.2 s per command, one per distinct model in a fan-out search), ~700 MB peak while embedding, download and cache integrity
- [reindex-throughput](reindex-throughput.md) — watch · ~33 ms per chunk dominates indexing; reuse by text hash, batch size and the shared cache decide cost; ResolveEdges adds a corpus-sized pass to every add/edit (29 ms at 5k entries)
- [embedding-cache-shared-file](embedding-cache-shared-file.md) — watch · one SQLite file shared by every project on the machine (wired in 3.3b); must degrade, never corrupt or stall
- [search-latency-and-scale](search-latency-and-scale.md) — watch · query embed + FTS + brute-force vec0 KNN per store; KNN cost grows linearly with chunk count and a fan-out search runs up to 9 stores (unmeasured); Related/--neighbors index-bounded since the W11 pass (0.7–0.9 ms at 5k entries); with [prior] on, every search reads the whole judged log (≈ 32 ms read + 4 ms index at 10k judged searches, Q146); kb eval reads it once per run and slices per replay (W13); phrase/NEAR tiers add up to 3 FTS statements per 2+-word search (+0.5 ms typical, +5 ms near-stopword at 5k entries, after the W13-pass `+rowid` fix)
- [filesystem-boundary](filesystem-boundary.md) — watch · discovery, symlinks, in-place sources: case-fold overlap and lazy Resolve fixed in the W7 pass; excludes do not prune the walk (Q69); Resolve still walks every source (stat only)
- [process-interruption](process-interruption.md) — watch · Ctrl-C or kill mid-reindex, mid-download or mid-migration must leave a consistent store

## Archive

- [archive](archive/README.md) — retired entries, with why they were retired and how each was resolved
