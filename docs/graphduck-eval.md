# GraphDuck evaluation for kb: DuckDB pivot, dual approach, and book-based changes

Date: 2026-11-03. Author: Fable 5.1, from the brief in `docs/graphduck-eval-brief.md`.
Book: *GraphDuck: duckdb for embedded AI agents and graphs*, Volodymyr Pavlyshyn, Leanpub,
version published 2026-04-04. Companion code: github.com/Volland/graphduck (cloned to
`/root/Software/research/graphduck`). DuckDB facts below were checked against the DuckDB
2.0 docs and run on the local DuckDB CLI 1.5.5 on 2026-11-03; where a fact depends on the
DuckDB version that kb's Go driver bundles (1.4.1), that is said explicitly.

## Summary

- **Do not pivot kb's storage to DuckDB.** The decisive reason is not performance but process
  model: every `kb search` writes to the search log, several agent sessions run `kb` at once,
  and DuckDB lets exactly one process hold a database file (a second process cannot even open
  it read-only while a writer has it). SQLite in WAL mode with a busy timeout is what makes
  kb's concurrent, write-on-read design work today. HNSW persistence in DuckDB is also still
  marked experimental and not for production, and the FTS index is not maintained on write by
  default. kb's two indexes are maintained transactionally, per entry, on every write.
- **Keep the dual approach kb already has, and make it the rule:** SQLite is the system of
  record and the serving path (index, FTS, vectors, search log); DuckDB is a read-only
  analytic copy for `kb stats`. Extend the copy with graph-shaped tables when there is
  something graph-shaped to analyse, and let DuckDB compute things (PageRank, clusters,
  staleness) that kb writes back into SQLite as plain columns. Never attach, never serve
  from DuckDB.
