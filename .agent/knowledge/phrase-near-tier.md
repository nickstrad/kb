# The phrase/NEAR tiers of the main FTS list (PLAN 4.6)

> With two or more query groups and `search.phrase_boost`, the main FTS list is phrase, NEAR(8) and AND rows concatenated in that order (OR only when AND is empty), one list and one RRF term; the tier is logged per hit (`fts_tier`).

**Fact.**
- `tiered(groups, boost)`: groups = query fields that survive sanitising (`countGroups`); one group is already a
  phrase in the AND query. Queries (search/tiers.go `mainList`): AND (with synonym and spell alternatives) first;
  if it is empty, the OR retry alone (rows tier `or`, no phrase/NEAR: both are subsets of AND). Otherwise
  `OpPhrase` (`"a b c"`) and `OpNear` (`NEAR("a" "b" "c", 8)`, `store.NearDistance`) with the literal words only,
  then `assembleTiers` (pure: tier order, dedupe, stamp, cut at `list_limit`).
- Tiers are not separate RRF lists. A chunk has one FTS rank (its place in the merged list) and a `Tier`
  (`store.FTSRow.Tier`, `Candidate.FTSTier`, `LoggedHit.FTSTier`). `fusedBefore` is unchanged.
- Every row is rescored by the AND query (`rescore`, `andScores`: one more AND restricted to `ChunkIDs` for
  phrase/NEAR rows below the AND top `list_limit`), because phrase/NEAR bm25 differs (they match inside one column)
  and the per-column scores of `kb explain` (`TestExplainColumnRelation`) use the AND expression.
- Phrase/NEAR match inside one column, so a title word plus a text word is an AND-only match.
- Property (`TestTierNeverLosesAnANDRow`): when the plain AND list is shorter than `list_limit` the tiered list has
  exactly its chunks; at the limit both are full (phrase rows can displace low AND rows). `FuzzSanitizeFTS` also
  parses `MatchPhrase`/`MatchNear` of arbitrary input and checks every tier row is an AND row (on its fixture and on
  a document made of the phrase's own words), the claim `mainList`'s skip and `rescore` rest on.
  `TestTierOnlyReordersFTS`: under the list limit the tiers change only the FTS order (same chunks, same AND scores).
- Independent oracle: `search_untiered.golden` is the defaults golden as it stood before 4.6; with
  `phrase_boost = false` the ranking must match it bit for bit (`TestSearchGoldenDefaults`).
- Cost (`BenchmarkTiers`, 5k entries, 3-word FTS search): +0.5 ms typical, +5 ms for near-stopword query words.
  The restricted rescoring query must keep `+f.rowid IN (...)` in `ftsSQL`: without the unary + SQLite passes the
  ids to FTS5 as a rowid constraint and runs the MATCH once per id (10x slower; `TestFTSChunkIDsPlan`).
- Log: v9 `fts_tier TEXT CHECK (IN phrase/near/and/or)`, `memory_rank`, `memory_score` on `search_results` and
  `search_candidates`; NULL = untiered list (one group, boost off, every older row) / not in the memory list.
  `searches.fts_query` stays the AND (or OR) expression. Explain: `tiers:` line (each tier's expression and raw
  row count) and `tier X` on a candidate's fts line; `tiers: off` when `phrase_boost = false`.
- Default `phrase_boost = true` moved `search_defaults.golden` and the `global-store` and `tuning-loop` journeys:
  the chunk holding the query words adjacent now leads (e.g. "history search").

**Why it matters.** Exact phrasing is the cheapest relevance signal a keyword list has; a new tier must keep one
FTS rank per chunk and one `fts_score` meaning, or the log, explain and eval stop agreeing.

**How to apply.** A new tier: add an `FTSOp`, a `tierOps` row, a `store.Tier*` constant and widen the `fts_tier`
CHECK in a new migration. Decide eval with `kb eval --save-baseline` then
`kb eval --set search.phrase_boost=false --compare` on a real judged log.

**Seen in:** internal/search/tiers.go, internal/search/tiers_bench_test.go, internal/search/golden_test.go, internal/store/sqlite/retrieve.go, internal/store/sqlite/migrations.go, internal/cli/explain.go · 2026-10-30
