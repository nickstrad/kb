# Cross-store search merges ranks, keyed by (root, path, ord)

> `search.SearchStores` runs each store's normal search, then `Merge` scores a candidate 1/(rrf_k + its position in that store's fused list); same-location stores sum, ties go primary then canonical ref; chunk ids are per store, so logs and caps key by (store, chunk) and (root, path).

**Fact:**
- Merge uses each store's *fused candidate list* (up to candidate_limit), not its capped/returned hits, then cuts to the primary's candidate_limit, caps per (location root, path), then k. With no extra store `SearchStores` returns the plain search unchanged (no rescoring).
- Identity is (location root, path, ord): two stores of one location indexing the same file sum their terms; the merged candidate is the best-ranked store's (tie: primary, then canonical ref).
- Tie-break is primary first, then canonical ref ascending (not the `--stores` order), so the result does not depend on the order stores are named in (TESTING §5.2 property `TestMergeStoreOrderProperty`, `FuzzMerge`).
- `search_results.store`/`search_candidates.store` (v5) are NULL for the primary and NAME@ABSROOT otherwise; `chunk_id` is that store's, so `returned` is decided per (store, chunk_id) in sqlite/log.go. `RecordOpen` matches `r.store IS ?` and, from v6, writes `search_opens.store` (key search_id, store, path), so one path opened from two stores is two opens.
- Hit tags (`Hit.Store`, `render.Hit.Store`) are the ref as the user typed it, rendered `store:path`, the form `kb show REF:path` accepts.
- The query is embedded once per (model, dim, prefixed text) through `search.QueryMemo`; the CLI builds one embedder per distinct `[embed]` config (`embedderSet`).

- Every search, even a single-store one, runs in a `search.fanOut` goroutine. A panic there is handed back as `errs.WorkerPanic{Value, Stack}` and raised again on the command goroutine, so `cli.Execute`'s recover still exits 3 with the worker's stack (`TestSearchStoresWorkerPanic`, `TestSearchWorkerPanicExitsInternal`). New worker goroutines must do the same.
- `kb eval` and `kb explain` never fan out; `all` is reserved (`workspace.AllName`) like `global`.

**Why it matters:** comparing raw scores across stores, or keying anything by chunk id alone, silently merges unrelated chunks.

**How to apply:** new per-hit log columns or caps must key by store as well; a new backend only needs to return an ordered list. Harness: `stores-fanout` (needs the `harnessfake` build tag for `fake:<dim>` models).

A skipped extra store's warning names the store once: `cli.skipReason` unwraps the error whose Op is
`search.OpSearchStores` (its message is "store REF: …") instead of trimming text; wrap an extra's
failure with that Op and nothing else.

**Seen in:** internal/search/multi.go, internal/store/sqlite/log.go, internal/cli/fanout.go · 2026-10-30