- **Take the book's modeling ideas, not its engine.** The valuable parts for kb are typed
  relations between entries, validity windows on knowledge, consolidation of experience
  (kb's search log and feedback are exactly the "events" the book consolidates), and
  graph expansion after vector seeding. All of these fit in SQLite tables and in the
  existing RRF fusion. Four concrete changes are proposed in section 5.

## 1. What kb is today (the code the claims below rest on)

- **One SQLite file per knowledge repo**, `$KB_ROOT/.kb/kb.sqlite`, opened with
  `foreign_keys=ON`, `busy_timeout=5000` in the DSN and WAL mode set once on the file
  (`internal/store/store.go:87`, `:101`, `:154`). The WAL switch is retried up to 50 times
  because concurrent first opens are expected (`store.go:154` comment). Schema migration runs
  under `BEGIN IMMEDIATE` so two processes racing to create the database are serialised
  (`store.go:215`, `:234`).
- **Five content tables** in `internal/store/schema.sql`: `entries` (title, summary, tags as a
  JSON array, `updated`, `verified`), `chunks`, a *contentless* FTS5 table `chunks_fts` with
  bm25 weights text 1.0 / heading 2.0 / title 3.0 (`schema.sql:28`), a sqlite-vec `vec0`
  table `chunks_vec` with `FLOAT[768] distance_metric=cosine` (`schema.sql:42`), and
  `embed_meta` pinning the model and dimension.
- **Writes are transactional across all four content tables.** `ReplaceEntryChunks`
  (`internal/store/entries.go:305`) upserts the entry, deletes the old chunks' FTS and vector
  rows by id, and inserts new chunks, FTS rows and vectors in one transaction. Unchanged
  chunks reuse their vectors by `text_hash` (`entries.go:267`), which is what keeps reindexing
  cheap when the embedder is slow.
- **Hybrid search** (`internal/search/search.go`): an FTS5 list and a `vec0` KNN list of 20
  each (`ListLimit`, `search.go:46`), fused with reciprocal rank fusion at k=60
  (`Fuse`, `search.go:441`), a per-entry diversity cap of 3 (`applyCap`, `search.go:508`),
  and a tag filter that is a `LIKE` over the JSON tags column (`search.go:294`, `:401`). The
  vector list over-fetches to 60 when a tag filter is present because `vec0` cannot filter on
  another table's column (`search.go:55`). Ties in fusion are broken deterministically so the
  log is comparable across runs (`search.go:441` comment).
- **Every search is logged in one transaction** (`internal/search/log.go:48`): a `searches`
  row, the returned hits, and the top-40 fused candidates. `kb feedback` writes
  `search_feedback` rows (`log.go:228`, `:284`). The live store at `/root/Raw/knowledge` has
  42 entries, 298 chunks, 604 logged searches and 536 feedback rows.
- **DuckDB is already in the binary, in exactly one place.** `internal/stats/duck.go:91`
  opens the SQLite file read-only (`mode=ro`), creates typed tables in an in-memory DuckDB
  and bulk-loads the four log tables plus `entries` with the Appender. The package header
  says it: "this package copies, it does not attach" (`duck.go:6`), and plan decision D2
  records why: the DuckDB `sqlite` extension is downloaded at runtime and would make `kb
  stats` depend on the network. Five named analytic queries live in
  `internal/stats/queries.go` (`overview`, `sources`, `trend`, `entries`, `gaps`) and use
  window functions, `quantile_cont` and correlated subqueries that SQLite expresses badly.
  Plan decision D1 records the cost: go-duckdb bundles DuckDB as a static library and the
  binary grew to about 67 MB.
- **No relations between entries exist in the model.** Front matter is title, summary, tags,
  updated, verified (`internal/entry/entry.go:80`). The only structure is file-versus-
  directory entry and the chunk order within an entry (`internal/chunk/chunk.go:62`). In the
  live index only 5 of 298 chunks contain a Markdown link to another entry.

## 2. What GraphDuck and DuckDB actually provide (as used in the evaluation)

Chapters read in full: "Why Embedded Databases Matter for Private AI Agents" (pp. 24-32),
"Semantic Spacetime" (four relation types, layers, validity windows), "DuckDB as a Triple
Store", "Vector Indexes in DuckDB", "Designing Graph Memory for AI Agents" (pp. 199-215),
"Agentic Memory Patterns", "Hybrid RAG Pipeline in DuckDB", and "Do We Need a 'Graph
Database' for Knowledge Graphs?" (pp. 299-309). Companion files: `ch09_vector_indexes/
01_vector_search.sql`, `ch10_memory_design/01_memory_ontology.sql`,
`ch11_agentic_memory/memory_agent.py`, `ch13_hybrid_rag/01_rag_schema_and_queries.sql`,
`ch13_hybrid_rag/hybrid_graph_rag.py`.

What the book builds on, feature by feature:

| Feature | Where the book uses it | Status on DuckDB (verified) |
| --- | --- | --- |
| Fixed-size `FLOAT[n]` columns and `array_cosine_similarity` / `array_distance` | ch09, ch10 DDL, ch11 `recall`, ch13 `query` | Core DuckDB; brute-force scan without an index. |
| `vss` extension, `CREATE INDEX ... USING HNSW` with `metric='cosine'` | ch09 ("Creating Vector Indexes"), ch10 "Complete Memory DDL" | Separate extension. On a file-backed database the index needs `SET hnsw_enable_experimental_persistence = true`; without it DuckDB 1.5.5 answers `HNSW indexes can only be created in in-memory databases`. The docs say WAL recovery is not implemented for custom indexes, a crash can corrupt the index, and "we still recommend that you do not use this feature in production environments". The index must fit in RAM and deletes are only marked until `PRAGMA hnsw_compact_index`. |
| `fts` extension, `PRAGMA create_fts_index`, `match_bm25` | ch09 "Combining Vector Search with Full-Text Search" | Separate extension. "By default, the FTS index will not update automatically when the input table changes." Verified locally: a row inserted after the PRAGMA is invisible to `match_bm25`. The 2.0 docs add an `incremental = 1` option; whether it exists in the 1.4.1 that go-duckdb bundles was not verified. |
| Recursive CTEs for paths and causal chains | ch02, ch10 "Why did this happen?" | Core DuckDB. SQLite has `WITH RECURSIVE` too (bundled 3.53). |
| Iterative PageRank with `CREATE OR REPLACE TABLE` in a loop | ch13 "Stage 3" | Plain SQL; works in any engine, including the in-memory DuckDB `kb stats` already builds. |
| Reciprocal rank fusion of a vector list and a graph-expansion list | ch13 "Stage 4", `hybrid_graph_rag.py` `query()` | Plain arithmetic; kb already implements RRF in Go (`search.go:441`). Note the book's SQL version computes `1.0 / (AVG(ec.entity_id) + 60)` for the graph score, which averages entity ids rather than ranks; the Python version is the one to read. |
| DuckPGQ (SQL/PGQ, Cypher-like patterns) and Onager (graph algorithms) | ch03, ch20 | Community extensions, not bundled; the book itself says pure SQL is the fallback ("Pure SQL Fallback", ch20). |
| Single-file, zero-server, "entire knowledge graph is a single file" | "Why Embedded Databases Matter" | True of DuckDB; equally true of kb's SQLite file today. |
| Multi-process access | ch13 "Comparison with LadybugDB": DuckDB has "no multi-user concurrency" | Docs: "Read-write mode: one process can both read and write"; "Read-only mode: multiple processes can read from the database, but no processes can write." Verified locally: while one CLI process holds the file, a second fails with `Could not set lock on file`, with or without `-readonly`. |

Go driver facts (`github.com/marcboeker/go-duckdb/v2` v2.4.3, from its README in the module
cache): the prebuilt library bundles DuckDB 1.4.1 and statically links only ICU, JSON,
Parquet and Autocomplete; "automatic extension loading is enabled", so `vss` or `fts` would be
downloaded from the extension repository on first use. Bundling them statically means building
DuckDB's `bundle-library` target yourself with `BUILD_EXTENSIONS` and linking with
`-tags=duckdb_use_static_lib`. The knowledge store also records that agent shells on this
droplet sometimes start with an empty `HOME`, which makes `INSTALL`/`LOAD` fail with
"Can't find the home directory" (kb entry `empty-home-in-agent-shells`).

## 3. (a) Pivot: move kb's storage from SQLite to DuckDB?

### What kb would gain

- **Query power for analytics.** Window functions, `quantile_cont`, `FILTER`, list and struct
  types, and fast aggregation over the search log. kb already has this, because `kb stats`
  copies the log into DuckDB. A pivot would only remove the copy step
  (all of `internal/stats/duck.go`).
- **Vector search via `vss`.** HNSW gives approximate nearest neighbour in sub-linear time.
  kb's `vec0` table is an exact, brute-force scan over 768-dimensional vectors
  (`search.go:340`). At 298 chunks, or at 100,000, that scan is milliseconds; the embedding
  call that precedes it is about 0.4 s (`search.go:686` comment). An HNSW index would not
  change what the user waits for until the corpus is three or four orders of magnitude larger.
  It would also make results approximate, which breaks the deterministic fusion the log
  design depends on (`search.go:441` comment).
- **Graph relations between entries.** The book's edge tables, recursive CTEs and PageRank are
  all plain SQL. SQLite runs the same recursive CTEs; the iterative PageRank is a loop of
  `CREATE TABLE AS` in either engine. DuckPGQ would add Cypher-like pattern syntax, but it is a
  community extension and kb's graph would have 42 nodes.
- **One engine instead of two drivers.** The binary would drop mattn/go-sqlite3 and sqlite-vec
  and keep go-duckdb. That is the only simplification on offer, and it is small next to the
  rewrite it costs (every file in `internal/store`, `internal/search`, and the CLI commands
  that touch the log).

### What kb would lose or risk

1. **Concurrent agents.** This is the blocker. `kb search` writes on every call
   (`log.go:48`), the global instructions assume several sessions search at once ("use the
   explicit N when another session may be searching too"), and `store.go` is built around
   that: WAL, busy timeout, retried WAL enable, `BEGIN IMMEDIATE` migrations. DuckDB's model
   is one process per file (verified in section 2). To keep the current behaviour kb would
   need its own daemon or lock-and-retry layer in front of DuckDB, which is the "server" the
   book's embedded argument is trying to avoid.
2. **Persistence of the vector index.** HNSW on a file-backed DuckDB is behind an experimental
   flag whose docs warn of index corruption on crash and say not to use it in production.
   sqlite-vec's `vec0` is an ordinary virtual table inside SQLite's transaction and WAL.
3. **Write pattern of the text index.** kb updates FTS per entry, inside the same transaction
   as the chunk write, using a contentless FTS5 table with `contentless_delete=1`
   (`entries.go:448`). DuckDB's FTS index is a snapshot built by a PRAGMA and not updated on
   write by default (verified). Every `kb add` or `kb reindex <entry>` would either rebuild
   the whole FTS index or rely on an incremental mode whose availability in 1.4.1 is
   unverified.
4. **Extensions at runtime.** `vss` and `fts` are not in the prebuilt go-duckdb library, so a
   fresh machine would download them on first use, which plan decision D2 already rejected
   for `kb stats`, and which the empty-`HOME` quirk on this droplet turns into a hard failure.
   The alternative is a custom static DuckDB build in `scripts/install.sh`.
5. **Maturity and size of the Go path.** go-duckdb v2 is cgo, pins one DuckDB version per
   bindings release, and already added about 60 MB to the binary (D1). Its Appender is the
   fast write path; `database/sql` inserts are slow by comparison, and kb's write path is
   row-at-a-time with `RETURNING id` (`entries.go:305`).
6. **The migration itself.** `schema.sql` is `migrations[0]` and must never change
   (`store.go:38`); a pivot means a new file format, a one-shot converter, and re-validating
   the fuzz and regression tests in `internal/search` and `internal/cli` that encode SQLite
   FTS5 tokenizer behaviour (`search.go:223`).

### Verdict on (a)

Not worth it. The gains are either already available through the copy (analytics), unneeded
at kb's scale (HNSW), or engine-neutral (graphs in SQL). The losses hit the two properties kb
is built on: many processes writing a shared file safely, and indexes that move with the
entry in one transaction.

## 4. (b) Dual approach: SQLite for some things, DuckDB for others

kb already runs a small, well-chosen version of this: SQLite is the system of record and the
serving path; DuckDB is a per-invocation, read-only, typed copy for analytic SQL. The split
should be stated as a rule and extended along the same line.

### The split that makes sense

| Concern | Engine | Why |
| --- | --- | --- |
| Entries, chunks, FTS, vectors, embed_meta | SQLite | Written per entry in one transaction; read by many processes; exact KNN keeps the log deterministic. |
| Search log and feedback | SQLite | Written on every search by whichever process ran it. |
| Entry relations (edges), validity windows, learned priors (section 5) | SQLite | They are read on the serving path and must be visible to every process immediately. |
| Search-effectiveness analytics (`kb stats`) | DuckDB in-memory copy | Already done; D2 and D3 hold. |
| Graph analytics over entries and edges (PageRank, components, tag clusters, hub and orphan reports) | DuckDB in-memory copy | Same copy, two more tables. Iterative PageRank is a loop of `CREATE OR REPLACE TABLE`, as in ch13. |
| Consolidation batch jobs (compute a prior, a staleness score, candidate edges) | DuckDB computes, kb writes back to SQLite | DuckDB reads the copy and returns rows; kb writes them into ordinary SQLite columns in one transaction, so there is still one writer path and one source of truth. |

What stays out of DuckDB: anything on the path of `kb search`, `kb add`, `kb reindex`,
`kb feedback`; and the SQLite file itself (no `ATTACH`, no `sqlite_scanner`, per D2).

### What the split costs

- **Two schemas for the same data.** `schemaDDL` in `duck.go:30` mirrors `schema.sql` by hand,
  and the Appender writes positionally, so a column added to one and not the other shifts
  every value after it (the hazard is documented at `duck.go:26`). Each table added to the
  copy adds one `copyX` function and one more place to keep in step.
- **Two type systems.** The copy widens SQLite's untyped columns into `TIMESTAMP`, `BOOLEAN`,
  `DOUBLE`, `INTEGER`, parses RFC3339 text, and `normalizeValue` folds DuckDB's narrow widths
  back into Go (`duck.go:201`). `sum()` over integers becoming `HUGEINT` is a real trap the
  queries already work around (`queries.go` header).
- **Two SQL dialects in one binary.** `bm25()` and `MATCH` on one side, `quantile_cont` and
  `FILTER` on the other. Contributors have to know which engine a query runs in; the
  registry in `queries.go` and the `--sql` escape hatch keep that boundary visible.
- **Consistency is snapshot consistency.** The copy is as fresh as the moment `Load` ran. For
  stats that is correct. For anything written back (a prior, an edge), the write-back must be
  idempotent and dated, and the serving path must tolerate the column being absent or stale.
- **Build and link time.** Both drivers are cgo. Every test package that imports `stats` links
  DuckDB (D1). Keeping DuckDB out of `store` and `search` keeps the fast test loop fast.

### Verdict on (b)

Keep the split exactly where it is, add graph-shaped tables to the copy when section 5's
edges exist, and allow DuckDB-computed values to flow back into SQLite as plain columns. Do
not move serving.

## 5. (c) Recommendation

Do not pivot kb to DuckDB. Keep SQLite with FTS5 and sqlite-vec as the single system of
record and serving engine, because kb's concurrent, write-on-every-search design depends on
SQLite's multi-process WAL behaviour, and because DuckDB's HNSW persistence and FTS
maintenance are not yet at the level kb's transactional per-entry indexing already has. Keep
DuckDB where it is, as the read-only analytic copy behind `kb stats`, and grow that role into
graph analytics and consolidation jobs whose results are written back into SQLite. Adopt the
book's modeling ideas in SQLite tables and in the existing RRF fusion: typed relations
between entries, validity windows, feedback-driven consolidation, and graph expansion after
vector seeding. Revisit the engine question only if the corpus approaches a million chunks or
kb needs SQL/PGQ pattern matching.

## 6. (d) Concrete changes for kb based on the book

### 6.1 Typed relations between entries, and one-hop expansion in search (M)

**Book idea.** Semantic Spacetime's four relation types, Similar, Contains, HasProperty and
LeadsTo, "form a complete basis" for knowledge edges ("Semantic Spacetime", "The Two
Organizing Axes"), and the memory chapter's DDL stores them as `edge_similar`,
`edge_contains`, `edge_leads_to` tables with a `method` column recording how the edge was
learned (ch10 "Complete Memory DDL", `01_memory_ontology.sql`). The retrieval pipeline then
seeds with vector search and "expands to neighbors via Similar edges", decaying the score per
hop ("Vector Search + Graph Traversal Pipeline", ch09, `similarity * 0.8`). ch11 puts the
payoff plainly: a flat system "returns the top-k most similar chunks. A graph memory system
returns those chunks plus their neighborhood."

**kb today.** Entries have no relations at all (`entry.go:80`); the only link is the free-text
Markdown link, present in 5 of 298 chunks. A search over "tmux daemons die at logout" cannot
reach the entry on systemd user services that explains the fix unless the words overlap.

**Change.** Add an optional `related:` front-matter list of `{path, type}` where type is one
of `similar`, `contains`, `leads-to` (HasProperty is what tags already are), validated by
`kb add` and `kb doctor` against existing paths. Store it in a new `entry_edges(src_id,
dst_id, type, method)` table in `schema.sql` via a new migration (`store.go:48` rules), with
`method='authored'`. In `Search` (`search.go:581`), after fusion, take the top-N entries of
the fused list, pull their neighbours from `entry_edges`, and add each neighbour's summary
chunk (ord 0) as a third list for `Fuse`, ranked by the seed's rank so the RRF weight decays
naturally. Log the expansion in `search_candidates` with a new `source` column so `kb stats`
can say whether expansion ever helped. Size M: one migration, front-matter parsing, a third
list in `Fuse`, tests, and a `kb show --related` that prints the neighbourhood.

### 6.2 Validity windows: `expired`, `superseded_by`, and stale-aware ranking (S)

**Book idea.** Every node in the memory ontology carries `learned_at` and `expired_at`, "A
node's validity window is [learned_at, expired_at]", and "A query 'What do we know as of date
X?' must join on the temporal bounds" ("Semantic Spacetime", "Temporality"). ch11 adds the
policy: "Forgetting: Temporal Decay and Expiry", with an `UPDATE ... SET archived = true`
over old, low-significance rows.

**kb today.** Front matter has `updated` and `verified` as verbatim strings
(`schema.sql:8`, `:9`) and nothing uses them in ranking; a wrong entry is removed with
`kb rm`, which loses the fact that it was wrong.

**Change.** Add `expired` and `superseded_by` to front matter and to `entries` (migration).
`Search` excludes expired entries from both lists unless `--as-of` or `--include-expired` is
passed, and an expired hit that is nonetheless returned carries its successor's path in the
JSON. `kb doctor` reports entries whose `verified` date is older than a threshold and which
have at least one not-useful verdict in `search_feedback`, using the `entries` stats query's
definitions (`queries.go:189`). Size S: two columns, a filter in `ftsList` and `vecList`, a
doctor check, and the skill template update.

### 6.3 Feedback-learned Similar edges and a usefulness prior in ranking (M)

**Book idea.** Consolidation turns repeated experience into durable structure: "If an event
repeats (same action, same outcome) N times, create a Rule node", and "In DuckDB,
consolidation is a batch process: periodically scan recent events, apply consolidation
rules, create new Fact nodes, update edge weights" (ch11, "Memory Consolidation"). ch09's
example notes that `similar_edges` "would normally be learned from data", and ch13 biases
retrieval with a precomputed `pagerank_scores` table.

**kb today.** The search log is a complete event stream (query, every candidate, every verdict)
and `kb stats` already computes per-entry usefulness (`queries.go:189`) and gaps
(`queries.go:244`), but nothing feeds back into ranking.

**Change.** A `kb consolidate` command that loads the DuckDB copy (`duck.go:91`), runs two
queries, and writes the results back into SQLite in one transaction: (1) pairs of entries both
marked useful for the same search at least N times become `entry_edges` rows with
`type='similar', method='feedback', weight=count`; (2) a per-entry usefulness prior
(useful verdicts over judged appearances, with a floor) is written to a new
`entries.prior REAL` column. `Fuse` (`search.go:441`) multiplies each candidate's RRF score by
`1 + alpha * prior`, with alpha small and configurable, and logs the prior in
`search_candidates`. This is the dual approach done deliberately: DuckDB computes, SQLite
serves. Size M: the command, two DuckDB queries, one write-back, the prior in fusion, and a
stats query to show whether the prior changes outcomes.

### 6.4 `kb stats graph`: entry-graph analytics in the existing DuckDB copy (M)

**Book idea.** "Most graph questions are analytical" and belong in DuckDB (Introduction, "The
Thesis"); ch13 computes PageRank with a loop of `CREATE OR REPLACE TABLE` because recursive
CTEs "cannot correctly implement iterative PageRank"; ch02 adds degree distributions and a
community-detection approximation; and the closing chapter's "Memory Is Not Retrieval" lists
the questions a store should answer about itself: what is central, what is isolated, what
contradicts what.

**kb today.** `copyEntries` already loads `entries` into DuckDB (`duck.go:460`) and the
registry has five log-centred queries. There is no view of the corpus as a graph.

**Change.** Once 6.1 and 6.3 exist, copy `entry_edges` and a tag-exploded table
(`entry_tags`) alongside `entries` and add three registry queries: `hubs` (PageRank over
authored and feedback edges, run as a fixed number of `CREATE OR REPLACE TABLE` iterations
inside `Load`), `orphans` (entries with no edges, no shared tags, and no useful verdict in the
window), and `clusters` (connected components over edges plus shared tags, reported with the
tag that best labels each). Each query gets a `Help` paragraph naming the decision it informs,
as D5 requires. Size M: two copy functions, three queries, tests on a small fixture.

### Deliberately not proposed

- **HNSW or any DuckDB vector index.** Exact `vec0` search is correct, deterministic and fast
  enough at kb's scale; see section 3.
- **Entity extraction from chunks** (ch13 `extract_entities_simple`). kb entries are
  hand-curated and already carry tags; capitalised-phrase extraction over shell commands and
  paths would add noise, not structure.
- **The time tree** (ch10 "Building the Time Tree"). kb's temporal questions are answered by
  `searches.ts` and the `trend` query; a calendar table buys nothing here.
- **Embedding the summary separately from the body.** The book's "Which Nodes Get
  Embeddings?" advice is already followed: chunk 0 is `title: summary` (`chunk.go:62`) and
  every section is its own chunk.
