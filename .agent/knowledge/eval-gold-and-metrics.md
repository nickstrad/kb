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
threshold and the sweep gate; `eval.holdout` the sweep's held-out share.

**Cross-store gold (W8 pass, Q77).** The primary's log also holds other stores' hits of a
cross-store search. `Verdict.Store` is that hit's canonical ref ("" = this store), and opens of
another store's entry come back in `JudgedSearch.OtherOpens`: `search_opens` has no store column
(Q82, PLAN 4.5), so `Judged` infers it from the search's results at that path (this store's own
hit wins, else the smallest other ref). `kb eval` replays this store only, so `Extract` keeps that
gold apart (`Query.OtherUseful/OtherOpens`, never a negative either) and `Report.OtherStore` counts
it; it is never checked against this index, where a same-named path is another entry.
`eval.Replayable` decides whether the embedder must load.

**Evidence times (6.2, W12).** `Verdict.At` (`search_feedback.ts`) and `JudgedSearch.OpenedAt[path]` (`search_opens.ts`) come back with the log; eval ignores them, the feedback prior (`search/prior.go`) uses them as event times. A damaged ts fails `Judged`.

**Baselines (2.4, W9).** `eval.Baseline` (baseline.go) = version 1 + name, kb version, date, embed
model, entry count, settings JSON+hash and the whole `Report` (per_query first_gold_rank, other_store).
It lives at `<store>/eval/<name>.json` (gitignored by init's `stores/*/eval/`), written atomically;
other versions are refused (Conflict, "rewrite with --save-baseline"), not migrated: it is derived.
`eval.Compare(base, cur)` pairs queries by `Query` key per variant (chunk/entry/opens), per metric
gives delta + paired bootstrap CI (reuses `Bootstrap` on the differences), wins/losses/ties
(|diff| <= 1e-9 ties) and `SignTest` (exact two-sided binomial, log-space). Different k is refused;
embed model, entry count, other-store, fallback differences are warnings. The CLI takes NAME as an
optional positional (`--save-baseline|--compare [NAME]`); `--set` goes through `config.Apply` before
`NewSettings`. A fallback run is never saved. 2.6's sweep can build `Baseline`s in memory and reuse Compare.

**Sweeps (2.6, W10).** `eval.Prepare` reads gold against the index once; `Prepared.run` replays it
per Searcher (Evaluate = Prepare + run). `kb eval --sweep` = `ParseAxis` ([search] keys only, not
k/stores) → `ExpandGrid` (cartesian, first axis slowest, `MaxGrid` 200 checked before applying,
every combo through `config.ApplyAs(c, "--sweep", …)`) → `Gate` on `Prepared.Judged()` (chunk gold
in the index; before the model loads) → `Sweep`. `Split` orders judged keys by SHA-256(seed LE ‖
key) and holds out round(holdout·n) clamped to [1, n−1] (none for n<2 or holdout 0); the seed-1
split of `q00…q39` is pinned in `TestSplitDeterministic`. Ranking: train chunk MRR, nDCG, hit
(tieEps), then the current settings, then grid order. Equal settings hashes share one replay;
the CLI hands every variant one `search.QueryMemo`, so a query is embedded once per sweep
(`TestEvalSweepWiring` counts Embed calls). Rows reuse `Compare` on per-side `subset` reports.
`Overfit` = best ≠ current, wins train MRR, not test. Mutation survivors left in sweep.go are
equivalent (capacity hints, the `[2]float64` literal, unreachable NewSettings error, asserts).

**Why it matters.** 2.4 (baselines) needs `Result.Chunk.FirstRank` per query; 2.6 (sweep) needs a
deterministic query order (`Query.Key`, sorted) to hash for the holdout and a Searcher to swap.

**How to apply.** New variants: add a matcher closure in `evaluate` and a `Variant`. `--set` (2.4):
apply to cfg before `search.NewSettings` in cli/eval.go, as explain does. Tests that need logged
verdicts or opens should read them with `Judged`, not raw SQL. Mutation survivors in
`ScoreRanking`'s postconditions, assertion messages and `discount`'s numerator are equivalent.

`Comparison.Warnings` are data (in `--json`); the CLI prints them on stderr, never in the stdout
table (W9 pass, CODING_STYLE §9). A per-query difference of exactly ±tieEps (1e-9) is a tie
(`TestMetricDiffTieBoundary`). `kb eval --set search.exact_list=false --compare` is the tested way to
measure the exact list (`TestEvalCompareExactListOff`).

**Seen in:** internal/eval (sweep.go), internal/store/judged.go, internal/store/sqlite/judged.go, internal/cli/eval.go · 2026-10-30
