# chunks_vec width is lazy (schema 3)

> chunks_vec exists only after EnsureEmbedMeta(model, dim>0), at FLOAT[dim]; test embedders may use any width, and code touching chunks_vec must handle "no such table".

**Fact:** Since PLAN 0.5b (migration 2→3) a fresh or upgraded database has no `chunks_vec`.
`schema.sql` still creates a FLOAT[768] table, but migration 3 drops it. `EnsureEmbedMeta` creates
it at the model's width (drops a leftover table when no embed_meta row exists); `ResetForReindex`
drops it; `none`/dim 0 never has one. `store.HasVectors` and `search.Searcher.hasVecTable` check
`sqlite_master`. Hybrid search with no table falls back to FTS with `search.ErrNoVectors`.
`rankedEmbedder` (internal/search/boundary_test.go) can pin exact vec ranks with `v[0]=1,
v[1]=n/1000` at any width.

**Why it matters:** a query like `SELECT count(*) FROM chunks_vec` in a test, or a new code path
that writes vectors, fails with "no such table" on FTS-only and not-yet-reindexed stores.

**How to apply:** call EnsureEmbedMeta before writing vectors; in tests, use small widths (16)
unless the fixture records a real model whose width it must match; check for the table before
reading it.

**Seen in:** internal/store/sqlite/index.go, internal/store/sqlite/sqlite.go (migration 3), internal/search/search.go · 2026-10-29
