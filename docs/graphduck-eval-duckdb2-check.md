# DuckDB 2.0 check of the GraphDuck evaluation's DuckDB claims

Date: 2026-11-03. Author: Claude Opus 5.5, from the brief in `docs/duckdb2-check-brief.md`.
Subject: `docs/graphduck-eval.md` (Fable 5.1, commit `ef9469a`), which recommends that kb
not move its storage from SQLite to DuckDB. This is a verification pass only. The eval and its
recommendation are unchanged; this document lists what Nick needs to reconsider.

## Versions and evidence

- **DuckDB 2.0.0** ("Cyanoptera", `d54cbbe3df`, released 2026-10-21): the official CLI
  `duckdb-cli-linux-amd64.tar.gz` from the GitHub v2.0.0 release, downloaded into
  `scratch/` in this worktree. Extensions were installed into a scratch extension directory:
  fts `223b8cb9e6`, vss `3685131eac`, quack `f96420ed46`, and the community extension duckpgq
  `9c11bc192a`. Nothing was installed outside the worktree. The scratch directory was
  deleted after the run.
- **DuckDB 1.4.1** (`b390a7c376`, the version bundled by go-duckdb v2.4.3, which kb uses
  today) and **1.5.5** (`d8cdaa33fd`, the CLI the eval actually ran, at
  `~/.local/bin/duckdb`): used only to check whether the FTS `incremental` option exists in
  those versions.
- **Docs:** the versioned 2.0 docs at `duckdb.org/docs/2.0/...` (`/docs/stable/` now
  redirects there), the 2.0 announcement, the Quack FAQ, and the duckdb-go v2.20000.0
  release and README.
- The machine has 4 CPUs. Timings are from this one machine and show orders of magnitude
  only.

TESTED means the claim was run against the 2.0.0 binary. DOC-BASED means it rests on the
2.0 docs only.

Note on the eval's provenance: the brief says the eval was verified on 1.4.1. The eval itself
says its local runs used the CLI **1.5.5** and its docs reading used 2.0. This check covers
all three versions where the difference matters.

## Claim list

Each of these load-bearing DuckDB claims was taken from `docs/graphduck-eval.md`:

| # | Claim (eval wording, condensed) | Where |
| --- | --- | --- |
| C1 | One process per database file. kb writes a log row on every search and runs several agent sessions at once, so DuckDB would need a kb-built daemon or lock-and-retry layer. | Summary; §2 table, "Multi-process access"; §3 loss 1 |
| C1b | A second process cannot open the file even with `-readonly` while a writer holds it. | Summary; §2 |
| C2 | HNSW persistence on a file-backed database is experimental: it needs a flag, WAL recovery is missing, a crash can corrupt the index, and the docs say not to use it in production. | Summary; §2; §3 loss 2 |
| C2b | The HNSW index must fit in RAM, and deletes are only marked until `PRAGMA hnsw_compact_index`. | §2 |
| C3 | The FTS index is not updated on write by default: a row inserted after `create_fts_index` is invisible to `match_bm25`. The `incremental = 1` option is in the 2.0 docs but unverified in 1.4.1. | Summary; §2; §3 loss 3 |
| C4 | `vss` and `fts` are not in the prebuilt Go driver library, so they would be downloaded at runtime. Static bundling means building DuckDB's `bundle-library` target. | §2 "Go driver facts"; §3 loss 4 |
| C5 | DuckPGQ (SQL/PGQ) is a community extension and is not bundled. | §2 table; §3 |
| C6 | The Go path is cgo and pins one DuckDB version per bindings release (`marcboeker/go-duckdb/v2`). | §2; §3 loss 5 |

## Per-claim findings under DuckDB 2.0

### C1: one writer process per file. Verdict: WEAKENED (TESTED + DOC-BASED)

What changed in 2.0: DuckDB now officially supports writing from multiple processes, but
only through a server.

