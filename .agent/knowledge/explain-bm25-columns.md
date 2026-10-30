# kb explain and single-column bm25

> Searcher.Explain re-scores the candidate set with (w,0,0)/(0,w,0)/(0,0,w) via Store.FTS; the columns bracket the total (max <= total <= sum), they never add up to it.

**Fact.** FTS5 bm25 sums the column-weighted term frequencies per phrase, then saturates (x/(x+c), concave, 0 at 0). So in magnitude max(columns) <= total <= sum(columns), strict in general (`TestExplainColumnRelation`). `Explain` calls `Store.FTS` directly with `ChunkIDs` = the fused candidate ids, the search's own expression/operator (AND, or OR when the retry fired) and one column weight each; it never goes through `ftsList`. A vec-only candidate that matches the expression still gets column scores (fts rank null). Explain does not log. A tiny corpus where every chunk matches gives idf ~1e-6, so bm25 prints as -0.0000: golden fixtures need non-matching entries.

**Why it matters.** Anyone tuning `search.bm25.*` reads these numbers as a decomposition; they are diagnostic only.

**How to apply.** Test fixtures for score-level output: `newExplainRepo` in internal/cli/explain_test.go. Golden: `go test -tags fts5 ./internal/cli -run Explain -update-explain`.

**Seen in:** internal/search/explain.go, internal/cli/explain.go · 2026-10-30
