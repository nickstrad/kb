# Eval replay leakage: asOf plus leave-own-query-out

> A replay with the prior or query memory runs as of its newest search's ts AND without its own query's searches as evidence; asOf alone leaks, because eval's gold is the union over every search of the query.

**Fact.** `eval.Extract` groups searches by `Normalize(query)` and unions their gold; the replay
runs the newest search's text. With `AsOf = newest ts` only, a verdict on an *earlier* search of
the same query (recorded before the newest search) is both gold and prior/memory evidence: the
query memory would hand the label straight back (circular MRR gains). So the replay also sets
`Request.ExcludeEvidence = Query.SearchIDs`: the prior and memory learn only from other queries'
feedback recorded strictly before the original search. A verdict on the replayed search itself is
always after its ts, so asOf alone already drops it; the exclusion matters for the earlier ones
(`TestEvalPriorLeakage` row "W's earlier search judged before W's newest search" is the row only
the exclusion kills). Memory then helps in eval only across queries that share a MemoryKey but
not a Normalize key (word order, punctuation), which under-counts its value for literal repeats.

"Own query" means the query's searches in the **evidence log**, not only in the evaluated window.
`kb eval --since/--caller` scores a window but the prior reads the whole log, so a same-query
search outside the window (another caller, or older) carried the same gold into the evidence
(W13 pass). `eval.Options.Evidence`/`SweepOptions.Evidence` take the log the prior reads; `run`
leaves out `leaveOut(q.SearchIDs, searchesByKey(evidence)[q.Key])`
(`TestEvaluateLeavesOutWholeLogQuery`, `TestEvalPriorWindowLeavesOutQuery`).

**Why it matters.** The owner decides whether to turn the prior on from `kb eval --set
prior.enabled=true --compare`; leakage would make it look better than it is.

**How to apply.** Any new replay-style evaluation (per-search gold, a time-split holdout) must keep
both rules, or change the gold unit to per search (then asOf alone suffices). Rejected: asOf =
the query's earliest search (strictly leak-free but starves the prior of everything after the
first asking).

**Seen in:** internal/eval/replay.go, internal/eval/sweep.go, internal/cli/eval.go (evalEvidence), internal/search/prior.go, internal/cli/eval_prior_test.go · 2026-10-30