- The 2.0 concurrency page keeps the in-process model unchanged: "In in-process mode, DuckDB
  has two configurable options for concurrency: Read-write mode: one process can both read and
  write to the database. Read-only mode: multiple processes can read from the database, but no
  processes can write". It then adds a new "Multiple Processes" section: "Writing to DuckDB's
  native database format from multiple processes is supported through the Quack remote
  protocol, which turns DuckDB into a client-server database. DuckDB 2.0 ships a polished
  version of Quack (first released in DuckDB 1.5.3) with substantial performance and
  stability improvements." The alternative it names is DuckLake with a PostgreSQL catalog.
  Source: https://duckdb.org/docs/2.0/connect/concurrency.html
- Quack is a server that a DuckDB session starts, not a mode of the file. "The Quack
  extension turns a DuckDB instance into a server that other DuckDB instances (clients) can
  connect to over HTTP." "A server is started from an existing DuckDB session"
  (`CALL quack_serve('quack:localhost')`, default port 9494). Clients authenticate with a
  token and use `ATTACH 'quack:host' AS name (TOKEN ...)`, then `CONNECT`. Source:
  https://duckdb.org/docs/2.0/quack/overview.html
- Quack's status: "Is Quack production-ready? Not yet. Quack is currently in a beta state.
  You can use Quack for prototyping but it is still in development, and breaking changes are
  expected to happen. These may include the protocol, function names, and default settings.
  DuckDB 2.0 ships a polished version of Quack ... but the protocol is not yet stable."
  Source: https://duckdb.org/quack/faq.html
- 2.0 adds no lock-wait setting. `duckdb_settings()` in 2.0.0 has nothing like SQLite's
  `busy_timeout`: a lock conflict fails immediately.

Tests, all on 2.0.0:

| Test | Result |
| --- | --- |
| T1: one process holds the file read-write; a second process inserts, then opens it with `-readonly` | Both fail: `IO Error: Could not set lock on file ".../t1.db": Conflicting lock is held in .../duckdb (PID n)`. **C1b HOLDS.** |
| T1b, kb's pattern on a plain file: 8 short-lived processes at once, each opening the file, inserting one row and exiting, 5 rounds | 40 attempted, **35 failed** with the lock error, 5 stored. |
| T2, the same pattern through Quack: one process owns the file and runs `quack_serve` on localhost; each client process ATTACHes and inserts one row | 8 clients × 5 rounds: 40/40 succeeded, mean 2.5 s and max 3.4 s per client. 16 clients × 3 rounds: 48/48 succeeded, mean 6.7 s and max 8.7 s per client. One client alone takes about 0.45 s. |
| T2, served file opened directly by another process | Lock error, even with `-readonly`. Once a server owns the file, every process must go through it. |
| T5 baseline: SQLite 3.53.4 (WAL, `.timeout 5000`), 16 processes × 3 rounds | 48/48 succeeded, about 1.1 s for all 48. |

What this means for kb: the eval's factual claim still holds. A DuckDB file has one writer
process, and a reader cannot coexist with it. Its consequence, that kb "would need its own
daemon or lock-and-retry layer", is now only half true: DuckDB supplies the daemon. A
DuckDB-backed kb would need:

- a long-lived Quack server per knowledge store, and something (systemd unit, first-caller
  spawn) to start it and keep it running;
- token handling for every agent session;
- a fallback for when the server is down, since direct file access conflicts with a running
  server;
- a beta protocol that the docs say will change.

Every `kb` call would also pay process startup plus `quack` load plus an HTTP attach. That
took about 0.45 s alone and several seconds under a 16-way burst on this box, against
milliseconds for SQLite. The blocker is now "it needs a beta server", not "it is
impossible".

### C2: HNSW persistence is experimental. Verdict: HOLDS (TESTED + DOC-BASED)

