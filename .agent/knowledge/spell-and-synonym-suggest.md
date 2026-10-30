# Spell correction and `kb synonyms suggest` (PLAN 5.6)

> Spell adds the closest common vocabulary term as an FTS alternative (via `FTSQuery.Alts`) only when the main list has < 3 rows; `kb synonyms suggest` mines one-word rewrites (fail -> useful/open, same caller, ≤ 5 min) from the log and never writes synonyms.txt.

**Fact.**
- Vocabulary: `store.Vocab` (store/vocab.go; sqlite/vocab.go over `chunks_exact_vocab`). Terms are
  `rtrim(term, '.:')`-merged (`kb.toml.` counts for `kb.toml`), so Doc is an upper bound. `VocabTerms`
  is one scan with doc/length bounds in SQL; `DocFreq` looks up `t`, `t.`, `t:` (fts5vocab answers
  `term = ?` without a scan), 256 terms per statement; terms must be pre-trimmed. Search finds it
  by type assertion on `Searcher.Store`: a fake Retriever without it never spells.
- Trigger (`spellWanted`): `Params.Spell` (search.spell, default true) and a Vocab store and the
  main FTS list ran and returned < `SpellBelow` (3) rows after any OR retry. Vec mode never.
- Words (`spellWord`): `trimIdentifier`, not `isIdentifier`, letters/marks only (no digits), ≥ 4
  runes; only a word with doc 0 is corrected. Candidates: letters-only, doc ≥ 2, OSA distance 1
  (2 from 8 runes). Best = fewest edits, then most docs, then smallest term (`Correct`).
- Rewrite: `alts[field] = synonyms(field) + correction + synonyms(correction)` (cap 16). Rerun
  once with the op the first run ended with: an OR retry stays OR (an AND with the correction can
  match yet drop OR rows; `herding zebracron` row). `keepOriginal` re-appends any first-run row
  the rerun's limit pushed out. `Outcome.Spell`, `FTSAlts` (what FTSMatch was built with; explain's
  column scores use it) vs `FTSSynonyms` (synonyms only). Logged `fts_query` = rewritten match.
- Porter stemming folds many typos before spell can see them: `kubernets` and `kubernetes` share the stem `kubernet`, so FTS matches directly and no note prints. A test or journey that wants spell to fire needs a typo the stemmer does not fold (a transposition such as `kuberntes`, or `lgout`).
- CLI: `note: also searched "kubernetes" for "kuberntes"` on stderr (search and explain); explain
  adds a `spell:` line and `fts.spell` in JSON. Cross-store: the primary's fixes only (Merge copies
  the primary's outcome); extra stores spell against their own vocab silently.
- Suggest (`search.Suggest`, pure): consecutive searches of one caller, second within the window
  (inclusive, not before), first not worked (no useful verdict, no open, other-store opens count as
  opens) and second worked, `QueryWords` differ by exactly one word each way. Groups are unordered
  pairs; already-linked synonyms skipped; `Typo` when `isTypo` (spell's rule). Gaps = words of
  non-worked searches with doc 0. Human output: every line a `#` comment except group lines.

**Why it matters.** Spell must only widen: `TestSpellNeverLosesResults` (list limits 2/3/20) is the
property; dropping the OR-stays-OR rule or `keepOriginal` fails it or `TestSearchSpell`.

**How to apply.** Another lexical caller (eval, context) gets spell for free through Searcher when
its store is sqlite; eval replays therefore include spell (settings hash already covers
search.spell via EffectiveJSON). A new note for the user goes through `cli.noteSpell`.

**Seen in:** internal/search/spell.go, internal/search/suggest.go, internal/store/sqlite/vocab.go, internal/cli/synonyms.go · 2026-10-30
