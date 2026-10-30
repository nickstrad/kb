# Tests outside the sqlite package read and write its schema directly
> 13 `(*sqlite.Store).DB()` uses (15 at W7) in cli, search, reindex and stats tests run raw SQL against table and column names, so a schema change breaks tests far from the backend.

- **Smell:** hidden coupling · shared fixture
- **Status:** needs work
- **Where:** `internal/cli/search_cmd_test.go:disableSearchLog` (`ALTER TABLE searches RENAME TO searches_disabled`), `cli/stats_test.go` (`UPDATE searches SET ts`), `cli/edit_regression_test.go` (`UPDATE embed_meta`), `cli/settings_log_test.go`, `cli/show_open_test.go`, `cli/feedback_test.go`, `cli/index_hooks_test.go`, `search/search_test.go:dbOf` (casts `Searcher.Store` to `*sqlite.Store`), `reindex/reindex_test.go:countRows`; the write-only `store.SearchLog` interface
- **Symptoms in tests:**
  - `TestSearchLogFailure` and `TestSearchFallbackLogFailurePrintsBothWarnings` make `LogSearch` fail by renaming the `searches` table. A wrapper would do it without the schema: `reindex_decision_test.go:interruptingStore` already embeds `store.Store` and overrides one method.
  - Tests read logged rows (`settings`, `settings_hash`, `fts_query`, `search_opens`, `caller`) with SQL because `SearchLog` has no read method for one search (only `LastSearch`).
  - Every migration that renames or splits a log column (v5 3.2, v6 4.5, v8 6.2 are queued) must edit tests in cli and search as well as in sqlite.
  - `internal/stats/queries_test.go:fixtureSQL` is exempt: stats *is* SQL over the log.
- **Why it happens:** the 1.2 store seam made production code backend-neutral, but test setup and verification went straight to the SQLite handle, because the seam offers no way to read back or to fault a write.
- **Remedy:** (1) A `failingLog{store.Store}` wrapper for log-failure tests, which drops the ALTER TABLE (cheap, now). (2) A test-support package `internal/store/sqlite/sqlitetest` with the few verbs tests need (`LoggedSearch(id)`, `SetSearchTime`, `ForceEmbedMeta`), so schema SQL lives next to the schema. (3) If 2.3 adds a real read API for logged searches (eval needs one), use it instead. → **2.3** (W8, new `sqlite/judged.go`) for the read side; (1)–(2) as a **new item** (Sonnet, tests only) or with `cli-shared-test-deps`.
- **Cost/benefit:** small to medium. Schema migrations stop breaking cli and search tests, and the failure tests say what they mean.
- **W8 pass:** the read side landed: `store.JudgedLog.Judged` (2.3) returns verdicts (with their hit's store), --none and opens, and storetest's `judgedLog` checks it for every backend. `openRows` (show_open_test.go) now reads through it and the stats fixture places searches with `Searcher.Now` instead of `UPDATE searches SET ts` (Q79 partial). Left: `feedbackRows` checks notes and raw row counts that `Judged` collapses; the write-side fault (`disableSearchLog` renames `searches`); `fanout_test.openRaw` reading `search_results.store`. Count: `rg -c '\.DB\(\)' internal -g '*_test.go'` outside internal/store/sqlite = 13 in 10 files.
- **Last reviewed:** 2026-10-30 · W8 wave-end pass → remaining remedy is T1 step 5 (`failingLog` wrapper, `sqlitetest` helpers)
