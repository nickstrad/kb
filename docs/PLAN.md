# kb v2 plan

Branch: `main` (the v2 rework replaces the old app). The orchestrator commits and pushes `main` after every wave once it's green (owner, 2026-10-30). Item agents never commit. Every command below assumes `go test -tags fts5 ./...` / `go build -tags fts5 ./cmd/kb`.

---

## 1. Goals, non-goals, decisions

### Goals
- A simple SQLite tool that adds ingestion and retrieval to projects. It works per project, per machine, and for one machine with many projects.
- Zero setup beyond `kb init`. No env vars, no external services, and the embedder runs in-process.
- Retrieval quality can be measured (`kb eval`, `kb explain`) before it is tuned (config weights, synonyms, priors).
- Output that agents can use cheaply (token budgets, expansion, `kb context`).

### Non-goals
- Servers, daemons, graph DBs, hosted vector DBs, remote embedding APIs (for now).
- A second storage backend. We build the *seam* only.
- **Cross-platform development or testing.** Development and `go test` happen on macOS darwin/arm64 only. Other platforms are *built* by the release pipeline and smoke-checked with `kb version`. They are not tested.
- Windows binaries.

### Binding decisions (owner) and why
| # | Decision | Why / consequence |
|---|---|---|
| D1 | Scope is per-project, per-machine (`--global`), and many projects on one machine (cross-store search). | Keeps kb a file-local tool. |
| D2 | Embedding is in-process via `github.com/rostamlabs/rembed` v0.3.0 (pure Go, Apache-2.0). Default model `BAAI/bge-small-en-v1.5` (384-d, CLS pooling, ~67 MB; listed in rembed's validated table). Model2Vec-in-repo is the fallback if the spike (0.1) fails. | No Ollama/API key. kb must add query/doc prefixes itself (per-model table, §2.4). go.mod moves to `go 1.26.1`. |
| D3 | Delete `internal/embed/{ollama,openai,provider}`. Keep `embed.Embedder`, `embed.None`, `embed/fake`. | Others can be added later behind the same interface. |
| D4 | Remove all `KB_*` / `OPENROUTER_API_KEY` env vars. Replace them with root discovery, `--root`, `--global` and config files (the store's `config.toml`, D9). `$EDITOR` stays. | `HF_TOKEN` (read by rembed itself) is not kb's variable and keeps working. |
| D5 | Remove DuckDB. `kb stats` runs on SQLite, with percentiles computed in Go. Add narrow store interfaces with `internal/store/sqlite` as the only implementation. | Drops ~20 indirect deps (arrow, duckdb bindings). A future DuckDB/libSQL backend implements the same 3 interfaces. |
| D6 | Remove `skills/update-knowledge-store`. A README snippet plus `kb help` become the source of truth. Feedback is optional. `kb show <path>` of a recent hit is logged as an implicit "opened" signal. | Agents skip bookkeeping and the log still gets signal. |
| D8 | No existing users before v1. Breaking changes are free until v1 ships; schema migrations matter again after v1 (owner, 2026-10-30). | Legacy layouts, adoption paths and old-user notes (§4) are dropped. The migration framework stays (1.8), because v1 stores in the owner's projects will need it. |
| D9 | Named stores: a *location* (`<root>/.kb/`, marker `.kb/kb.toml`) holds *n* named stores, each under `.kb/stores/<name>/{config.toml,kb.sqlite}` with its own sources, embedder and search log (owner, 2026-10-30; design: `docs/design/named-stores.md`). | `kb init [--name]`, `--store REF`, `kb stores`; refs `NAME`, alias, `global`, `NAME@LOC`; cross-store search covers stores in the same location and in other locations. Items 1.7a/1.7b. |
| D10 | Migrations are first-class: every versioned artefact (store index DB, embedding cache, store config, location file) implements `migrate.Artefact`. Durable artefacts migrate only through `kb migrate up` (with an automatic backup); the disposable embedding cache migrates on open (owner, 2026-10-30; design: `docs/design/migrations.md`). | Commands refuse an older store with "run kb migrate up". Items 1.8a/1.8b. |
| D7 | Item agents never commit. The orchestrator commits and pushes `main` after each wave-end pass, once vet, tests and staticcheck are green (owner, 2026-10-30). | `main` tracks progress wave by wave; agents stay out of git. |

### Planner decisions (can be changed; see Open questions)
- **Config format is TOML** via `github.com/BurntSushi/toml` (zero deps). **Globs** use `github.com/bmatcuk/doublestar/v4` (zero deps), because `filepath.Match` has no `**`.
- **Two coupled items are pulled forward into Phase 0:**
  - The lazy vector width (owner listed it under Phase 1). The default 384-d model cannot be stored in today's `FLOAT[768]` `chunks_vec`.
  - Removing `KB_ROOT` waits for root discovery (1.1). Every other env var goes in Phase 0.
- **`searches.kb_version`** is renamed to `corpus_rev` (historic rows keep their meaning). A new `kb_version` stores the binary version. Recording the corpus rev is opt-in (`log.corpus_rev = true`), so there is no git shell-out per search by default.
- **The global location and machine config are one thing.** The global root is `$XDG_DATA_HOME/kb` (default `~/.local/share/kb` on both OSes). Its `.kb/kb.toml` holds machine-level `[aliases]` (D9). There is no separate machine config file.
- **Release builds use native GitHub runners for cgo:** `macos-14` (arm64) builds darwin/arm64 natively and darwin/amd64 with `CC="clang -arch x86_64" GOARCH=amd64`. `ubuntu-latest` builds linux/amd64 and `ubuntu-24.04-arm` builds linux/arm64, both natively. Tests run only on the developer's Mac and in a macOS-only CI job. Fallback if a Linux runner is unavailable: `CC="zig cc -target <arch>-linux-gnu"` from the macOS job.
- **Search never downloads a model.** `kb init`, `kb model pull`, `kb reindex` and `kb add` may download. If the model is missing at search time, hybrid search degrades to `fts-fallback` (exit 2, as today) with the hint `kb model pull`.

---

## 2. Target architecture

### 2.1 Package layout (after Phase 1)
```
cmd/kb/main.go                version from runtime/debug.ReadBuildInfo (ldflag override for releases)
internal/workspace/           location discovery (.kb/kb.toml), --root/--global/--store, store refs, paths (DB, config, models dir, cache dir)
internal/migrate/             Artefact interface, Steps[T], runner (lock, backup, apply, verify) (1.8)
internal/config/              TOML schema, defaults, validation, --set overrides, effective-settings JSON
internal/store/               interfaces + row/value types only (no SQL)
internal/store/sqlite/        Open/migrations/schema.sql, Index+Retriever+SearchLog impl, FTS5 quoting
internal/embed/               Embedder, None, prefixes (DocText/QueryText), Cached decorator (Phase 3)
internal/embed/local/         rembed wrapper + model table (prefixes, dims, matryoshka flag)
internal/embed/fake/          deterministic test embedder (unchanged)
internal/embed/cache/         machine-wide embedding cache (Phase 3)
internal/entry/               Discover over configured sources; front matter (optional for glob sources)
internal/chunk/               unchanged rules; title fallback inputs
internal/reindex/             takes store.Index
internal/search/              Searcher over store.Retriever; Fuse, cap, query groups, explain trace
internal/render/              (Phase 4) budgeted/elastic rendering, expansion
internal/eval/                (Phase 2) gold extraction, replay, metrics, baseline, sweep
internal/stats/               query registry in SQLite SQL + Go percentiles
internal/links/               (Phase 5) markdown/wikilink/front-matter edge extraction
internal/prior/               (Phase 6) feedback prior, pure functions
internal/index/               index.md export (unchanged; scoped to the `entries` source)
internal/cli/                 commands
```
Deleted: `internal/embed/ollama`, `internal/embed/openai`, `internal/embed/provider`, `internal/stats/duck.go` (+`duck_test.go`), `skills/`.

### 2.2 Store interfaces (derived from today's callers)
Today's callers and what they actually use:
- `reindex.reindexOne`: `GetEntryByPath`, `ExistingEmbeddings`, `ReplaceEntryChunks`.
- `reindex.All`: `ListEntries`, `DeleteEntry`.
- `cli/reindex.go:ensureEmbedMetaForReindex`: `EnsureEmbedMeta`, `EmbedMeta`, `ResetForReindex`.
- `cli/show.go:showEntryChunks`: `ListChunks`.
- `search.Searcher`: inline SQL in `ftsList`, `vecList`, `filterByTag`, `loadChunkMeta`.
- `search/log.go`: `Log`, `Feedback`, `FeedbackNone`, `Last`.
- `stats`: SQL over the log.

```go
// internal/store/store.go
package store

type Store interface {
	Index
	Retriever
	SearchLog
	Path() string
	ReadOnly() bool
	Close() error
}

// Index: everything reindex/add/edit/rm/show need. Moved from store/entries.go.
type Index interface {
	EmbedMeta(ctx context.Context) (EmbedMeta, bool, error)
	EnsureEmbedMeta(ctx context.Context, m EmbedMeta) error // creates the vector table at m.Dim lazily
	ResetIndex(ctx context.Context) error                   // was ResetForReindex; never touches the log
	Entry(ctx context.Context, path string) (*EntryRow, error)
	Entries(ctx context.Context) ([]EntryRow, error)
	Chunks(ctx context.Context, entryID int64) ([]ChunkRow, error)
	Embeddings(ctx context.Context, entryID int64) (map[string][]float32, error) // by text_hash
	ReplaceEntry(ctx context.Context, e EntryInput, chunks []ChunkInput) (int64, error)
	DeleteEntry(ctx context.Context, path string) error
	// Phase 5: ResolveEdges(ctx) error; Related(ctx, path string, depth int) ([]Edge, error)
}

// Retriever: the backend-specific retrieval primitives. Fusion stays in internal/search.
type Retriever interface {
	FTS(ctx context.Context, q FTSQuery) (FTSResult, error)
	KNN(ctx context.Context, q KNNQuery) ([]VecRow, error)
	ChunkMeta(ctx context.Context, ids []int64) (map[int64]ChunkMeta, error) // entry path/id, ord, heading, text, updated
}

type FTSQuery struct {
	Groups  [][]string  // raw user fields; inner slice = OR'd alternatives (synonyms), groups joined by Op
	Op      Op          // OpAnd | OpOr | OpPhrase | OpNear (Phase 4)
	Weights BM25Weights // {Text, Heading, Title}; Explain passes single-column weights
	Exact   bool        // Phase 4: query the unstemmed identifier table
	Tag     string
	Limit   int
}
type FTSResult struct {
	Rows  []FTSRow
	Match string // the backend-native expression actually run (logged, shown by explain)
}
type KNNQuery struct {
	Vec       []float32
	K         int
	Tag       string
	OverFetch int
}

// SearchLog: the log tables. SearchRecord is today's search.Outcome minus the Result.
type SearchLog interface {
	LogSearch(ctx context.Context, r SearchRecord) (int64, error)
	Feedback(ctx context.Context, searchID int64, rank int, useful bool, note string) error
	FeedbackNone(ctx context.Context, searchID int64, note string) error
	LastSearch(ctx context.Context, caller string) (LastSearch, error)
	RecordOpen(ctx context.Context, entryPath string, window time.Duration) (searchID int64, ok bool, err error)
	Judged(ctx context.Context, f LogFilter) ([]JudgedSearch, error) // Phase 2 (eval), Phase 6 (prior)
	QueryLog(ctx context.Context, sql string, args ...any) (*Table, error) // stats; SQL is backend dialect
}
```
Rules:
- The FTS5 quoting/sanitising rules move from `search.SanitizeFTS` into `store/sqlite`, because they encode unicode61 tokenizer behaviour (`ftsBaseRune`, `ftsSkewNotToken`). This includes the fuzz corpus `internal/search/testdata/fuzz/FuzzSanitizeFTS`.
- `search` only splits fields and builds `Groups`.
- `stats` keeps SQLite-dialect SQL. A future backend would supply its own SQL map; there is no further abstraction now.

### 2.3 Config file
Two files (D9). The **location file** `.kb/kb.toml` marks the location, names the default store and holds aliases. The **store config** `.kb/stores/<name>/config.toml` holds everything else. In a store config, a missing key means the default, and unknown keys are an error (BurntSushi `MetaData.Undecoded`). Aliases are read from the current location's `kb.toml`, then from the global location's.

Example `.kb/kb.toml` (1.7b):
```toml
version = 1
default_store = "default"

[aliases]                          # store refs: NAME | NAME@LOC | PATH (a location's default store)
notes = "~/knowledge"
adr   = "adr@~/src/proj"
```

Example `.kb/stores/default/config.toml`:
```toml
version = 1                        # config schema version

[embed]
model   = "BAAI/bge-small-en-v1.5" # HF id, local model dir, or "none" (FTS only)
dim     = 0                        # 0 = native; >0 = Matryoshka truncation (only models flagged matryoshka)
int8    = false                    # rembed.WithInt8 (weight-only); part of the embed_meta identity
workers = 0                        # 0 = rembed default
batch   = 16                       # texts per Embed call (reindex.defaultBatchSize today)

# Sources: explicit, written by kb init (default store: path = "data"; a named store: "data-<name>").
# Paths are relative to the location root. No [[sources]] = nothing indexed (doctor warns).
[[sources]]
path   = "data"
layout = "entries"                 # *.md + <dir>/README.md + docs/*.md, strict front matter (today's rules)

[[sources]]
glob    = "docs/**/*.md"
layout  = "files"                  # each match is an entry; front matter optional
exclude = ["docs/archive/**"]

[search]
k               = 8
list_limit      = 20               # search.ListLimit
candidate_limit = 40               # search.CandidateLimit
per_entry_cap   = 3                # search.PerEntryCap
rrf_k           = 60               # search.RRFConstant
vec_overfetch   = 60               # search.vecOverFetch
bm25            = { text = 1.0, heading = 2.0, title = 3.0 }
list_weights    = { fts = 1.0, vec = 1.0, exact = 1.0, memory = 1.0 }  # RRF term multipliers
phrase_boost    = true             # Phase 4
exact_list      = true             # Phase 4
synonyms        = ""               # Phase 5; "" = .kb/synonyms.txt, then the global store's
spell           = true             # Phase 5; only when FTS returns < 3 rows
supersede_penalty = 0.5            # Phase 5
stores          = []               # Phase 3; default extra store refs, e.g. ["global", "adr"]

[output]
budget = 0                         # tokens for kb search; 0 = unlimited (Phase 4)
context_budget = 2000              # default for kb context
expand = "none"                    # none | section | entry

[log]
corpus_rev  = false                # record `git rev-parse --short HEAD` of the root per search
open_window = "2h"                 # kb show counts as "opened" for searches this recent

[stale]
days = 180                         # Phase 5

[eval]
min_judged = 30                    # gate for --sweep
holdout    = 0.3

[prior]                            # Phase 6; all off by default
enabled        = false
min_judged     = 50
half_life_days = 90
max_boost      = 0.2
query_memory   = false
```

### 2.4 Embedding model table (`internal/embed/local/models.go`)
| model | dim | query prefix | doc prefix | matryoshka |
|---|---|---|---|---|
| BAAI/bge-small-en-v1.5 (default) | 384 | `Represent this sentence for searching relevant passages: ` | — | no |
| BAAI/bge-base-en-v1.5 | 768 | same as bge-small | — | no |
| Snowflake/snowflake-arctic-embed-s | 384 | same as bge | — | no |
| intfloat/multilingual-e5-small / -base | 384/768 | `query: ` | `passage: ` | no |
| nomic-ai/modernbert-embed-base | 768 | `search_query: ` | `search_document: ` | yes |
| google/embeddinggemma-300m | 768 | `task: search result \| query: ` | `title: none \| text: ` | yes |
| sentence-transformers/all-MiniLM-L6-v2, thenlper/gte-small | 384 | — | — | no |

- An unknown model (a local dir or another HF id) gets no prefixes and a warning.
- The embed_meta identity is `model[+int8]`, with `dim` stored separately.
- `embed.DocText`/`QueryText` stop defaulting to nomic prefixes (`embed.go:DocPrefix/QueryPrefix`). No `Prefixer` now means no prefix.

**rembed cache / loading facts.** These were verified by reading `~/go/pkg/mod/github.com/rostamlabs/rembed@v0.3.0`:
- `internal/hub.CacheDir` reads `$REMBED_CACHE`, else `os.UserCacheDir()/rembed`. kb calls `os.Setenv("REMBED_CACHE", <UserCacheDir>/kb/models)` in-process, once, before `Load`. It is process-local and invisible to users.
- `Load(ref)` treats `org/name` as a *local path* when its first segment exists relative to cwd. kb therefore always passes `"hf:"+id` for hub ids, and absolute paths for local dirs.
- `HF_TOKEN` is honoured by rembed, which is fine.
- `Embedder.Close()` exists. kb's `embed.Embedder` gains an optional `io.Closer` check in the CLI.

### 2.5 Schema migrations (append-only)
Each migration below is appended in `internal/store/sqlite/migrations.go` and never edits `schema.sql`. Every one preserves `searches`, `search_results`, `search_candidates` and `search_feedback`. From 1.8a each is a `migrate.Step` with a stable ID `index/000N-<slug>` and an Effect: v3 = `reembed`, v7 = `reindex`, the others `none`. The "Reindex?" column is that Effect. From 1.8b, migrations run only through `kb migrate up`, which backs up first (D10, `docs/design/migrations.md`). At the v1 tag, all pre-v1 steps are squashed into one baseline (D8).

| v→ | Phase | Statements | Reindex? |
|---|---|---|---|
| 1→2 | 0.6 | `ALTER TABLE searches RENAME COLUMN kb_version TO corpus_rev; ALTER TABLE searches ADD COLUMN kb_version TEXT;` `CREATE TABLE search_opens(search_id INTEGER NOT NULL REFERENCES searches(id) ON DELETE CASCADE, entry_path TEXT NOT NULL, ts TEXT NOT NULL, PRIMARY KEY(search_id, entry_path));` | no |
| 2→3 | 0.5 | `DROP TABLE chunks_vec; DELETE FROM embed_meta; UPDATE entries SET body_hash = '';`. The vector table is then created by `EnsureEmbedMeta` as `CREATE VIRTUAL TABLE IF NOT EXISTS chunks_vec USING vec0(chunk_id INTEGER PRIMARY KEY, embedding FLOAT[%d] distance_metric=cosine)` with the recorded dim. FTS keeps working before the reindex. | **yes**: plain `kb reindex` re-embeds everything (body_hash cleared) |
| 3→4 | 2.5 | `ALTER TABLE searches ADD COLUMN settings TEXT; ADD COLUMN settings_hash TEXT; ADD COLUMN fts_query TEXT;` | no |
| 4→5 | 3.2 | `ALTER TABLE search_results ADD COLUMN store TEXT; ALTER TABLE search_candidates ADD COLUMN store TEXT;` (NULL = primary; otherwise the canonical ref `NAME@ABSROOT`, D9) | no |
| 5→6 | 4.5 | `CREATE VIRTUAL TABLE chunks_exact USING fts5(text, heading, content='', contentless_delete=1, tokenize="unicode61 remove_diacritics 0 tokenchars '_-.:/'");` backfilled with `INSERT INTO chunks_exact(rowid,text,heading) SELECT id,text,heading FROM chunks;`. Add `exact_rank INTEGER, exact_score REAL` to `search_results` and `search_candidates`. | no (backfill from `chunks`) |
| 6→7 | 5.2 | `CREATE TABLE edges(src_entry_id INTEGER NOT NULL REFERENCES entries(id) ON DELETE CASCADE, dst_path TEXT NOT NULL, dst_entry_id INTEGER, rel TEXT NOT NULL, PRIMARY KEY(src_entry_id,dst_path,rel));` `CREATE INDEX edges_dst ON edges(dst_entry_id);` `CREATE TABLE entry_tags(entry_id INTEGER NOT NULL REFERENCES entries(id) ON DELETE CASCADE, tag TEXT NOT NULL, PRIMARY KEY(entry_id,tag));` backfilled via `json_each(entries.tags)` lowercased. `ALTER TABLE entries ADD COLUMN aliases TEXT NOT NULL DEFAULT '[]'; UPDATE entries SET body_hash='';` | **yes**: plain `kb reindex` (edges/aliases); embeddings are reused by text_hash, so it is cheap |
| 7→8 | 6.2 | `ALTER TABLE search_results ADD COLUMN prior REAL; ALTER TABLE search_candidates ADD COLUMN prior REAL;` | no |

Contentless FTS tables can be repopulated from `chunks` + `entries.title`, so tokenizer changes never need re-embedding.

### 2.6 Containers (C4 sketch, after D9/D10)
```
[ kb CLI process ]  workspace: Location (.kb/kb.toml) -> Store(name); migrate: Artefact per versioned file
   +--rw--> <root>/.kb/stores/<primary>/{config.toml, kb.sqlite}   index + search log (the log is written here only)
   +--ro--> <root>/.kb/stores/<other>/kb.sqlite                    same-location stores    } cross-store
   +--ro--> <other-root>/.kb/stores/<name>/kb.sqlite               aliases / NAME@LOC      } fan-out (3.x)
   +--ro--> $XDG_DATA_HOME/kb/.kb/{kb.toml [aliases], stores/default/}  global location
   +--> store.Store per store (sqlite today; a backend key later, through the seam)
   +--> embed/local: one loaded model per distinct store model --> <user cache>/kb/models
   +--> embed cache keyed (model, dim, hash) --> <user cache>/kb/embed-cache.sqlite (shared; migrates on open)
```
Before D9: one store per root (`<root>/.kb/{config.toml, kb.sqlite}`), and every open migrated automatically.

---

## 3. Phases

Item format: **Goal / Files / Steps / Tests / Accept / Deps / Risk**. ∥ means it can run in parallel with the other ∥ items of that phase.

### Phase 0: Cleanup and removals

**0.1 rembed spike (darwin/arm64) ∥**
- Model: Sonnet (bounded measurement spike, numbers not design)
- Status: done
- Goal: prove rembed works for kb on the dev platform before anything is deleted, and confirm that the release targets still compile.
- Files: a throwaway `cmd/rembed-spike/main.go` on a scratch branch (not merged).
- Steps:
  1. `go get github.com/rostamlabs/rembed@v0.3.0` and bump go.mod to `go 1.26.1`.
  2. Build together with `-tags fts5` on darwin/arm64 (check there is no symbol clash with the mattn/sqlite-vec cgo objects).
  3. `Load("hf:BAAI/bge-small-en-v1.5")` with `REMBED_CACHE` set by `os.Setenv`. Embed 200 real chunks from `~/knowledge` in batches of 16.
  4. Record cold load time, warm load time (the per-invocation cost), single-query latency, batch throughput, RSS and binary size delta. Repeat with `WithInt8()`.
  5. Sanity-check retrieval: cosine of query↔own chunk greater than query↔random.
  6. Compile-only check for the other targets:
     - `CGO_ENABLED=1 GOARCH=amd64 CC="clang -arch x86_64" go build -tags fts5 ./cmd/kb` on the Mac (darwin/amd64).
     - `GOOS=linux GOARCH=amd64 go vet -tags fts5 ./internal/embed/...` (rembed's pure-Go part; no cgo needed).
     - The full Linux builds are confirmed by the first dry-run of the release workflow (1.6).
- Tests: none merged. Put the numbers in the PR description and the README "Performance" note.
- Accept: warm load plus one query is under ~500 ms on darwin/arm64, and the darwin/amd64 build succeeds. If it fails, use the fallback: Model2Vec (static token-embedding table, ~30 MB, pure Go lookup+mean) in `internal/embed/local/model2vec.go` behind the same interface.
- Deps: none.
- Risk: warm-load cost dominates per-command latency because the 67 MB safetensors parse every time. Mitigation: `WithDiskWeights` is "currently wired for qwen3" only (README), so measure first. If it is too slow, cache a converted pack under the models dir (ask upstream) or accept it. darwin/amd64 binaries can only be smoke-run under Rosetta (`arch -x86_64 ./kb version`).

**0.2 Hygiene ∥**
- Model: Haiku (mechanical renames and comment rewording)
- Status: done
- Goal: remove stale references and make tests pass on macOS.
- Files: the 28 files that cite `plan.md`/P-labels/D-labels (chunk.go, entry.go, add.go, show.go, edit.go, rm.go, search.go, doctor.go, reindex.go, root.go, log.go, index.go, stats/*, store/*, and tests). Rename `internal/cli/p42_test.go`→`show_edit_test.go`, `p42_regression_test.go`→`edit_regression_test.go`, `p44_test.go`→`index_hooks_test.go`. Delete `root.go:notImplemented`.
- Steps:
  1. Reword comments to state the rule itself instead of a phase label (e.g. "chunking rule 6" stays, "plan.md P2.4" goes).
  2. Replace the `sed -i` EDITOR in `p44_test.go:52` and `p42_regression_test.go:70` with a helper `fakeEditor(t, from, to) string`. It writes `#!/bin/sh\nsed 's/FROM/TO/' "$1" > "$1.tmp" && mv "$1.tmp" "$1"` into `t.TempDir()`, chmods it 0755 and returns its path. `runEditor` already goes through `sh -c`.
- Tests: the renamed tests pass on macOS.
- Accept: `go test -tags fts5 ./internal/cli/` passes on macOS, and `grep -rnE 'plan\.md|\bP[0-9]\.[0-9]\b|notImplemented' --include='*.go' .` is empty. The portable `sh`/`sed` fake editor is still preferred: it avoids GNU-vs-BSD `sed -i` and has no Linux test target. It is an interim fix: once the `deps.editor` injection (CODING_STYLE.md §5) lands, tests inject a Go func editor instead (TESTING.md §7).
- Deps: none.
- Risk: none.

**0.3 Remove the skill; make help the source of truth ∥**
- Model: Haiku (help text and README prose)
- Status: done
- Goal: no skill. Agents learn from `kb help` and the README snippet.
- Files: delete `skills/`. Edit `README.md` and `internal/cli/root.go:newRootCmd` (`Long`), `feedback.go`, `search.go` help.
- Steps:
  1. Rewrite the root `Long` text:
     - what kb is;
     - the store-discovery rule;
     - the 2-step loop "search → show" with feedback optional ("`kb feedback` is optional; `kb show` of a hit is recorded automatically");
     - config file location.
  2. Add a README section "Using kb from an agent". It is a ≤15-line block to paste into CLAUDE.md/AGENTS.md (`kb search "<q>" --caller claude`, `kb show <path>`, `kb context "<task>" --budget 2000` once Phase 4 lands, optional `kb feedback`).
- Tests: `TestRootHelpMentionsShowAndOptionalFeedback` (string checks).
- Accept: `kb help | grep -c KB_` gives 0, and `ls skills` fails.
- Deps: env text is finalised in 0.5/1.1 (edit help again there).
- Risk: none.

**0.4 Remove DuckDB; stats on SQLite ∥**
- Model: Sonnet (SQL dialect port against existing expectations); review: Opus via testing-standards-pass
- Status: done
- Goal: `kb stats` gives identical numbers with no DuckDB.
- Files: delete `internal/stats/duck.go` and `duck_test.go`. Rewrite `internal/stats/queries.go`, add `internal/stats/run.go` and `percentile.go`. Edit `internal/cli/stats.go:runStats`.
- Steps:
  1. Open the DB read-only (`?mode=ro`). Apply the `Filter{Since, Caller}` by prefixing every query with `WITH fs AS (SELECT * FROM searches WHERE ts >= ?1 AND (?2 = '' OR caller = ?2))`, and rewrite the queries to read `fs` (and join `search_results`/`search_feedback` through it).
  2. Translate the dialect:
     - `bool_or(x)` → `max(x)`;
     - `CAST(.. AS DOUBLE)` → `CAST(.. AS REAL)`;
     - `date_trunc('week', ts)` → `date(ts, 'weekday 0', '-6 days')` (Monday);
     - `string_agg(DISTINCT caller, ',' ORDER BY caller)` → `(SELECT group_concat(c, ',') FROM (SELECT DISTINCT caller c FROM fs WHERE ... ORDER BY c))`;
     - remove the HUGEINT casts.
     - `FILTER (WHERE ...)` works in SQLite ≥3.30 (bundled 3.53).
  3. Percentiles: `Query` gains `Percentiles []Pct{Column, Source, P}`. The SQL selects `json_group_array(total_ms) AS p50_ms`, and `run.go` replaces the JSON array cell with `quantileCont(xs, p)`, which uses linear interpolation at `p*(n-1)`, the same as DuckDB `quantile_cont`.
  4. `go mod tidy`.
- Tests: port every expectation from `duck_test.go`/`queries_test.go` to run against a SQLite fixture. Add a `quantileCont` table test (n=0,1,2, even, odd).
- Accept: `go list -deps ./cmd/kb | grep -ci duckdb` gives 0. `kb stats overview` on a copy of `~/knowledge/.kb/kb.sqlite` matches the old binary's output (diff the two).
- Deps: none.
- Risk: subtle semantic drift in the gaps/trend queries. Mitigate by diffing old vs new outputs on the real log before deleting the old code.

**0.5 Replace embedders; lazy vector width** (pulled forward from Phase 1)
- Model: Opus (data-dropping migration, store/embed/cli seam, lazy vec table); review: Opus via testing-standards-pass + error-handling-pass
- Status: done (0.5a W1, 0.5b W2)
- Goal: in-process rembed embedder, FTS-only `none`, any dim.
- Files:
  - Delete `internal/embed/{ollama,openai,provider}`.
  - New `internal/embed/local/{local.go,models.go,local_test.go}`.
  - Edit `internal/embed/embed.go` (drop the nomic default prefixes, `ErrUnavailable` doc).
  - Edit `internal/cli/setup.go:newEmbedder` and `root.go` flags.
  - Edit `internal/store/entries.go` (`VecDim` gone; `EnsureEmbedMeta` creates `chunks_vec`; `clearChunksTx`/`ResetForReindex` skip `chunks_vec` when absent).
  - Edit `store.go` (migration 2→3) and `search/search.go:vecList` (skip when there is no vec table).
- Steps:
  1. `local.New(ModelSpec)` → `embed.Embedder` + `Prefixer` + `io.Closer`. It sets `REMBED_CACHE` before loading.
  2. `local.Pull(id)` downloads explicitly.
  3. Load failure or a missing model is wrapped in `embed.ErrUnavailable`, which keeps the search `fts-fallback` path and exit 2.
  4. Interim selection until config lands: persistent flags `--embedder local|none` and `--embed-model`. Defaults: local / bge-small.
  5. Delete `KB_EMBEDDER`, `KB_EMBED_*`, `KB_OLLAMA_URL` and the OpenRouter keys.
- Tests:
  - `local_test.go` is behind `//go:build model` (TESTING.md §7–8) and fails if the model is missing; the nightly job runs `kb model pull` first. It checks dim=384, unit norm, and that the query prefix is applied.
  - Store tests: open a v1 DB fixture → migrate → `EmbedMeta` absent, FTS search still works, `EnsureEmbedMeta(dim=384)` creates the table, insert/search OK.
  - Update `useFakeEmbedder` (`add_test.go:21`) to `fake.New("fake", 16)`.
- Accept: `KB_EMBEDDER=ollama kb reindex --all` ignores the env var. `kb reindex --all` on a fresh root records `BAAI/bge-small-en-v1.5`/384 in embed_meta, and `kb search` returns `mode hybrid`.
- Deps: 0.1.
- Risk: the migration runs on *any* open, including by a read-only command. The FTS-only interim behaviour plus a stderr hint ("vectors missing: run kb reindex") covers it.
- Carried from W1 pass (0.5b): `internal/store` is at 72% statement coverage, just above the 70% floor; most unreached lines are driver-error returns in `entries.go`/`store.go`. When 0.5b rewrites `entries.go`, add unit tests for its new paths (vector-width check at dim 0, the recorded dim and dim±1; migration 2→3 on a v2 database with vectors and on one without) so the package does not drop below the floor. `store.migrate` uses `context.Background()` because `Open` takes no ctx; decide in 0.5b/1.2 whether `sqlite.Open` takes a ctx.

**0.6 Log provenance, version, doctor ∥**
- Model: Opus (orchestrator; migration renames a logged column); review: Sonnet via testing-standards-pass
- Status: done
- Goal: an honest `kb_version`, no git per search, no `/usr/local/bin` check.
- Files:
  - `internal/search/log.go` (delete `GitShortHash` from the hot path; it moves to an optional helper).
  - `cli/search.go`.
  - `store` migration 1→2.
  - `cmd/kb/main.go`: `version()` uses `debug.ReadBuildInfo()`: `Main.Version` (or `(devel)`) + `vcs.revision[:12]` + `-dirty` when `vcs.modified`. An ldflag `-X main.version` wins.
  - `cli/doctor.go` (delete `checkBinary`; add a `version` ok-line with `os.Executable()`).
- Tests: log test asserts `kb_version` = `cli.Version` and `corpus_rev` NULL by default. Version-string unit test.
- Accept: `kb version` after `go install -tags fts5 ./cmd/kb` prints `kb (devel) <rev>` (not `dev`). `kb doctor` has no "binary" warning.
- Deps: none.
- Risk: none.

**0.7 Optional feedback + implicit opens ∥**
- Model: Sonnet (small, fully specified insert + tests); review: Sonnet via error-handling-pass (silent-error path)
- Status: done (W2)
- Goal: `kb show` records an open against the newest recent search that returned that entry.
- Files: `internal/cli/show.go:runShow`, `search/log.go` (or `sqlite` after 1.2), `cli/search.go:printResults` (drop the feedback hint line), `stats/queries.go` (`opened_pct` column in overview).
- Steps:
  1. After printing, open the DB only if it exists. Run `INSERT OR IGNORE INTO search_opens SELECT s.id, ?, now FROM searches s JOIN search_results r ON r.search_id = s.id WHERE r.entry_path = ? AND s.ts >= now - window ORDER BY s.id DESC LIMIT 1`.
  2. Errors are silent (stderr only with the root `--debug` flag, CODING_STYLE §1.5).
  3. Add `--no-log`.
  4. `--last` keeps requiring `--caller`, since `KB_CALLER` is gone (`root.go:Caller`).
- Tests: search → show → row exists. Show without a search → no row. Show of a non-returned entry → no row. A failure doesn't change the exit code.
- Accept: `kb search "x" --caller t && kb show <hit>` then `sqlite3 .kb/kb.sqlite 'select * from search_opens'` returns 1 row.
- Deps: migration 1→2 (0.6).
- Risk: none.

### Phase 1: Groundwork

**1.1 Workspace: root discovery, `--root`, `--global`; drop KB_ROOT**
- Carried from W3 pass: `cli.incidentLogPath` (report.go) finds `<root>/.kb` through the env-reading `Root()`; once discovery resolves the root, pass it through `deps` (or the resolved workspace) so an incident lands in the store the command ran on, and an incident before discovery succeeds goes to the user cache dir. `Root()`, `Caller()` (KB_CALLER) and `runEditor` ($EDITOR) are the last env reads in cli; route the root through deps here and leave KB_CALLER/EDITOR to 1.3.
- Model: Sonnet (well-specified; many mechanical call-site edits); review: Opus via error-handling-pass
- Status: done (W4). Partly superseded by D8/D9: the legacy `.kb/kb.sqlite` marker and the interim creation by `add`/`reindex` (DEFAULTS #110/#111) are removed in 1.7b, and the marker becomes `.kb/kb.toml`.
- Files: new `internal/workspace/workspace.go`. Edit `cli/root.go` (delete `Root`, `DefaultRoot`, `defaultRoot`, `KB_ROOT`, `Caller`'s env read), every `Root()` call site (add.go, edit.go, rm.go, show.go, list.go, index.go, search.go, feedback.go, stats.go, doctor.go, reindex.go) and `cli/setup.go`.
- Steps:
  1. `workspace.Resolve(rootFlag string, global bool) (*Workspace, error)`:
     - `--root` wins;
     - else `--global` → `$XDG_DATA_HOME/kb` or `~/.local/share/kb`;
     - else walk up from cwd for a directory containing `.kb/config.toml` or (legacy) `.kb/kb.sqlite`.
  2. No match → error: `no kb store found in <cwd> or its parents; run "kb init" here, or use --global / --root`.
  3. The struct carries `Root`, `KBDir`, `DBPath`, `ConfigPath`, and `ModelsDir`/`CacheDir` (from `os.UserCacheDir()/kb`).
  4. Switch test helpers: `newTempRoot` (`add_test.go:30`) runs `kb init --embedder none` semantics and `t.Chdir(root)`. The CLI tests are not parallel.
- Tests: discovery from a nested subdir; `--root` overrides; `--global`; legacy `.kb/kb.sqlite` without config; not found.
- Accept: `cd ~/knowledge/data && kb list` works, and `cd /tmp && kb list` prints the not-found message with exit 1.
- Deps: 0.5.
- Risk: tests that relied on `t.Setenv("KB_ROOT")` (feedback_test, search_cmd_test, stats_test, embedder_none_test) all need touching. This is mechanical.

**1.2 Store seam: interfaces + `internal/store/sqlite` ∥**
- Carried from W2 pass: (a) `search.hasVecTable` duplicates `store.vecTableExists`; keep one, in `sqlite/retrieve.go`. (b) `context.Canceled` (SIGINT/SIGTERM) is mapped to a friendly "interrupted" message only in `kb reindex` (Q21); add/edit/search/show still flatten it into "… context canceled" via `usageErr` — give `report` one rule (errors.Is Canceled → "interrupted", exit 1) once the chain survives to the top. (c) `stats.Open` now refuses any user_version but `store.SchemaVersion()` (Q19); when step 3 adds `sqlite.Open(ReadOnly)`, make stats use it so there is one version check. (d) Mutation survivors that only a driver failure reaches (store EnsureEmbedMeta/ReplaceEntryChunks/clearChunksTx err-checks) are listed in H20; the seam is the natural place for a fault-injecting test backend.
- Status: done (W3)
- Model: Opus (store seam, large cross-package move, interface design); review: Opus via testing-standards-pass
- Files: move `store.go`, `entries.go` and `schema.sql` to `internal/store/sqlite/`. New `internal/store/{store.go,types.go}`. Move the SQL from `search/search.go` (`ftsList`, `runFTS`, `vecList`, `filterByTag`, `loadChunkMeta`, `SanitizeFTS`, `tagPattern`) and `search/log.go` (all SQL) into `sqlite/retrieve.go` and `sqlite/log.go`. `search.Searcher{Store store.Retriever; Log store.SearchLog; ...}`. `reindex.Options.Store store.Index`.
- Steps:
  1. Pure move first: identical behaviour, all tests green.
  2. Then narrow the callers to the interfaces.
  3. `sqlite.Open(path, sqlite.Options{ReadOnly bool})`. Read-only skips migrations and refuses when `user_version < schemaVersion`, with "open it once with this kb to upgrade".
  4. `search.Fuse`, `applyCap` and `ClampK` stay in search.
- Tests: move the existing ones; add a compile-time `var _ store.Store = (*sqlite.Store)(nil)`.
- Accept: `go test -tags fts5 ./...` green, and `grep -rn 'database/sql' internal/search internal/reindex internal/cli` is empty except for stats.
- Deps: none (can start alongside 1.1). Do it before Phase 2.
- Risk: a large diff. Keep it a move-only commit plus a follow-up.
- Carried from W1 pass (errs package, CODING_STYLE §1.5, lands with 1.2/1.3):
  - `cli/add.go:143,284` and `cli/edit.go:97` rewrite error text with `strings.TrimPrefix(err.Error(), "kb reindex: ")` and flatten the chain into `usageErr`. Fix: `ensureEmbedMetaForReindex` returns an `*errs.Error` without a command prefix; each command passes it up.
  - Every `usageErr("kb <cmd>: %s", err)` on an unexpected store/stats/driver error (e.g. `cli/stats.go` named-query and `SearchesMatching` failures, `store.Open` failures in search/add/edit/show) exits 1 as a usage error; with `errs` these become `Internal` (exit 3) via `report`.
  - Boundaries that return plain `fmt.Errorf` errors to other packages: `stats.Open/Run/SQL/SearchesMatching`, `local.Embed/Ping/Pull` (Unavailable is a sentinel today), `reindex.All/One`. Wrap with `errs.Wrap` and the right Kind; an ad-hoc `--sql` syntax error is `Usage`, a named-query failure is `Internal`.
  - Assertions to add with `internal/assert`: `stats.quantileCont` Pre `0 <= p <= 1` (out of range panics with an index error today; p comes from the registry); `chunk.splitOversized` Pre `len(blocks) > 0` (unreachable from `Split`; covered by `TestSplitOversizedNoBlocks`); `reindex` Pre embedder Model/Dim equal embed_meta (TESTING §3.5).

**1.3 Config package + `kb config`**
- Carried from W4 pass: `cli.DBPath(root)` and `EnsureDBDir(root)` (root.go) still compute `<root>/.kb/kb.sqlite` beside `workspace.Workspace.DBPath`/`KBDir`; search.go, doctor.go and setup.go use the cli helpers. When setup.go is rewritten, pass the `*workspace.Workspace` (not a root string) to `openForWrite`/`openForRead` and delete the duplicates. `ws.ConfigPath` is where `Load` reads from.
- Carried from W3 pass: (a) `reindex.preconditions` asserts that embed_meta read back from the database equals the embedder (TESTING §3.5, the documented exception to "no assertions on DB contents"). Another kb process with a different `--embed-model` can change embed_meta between `EnsureEmbedMeta` and that read, which would panic (exit 3) instead of a Conflict. Once the embedder comes from config (one model per store), the race needs two configs on one store; decide then whether the check stays an assertion or becomes a Conflict error. (b) `KB_CALLER` and `$EDITOR` are read with os.Getenv in cli; config/deps should carry them.
- Carried from the adversarial harness (W2): a store indexed with `--embedder none` makes a plain `kb search` exit 1 unless the flag is passed every time. The config must record the embedder so commands pick it up (DEFAULTS #66).
- Status: done (W5). Wired keys: embed.model, embed.batch, search.k, log.open_window (default 30m), agent.caller, edit.editor. Every other key is reserved and range-checked. embed.dim≠0, embed.int8 and [[sources]] are rejected until wired. KB_CALLER is gone. `kb config [--path|--json|--set]` exists. Named-stores prep done in 1.3: every path comes from `*workspace.Workspace` (`cli.DBPath`/`EnsureDBDir` deleted), `[stores]`/`MergeStores` and the global-config read are removed, there is no embed_meta fallback (a missing config means the defaults), and Config has no location keys. The W5 pass reads config `version` before the strict decode (1.8b checks it).
- Model: Sonnet (new self-contained package with clear tests); review: Sonnet via error-handling-pass
- Files: new `internal/config/{config.go,defaults.go,override.go}`. Edit `cli/setup.go` (embedder from config; the `--embedder`/`--embed-model` flags are deleted) and add `cli/config_cmd.go`.
- Steps:
  1. Define the struct for §2.3 with defaults. `Load(path) (Config, error)` rejects undecoded keys.
  2. `Apply(cfg, "search.rrf_k=30")` handles `--set` (used by eval).
  3. `EffectiveJSON()` gives a canonical, sorted JSON of `[search]`+`[embed]`, plus a sha256 as `settings_hash`.
  4. `kb config` prints the effective TOML; `kb config --path` prints the file location.
  5. ~~The global store's `[stores]` is merged under the project's.~~ Dropped in 1.3 (D9): aliases live in `kb.toml [aliases]` (1.7b, 3.1).
- Tests: defaults round-trip; unknown key error; `--set` on nested map keys (`search.bm25.title=4`); bad types.
- Accept: `kb config | grep rrf_k` shows `rrf_k = 60`.
- Deps: 1.1.
- Risk: none.

**1.4 `kb init [--name]` and `kb model pull`**
- Changed by D8/D9 (`docs/design/named-stores.md`): `kb init` creates a *location* and a *named store*. There is no legacy adoption.
- Carried from the W4 pass decision (DEFAULTS #121): creating a **location** (`.kb/kb.toml`) at `$HOME` or `/` is refused unless `--force`, so a stray ancestor location can't capture discovery (H28). Add a harness row for it. After 1.7b, only `kb init` creates locations or stores, which settles W4 note (b): `add`/`reindex` on a `--root` with no location refuse with "run kb init".
- Carried from W4 pass: (a) `workspace.Workspace.ModelsDir` (`os.UserCacheDir()/kb/models`) is computed but unused; `local.DefaultCacheDir` derives the same path on its own. `kb model pull|path` should take it from the workspace so there is one rule. (c) `kb init` is the only command that writes `kb.toml` or a store `config.toml`; the harness `workspace-discovery` scenario gains init rows.
- Carried from W2 pass: `reindex.All` reads a missing `data/` as an empty corpus via a local `discover` wrapper (Q23). Step 5 (Discover tolerates a missing source dir) should absorb it: move the rule into `entry.Discover` and delete the wrapper. Keep the doctor warning.
- Model: Sonnet (commands from spec); review: Opus via testing-standards-pass
- Status: done (W6)
- Files: new `cli/init.go` and `cli/model.go`; `cli/doctor.go`; `entry/entry.go` (Discover tolerance).
- Steps:
  1. `kb init [--global] [--name NAME] [--model ID|none] [--source GLOB]... [--force]`:
     - if no location exists at the target (cwd, `--root` or `--global`), create `.kb/kb.toml` (`version = 1`, `default_store` = this store's name) and `.kb/.gitignore` (`stores/*/kb.sqlite*`, `stores/*/eval/`, `*.bak-v*`, `kb.log`, `migrate.lock`);
     - create `.kb/stores/<NAME>/config.toml` (NAME defaults to `default`; `workspace.ValidName`). It holds commented defaults and **explicit** `[[sources]]`: `--source` globs, else an entries source at `data` (default store) or `data-<NAME>` (named store);
     - create the entries directory when an entries source is configured.
  2. Then it opens the DB (fresh create at the target schema), runs `EnsureEmbedMeta` and pulls the model, with progress on stderr.
  3. Idempotent: an existing store is "already initialised" (exit 0, nothing written). A second `kb init --name adr` in an existing location adds the store and leaves `default_store` alone.
  4. `kb model pull|path|ls` covers model management.
  5. Make `entry.Discover` tolerate a missing source dir (empty list plus a doctor warning). This fixes `kb list` on an empty dir.
  6. Doctor gains checks: `config` (parses), `model` (cached + loads + embeds "ping", with timing), `embed_meta` vs config, `sources` (none configured → warning). "Database missing" becomes "not indexed: run kb reindex"; no location becomes "not initialised: run kb init".
- Tests:
  - decision table for init: {no location, location without this store, store exists} × {`--name` given, not} × {target is `$HOME`/`/`, other} × {`--force`};
  - init on an empty dir, then `kb list` → "no entries", exit 0;
  - init twice;
  - `kb init --name adr` in an initialised location keeps `default_store`;
  - an invalid name (`../x`, `A`, `global`, 33 chars, empty) gives Usage;
  - `--global` writes under a temp `XDG_DATA_HOME`.
- Accept: `mkdir /tmp/p && cd /tmp/p && kb init --model none && kb init --name adr --model none && kb stores && kb list && kb --store adr list && kb doctor` all exit 0.
- Deps: 1.3, 1.7b.
- Risk: none.

**1.5 In-place sources with optional front matter**
- Carried from W6 pass: `entry.Discover` already reads a missing `data/` as an empty corpus (1.4 step 5; the reindex wrapper is gone); keep that for a missing source dir. Named stores share `data/` until sources land (Q53), and doctor's `sources` check must become a warning for none (Q55).
- Carried from W3 pass: Q31 (entry returns plain fmt errors; every cli call site wraps them as Usage). When `Discover`/`Resolve`/`Load` are rewritten here, return `*errs.Error` at the entry boundary (NotFound for "no such entry", Usage with the path for front matter) and drop the `usageErr("%s", err)` wrappers in add/edit/rm/show/reindex.
- Carried from W2 pass: `reindex.All` prunes only rows that neither the first Discover nor a second Discover run after `ListEntries` found (Q22; concurrent `kb add` writes the file before committing the row). When `Discover(root, []config.Source)` replaces it, keep that two-discovery rule and `TestAllKeepsEntryAddedDuringRun`. Front-matter title/summary now have caps and a control-character rule (`entry.MaxTitleRunes` 200, `MaxSummaryRunes` 1000, Q20); `fallbackMeta` must produce values that pass `checkText` (truncate the fallback summary to 200 runes already does; also strip control characters from a humanised filename).
- Carried from the adversarial harness (W2): a symlink under `data/` that points outside the root is followed and indexed today. Discovery must skip such links with a warning, and doctor must report them (DEFAULTS #65). Add the harness scenario to the acceptance checks.
- Changed by D9: source paths and globs are relative to the **location root**, not the store directory. Sources are always explicit (`kb init` writes them), and a store config with no `[[sources]]` indexes nothing (doctor warns). Overlap inside one store is a config error (step 5); overlap **across stores is allowed** (same corpus under another model, or a subset). `kb add --store REF` copies into that store's single `entries` source; none or several is Usage. The `files` layout also always skips `.kb/**` (already in step 2), which covers every store directory.
- Model: Opus (touches entry/chunk/reindex/cli/index; goldens must hold); review: Sonnet via testing-standards-pass
- Status: done (W7). `entry.Discover`/`Resolve` now take `[]config.Source`; hand-written test configs must include the data source.
- Files: `internal/entry/entry.go` (`Discover(root, []config.Source)`, `Resolve`, new `fallbackMeta`), `internal/chunk/chunk.go:Split` (empty-summary case), `reindex.All`, `cli/add.go` and `cli/rm.go` (refuse non-`entries` sources), `index` (entries source only).
- Steps:
  1. The `entries` layout keeps today's code verbatim (`excludedRootFiles`, `excludedDirs`, `dirFiles`).
  2. The `files` layout runs `doublestar.Glob(os.DirFS(root), glob)`, minus `exclude`, and always skips `.kb/**`, `.git/**` and `node_modules/**`. Each file becomes `Entry{Path: rootRelative, Kind: KindFile}`.
  3. Front matter is optional:
     - title = front matter title, else the first `# ` line (outside fences), else the filename humanised;
     - summary = front matter summary, else the first non-heading paragraph collapsed to one line and truncated to 200 runes, else "".
  4. `chunk.Split` accepts an empty summary for `files` sources: chunk 0 becomes the `title` alone.
  5. Two sources matching the same path is a config error at load time.
  6. `Resolve` tries the root-relative path first, then the `data/`-prefixed path.
  7. `kb rm` on a `files` entry → "not owned by kb; delete the file and run kb reindex".
- Tests:
  - fallback precedence table;
  - glob + exclude;
  - files without front matter get indexed;
  - entries layout golden tests (`chunk/testdata/golden`) unchanged;
  - two stores in one location with overlapping globs both index the shared file;
  - removing a file → `reindex.All` removes the entry.
- Accept: in this repo, `kb init --model none --source 'README.md'` → `kb reindex` → `kb search install` returns `README.md`; `kb init --name docs --model none --source 'docs/*.md'` → `kb --store docs reindex` → `kb --store docs search plan` returns `docs/PLAN.md`.
- Deps: 1.3, 1.4, 1.7b.
- Risk: H1 handling. Most docs use `#` title + `##` sections, which works. Docs with only `#` sections become one large intro chunk split at 1500 chars. That is acceptable; note it in the README.

**1.6 Install story: go install, mac CI, multi-platform release builds ∥**
- Model: Sonnet (install.sh logic; ci.yml alone could be Haiku)
- Status: done (W1)
- Files: new `.github/workflows/ci.yml` and `.github/workflows/release.yml`. Rewrite `scripts/install.sh` and the README "Install" section.
- Steps:
  1. **CI (tests, mac only):** one job on `macos-14` (arm64) with `actions/setup-go` `go-version: '1.26.x'`, running `go vet -tags fts5 ./...` and `go test -tags fts5 ./...`. Model tests (`-tags model`) run only in the nightly job, after `kb model pull`. Cache the Go build cache, because the first cgo sqlite build takes ~2 min. There is no Linux test job.
  2. **Release (build only), on tag `v*`, as a matrix:**

     | target | runner | build env |
     |---|---|---|
     | darwin/arm64 | `macos-14` | native |
     | darwin/amd64 | `macos-14` | `GOARCH=amd64 CC="clang -arch x86_64"` |
     | linux/amd64 | `ubuntu-latest` | native (`gcc` preinstalled) |
     | linux/arm64 | `ubuntu-24.04-arm` | native |

     - Every job runs `CGO_ENABLED=1 go build -tags fts5 -trimpath -ldflags "-s -w -X main.version=$TAG" -o kb ./cmd/kb`, then `./kb version` as a smoke check. darwin/amd64 is smoke-run under Rosetta: `arch -x86_64 ./kb version`.
     - Each job then tars `kb-$TAG-$GOOS-$GOARCH.tar.gz`, writes its sha256, and runs `gh release upload`.
     - Linux binaries link glibc dynamically, which is fine for current distros. Static musl is a later option (Open questions).
     - `workflow_dispatch` lets the release workflow be dry-run without a tag (artifacts only, no upload). That run is the compile confirmation for the Linux targets in 0.1.
  3. **Fallback if an arm64 Linux runner is unavailable:** build linux targets from the macOS job with `CC="zig cc -target aarch64-linux-gnu"` / `x86_64-linux-gnu` (needs `brew install zig` in the job). It's documented but not the default.
  4. **`install.sh`:** detects OS/arch with `uname`, downloads the matching release asset into `~/.local/bin/kb` (no sudo) and verifies the sha256. `--from-source` runs `go install -tags fts5 github.com/nickstrad/kb/cmd/kb@latest` (needs Go and a C compiler). It does no root stamping and no `/usr/local/bin` symlink.
  5. **README:** `go install -tags fts5 …@latest` is the primary path on macOS. The release tarballs are for other machines. macOS binaries are unsigned (`xattr -d com.apple.quarantine ~/.local/bin/kb`).
- Tests: the CI job itself.
- Accept:
  - A PR shows the macOS CI job green.
  - `gh workflow run release.yml` produces 4 artifacts, each of whose smoke step printed `kb <version>`.
  - A test tag `v0.0.0-rc1` uploads 4 tarballs plus checksums.
- Deps: 0.2 (macOS tests), 0.4.
- Risk: `ubuntu-24.04-arm` availability (the zig fallback covers it). Linux binaries are built but never tested, so a Linux-only runtime bug would only surface in user reports. The `kb version` smoke check catches only link/startup failures.

**1.7a Named stores: workspace location and store resolution (new files only)**
- Design: `docs/design/named-stores.md` (D9). 1.3 already did its prep (see 1.3 Status), so nothing from 1.3 carries over.
- Model: Sonnet (pure resolution with a clear spec); review: Opus via testing-standards-pass (path boundary)
- Status: done (W6)
- Files: new `internal/workspace/location.go` and `location_test.go`. `workspace.go` stays untouched until 1.7b.
- Steps:
  1. `ResolveLocation(rootFlag string, global bool, env Env) (*Location, error)`: `--root` > `--global` > walk up for `.kb/kb.toml` (a regular file). `Location{Root, KBDir, FilePath}`. Errors as in 1.1.
  2. `ValidName(name) error`: `^[a-z0-9][a-z0-9_-]{0,31}$`; `global` is reserved.
  3. `(*Location).Store(name) (*Workspace, error)`: `Workspace` gains `Location *Location`, `Store string`, `StoreDir` (`<KBDir>/stores/<name>`). `DBPath` and `ConfigPath` sit inside `StoreDir`. A missing store gives NotFound naming the existing stores.
  4. `(*Location).Stores() ([]string, error)`: the names whose `config.toml` is a regular file, sorted. Directories with an invalid name are skipped and reported in a second return value (doctor shows them).
- Tests:
  - `TestResolveLocation`, one row per branch outcome (as `TestResolve`);
  - a `ValidName` boundary table: 0, 1, 32 and 33 chars; leading `-`/`_`; uppercase; `/`, `@`, `:`, `..`; `global`;
  - `FuzzValidName` with the property "valid ⇒ `StoreDir` is a direct child of `<KBDir>/stores`";
  - `Stores()` with a file named like a store, a dir without config, a symlinked config, an invalid name.
- Accept: `go test -tags fts5 ./internal/workspace/` green; mutation run on `location.go` with no non-equivalent survivors.
- Deps: 1.1.
- Risk: none (no caller yet).

**1.7b Named stores: CLI on location + store, `--store`, `kb stores`**
- Carried from W5 pass: `Config.Search.Stores` and `CheckModel(src, …)` take a source label. If the location file adds a second config layer, pass the right source (Q50).
- Model: Sonnet (mechanical cli switch plus one new command); review: Opus via error-handling-pass
- Status: done (W6)
- Files: `internal/workspace/workspace.go` (delete `Resolve`, `discover`, `isStore` and the legacy marker; keep `Env`/`OSEnv`); new `internal/workspace/refs.go` (local NAME | `global` only; 3.1 adds aliases and `NAME@LOC`); new `internal/config/location.go` (`Location{Version, DefaultStore, Aliases}`, `ReadLocation`, `Validate`; aliases are parsed and validated but resolved in 3.1); `cli/root.go` (`--store` persistent flag, `deps.workspace` = location, then store), `cli/setup.go` (`openStoreForWrite` no longer creates `.kb/`; a missing store is NotFound "run kb init"), new `cli/stores.go`; `cli/report.go` (the incident log goes to `<KBDir>`). The `AddCommand` line is added by the orchestrator.
- Steps:
  1. Resolution: location (1.7a), then store = `--store` (a local NAME or `global`) or `kb.toml default_store`. A store missing from the location is NotFound; so is a `default_store` that names no store (the error names `kb.toml`).
  2. Remove the interim creation (DEFAULTS #111): `add`/`reindex` never create a location or store.
  3. `kb stores [--json]`: name, `*` for the default, model, entry count, DB size or "not indexed".
  4. The `--debug` store line (DEFAULTS #118) prints `location <root> store <name> (--root | --global | discovered from <cwd>)`.
  5. Update cli test helpers (`newTempRoot` writes `kb.toml` + `stores/default/config.toml`) and the harness `--root` setup.
- Tests:
  - decision table for resolution: {`--root`, `--global`, discovered} × {`--store` absent, a valid existing name, a missing name, an invalid name, `global`} × {`default_store` valid, missing};
  - `kb stores` golden output, text and JSON;
  - add/reindex on a bare `--root` refuse;
  - harness `stores-layout` scenario (init default + named, list, unknown store, missing default_store).
- Accept: every existing cli test is green on the new layout; `kb --store nope list` exits 1 with the stores listed; `grep -rn 'kb.sqlite\|config.toml' internal/cli` finds only help text.
- Deps: 1.7a, 1.3.
- Risk: it touches every cli test helper. This is mechanical. Coordinate with 2.1, which owns `cli/search.go` in W6: 1.7b does not edit it beyond what compiles.

**1.8a Migration framework: `internal/migrate` and the existing artefacts (no behaviour change)**
- Design: `docs/design/migrations.md` (D10).
- Model: Opus (cross-package interface, concurrency and atomicity rules); review: Opus via testing-standards-pass + error-handling-pass
- Status: done (W6). CODING_STYLE §6.1 and TESTING §6.1 hold the authoring rules.
- Files: new `internal/migrate/{doc.go,migrate.go,run.go}` + tests; `internal/store/sqlite/migrations.go` (steps become `migrate.Steps[execer]` with IDs `index/0001-baseline`, `index/0002-log-provenance`, `index/0003-vec-width` (Effect reembed)); `internal/store/sqlite/sqlite.go` (`Status`, `Backup` via `VACUUM INTO`, `Verify`: `integrity_check` + `foreign_key_check` + the schema compared with a fresh database at the same version); new `internal/store/sqlite/artefact.go`; `internal/embed/cache/cache.go` (its schema becomes `cache/0001-baseline`) + new `cache/artefact.go`; new `internal/store/sqlite/testdata/mkfixture` and per-version SQL fixtures.
- Steps:
  1. `migrate.Info{From, ID, Desc, Effect}`, `Step[T]`, `Steps[T]` (`Target`, `Pending`, `Check`), `Status`, the `Artefact{Status, Backup, Apply}` interface, the `Verifier` interface, and `Run(ctx, arts, Options{DryRun, NoBackup, LockWait, Now})` with an advisory `flock` on a sibling `migrate.lock`, backup naming `<file>.bak-v<N>-<YYYYMMDD-HHMMSS>` and pruning to the newest 3.
  2. Move the sqlite index steps and the cache schema onto `Steps`. `Open` behaviour is unchanged in this item (it still migrates automatically).
  3. A replay test per artefact: stepwise from 0 equals creation at the target (normalised `sqlite_schema`), and every fixture upgraded to the target passes `Verify`.
  4. Write the authoring rules (design §2.5) as the text for CODING_STYLE §6 ("Migrations") and TESTING ("Migration tests: fixture per step, replay, effect table"). The orchestrator applies them to those docs.
- Tests:
  - `Steps.Check` table (gap, duplicate ID, bad ID shape, From ≠ index);
  - `Run` decision table: {nothing pending, pending, newer} × {DryRun, NoBackup} × {lock free, held then released, held past LockWait} × {backup fails, apply fails, verify fails}. Each row asserts the files on disk (backup present or absent, version unchanged on failure);
  - a cancel during apply (fault driver) leaves the starting version and the backup;
  - backup pruning keeps exactly 3;
  - `-race` with two `Run` on one file: one applies, the other is a no-op.
- Accept: `go test -tags fts5 ./internal/migrate/ ./internal/store/sqlite/ ./internal/embed/cache/` green; the existing migrate3 and readonly tests unchanged and green; coverage of `internal/migrate` ≥ 90%.
- Deps: 1.2, 3.3a.
- Risk: the flock is advisory and POSIX-only. That is fine (macOS-only development; Windows is a non-goal).

**1.8b `kb migrate` and explicit migration for durable artefacts**
- Carried from W6 pass: (a) `kb doctor` opens the index read-write, so today it migrates an older database silently; with auto-migration off it must report the pending steps (`sqlite.NewArtefact(ws.DBPath).Status`) as its own check instead of a driver FAIL. (b) `kb stores` opens each DB read-only and shows the `checkVersion` text in the row ("run kb doctor once to upgrade it" today): when that text becomes "run kb migrate up", update `TestStoresAndDoctorOnOtherSchemaVersions`. (c) `kb init` already creates the DB at the target with nothing pending (`TestInitIndexIsAtMigrateTarget`); switch it to `Options.Create` and keep that test. (d) New `.kb/.gitignore` files ignore `migrate.lock`; the location artefact's lock is `.kb/migrate.lock`. (e) Q56 (lock-dir error and Status-error-with-pending rows).
- Carried from 1.8a: build `sqlite.NewArtefact(<store DB path>)` and `cache.NewArtefact(<cache path>)` in `cli/migrate_cmd.go` and call `migrate.Run`/`Status`. Change the read-only refusal text in `sqlite.checkVersion` ("run kb doctor once to upgrade it") to "run kb migrate up". Turn off auto-migration on open for the index DB; the cache keeps auto-migrating. Q52: delete `TestMigration2KeepsSearchLog` (duplicates `TestStepEffects/index/0002`).
- Carried from W5 pass: `config.Parse` already emits the older-version Conflict "run kb migrate up" through `checkVersion`. Add the config migration step, plus a test at current+1 once `config.SchemaVersion` is 2.
- Model: Sonnet (command and wiring from spec); review: Opus via testing-standards-pass
- Status: done (W7)
- Files: `internal/store/sqlite/sqlite.go` (`Options.Create`: only a version-0 file is migrated on open; an older one is Conflict "store … is at schema vN; this kb needs vM. Run: kb migrate up"); new `internal/config/migrate.go` (config and location artefacts: `Steps[map[string]any]`, temp-file + rename, `config.Save`); new `cli/migrate_cmd.go`; `cli/init.go` (`Options.Create`). The `AddCommand` line is added by the orchestrator. It does not edit `cli/setup.go` (3.3b owns it in W7). The refusal comes from `sqlite.Open`, and `report` already maps Conflict to exit 1.
- Steps:
  1. `kb migrate status [--store REF | --all] [--json]` exits 1 when anything is pending or newer. `kb migrate up [...] [--dry-run] [--no-backup]` ends with the combined effects ("needs: kb reindex"). `kb migrate verify`.
  2. Scope: by default the resolved store plus the location file; `--all` adds every store of the location and the global location. The embedding cache is shown but never needs `up`; it keeps migrating on open.
  3. Exempt from the refusal: `kb migrate`, `kb doctor` (which reports the pending steps), `kb stores`, `kb config --path`, `kb version`.
  4. Check that the W5 pass's config `version`-first ordering is there (Parse reads `version` before the strict decode). If it is missing, add it.
  5. README: an upgrade section with the one-liner for many projects.
- Tests:
  - decision table of commands × store state {current, older, newer, missing}, asserting exit code and message;
  - golden `kb migrate status` output;
  - `up --dry-run` writes nothing;
  - harness scenarios `migrate-refusal` (v3 fixture: `kb search` exit 1 with the hint; `kb migrate up`; search works; the backup exists), `migrate-concurrent` (two `up` processes) and `migrate-interrupt` (SIGINT mid-apply, then a rerun).
- Accept: on a v3 fixture store, `kb search x` exits 1 with "run kb migrate up", `kb migrate up` prints the backup path and "needs: kb reindex" where an Effect asks for it, and `kb search x` then exits 0.
- Deps: 1.8a, 1.7b.
- Risk: users forget to migrate after an upgrade. The message names the exact command, and doctor shows it too.

### Phase 2: Measurement, then config-driven weights

**2.1 Search trace plumbing**
- Carried from W5 pass: `settings_hash` (`config.EffectiveJSON`) covers `[embed]` and `[search]` only. If a new key changes results, add it there with a hash-changes row in `TestEffectiveJSON`. Wiring a reserved key: delete its rejection in `Validate`, add a boundary row, update the wired/reserved list in `doc.go`; `TestDefaultsGolden` forces a reviewed golden change.
- Model: Opus (fusion params/trace; defaults must stay bit-identical); review: Sonnet via testing-standards-pass
- Status: done (W6)
- Files: `internal/search/search.go`, `internal/store/sqlite/retrieve.go`.
- Steps:
  1. `Searcher` reads all limits/weights from `config.Search` instead of the constants (`ListLimit`, `CandidateLimit`, `PerEntryCap`, `RRFConstant`, `vecOverFetch`, and the hard-coded `bm25(chunks_fts, 1.0, 2.0, 3.0)` in `ftsList`). The defaults stay identical.
  2. `Outcome` gains `FTSMatch string`, `FTSFallbackToOr bool`, and per-candidate `CapDropped bool`. `applyCap` records which candidates were skipped.
  3. `Fuse` takes `rrfK` and list weights.
- Tests: the existing `Fuse`/`applyCap` tests with explicit params; a cap-drop flag test; AND→OR fallback reported.
- Accept: all existing search tests pass unchanged with the default config.
- Deps: 1.2, 1.3.
- Risk: none.

**2.2 `kb explain "<query>"`**
- Carried from W6 pass: `FTS` with an empty non-nil `ChunkIDs` runs nothing but returns the MATCH expression (a storetest contract). Call it directly for the single-column re-scores; do not route it through `Searcher.ftsList`, whose OR retry fires on "0 rows and a non-empty Match".
- Model: Sonnet (new command over existing trace); review: Sonnet via testing-standards-pass
- Status: done (W7)
- Files: new `cli/explain.go` and `search/explain.go`.
- Steps:
  1. Run the search without logging.
  2. For each candidate (top `candidate_limit`), print:
     - fused rank, returned/cap-dropped;
     - FTS rank + bm25;
     - **single-column bm25** from 3 extra FTS calls with weights (w,0,0), (0,w,0), (0,0,w), restricted to the candidate ids (`rowid IN (...)`);
     - vec rank + cosine distance;
     - each RRF term `w/(k+rank)`.
  3. Header line: the MATCH expression used and whether the OR retry fired.
  4. Document that FTS5 bm25 sums the column-weighted term frequencies *before* saturation, so the single-column scores are diagnostic and do not add up to the total.
  5. Flags: `--json`, `--mode`, `--set`.
  6. Note that the chunk text repeats the title in its context line (`chunk.Split`: `title › heading`), so title terms hit both the text and title columns.
- Tests: golden output on the fake embedder + fixture corpus; JSON schema.
- Accept: `kb explain "tmux logout"` lists the expected hit with nonzero title-column score and `match: "tmux" AND "logout"`.
- Deps: 2.1.
- Risk: none.

**2.3 `kb eval` core**
- Carried from testability (#3, #4): `parseSince(s, now)` with an injected clock; give eval a real read API for logged searches instead of raw `.DB()` SQL in tests.
- Model: Opus (eval methodology: gold extraction, metrics, CI); review: Opus via testing-standards-pass
- Files: new `internal/eval/{gold.go,replay.go,metrics.go}` and `cli/eval.go`. Add `SearchLog.Judged` in sqlite.
- Steps:
  1. Gold per logged search:
     - useful verdicts become chunk-level gold `(entry_path, heading)`, joined `search_feedback`→`search_results` on (search_id, rank). Never key on `chunk_id`.
     - opens become entry-level gold `(entry_path, *)`.
     - not-useful verdicts are recorded only as negatives for the report.
  2. Searches with no gold (unjudged, or `--none` only) are excluded; report their count.
  3. Group by normalised query (`lower`, collapse whitespace), taking the union of gold, so each query counts once.
  4. Replay each query through `Searcher.Search` (no log) with the current config.
  5. Metrics at k (default `search.k`):
     - hit@k;
     - MRR;
     - nDCG@k with binary gains, where the ideal DCG uses `min(|gold|, k)` and entry-level gold matches any heading of that path;
     - report chunk-level and entry-level variants.
  6. Print n, and a 95% bootstrap CI (1000 resamples, fixed seed) on MRR. If n < 30, print the caveat "small sample: differences under ±CI are noise".
  7. Flags: `--mode`, `--k`, `--since`, `--caller`, `--json`, `--per-query`.
- Tests: metric unit tests (hand-computed); gold extraction from a fixture log including `--none`, re-marked verdicts, opens, and a reindex-reused chunk_id case.
- Accept: on the `~/knowledge` log, `kb eval` prints `queries=N hit@8=… mrr=… ndcg@8=…` with a CI.
- Deps: 2.1, 0.7.
- Risk: gold drift when headings are renamed. Report "gold not in index" as its own count.

**2.4 Baselines and variants**
- Model: Sonnet (baseline I/O plus sign test); review: Opus via testing-standards-pass (stats check)
- Files: `internal/eval/baseline.go`, `cli/eval.go`.
- Steps:
  1. `--save-baseline [name]` writes `.kb/eval/<name>.json` (settings JSON, embed model, entry count, kb version, per-query first-gold rank, metrics).
  2. `--compare [name]` prints metric deltas, per-query wins/losses/ties, a sign test p-value, and warns when the embed model or entry count differs.
  3. `--set key=value` (repeatable) evaluates a variant without touching the config.
  4. `--mode fts|vec|hybrid` compares modes.
- Tests: round-trip; compare against itself → all ties.
- Accept: `kb eval --save-baseline && kb eval --set search.rrf_k=20 --compare` prints the deltas table.
- Deps: 2.3.
- Risk: none.

**2.5 Log effective settings**
- Carried from W6 pass: `Outcome.Settings` already carries the per-store `JSON`/`Hash` (each named store's own `[search]`, `TestSearchSettingsArePerStore`); log those, not `config.Default()`'s.
- Model: Sonnet (additive migration, hash plumbing); review: Sonnet via testing-standards-pass
- Status: done (W7)
- Files: migration 3→4, `sqlite/log.go`, `search` (SearchRecord gets Settings, SettingsHash, FTSMatch), `stats/queries.go` (a `settings` query grouping hit-rate/MRR by `settings_hash`).
- Tests: the logged row has a hash equal to `config.EffectiveJSON` hash.
- Accept: `kb stats settings` shows one row per config ever used.
- Deps: 1.3, 2.1.
- Risk: none.

**2.6 `kb eval --sweep`**
- Model: Opus (holdout split, overfitting guards, vector reuse); review: Sonnet via testing-standards-pass
- Files: `internal/eval/sweep.go`.
- Steps:
  1. `--sweep 'search.rrf_k=20,40,60' --sweep 'search.bm25.title=1,3,5'` builds a cartesian grid (cap 200 combos).
  2. Refuse when queries with gold < `eval.min_judged` (default 30), unless `--force`.
  3. Deterministic split by query hash: choose on 70%, report on the 30% `eval.holdout`.
  4. Print the top 5 with holdout MRR ± CI and the delta vs current, plus a ready-to-paste TOML snippet. Never write config.
  5. Embed each query once and reuse the vector across combos (only the fusion and FTS params vary).
- Tests: a tiny grid on a fixture; the gate message.
- Accept: `kb eval --sweep search.rrf_k=10,60` with fewer than 30 judged queries exits 1 with "need ≥30 judged queries (have N); use --force".
- Deps: 2.4.
- Risk: overfitting on small logs. The holdout and the gate are the mitigation.

### Phase 3: Cross-store search

**3.1 Store registry and read-only open**
- Carried from W6 pass: `workspace.ParseRef` refuses a ref containing `@` or `:` with "NAME@LOCATION refs are not supported yet" (`TestParseRef` rows); 3.1 replaces that branch with the real grammar and must update those rows.
- Model: Sonnet (resolution order and warnings, clear spec); review: Sonnet via error-handling-pass
- Status: done (W7). 3.2 fans out with `deps.openStoreReadOnly(cmd, ref)` (internal/cli/refs.go) and adds `--stores all`.
- Changed by D9: refs instead of names; aliases live in `kb.toml [aliases]`.
- Files: `internal/workspace/refs.go` (extends 1.7b's), `sqlite.Open(..ReadOnly)` (from 1.2).
- Steps:
  1. Resolve refs: `NAME` (a store of the current location) → alias (`[aliases]` of the current location, then of the global location) → `global` (the global location's default store) → `NAME@LOC` (LOC is `global` or a path) → PATH (a location's default store). Warn when a local name shadows an alias. `--stores all` means every store of the current location. `--store` accepts the same grammar (an alias or `NAME@LOC` together with `--root`/`--global` is Usage). Each resolved store gets a canonical ref `NAME@ABSROOT`. `kb stores` gains the alias rows.
  2. Every other store opens read-only. A schema-version mismatch or a missing DB is a warning and that store is skipped.
  3. Read each store's embed_meta.
- Tests:
  - decision table for ref resolution (each grammar branch, plus shadowing, an unknown ref, a `NAME@LOC` whose location or store is missing, `~` expansion, a relative PATH);
  - `FuzzParseRef` (never panics; a parsed ref re-renders to the same canonical form);
  - a missing store warns; a newer schema warns; an older schema warns "run kb migrate up there" (D10);
  - harness `stores-alias`.
- Accept: `kb search q --stores global,notes` with a nonexistent `notes` prints a warning and still returns primary hits; `kb search q --stores adr` finds the `adr` store of the same location.
- Deps: 1.7b, 1.3.
- Risk: none.

**3.2 Merge and log**
- Carried from testability (#1, #2, #3): (a) extract a shared `d.openSearcher` + pure `resolveMode` used by search and explain (Q63); `renderHits` takes a one-method interface, not `store.Store`. (b) Replace the sqlite package vars `migrations`/`schemaVersion` reassigned by tests with a `steps` field on `Options`/`Artefact` (as `config.tomlArtefact` does), then allow `t.Parallel`. (c) Set `SearchRecord.TS` from an injected clock (`Searcher.Now`) instead of `sqlite.nowUTC`.
- Carried from 4.1: `render.Hit` has no store field, so cross-store hits need a store tag in the header (`[r] store:path › heading`). Add the field and update the goldens.
- Carried from W7 pass: (a) Q63 part (a) is done: `deps.openSearch`/`searchEmbedder` in cli/search.go are shared by search and explain (explain opens read-only); `renderHits` still takes `store.Store`. (b) `--store` resolution now has three copies: `resolveRef`, `resolveRefIn` (fan-out) and `migrateRefStore` (kb migrate, which reads kb.toml leniently through `rawLocationInfo`), plus `isDefaultStore` (index.go) reading kb.toml a third way. Fold them into one resolver that takes the `Read` func, before 3.2 adds fan-out refs; `TestMigrateScopeMatchesStoreRefs` and `TestStoreRefDecisions` pin the behaviour. (c) Render looks up one entry row per hit path (`renderHits`, memoised); with fan-out, batch it per store (`EntriesByPath`). (d) Harness H39: fan-out over an older store, one with no database and one ok. (e) The settings/settings_hash/fts_query round-trip is tested only in sqlite (searchlog_test.go); add it to storetest once the log has a read method. (f) `index.md` describes the location's default store only (`kb index` refuses others, doctor skips the check); if per-store snapshots are wanted, write them per store rather than lifting the refusal.
- Model: Opus (cross-store fusion, per-store models, migration, logging); review: Opus via testing-standards-pass
- Files: `search/multi.go`, migration 4→5, `sqlite/log.go`, `cli/search.go` (`--stores a,b`; default `search.stores`; `--stores ''` disables).
- Steps:
  1. Run the normal per-store search, using the *primary's* search settings and each store's own embed model. Embed the query once per distinct (model, prefix). At most 8 extra stores (Usage above that); warn when more than 2 distinct models load.
  2. Merge with RRF over the per-store returned ranks (`1/(rrf_k + rank)`); scores are never compared across stores, so any backend or paradigm that returns an ordered hit list can join. Hit identity is `(location root, path, ord)`: stores in the **same location** that index the same file add their RRF terms (an ensemble), and the hit is tagged with the best-ranked store. Stores in different locations never merge. Tie-break: primary first, then store order.
  3. Apply the per-entry cap on `(location root, path)`, then k.
  4. Hits carry `Store string` (empty = primary), shown as `[notes] data/x.md › H`.
  5. Log only in the primary, with the `store` column set to the canonical ref `NAME@ABSROOT`.
  6. `kb show REF:data/x.md` (REF is a name, an alias or `global`) resolves a store-qualified path. The open is recorded in the primary log.
- Tests: two temp stores with the fake embedder; merged order; logging columns; the same path in two *locations* is not capped together; the same path in two stores of *one* location sums its RRF terms; a table over {same location, different location} × {same model, different model, `none`}; harness `stores-fanout`.
- Accept: `kb search "x" --stores global` shows tagged global hits, and `select store from search_results` has them.
- Deps: 3.1, 2.1.
- Risk: different models across stores. This is handled by per-store query embedding, and the shared cache keeps it cheap.

**3.3 Shared embedding cache ∥**
- Carried from W6 pass (3.3b): the cache's damage and newer-schema messages (`cache.Artefact`, docs/design/migrations.md) tell the user to run `kb model cache --clear`; that command must exist when the cache is wired.
- Model: Opus (concurrent writers, WAL, degrade-on-error decorator); review: Opus via testing-standards-pass + error-handling-pass
- Status: 3.3a done (W2), 3.3b done (W7): `internal/embed/cache` plus `embed.Cached`, not wired. 3.3b wiring notes from 3.3a:
  - Open `cache.DefaultPath()` (or a config override) in setup. On any `Open` error, including `ErrNewerSchema`, warn and use the plain embedder.
  - Wrap with `embed.Cached(e, c)` (not `None`) and close the cache at exit.
  - After reindex, add and search, warn if `ce.Err() != nil`. `Stats().ModelCalls` is the acceptance number ("embed calls: 0").
  - Keep `store.ExistingEmbeddings` reuse ahead of the cache. `local.Model()` must include `+int8` before int8 ships.
  - `kb model cache --stats|--clear` maps to `Cache.Stats` and `Cache.Clear`.
  - Hermetic tests pass a cache path in `t.TempDir()` through deps.
  - Carried from W2 pass: `cache.Open`/`init` error paths survive mutation (cache.go err-checks in Open and init: cache directory is a file, sql.Open failure, user_version read and schema create failures; ErrNewerSchema is already tested) → add a `TestOpenErrors` table before wiring, since 3.3b makes them user-reachable (each must degrade to "no cache", never fail the command). Harness scenario for the shared file across processes is H19.
- Files: new `internal/embed/cache/cache.go` (own mattn SQLite file `os.UserCacheDir()/kb/embed-cache.sqlite`, WAL, `busy_timeout`), `embed.Cached(e Embedder, c *cache.Cache) Embedder`.
- Steps:
  1. Table: `emb(model TEXT, hash TEXT, dim INT, vec BLOB, PRIMARY KEY(model, hash))`.
  2. The key is `sha256(exact text sent to the model, prefix included)`. The model key includes `+int8` and the dim.
  3. The decorator looks up a batch, embeds only the misses, and writes back. Cache errors degrade to no cache.
  4. Used by reindex/add/search.
  5. The per-entry reuse in `store.ExistingEmbeddings` stays (it is cheaper).
  6. `kb model cache --stats|--clear`.
- Tests: second embed of the same text has zero model calls (counting fake); concurrent writers.
- Accept: `kb reindex --all` in a second project with copied docs reports `embed calls: 0`.
- Deps: 0.5.
- Risk: cache growth is negligible (~1.5 KB/chunk). Cache paths come from `os.UserCacheDir()`, which also gives the correct location on the untested Linux builds.

### Phase 4: Agent-facing output and lexical quality

**4.1 Compact rendering + `--budget`**
- Model: Sonnet (renderer with golden tests); review: Sonnet via testing-standards-pass
- Status: done (W7)
- Files: new `internal/render/render.go`; `cli/search.go:printResults` (replaced).
- Steps:
  1. Header line: `search N · M hits` (unchanged).
  2. Per hit: `[r] path › heading · updated 2026-03-01` (store tag when cross-store); `ChunkMeta` carries `updated`.
  3. Drop the RRF score and the feedback hint from the human output (both stay in `--json`).
  4. `--budget N`: tokens are estimated as `ceil(runes/4)`, documented as approximate.
  5. Elastic greedy: start every hit at level 1 (header only), then upgrade hits in rank order to level 2 (header + entry summary) and then level 3 (full chunk text), while the total fits. The last line says `(+K hits shown as summaries; raise --budget)`.
  6. Default from `output.budget`.
- Tests: golden outputs at budgets 0/50/300; every hit is always present at level ≥1 when the headers fit.
- Accept: `kb search q --budget 200 | wc -c` is ≤ ~800 chars.
- Deps: 2.1.
- Risk: output format break; listed in Breaking changes.

**4.2 `--expand section|entry`**
- Carried from W7 pass: one entry's chunks can appear as several hits, and a hit header repeats the chunk's own context line ("› (intro)" then "Alpha › (intro)"); dedupe per entry and drop the repeated context line when expanding.
- Model: Sonnet (bounded rendering feature)
- Files: `render/expand.go`; `Index.Chunks`; `ChunkMeta` gains entry_id/ord/source_file.
- Steps:
  1. `section`: the hit chunk plus ord±1 chunks of the same `source_file`, merged in ord order, skipping chunk 0.
  2. `entry`: all chunks of the entry in ord order.
  3. Deduplicate when several hits share an entry (render once at the best rank).
  4. Expansion happens before budget allocation, so expanded text competes for budget.
- Tests: neighbours at file boundaries; dedupe.
- Accept: `kb search q --expand entry --k 1` prints the whole entry once.
- Deps: 4.1.
- Risk: none.

**4.3 `kb context "<task>"`**
- Model: Haiku (thin command composing 4.1/4.2, golden test); review: Sonnet via testing-standards-pass
- Files: `cli/context.go`.
- Steps:
  1. Run a hybrid search with k=8 and `--expand section --budget output.context_budget`, logged with `--caller`.
  2. Output Markdown: `## kb context (search N)`, the hits, and a footer `More: kb show <path>`.
  3. Accept `--stores`.
- Tests: golden output.
- Accept: `kb context "rotate tmux logs" --budget 1500` prints Markdown under the budget.
- Deps: 4.1, 4.2.
- Risk: none.

**4.4 Contextual embedding audit (small) ∥**
- Model: Sonnet (chunk rule change; golden diff must stay scoped)
- Status: done (W1)
- Files: `internal/chunk/chunk.go:Split`.
- Finding: chunk text already starts with `title › heading` (and chunk 0 is `title: summary`), so the embedding is already contextual. Do **not** add another prefix in reindex or `embed.DocText`; the only prefix there is the model's task prefix.
- Steps:
  1. For `docs/*.md` and secondary files, use `entry title › file H1 › heading` when the file has its own H1/title.
  2. Write a unit test asserting there is exactly one context line.
- Accept: the golden diff is limited to docs files. A plain `kb reindex` re-embeds only the changed chunks (text_hash).
- Deps: none.
- Risk: none.

**4.5 Identifier-aware exact list**
- Model: Opus (backfilled migration, three write paths, third rank list); review: Opus via testing-standards-pass
- Files: migration 5→6; `sqlite/entries.go` (maintain `chunks_exact` alongside `chunks_fts` in `ReplaceEntry`, `clearChunks`, `ResetIndex`); `sqlite/retrieve.go` (`FTSQuery.Exact`); `search` (third list when the query contains an identifier-like token matching `[_./:-]|[a-z][A-Z]|\d[a-z]`); logs `exact_rank/exact_score`; `explain` shows it.
- Steps:
  1. The main table keeps `porter unicode61`, so `draw-visual` phrase matching (`SanitizeFTS` doc) is unchanged.
  2. The exact table (`unicode61 tokenchars '_-.:/'`, no porter) matches `snake_case`, `pkg.Func`, `--flag` and `path/to` as single tokens.
- Tests: `kb search reindex_test` / `ExistingEmbeddings` / `-tags` finds the exact chunk at rank 1 in a fixture; the backfill migration test.
- Accept: `kb explain "EnsureEmbedMeta"` shows an exact rank of 1.
- Deps: 2.1, 2.5.
- Risk: tokenchars `-` makes `set -e` a single token `-e`. That is fine because this is a separate list.

**4.6 Phrase/NEAR tier**
- Model: Sonnet (FTS tier assembly from precise spec); review: Opus via testing-standards-pass (ranking)
- Files: `search/search.go` (FTS list assembly).
- Steps:
  1. With ≥2 groups and `phrase_boost`, run `OpPhrase` (`"a b c"`), then `OpNear` (`NEAR("a" "b", 8)`), then AND, then the OR fallback.
  2. Concatenate them deduplicated in tier order, truncated to `list_limit`. The FTS rank therefore prefers exact phrasing.
  3. Log the tier in `fts_query`. Validate with `kb eval --set search.phrase_boost=false --compare`.
- Tests: tier ordering; dedupe.
- Accept: the eval delta is reported, and phrase_boost is kept on only if it is not worse.
- Deps: 4.5 (shared assembly), 2.4.
- Risk: none.

### Phase 5: Edges, links, tags, synonyms

**5.1 Link extraction and front-matter lists ∥**
- Model: Sonnet (pure parsing with table tests); review: Sonnet via testing-standards-pass (fuzz target)
- Status: done (W4)
- Files: new `internal/links/links.go`; `internal/entry/entry.go:ParseFrontMatter` (generalise `parseTags` list parsing to `related`, `supersedes`, `depends_on`, `aliases`; `FrontMatter` gains those fields).
- Steps:
  1. Extract relative Markdown links `[t](../x.md#a)` (ignore URLs with a scheme, ignore fenced code) and resolve them relative to the source file → root-relative path.
  2. `[[wikilink]]` targets resolve later by path, basename or alias.
  3. Front-matter lists produce rel = the key; body links produce rel `links`.
- Tests: table tests for path resolution, anchors, fences, wikilinks.
- Accept: unit tests.
- Deps: 1.5.
- Risk: the front-matter parser is hand-rolled, so keep list syntax to the two forms it already supports.

**5.2 Schema: edges, entry_tags, aliases**
- Carried from W4 pass: `links.Extract` does not extract reference-style links (`[t][ref]` + `[ref]: path`) or `[[]](x.md)` (CommonMark makes the latter a link); decide whether edges need them before backfilling. `links.resolve` now asserts its target is a clean relative path (L1); FuzzExtract adds two metamorphic properties (leading blank line shifts lines; a fenced body yields nothing).
- Carried from 5.1: inline front-matter lists split on every comma, so paths or aliases containing commas need the block form. Front-matter paths resolve against the entry file's own directory (`related: [x.md]` in `data/droplet/README.md` means `data/droplet/x.md`); document this. Use `links.Dedupe` before writing edges. A body link may resolve to a directory (`data/other`); decide whether that matches a directory entry.
- Model: Opus (backfilled migration, reindex reset, tag semantics change); review: Opus via testing-standards-pass
- Files: migration 6→7; `sqlite/entries.go:ReplaceEntry` (write edges/tags/aliases; FTS `title` column = `title + " " + aliases`); `Index.ResolveEdges` (`UPDATE edges SET dst_entry_id = (SELECT id FROM entries WHERE path = dst_path OR <wikilink match>)`), called at the end of `reindex.All` and after `One`; the tag filter in `sqlite/retrieve.go` becomes `JOIN entry_tags` (replacing `tagPattern`/`LIKE`).
- Tests: tag filter is case-insensitive (behaviour change from the case-sensitive LIKE); aliases findable via FTS; migration backfills tags; edges resolved after reindex.
- Accept: `kb reindex` then `kb search q --tag Docker` behaves the same as `--tag docker`.
- Deps: 5.1.
- Risk: requires a plain `kb reindex` (body_hash reset in the migration).

**5.3 `kb related`, supersede demotion, `--neighbors`**
- Model: Sonnet (CTE, command, render section); review: Opus via testing-standards-pass (cycles, demotion)
- Files: `cli/related.go`, `Index.Related` (recursive CTE, depth ≤ 3, both directions, distinct), `search` (after fusion, multiply RRF by `supersede_penalty` for entries that are `dst` of a `supersedes` edge from a live entry; explain shows it), `render` (`--neighbors` appends a "Related:" one-liner section for the returned entries; these are not logged as results).
- Tests: cycle safety in the CTE; demotion order; neighbours dedupe against hits.
- Accept: `kb related data/foo.md` lists inbound and outbound edges with rel.
- Deps: 5.2.
- Risk: none.

**5.4 `kb stale` ∥**
- Model: Haiku (small read-only listing command); review: Sonnet via testing-standards-pass
- Files: `cli/stale.go`.
- Steps: list entries whose `coalesce(verified, updated)` is older than `stale.days` or missing, plus superseded entries and broken links (`dst_entry_id IS NULL` for path links). `--json`.
- Tests: date parsing of verbatim front-matter dates (YYYY-MM-DD; unparseable means "unknown").
- Accept: `kb stale --days 0` lists every entry.
- Deps: 5.2.
- Risk: none.

**5.5 Synonyms**
- Model: Sonnet (query expansion from clear spec)
- Files: `search/synonyms.go`, `cli/search.go`.
- Steps:
  1. `synonyms.txt` holds one comma-separated group per line, with `#` comments. Path: `search.synonyms`, else `.kb/synonyms.txt`, else the global store's.
  2. At query time, each field whose lowercase form is in a group becomes a `Groups[i]` of alternatives. The sqlite backend renders `("k8s" OR "kubernetes")` inside the AND/OR/phrase queries. The vector query is unchanged.
  3. The logged `fts_query` shows the expansion.
- Tests: expansion, multi-word synonyms rendered as phrases, no file means no-op.
- Accept: with `k8s, kubernetes` in the file, `kb explain k8s` shows the expanded match.
- Deps: 1.2 (Groups), 2.5.
- Risk: none.

**5.6 `kb synonyms suggest` + spell correction**
- Model: Opus (spell heuristics and log mining, threshold logic); review: Sonnet via testing-standards-pass
- Files: `cli/synonyms.go`, `search/spell.go`, an fts5vocab table in migration 6→7 (`CREATE VIRTUAL TABLE chunks_exact_vocab USING fts5vocab(chunks_exact, 'row')`).
- Steps:
  1. Suggest: pairs of searches by the same caller within 5 min where the first had no useful/open and the second did. Diff their token sets and count the pairs. Also list `gaps` query terms absent from the vocab. Print paste-ready lines; never write.
  2. Spell: when `search.spell` is on and the FTS rows are < 3, each term with doc-frequency 0 is replaced by the vocab term at Damerau-Levenshtein ≤1 (≤2 for len ≥8) with the highest doc frequency. Rerun once and print `did you mean: …` on stderr. The rewrite is logged in `fts_query`.
- Tests: fixture log for suggest; edit-distance unit tests; spell only fires under the threshold.
- Accept: `kb search "kuberntes"` returns kubernetes hits with a note.
- Deps: 5.5, 4.5.
- Risk: vocab size. Restrict it to terms with doc ≥2.

### Phase 6: Feedback-based boosts (off by default)

**6.1 Prior model (pure) ∥**
- Model: Sonnet (pure formula given in full); review: Opus via testing-standards-pass (math)
- Files: new `internal/prior/prior.go` and its test.
- Steps:
  1. For a key `(entry_path, heading)`:
     - u = Σ wᵢ over useful verdicts, plus 0.5·wᵢ per open (opens apply to every heading of the path);
     - n = Σ wᵢ over not-useful verdicts;
     - wᵢ = 0.5^(age_days/half_life) × posWeight;
     - posWeight = min(3, rank^0.5) for positives (corrects position bias: a useful hit at rank 6 is stronger evidence), and 1 for negatives.
  2. Unjudged results contribute nothing.
  3. p = (u+2)/(u+n+4), and mult = clamp(1 + max_boost·(2p−1), 1−max_boost, 1+max_boost).
  4. `Prior(ctx, keys, asOf time.Time)` uses only events with ts < asOf.
- Tests: no evidence → 1.0; decay; cap; asOf excludes later events.
- Accept: unit tests.
- Deps: 2.3 (`Judged`).
- Risk: none.

**6.2 Wire the prior into search**
- Model: Sonnet (gated wiring plus additive migration); review: Opus via testing-standards-pass
- Files: `search/search.go` (multiply the fused RRF before the cap), migration 7→8 (`prior` columns), `cli/search.go` (`--no-prior`), explain.
- Gate: `prior.enabled` AND judged searches (≥1 verdict or open) ≥ `prior.min_judged`; otherwise a no-op that logs NULL. Settings JSON includes the prior params.
- Tests: gating; logged values; `--no-prior`.
- Accept: with `prior.enabled=true` on a small log, `kb explain q` says "prior inactive: 12/50 judged searches".
- Deps: 6.1, 2.5.
- Risk: none.

**6.3 Query memory list**
- Model: Sonnet (extra RRF list from spec); review: Opus via testing-standards-pass
- Files: `search/memory.go`.
- Steps: the normalised key is the lowercase sorted unique tokens. Past searches with the same key and useful/open gold give a ranked list of `(path, heading)` (weighted u from 6.1), added as a third RRF list weighted by `list_weights.memory`. It is subject to the same gate plus `prior.query_memory`.
- Tests: same-key retrieval; different key → empty.
- Accept: a repeated query surfaces its previously useful hit at rank 1 in explain.
- Deps: 6.1.
- Risk: none.

**6.4 Validate with eval**
- Model: Opus (leakage-free replay methodology); review: Sonnet via testing-standards-pass
- Files: `internal/eval/replay.go`.
- Steps: replay passes `asOf = original search ts` to prior/memory, so there is no leakage. Add `--set prior.enabled=true` comparisons, and refuse a sweep of prior params under `eval.min_judged`.
- Accept: `kb eval --set prior.enabled=true --compare` prints deltas. Only turn the defaults on if the holdout MRR improves beyond the CI.
- Deps: 6.2, 6.3, 2.6.
- Risk: none.

---

**T1 Test infrastructure: per-test deps, typed CLI outcomes, shared fault seams** (from .agent/testability #4–#9)
- Model: Sonnet (mechanical test refactor); review: Opus via testability-check
- Status: planned. Schedule: its own wave after W8 (it touches every cli test file).
- Steps:
  1. `newTestDeps(t)`, with the cwd set through `workspaceEnv`. Move per-run caches out of `deps`, so CLI tests can use `t.Parallel`.
  2. An `execute` variant that returns the typed error, so decision tests assert kind and cause. Keep one wording test per message family.
  3. Doctor records `[]doctorCheck{Name, Level, Detail}` and prints once at the end (Q60).
  4. Move the sqlite fault driver into a shared test-support package; give `cache.Open` the same hook (Q47).
  5. Add a `sqlitetest` helper plus a `failingLog` wrapper to replace raw `.DB()` SQL in non-sqlite tests.
  6. Harness: a `--debug` progress line to wait on instead of sleeps (H23), and a `harnessfake` build tag with a `fake:<dim>` model (DEFAULTS #189), enabling H2, H14 and H18.
- Accept: `go test -race ./internal/cli/` passes with `t.Parallel` in the CLI tests; no `.DB()` outside the sqlite packages' own tests; the harness runs vector scenarios with the fake model.

## 4. Breaking changes / migration notes

**Obsolete (D8):** there are no users before v1, so nothing below needs a migration path or a user note. The table is kept for history only. Its layout rows (`.kb/kb.sqlite`, `~/knowledge` adoption, `[stores]`) are superseded by D9. After v1, upgrades go through `kb migrate` (D10).

| Change | Who is affected | What to do |
|---|---|---|
| All `KB_*` env vars and `OPENROUTER_API_KEY` are ignored (`KB_ROOT`, `KB_CALLER`, `KB_EMBEDDER`, `KB_EMBED_MODEL/URL/DIM/API_KEY/...`, `KB_OLLAMA_URL`, `KB_OPENROUTER_API_KEY`) | everyone | Run kb inside the store (it walks up to `.kb/`), or pass `--root DIR` / `--global`. Pass `--caller` explicitly. |
| `--embedder` / `--embed-model` flags removed (after 1.3) | scripts | Set `[embed] model` in `.kb/config.toml` (`"none"` = FTS only). |
| Ollama/OpenAI/OpenRouter embedders removed; default model is bge-small (384-d) | everyone with vectors | Migration 2→3 drops vectors (FTS keeps working). Run `kb reindex` once (downloads ~67 MB the first time). |
| Compile-time default root (`install.sh` `KB_DEFAULT_ROOT`, `~/knowledge` fallback) removed | `~/knowledge` user | `cd ~/knowledge && kb init` (adopts the existing DB and keeps the search log), then `kb reindex`. To use it from anywhere: `ln -s ~/knowledge ~/.local/share/kb` and use `--global`, or add `notes = "~/knowledge"` under `[stores]` in the global config and search with `--stores notes`. |
| `/usr/local/bin/kb` symlink and `install.sh` behaviour | installed users | `rm /usr/local/bin/kb` (it points into the old checkout's `.cache/`). Reinstall via `go install -tags fts5 github.com/nickstrad/kb/cmd/kb@latest` or the new `install.sh` (→ `~/.local/bin`). |
| `kb doctor` no longer checks `/usr/local/bin/kb`; "database missing" becomes "not initialised" | scripts parsing doctor | none |
| `searches.kb_version` renamed to `corpus_rev`; the new `kb_version` holds the binary version | hand-written SQL | Update queries. |
| Search human output: no RRF score, no feedback hint, new hit header format | scripts parsing text | Use `--json` (unchanged fields plus `store`, `updated`). |
| `kb feedback` is optional; the hint line is gone | agents/skill users | Remove the skill symlink (`<project>/.claude/skills/update-knowledge-store`) and paste the README snippet. |
| `kb stats` uses SQLite (DuckDB gone); new `opened_pct` and `settings` outputs | `--json` consumers | The column set of the existing 5 queries is unchanged except for the added columns. |
| Tag filter becomes case-insensitive (5.2) | edge case | none |
| Migrations 1→8 are one-way; old binaries refuse the upgraded DB ("schema version N, this kb only understands 1") | anyone running two versions | Upgrade all copies. Back up `.kb/kb.sqlite` first; the search log is the only irreplaceable data. |
| Reindex required at: 2→3 (vectors) and 6→7 (edges/aliases) | everyone | A plain `kb reindex` after upgrading; `--all` is not needed. |
| Release binaries for linux/darwin are built but only macOS is tested | Linux users | Report issues; `kb doctor` is the first diagnostic. |

---

## 5. Open questions (with proposed defaults)
1. **Fallback when no `.kb/` is found from cwd.** Default: error with a hint. Alternative: read-only commands silently use the global store.
2. **Global store location on macOS.** Default `~/.local/share/kb` on both OSes. Alternative: `~/Library/Application Support/kb`.
3. **Should `.kb/eval/` baselines be committed?** Default: gitignored (they depend on the local log). Alternative: commit them for team-shared stores.
4. **Respect `.gitignore` in `files` sources?** Default: no; use explicit `exclude` plus built-in `.git`/`.kb`/`node_modules`.
5. **`int8` default.** Default: off for bge-small (small model, quality first). Revisit after the spike numbers.
6. **Spell correction default.** Default: on, but only when FTS returns < 3 rows, and it always says so on stderr.
7. **Model2Vec fallback trigger.** Default: only if the spike fails to build or load, or if warm load + query > 1 s on darwin/arm64.
8. **Static Linux binaries (musl via `zig cc -target x86_64-linux-musl`) instead of glibc-dynamic?** Default: glibc-dynamic from native runners. Revisit if someone needs Alpine or old distros.

Defaults chosen for these questions are tracked in the local, unmerged `DEFAULTS.md`.

### Critical files for implementation
- /Users/nick/Software/kb/internal/search/search.go
- /Users/nick/Software/kb/internal/store/entries.go
- /Users/nick/Software/kb/internal/store/store.go
- /Users/nick/Software/kb/internal/entry/entry.go
- /Users/nick/Software/kb/internal/cli/root.go
- /Users/nick/Software/kb/internal/stats/queries.go
- /Users/nick/Software/kb/internal/search/log.go
- /Users/nick/Software/kb/scripts/install.sh (rewritten in 1.6; release workflow is new at /Users/nick/Software/kb/.github/workflows/release.yml)

---

## 6. Execution map

Who builds each item, what runs at the same time, and in what order. Model choice and review pass are also on each item in §3. "Hotspots" are shared files that only one agent may edit at a time. After 1.2, read `store.go`/`entries.go` as `internal/store/sqlite/…` and migrations as `internal/store/sqlite/migrations.go`.

### 6.1 Item table
| id | title | model | deps | hotspots | wave |
|---|---|---|---|---|---|
| 0.1 | rembed spike | Sonnet | — | go.mod | done |
| 0.2 | Hygiene | Haiku | — | cli tests, comments | done |
| 0.3 | Remove skill; help is truth | Haiku | — | README, root.go | done |
| 0.4 | Remove DuckDB | Sonnet | — | stats/*, go.mod | done |
| 0.5 | Embedders; lazy vec width | Opus | 0.1, (0.6 for migration order) | embed/*, cli/setup.go, root.go, go.mod (0.5a); store/entries.go, store.go **mig v3**, search/search.go (0.5b) | W1 (0.5a), W2 (0.5b) |
| 0.6 | Log provenance, version, doctor | Opus | — | store.go **mig v2**, search/log.go, cli/search.go, cli/doctor.go, cmd/kb/main.go | W1 (in progress) |
| 0.7 | Optional feedback + opens | Sonnet | 0.6 | cli/show.go, search/log.go, cli/search.go, stats/queries.go | W2 |
| 1.1 | Workspace / root discovery | Sonnet | 0.5 | root.go, cli/setup.go, every cli/*.go `Root()` site, cli tests | W4 |
| 1.2 | Store seam | Opus | (all of Phase 0) | store/*, search/search.go, search/log.go, reindex, cli imports | W3 |
| 1.3 | Config + `kb config` | Sonnet | 1.1 | cli/setup.go, root.go flags | done (W5) |
| 1.4 | `kb init [--name]`, `kb model` | Sonnet | 1.3, 1.7b | cli/init.go, model.go, doctor.go, entry/entry.go (Discover) | W6 (lane A, after 1.7b) |
| 1.5 | In-place sources | Opus | 1.3, 1.4, 1.7b | entry/entry.go, chunk/chunk.go, reindex, cli/add.go, cli/rm.go, index | W7 |
| 1.6 | CI + release + install.sh | Sonnet | 0.2, 0.4 | .github/*, scripts/install.sh, README Install | W1 |
| 1.7a | Named stores: location + store resolution | Sonnet | 1.1 | workspace/location.go (new) | W6 (lane A, first) |
| 1.7b | Named stores: cli switch, `--store`, `kb stores` | Sonnet | 1.7a, 1.3 | workspace/workspace.go, workspace/refs.go (new), config/location.go (new), cli/root.go, cli/setup.go, cli/report.go, cli/stores.go (new), cli test helpers | W6 (lane A, second) |
| 1.8a | Migration framework + existing artefacts | Opus | 1.2, 3.3a | migrate/ (new), sqlite/sqlite.go, sqlite/migrations.go, sqlite/artefact.go (new), embed/cache/cache.go | W6 (lane C) |
| 1.8b | `kb migrate`; explicit migration | Sonnet | 1.8a, 1.7b | sqlite/sqlite.go, config/migrate.go (new), cli/migrate_cmd.go (new), cli/init.go, README upgrade section | W7 (lane, before 2.5) |
| 2.1 | Search trace plumbing | Opus | 1.2, 1.3 | search/search.go, sqlite/retrieve.go, store/types.go, cli/search.go | W6 |
| 2.2 | `kb explain` | Sonnet | 2.1 | new files only | W7 |
| 2.3 | `kb eval` core | Opus | 2.1, 0.7 | eval/*, cli/eval.go, new sqlite/judged.go | W8 |
| 2.4 | Baselines/variants | Sonnet | 2.3 | eval/*, cli/eval.go | W9 |
| 2.5 | Log effective settings | Sonnet | 1.3, 2.1, 1.8b | **mig v4**, sqlite/log.go, search/search.go, store/types.go, stats/queries.go | W7 |
| 2.6 | `kb eval --sweep` | Opus | 2.4 | eval/sweep.go, cli/eval.go | W10 |
| 3.1 | Store refs and registry | Sonnet | 1.7b, 1.3 | workspace/refs.go, cli/root.go (`--store` grammar), cli/stores.go (alias rows) | W7 |
| 3.2 | Cross-store merge + log | Opus | 3.1, 2.1, (v4 landed) | **mig v5**, sqlite/log.go, cli/search.go, store/types.go, search/multi.go | W8 |
| 3.3 | Embedding cache | Opus | 0.5 | 3.3a: new embed/cache/, embed/cached.go; 3.3b: cli/setup.go wiring | W2 (3.3a), W7 (3.3b) |
| 4.1 | Compact render + budget | Sonnet | 2.1 | cli/search.go, render/ (new) | W7 |
| 4.2 | `--expand` | Sonnet | 4.1 | render/*, cli/search.go | W9 |
| 4.3 | `kb context` | Haiku | 4.1, 4.2 | cli/context.go (new) | W10 |
| 4.4 | Contextual embedding audit | Sonnet | — | chunk/chunk.go, chunk goldens | W1 |
| 4.5 | Exact identifier list | Opus | 2.1, 2.5, (v5 landed) | **mig v6**, sqlite/entries.go, sqlite/retrieve.go, sqlite/log.go, search/search.go, search/explain.go | W9 |
| 4.6 | Phrase/NEAR tier | Sonnet | 4.5, 2.4 | search/search.go, sqlite/retrieve.go | W13 |
| 5.1 | Link extraction | Sonnet | 1.5 (see 6.5) | entry/entry.go (ParseFrontMatter), links/ (new) | W4–W5 |
| 5.2 | Edges, entry_tags, aliases | Opus | 5.1, (v6 landed) | **mig v7**, sqlite/entries.go, sqlite/retrieve.go, store/store.go (Index), reindex | W10 |
| 5.3 | `kb related`, supersede | Sonnet | 5.2 | search/search.go, sqlite (Related, new file), render, cli/related.go | W11 |
| 5.4 | `kb stale` | Haiku | 5.2 | cli/stale.go (new) | W11 |
| 5.5 | Synonyms | Sonnet | 1.2, 2.5 | search/search.go, search/synonyms.go, cli/search.go | W10 (lane, then 5.6) |
| 5.6 | Synonym suggest + spell | Opus | 5.5, 4.5 | search/search.go, search/spell.go, cli/synonyms.go, new sqlite/vocab.go | W10 (after 5.5) |
| 6.1 | Prior model (pure) | Sonnet | 2.3 | prior/ (new) | W9 |
| 6.2 | Wire prior | Sonnet | 6.1, 2.5, (v7 landed) | **mig v8**, search/search.go, cli/search.go, sqlite/log.go, explain | W12 (lane, then 6.3) |
| 6.3 | Query memory list | Sonnet | 6.1 | search/search.go, search/memory.go | W12 (after 6.2) |
| 6.4 | Validate prior with eval | Opus | 6.2, 6.3, 2.6 | eval/replay.go | W13 |

Counts: Opus 14, Sonnet 24, Haiku 4 (42 items).

### 6.2 Waves
A wave starts when every item of the previous wave is merged and its review passes are clean. Every wave ends with the wave-end standards pass (6.4 rule 4): `testing-standards-pass` and `error-handling-pass` over the wave's diff, then fixes, before the next wave starts. A "lane" is one agent; an arrow inside a lane means the items run one after the other in that lane.

- **W0 (done):** 0.1, 0.2, 0.3, 0.4.
- **W1 (done):**
  - 0.6 (orchestrator). Owns store.go (mig v2), search/log.go, cli/search.go, cli/doctor.go, cmd/kb/main.go.
  - 0.5a: delete `embed/{ollama,openai,provider}`, finish `embed/local`, `embed/embed.go`, `cli/setup.go`, root.go flags/help, `add_test.go:useFakeEmbedder`, go.mod/go.sum. It must not touch store or search files.
  - 1.6: `.github/`, `scripts/install.sh`, README (sole README owner this wave).
  - 4.4: `chunk/chunk.go` + goldens. Its deps are already met and nothing else touches chunk.go until 1.5.
- **W2 (done):**
  - 0.5b: `store/entries.go`, store.go mig v3 (after v2 has landed), `search/search.go:vecList`.
  - 0.7: show.go, search/log.go, cli/search.go, stats/queries.go. No overlap with 0.5b.
  - 3.3a: new `embed/cache/` + `embed/cached.go` only. No wiring.
  - → wave-end standards pass (6.4 rule 4).
- **W3 (done):** 1.2 alone. It moves store/search/log SQL and rewrites imports across cli, so it collides with everything except 1.6 and 3.3a (which may still be finishing). Move-only commit first, then narrowing.
- **W4 (done):** 1.1 (owns all of `internal/cli/*`, root.go, setup.go) ∥ 5.1 (links/ new + `entry.ParseFrontMatter`; pulled forward, see 6.5).
- **W5 (done):** 1.3 (config/ new, setup.go, root.go flags, cli/config_cmd.go), including the named-stores prep (D9). The W5 pass adds the config `version`-first ordering.
- **W6 (done):**
  - lane A: 1.7a → 1.7b → 1.4. 1.7a: workspace/location.go (new). 1.7b: workspace.go, refs.go, config/location.go, cli/root.go, setup.go, report.go, stores.go, cli test helpers. 1.4: init.go, model.go, doctor.go, entry.Discover (5.1 must be merged first, because both edit entry.go).
  - lane B: 2.1: search.go, sqlite/retrieve.go, store/types.go, cli/search.go. It also adds the retrieval-side fields that later items need (see 6.5). 1.7b may touch cli/search.go only where it must to compile, and coordinates with 2.1.
  - lane C: 1.8a: migrate/ (new), sqlite/sqlite.go, sqlite/migrations.go, sqlite/artefact.go, embed/cache/. No overlap with 2.1 (retrieve.go) or lane A.
  - 3.1 moves to W7: it needs 1.7b's location file and refs.
- **W7 (done):**
  - 1.5: entry, chunk, reindex, add.go, rm.go, index.
  - 2.2: explain new files only.
  - lane 1.8b → 2.5. 1.8b: sqlite/sqlite.go, config/migrate.go, cli/migrate_cmd.go, cli/init.go. 2.5: mig v4 (written as a `migrate.Step`), sqlite/log.go, migrations.go, search.go, store/types.go, stats/queries.go.
  - 3.1: workspace/refs.go, cli/root.go, cli/stores.go.
  - 4.1: render/ new, cli/search.go.
  - 3.3b: setup.go wiring.
- **W8:** 3.2 (mig v5, sqlite/log.go, store/types.go, cli/search.go, search/multi.go) ∥ 2.3 (eval/, cli/eval.go, new `sqlite/judged.go` + `store/judged.go` to stay out of log.go/types.go).
- **W9:**
  - 4.5: mig v6, sqlite/entries.go, retrieve.go, log.go, search.go, explain.
  - 4.2: render/, cli/search.go.
  - 2.4: eval/, cli/eval.go.
  - 6.1: prior/ (new).
- **W10:**
  - 5.2: mig v7, sqlite/entries.go, retrieve.go, store/store.go, reindex.
  - lane 5.5 → 5.6: search.go, synonyms.go, spell.go, cli/search.go, cli/synonyms.go, new sqlite/vocab.go.
  - 2.6: eval/sweep.go, cli/eval.go.
  - 4.3: cli/context.go.
- **W11:** 5.3 (search.go, render, sqlite Related in a new file, cli/related.go) ∥ 5.4 (cli/stale.go).
- **W12:** lane 6.2 → 6.3: mig v8, search.go, memory.go, cli/search.go, sqlite/log.go.
- **W13:** 4.6 (search.go, retrieve.go) ∥ 6.4 (eval/replay.go).

**Why the later waves are narrow.** Two chains force serial work:
- Migrations must land in number order (v4 → v8).
- `search/search.go` has several writers: 2.1, 2.5, 4.5, 5.5/5.6, 5.3, 6.2/6.3 and 4.6.

**Critical path:** 0.6 → 0.5b → 1.2 → 1.1 → 1.3 → 2.1 / 1.8a → 1.8b → 2.5 (v4) → 3.2 (v5) → 4.5 (v6) → 5.2 (v7) → 5.3 → 6.2/6.3 (v8) → 6.4. That is 13 waves after W0.

### 6.3 Migration numbers (pre-assigned, fixed)
| version | item | wave | lands after |
|---|---|---|---|
| v2 | 0.6 | W1 | — |
| v3 | 0.5b | W2 | v2 |
| v4 | 2.5 | W7 | v3 |
| v5 | 3.2 | W8 | v4 |
| v6 | 4.5 (+ `chunks_exact_vocab`, moved from v7; see 6.5) | W9 | v5 |
| v7 | 5.2 | W10 | v6 |
| v8 | 6.2 | W12 | v7 |

- The migrations list is positional, so items merge in version order even when they are built in parallel.
- If a lower-numbered item stalls, the orchestrator decides whether to swap numbers. Any swap updates §2.5 and this table in the same change.
- No agent adds a migration that is not in this table.
- From 1.8a, every step is a `migrate.Step` with an ID `index/000N-<slug>` and an Effect (§2.5). From 1.8b, steps run only through `kb migrate up` (D10). The embedding cache (`cache/000N-…`) and the config/location files (`config/…`, `location/…`) number their steps independently of this table.

### 6.4 Orchestration rules
1. **One owner per hotspot file per wave.** The hotspots are:
   - `store.go`/`migrations.go`, `store/types.go`, `store/store.go`;
   - `sqlite/log.go`, `sqlite/retrieve.go`, `sqlite/entries.go`;
   - `search/search.go`, `cli/search.go`, `cli/root.go`, `cli/setup.go`;
   - `go.mod`/`go.sum`, `README.md`.

   A non-owner who needs a change there either puts the code in a new file or asks the owner to make it. A non-owner that needs a new store method declares a small interface in a new file instead of editing `store/store.go`.
2. **New-command registration** (`AddCommand` lines in `root.go`) is added by the orchestrator at merge time, not by item agents.
3. **State log.** Each agent keeps a state log in `.agent/state/<item-id>.md` per AGENTS.md: start, decisions, blockers, files touched, and the exact test commands run.
4. **Wave-end standards pass (mandatory, every wave).** At the end of EVERY wave, run both project skills, `testing-standards-pass` and `error-handling-pass` (CODING_STYLE), over that wave's diff. Then address the findings before the next wave starts:
   - **High:** fixed in-wave, by the agent that owns the file or a small fix-up agent.
   - **Medium:** fixed, unless the fix belongs to an upcoming item; then note it on that item.
   - **Low:** appended to `.agent/queue/low-findings.md` (AGENTS.md "Low-findings queue") and drained by **Haiku** sub-agents in batches before the next wave starts, or alongside it on files no active agent owns. Small mechanical Mediums may go there too. Anything Haiku marks `needs: sonnet|opus` becomes a fix-up or is attached to an upcoming item.

   **Adversarial coverage verdict (part of every pass).** After the two skills, the pass states, per item in the wave, whether the existing adversarial tools (`docs/ADVERSARIAL_TESTING.md`, the `adversarial-testing` skill, `.agent/scripts/adversarial/`) already probe what changed. Examples: a mutation run over the changed packages; a CLI-harness scenario for each new command, flag, exit code or integration point; hostile inputs for each new parser. The verdict is either **"suffices"**, naming which tool or scenario covers it, or **"gaps"**, with concrete suggestions. Run the relevant tools when they exist; a survivor or a failing scenario is a finding like any other. Each suggestion goes to `.agent/queue/harness-improvements.md` as an `H<n>` entry, to be worked on later, not in the same wave unless it's High value and cheap.

   **Leverage review (part of every pass).** Run the `leverage-check` skill. Update the `.agent/leverage/` entries the wave touched, and archive any whose area was refactored, redesigned or stripped out (`.agent/leverage/archive/`) (status, evidence, coverage today, next checks, last reviewed). Propose additions or retirements, keeping it to about 10 active entries. Turn any "next check" that is now due into an H entry, a queue entry or a PLAN note.

   **Testability review (part of every pass).** Run the `testability-check` skill. Update the `.agent/testability/` entries the wave touched (symptoms, counts, status, suggested PLAN home, last reviewed), archive the ones whose remedy landed (`.agent/testability/archive/`), and add new candidates only with evidence, keeping about 10–15 active entries. When an upcoming item owns an entry's files, note the remedy on that item.

   The orchestrator records the pass result (counts per severity, what was fixed or deferred, and the adversarial verdict) in the wave's state log and folds lasting lessons into `.agent/knowledge/`. Model for the pass: Opus for waves that contain Opus items, Sonnet otherwise.
5. **Judgment calls** that deviate from or fill gaps in this plan go in `DEFAULTS.md`, with the item id.
6. **Green before merge.** `go vet -tags fts5 ./...` and `go test -tags fts5 ./...` pass before merge. Items that touch embedding also run `-tags model`.
7. **Commit per wave (D7).** After the wave-end pass, when everything is green, the orchestrator scans for secrets, commits once with a summary of the wave's items, and pushes `main`. `docs/DEFAULTS.md`, state logs and sandboxes are never committed.

### 6.5 Proposed reorders and splits (not yet applied to §3)
- **0.5 split into 0.5a/0.5b.** 0.5a (the embed side) can run alongside 0.6. 0.5b (the store side, mig v3) must follow 0.6's v2.
- **1.2 cannot run in parallel with 1.1, despite its note.** Both rewrite `internal/cli/*`. 1.2 goes first: it has no Phase 0 deps beyond the code settling, and Phase 2 needs it.
- **3.3 split into 3.3a/3.3b.** The cache package (new files) can start at W2. Wiring waits for `cli/setup.go` to be free (W7).
- **5.1 pulled forward to W4–W5.** It is pure parsing plus `ParseFrontMatter`. It depends on 1.5 only through entry.go, so it must merge before 1.4/1.5 edit entry.go.
- **fts5vocab table moved from mig v7 (5.2) to v6 (4.5).** It depends only on `chunks_exact`, and the move lets 5.6 run alongside 5.2 in W10.
- **2.1 absorbs the store-side fields** needed later: the `rowid IN (...)` restriction for 2.2's single-column bm25, and the `ChunkMeta` `updated`/`entry_id`/`ord`/`source_file` fields for 4.1/4.2. This keeps `sqlite/retrieve.go` with one owner in W7 and W9.
- **2.3's `Judged` goes in new files** (`sqlite/judged.go`, `store/judged.go`), so it can run alongside 3.2.
- **4.6 moved to the last wave** (W13). It is not on the critical path and it is another `search.go` writer.
- **Optional split of 1.6:** ci.yml (Haiku) and release.yml + install.sh (Sonnet).
