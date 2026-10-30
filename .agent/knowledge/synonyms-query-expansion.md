# Synonyms: query-time expansion of the main FTS list

> `synonyms.txt` (store dir, then the global store's, or `search.synonyms`) expands query terms into `("a" OR "b")` through `store.FTSQuery.Alts`; the exact list and the vector query never see it.

**Fact.**
- File: one comma-separated group per line; only a line that *starts* with `#` is a comment (`c#, csharp` works); members trimmed, inner whitespace collapsed, lowercased; groups sharing a member do not merge (a term expands to the members of every group it is in). Multi-word members are phrases, but only a single-word member is reachable from a query field. At most `MaxAlternatives` (16) per term; file is capped like config (`config.ReadCapped`: regular file, 1 MiB).
- `search.Synonyms` (nil-safe) -> `Expand(fields)` gives `FTSQuery.Alts` keyed by the field as typed. `sqlite.MatchExprAlts` renders it; AND and the OR retry both expand. `Outcome.FTSSynonyms`, `ExplainMatch.Synonyms` and the `synonyms:` line of `kb explain` record it; `FTSMatch` is the expanded expression (so `fts_query` is too).
- A field that sanitises away (`or`, `***`) stays dropped even if it has synonyms: resurrecting it as a required term would narrow the AND query (FuzzSynonyms found it).
- Lookup order: `search.synonyms` (exactly that file, missing = error) else `<store>/synonyms.txt` else the global location's default store's; none = no-op. Each fan-out store loads its own.
- `Settings.WithSynonyms(hash)` folds the canonical content hash (sorted terms, comment/space/case-insensitive) into `Settings.Hash`, so an eval baseline run with other synonyms is not comparable; no synonyms leave the hash unchanged. `Settings.JSON` is untouched. `kb eval` does not load synonyms yet (queue).

**Why it matters.** Expansion must only widen a query: `TestSynonymExpansionIsASuperset` and `FuzzSynonyms` pin it for AND and OR at the store level. (A search can still differ in rows because the list limit truncates and an expanded AND can match where the unexpanded one fell back to OR.)

**How to apply.** A new place that runs the main lexical query (eval, spell) must pass the same `Alts`; use `Searcher.Synonyms` and `cli.loadSynonyms`. Spell (5.6) extends the Alts with corrections: `Outcome.FTSSynonyms` stays synonyms-only, `Outcome.FTSAlts` is what the match was built with (spell-and-synonym-suggest.md).

**Parse cost (W10 pass).** `ParseSynonyms` keeps at most `MaxAlternatives` (16) alternatives per term at parse time (file order, the same 16 Lookup returns) and dedupes with maps: one 10k-member line used to be cubic and hang every search (`TestParseSynonymsLargeGroup`). The hash sorts each term's alternatives (they are ORed), so member and line order never change it. Every command that runs the main lexical query must load synonyms: `kb context` missed them on its primary store until the pass (`TestContextSynonyms`); `kb eval` is Q98.

**Seen in:** internal/search/synonyms.go, internal/cli/synonyms_load.go, internal/store/sqlite/retrieve.go · 2026-10-30
