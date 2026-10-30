# The exact-identifier list (chunks_exact, schema v6)

> A query field that looks like an identifier (`[_./:-]|[a-z][A-Z]|\d[a-z]`) also searches chunks_exact, a second contentless FTS5 index whose tokenizer keeps `_ - . : /` inside tokens; its rows are a third RRF list (list_weights.exact, exact_list).

**Fact.**
- `chunks_exact(text, heading)`: `unicode61 remove_diacritics 0 tokenchars '_-.:/'`, no porter, no
  title column (`FTSQuery.Exact`; Weights.Title is ignored). `foo_bar` matches only `foo_bar`
  (never `foo bar`, never `foo`), `--embed-model` never `embed-model`, `café` never `cafe`, case
  still folds. `chunks_exact_vocab` (fts5vocab 'row') is read by 5.6 spell and suggest (sqlite/vocab.go).
- Documents keep a sentence-final `.`/`:` on the token (`see kb.toml.` → `kb.toml.`); the query
  side trims it (`search.ExactTerms`, to a fixed point: `FuzzExactTerms` found the one-pass trim).
  So a sentence-final identifier is found only by the main list.
- `search.ExactTerms` picks the fields; the exact list runs only when it returns some, never in vec
  mode, and reuses `lexicalList` (AND, then OR retry). `Fuse(fts, vec, exact, p)`; tie-break
  appends exact rank before chunk id, so with no exact rows the order is the two-list order.
- Every write path fills it: ReplaceEntry insert + clearChunks delete + ResetIndex 'delete-all';
  `assertRowSets` checks chunks = chunks_fts = chunks_exact (= chunks_vec). Seven tables move together
  with a chunk write: those four plus `entry_tags` and `edges` (v7, `assertLinkSets`); see edges-tags-aliases.md. So ReplaceEntry needs
  today's schema: tests that open at an old version must upgrade before writing.
- Common terms are never dropped: FTS5 clamps idf to ~1e-6, so a term in N-1 of N chunks scores
  about -1e-6 but every match is a row (`commonTerm` storetest row, `TestCommonTermIsNeverDropped`).
- v6 also gave `search_opens` a `store` column and the key `(search_id, ifnull(store,''), entry_path)`
  (a unique expression index: a NULL in a PRIMARY KEY would not dedupe); older opens got the
  store the reader used to infer.
- `kb search -- --embed-model`: a query that starts with `-` needs `--` before it.

**Why it matters.** Identifier queries (code names, flags, file names) are what agents search for
most; the porter table splits them into words that match anywhere.

**How to apply.** A new write path must touch all three FTS/vec tables (the storetest model test
and `assertRowSets` catch a miss). A new lexical list follows the same shape: a `store.FTSQuery`
flag, a third `Candidate` rank pair, a `fusedBefore` level before chunk id, log columns.
`TestExactListImproves` + `search_exact.golden` are the evidence for exact_list = true.

`kb search` has no `--set`: the list is switched per store (`[search] exact_list`) or per run in
`kb explain`/`kb eval --set`. A query that starts with `-` goes after the flags and `--`
(`kb search --json -- --embed-model`); flags after `--` become query words.

**Seen in:** internal/search/exact.go, internal/search/search.go, internal/store/sqlite/migrations.go, internal/store/sqlite/index.go · 2026-10-30