- The 2.0 VSS page is unchanged in substance: "The vss extension is an experimental
  extension". "The HNSW index can only be created on tables in in-memory databases by
  default, unless the SET hnsw_enable_experimental_persistence = bool configuration option is
  set to true." It also says: "'WAL' recovery is not yet properly implemented for custom
  indexes, meaning that if a crash occurs or the database is shut down unexpectedly while
  there are uncommitted changes to a HNSW-indexed table, you can end up with data loss or
  corruption of the index ... But we still recommend that you do not use this feature in
  production environments." Source: https://duckdb.org/docs/2.0/core_extensions/vss.html
- Tested on 2.0.0:
  - Without the flag, `CREATE INDEX ... USING HNSW` on a file database fails with `Binder
    Error: HNSW indexes can only be created in in-memory databases, or when the
    configuration option 'hnsw_enable_experimental_persistence' is set to true.`
  - With the flag, the index persists across close and reopen. After an insert, the plan
    shows `Hnsw Index Scan`.
  - `duckdb_settings()` describes the flag as "experimental: enable creating HNSW indexes in
    persistent databases".
- New in 2.0: there is a `hnsw_prefilter` setting ("experimental: apply static table filters
  before searching HNSW indexes"). It does not change the verdict.

### C2b: HNSW in RAM and delete marking. Verdict: HOLDS (DOC-BASED)

The 2.0 VSS page says: "The index itself is not buffer managed and must be able to fit into
RAM memory". Deletes are "marked as deleted" until `PRAGMA hnsw_compact_index`. The page also
says "there are no incremental updates to persistent index storage, so every time DuckDB
performs a checkpoint the entire index will be serialized to disk". Same source as C2.

### C3: FTS is not maintained on write. Verdict: WEAKENED (TESTED + DOC-BASED)

What changed in 2.0: an opt-in incremental mode now exists and works for inserts and
deletes.

- 2.0 docs: `create_fts_index(..., overwrite = 0, incremental = 0)`. "incremental: Whether to
  maintain the FTS index with triggers after inserts and deletes on the input table. Requires
  the document identifier column to be declared NOT NULL or PRIMARY KEY." The warning now
  reads: "By default, the FTS index will not update automatically when the input table
  changes. To keep it up to date, create the index with incremental = 1 ..., or recreate the
  index to refresh it." Source:
  https://duckdb.org/docs/2.0/core_extensions/full_text_search.html
- The option is **new in 2.0**. The 1.4 and 1.5 FTS pages list no `incremental` parameter
  (https://duckdb.org/docs/1.4/core_extensions/full_text_search.html,
  https://duckdb.org/docs/1.5/core_extensions/full_text_search.html). Both 1.4.1 and 1.5.5
  reject it: `Binder Error: Invalid named parameter "incremental" for function
  create_fts_index`. This settles the eval's open question: kb's bundled 1.4.1 does not have
  it.
- Tested on 2.0.0 (`scratch/t3_fts.sql`, run under the default and `incremental = 1`):

  | Case | Default index | `incremental = 1` |
  | --- | --- | --- |
  | Row inserted after index creation matches | no (0) | **yes (1)** |
  | Deleted row still matches | n/a | no (correct) |
  | Inserted then `ROLLBACK`: ghost term matches | n/a | no (correct) |
  | Insert after close and reopen | n/a | matches (triggers persist) |
  | `UPDATE` of the text column: new term matches | n/a | **no (0)** |
  | `UPDATE`: old term still matches | n/a | **yes (1), stale** |
  | `UPDATE` with `SET force_update_to_del_and_insert = true` | n/a | still stale |
  | Index on a nullable id column | n/a | `Invalid Input Error: incremental FTS indexes require the document id column to be NOT NULL` |

  The extension maintains the index with 10 `AFTER INSERT` and `AFTER DELETE` triggers
  (`__fts_fts_main_c_ai_*` and `__fts_fts_main_c_ad_*`). It has no update triggers.

What this means for kb: the eval's "not maintained on write by default" is still literally
true, and still true of the DuckDB that kb links today. Under 2.0, though, an incremental
index would fit kb's write path. `ReplaceEntryChunks` deletes old chunks and inserts new
ones and never updates chunk text in place, so the insert and delete triggers cover it, and
rollback behaves. The remaining gaps:

- it needs the Go driver moved to DuckDB 2.0 (see C6);
- `UPDATE` silently leaves the index stale, which is a trap for future code;
- it is a new feature whose cost per write (trigger maintenance of the dictionary and stats
  tables) was not measured here.

### C4: extensions are not in the Go driver and are fetched at runtime. Verdict: HOLDS (DOC-BASED); one instruction is stale

- The duckdb-go README ("By default, duckdb-go statically links pre-built DuckDB libraries")
  shows a prebuilt link line with autocomplete, core_functions, icu, json, parquet, tpcds and
  tpch. It has no fts, vss or quack, so all three would still be autoloaded from the extension
  repository on first use. In 2.0, Quack too is "transparently autoinstalled and autoloaded on
  first use" (https://duckdb.org/docs/2.0/core_extensions/quack.html).
- Stale detail: the eval says static bundling means "building DuckDB's `bundle-library`
  target". For v2 the README says: "DuckDB v2 removed the `bundle-library` target. Link a
  static extension loader and the v2 set of archives instead." 2.0 also adds static linking
  of C-API extensions (duckdb/duckdb PR #22251). A static build of fts and vss is still
  possible, through a different recipe. Source:
  https://github.com/duckdb/duckdb-go/blob/main/README.md

### C5: DuckPGQ is a community extension, not bundled. Verdict: HOLDS (TESTED)

`INSTALL duckpgq FROM community; LOAD duckpgq;` works on 2.0.0 (duckpgq `9c11bc192a`). It is
still a community extension, downloaded at runtime. The 2.0 announcement does not mention
graph or PGQ support (https://duckdb.org/2026/10/21/announcing-duckdb-200.html).

### C6: Go driver facts. Verdict: HOLDS in substance; the module coordinates are out of date (DOC-BASED)

- `github.com/marcboeker/go-duckdb` is **archived** on GitHub. Maintenance moved to
  `github.com/duckdb/duckdb-go`. Its v2.20000.0 (2026-10-22) "Upgrades to DuckDB v2.0.0
  through duckdb-go-bindings v0.20000.0" and notes behaviour changes, including "dropped rows
  after a failed appender flush" and native libraries now served from `go.duckdb.org`.
  Source: https://github.com/duckdb/duckdb-go/releases/tag/v2.20000.0
- It is still cgo, and still pins one DuckDB version per bindings release (the version is
  encoded in the tag). The eval's point holds.
- This bears on kb even with no pivot: `kb stats` depends on an archived module that will
  never ship DuckDB 2.0.

## Surprises in 2.0 that matter for kb

1. **Quack** is the one change that touches the decisive argument. DuckDB now has an
   official, MIT-licensed, localhost-by-default server with multi-process writers. The FAQ
   claims "a few thousand writes per second on a server with 8 CPUs and 32 GB RAM". The tests
   above confirm it serialises concurrent short-lived writers correctly. It is beta, and it
   changes kb from "a file" into "a file plus a daemon".
2. **FTS `incremental = 1`** (2.0 only) removes most of the FTS-maintenance objection for
   kb's delete-and-insert write path.
3. **Triggers** are core SQL in 2.0 (`CREATE TRIGGER`, `BEFORE`/`AFTER`, row and statement
   scope, "maintain materialized views incrementally"). This is how incremental FTS works,
   and it would let edge or prior tables be maintained in-engine.
4. **`NEAREST` joins**, which "turn top-k similarity search into a join clause", plus the
   `hnsw_prefilter` setting. These are ergonomic gains, but they do not address the
   experimental persistence of HNSW.
5. **Storage format:** "DuckDB versions before 2.0 cannot read database files created by
   DuckDB 2.0." This does not matter for kb's in-memory `kb stats` copy, but it would matter
   for any persisted DuckDB file.

None of these changes the graph picture: DuckPGQ is still community-only, and the eval's
plan (graph tables in SQLite, analytics in the DuckDB copy) does not depend on DuckDB graph
features.

## Verdicts

| Claim | Verdict | Evidence | Key 2.0 source |
| --- | --- | --- | --- |
| C1 one writer process per file; kb would need its own daemon | **WEAKENED**: still one in-process writer and no lock wait, but 2.0 offers Quack (beta) for multi-process writes | TESTED 2.0.0 + DOC | docs/2.0/connect/concurrency; quack/faq ("Not yet ... beta") |
| C1b no reader alongside a writer | **HOLDS** | TESTED 2.0.0 | docs/2.0/connect/concurrency |
| C2 HNSW persistence experimental | **HOLDS** | TESTED 2.0.0 + DOC | docs/2.0/core_extensions/vss |
| C2b HNSW in RAM, delete marking | **HOLDS** | DOC | docs/2.0/core_extensions/vss |
| C3 FTS not maintained on write | **WEAKENED**: `incremental = 1` (new in 2.0, absent in 1.4.1 and 1.5.5) maintains it on insert and delete, but not on UPDATE | TESTED 2.0.0, 1.5.5, 1.4.1 + DOC | docs/2.0/core_extensions/full_text_search |
| C4 fts/vss not bundled in the Go driver | **HOLDS** (the `bundle-library` recipe is stale) | DOC | duckdb-go README |
| C5 DuckPGQ community-only | **HOLDS** | TESTED 2.0.0 | 2.0 announcement; community install |
| C6 cgo, version-pinned Go driver | **HOLDS** (module is now `duckdb/duckdb-go`; marcboeker's repo is archived) | DOC | duckdb-go v2.20000.0 release |

## Overall verdict

**The no-pivot recommendation STANDS WITH QUALIFICATIONS.** No pillar fell. The process-model
blocker survives 2.0 for kb's workload, many short-lived agent processes each writing a log
row, because the only multi-process path is a beta client-server protocol. HNSW persistence is
still experimental. The qualifications:

1. **Restate the concurrency argument.** It is no longer "DuckDB cannot do multi-process
   writes; kb would have to build a daemon". It is now: "multi-process writes need DuckDB's
   Quack server, which is beta, adds a daemon, token and lifecycle to every knowledge store,
   forbids direct file access while it runs, and costs roughly 0.45 s or more per short-lived
   client call against milliseconds for SQLite".
2. **Restate the FTS argument.** Incremental FTS exists in 2.0 and fits kb's
   delete-and-insert write path. The objection is now narrower: it needs a 2.0 driver, it is
   new and unmeasured, and `UPDATE` leaves it silently stale.
3. **Update the Go driver facts.** kb's `marcboeker/go-duckdb/v2` dependency is archived; the
   live module is `github.com/duckdb/duckdb-go/v2` (v2.20000.0 = DuckDB 2.0.0). The static
   extension build recipe changed. This is a maintenance item for `kb stats` regardless of the
   pivot question.

What would reopen the question: Quack declared stable or production-ready, or kb gaining a
long-lived per-store daemon for other reasons (at which point Quack's cost mostly
disappears), together with HNSW persistence leaving experimental status. Until then, the
eval's §4 split (SQLite serves, DuckDB analyses an in-memory copy) is unaffected by 2.0.

## Reproducing

The test scripts used were (scratch, not committed):

- `t1_lock.sh` (holder plus second writer or reader);
- `t1b_race.sh` (N concurrent direct writers);
- `t2c_burst.sh` (Quack server plus N concurrent client writers, each `ATTACH
  'quack:localhost:PORT' AS kb (TOKEN ...); INSERT ...`);
- `t5_sqlite.sh` (the SQLite baseline);
- `t3_fts.sql` (the FTS cases in the C3 table);
- `t4_hnsw.sh` (HNSW with and without the flag, plus reopen).

Each is a few lines and fully described by the tables above. Download:
`https://github.com/duckdb/duckdb/releases/download/v2.0.0/duckdb-cli-linux-amd64.tar.gz`.
Use `SET extension_directory=...` to keep extensions in the scratch directory.
