# kb eval: gold, metrics and the log read API

> Gold comes from store.JudgedLog (useful verdicts on (path, heading); opens a separate variant); eval.ScoreRanking credits each gold item once; bootstrap is PCG-seeded and deterministic.

**Fact.** `store.JudgedLog` (internal/store/judged.go, implemented in sqlite/judged.go) is the log's
read side: every search in a since/caller window with its current verdicts (joined to the
`search_results` row at that rank for path and heading, never chunk_id), `--none` and opens.
`eval.Extract` is pure: groups by `Normalize` (lower, collapsed whitespace), unions gold, replays
the newest search's text, counts each search once (useful > none > not-useful-only > unjudged).
`eval.Evaluate` checks gold against the index (one Entry+Chunks read per path), replays through
`eval.Searcher` (a `*search.Searcher` without Log) and scores three variants: chunk (headline),
entry, opens. `ScoreRanking(matches, nGold, k)` works on gold indexes, so the matching rule and
the metric are separate; a gold item earns gain once (several chunks of one gold path cannot push
nDCG over 1). `Bootstrap` = percentile method over PCG(seed, seed^const), 1000 resamples, seed 1.
`kb eval` loads the embedder only when some query has gold. `eval.min_judged` is the small-sample
threshold (holdout stays reserved for the 2.6 sweep).

**Cross-store gold (W8 pass, Q77).** The primary's log also holds other stores' hits of a
cross-store search. `Verdict.Store` is that hit's canonical ref ("" = this store), and opens of
another store's entry come back in `JudgedSearch.OtherOpens`: `search_opens` has no store column
(Q82, PLAN 4.5), so `Judged` infers it from the search's results at that path (this store's own
hit wins, else the smallest other ref). `kb eval` replays this store only, so `Extract` keeps that
gold apart (`Query.OtherUseful/OtherOpens`, never a negative either) and `Report.OtherStore` counts
it; it is never checked against this index, where a same-named path is another entry.
`eval.Replayable` decides whether the embedder must load.

**Why it matters.** 2.4 (baselines) needs `Result.Chunk.FirstRank` per query; 2.6 (sweep) needs a
deterministic query order (`Query.Key`, sorted) to hash for the holdout and a Searcher to swap.

**How to apply.** New variants: add a matcher closure in `evaluate` and a `Variant`. `--set` (2.4):
apply to cfg before `search.NewSettings` in cli/eval.go, as explain does. Tests that need logged
verdicts or opens should read them with `Judged`, not raw SQL. Mutation survivors in
`ScoreRanking`'s postconditions, assertion messages and `discount`'s numerator are equivalent.

**Seen in:** internal/eval, internal/store/judged.go, internal/store/sqlite/judged.go, internal/cli/eval.go · 2026-10-30
