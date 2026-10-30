# kb search and kb explain each carry their own copy of the search setup
> About 45 lines (mode check, index stat, store open, embedder build, none→fts, embed_meta mismatch, settings) live in both handlers, and each copy has a different untested branch.

- **Smell:** duplicated rule · decision+effect mixed
- **Status:** needs work
- **Where:** `internal/cli/search.go:runSearch`, `internal/cli/explain.go:runExplain`; also `search.go:printResults`/`renderHits` (take the whole `store.Store` to call `Entry`)
- **Symptoms in tests:**
  - The mode/embedder decision (hybrid|vec × none|real × meta matches|differs) can only be tested through a full CLI run on a populated store (`newSearchTestRepo`).
  - The two copies are covered differently. `TestSearchEmbedMetaMismatch` covers the mismatch for search only. `TestExplainFlagErrors` ("vec with none embedder") covers vec+none for explain only. No search test hits "--mode vec needs an embedder" (0 hits), and no explain test hits a mismatch.
  - Q63 (low-findings) records the duplication. 2.2 could not edit `cli/search.go` during W7.
  - `printResults` needs a real store for the per-hit summary lookup (knowledge `render-budget.md`), so `TestSearchBudgetElastic` builds a database to test rendering.
- **Why it happens:** the setup is inline in each RunE, and the decision is interleaved with opening the store and building the embedder.
- **Remedy:** (1) A pure `resolveMode(mode, embedder, stored EmbedMeta, ok bool) (string, error)`, table-tested once per branch outcome. (2) `d.openSearcher(cmd, cfg, mode) (store.Store, *search.Searcher, string, error)`, called by both commands. (3) `renderHits` takes `interface{ Entry(ctx, path) (*store.EntryRow, error) }`. → **3.2** (W8 owns `cli/search.go`; cross-store search would otherwise add a third copy).
- **Cost/benefit:** small, about 1 file plus a table test. It removes about 45 duplicated lines, closes both untested branches, and gives 3.2 one place to fan out.
- **Last reviewed:** 2026-10-30 · testability first pass
