# Named stores

Status: **accepted** (2026-10-30). Folded into docs/PLAN.md as items 1.7a, 1.7b and as changes to 1.4, 1.5, 3.1 and 3.2.
Owner context: kb has no users before v1 (PLAN D8). This design drops the single-store layout outright. There is no legacy marker and no adoption code.

## Action for 1.3 now

Sent on 2026-10-30. **Done in 1.3 (W5)**, with nothing deferred:

1. Every store path comes from `*workspace.Workspace`. `cli.DBPath`/`EnsureDBDir` are deleted, and `setup.go loadConfig(cmd, ws)` is the only place a config file is read.
2. `[stores]`, `MergeStores` and the global-config read are removed from the store `Config`. `search.stores` stays (non-empty check only).
3. The legacy embed_meta fallback (`recordedModel`) is removed; a missing config means the defaults.
4. `Config` has no location keys.

The W5 pass also owns the config `version` ordering fix (migrations.md §2.4).

## 1. Model

| Term | What it is | On disk |
|---|---|---|
| **Location** | A directory that holds a `.kb/` folder. Discovery finds a location, not a store. | `<root>/.kb/kb.toml` (the marker and location file) |
| **Store** | A named, self-contained index. It has: a name; sources (its corpus); its own config (embedder identity, search settings); its own database (index plus search log); and a backend kind. | `<root>/.kb/stores/<name>/` |
| **Default store** | The store that commands use when no `--store` is given. | `default_store` in `kb.toml`; `kb init` sets it to `"default"` |
| **Global location** | The machine-wide location. Its `kb.toml [aliases]` holds machine-level aliases (as DEFAULTS #12 says for the global store today). | `$XDG_DATA_HOME/kb/.kb/` |

A store is everything that today's single `.kb/` holds:

```
<root>/
  .kb/
    kb.toml                 # location: version, default_store, [aliases]
    .gitignore              # "stores/*/kb.sqlite*", "stores/*/eval/"
    stores/
      default/
        config.toml         # 1.3's Config schema, unchanged: [embed] [[sources]] [search] ...
        kb.sqlite           # index + search log (WAL: -wal, -shm beside it)
        eval/               # baselines (Phase 2), git-ignored
        synonyms.txt        # Phase 5, optional
      adr/
        config.toml
        kb.sqlite
  data/                     # entries source of "default"
  docs/ adr/ ...            # in-place sources (1.5), relative to <root>
```

`kb.toml`:

```toml
version = 1
default_store = "default"

[aliases]                   # store refs (§3); meaningful mostly in the global location
notes = "~/knowledge"       # the default store of that location
adr   = "adr@~/src/proj"    # a named store of another location
```

**Names.** A name matches `^[a-z0-9][a-z0-9_-]{0,31}$`. It is lowercase so the case-insensitive macOS filesystem can't make two names collide. It contains no `@`, `:` or `/`, so refs and qualified paths stay unambiguous. The word `global` is reserved, because it is a ref keyword.

**A store exists** when `stores/<name>/config.toml` is a regular file. Its database may be missing ("not indexed yet").

**Backend kind.** Every store is `sqlite` in v1. A later `backend = "duckdb" | "turso"` key in the store's `config.toml` would pick the `store.Store` implementation. This is a seam only: nothing is added now (non-goal: no second backend).

## 2. On-disk layout: options considered

| | **A. Store dirs + location file** (chosen) | B. Default at `.kb/`, named ones in `.kb/stores/` | C. One config with `[store.<name>]` tables, `.kb/<name>.sqlite` |
|---|---|---|---|
| Discovery | One marker: `.kb/kb.toml` | Two markers (`config.toml`, or a `stores/` dir when there is no default) | One marker |
| Per-store embedder/settings | The store's own `config.toml`, with 1.3's schema unchanged | Same, but the default store's file is also the location file (mixed schema) | Every key nests (`[store.adr.search] k = 8`); `--set` needs a store prefix; 1.3's `Config` becomes a map |
| Uniformity | Every store is the same shape | The default store is a special case in every path rule | Uniform, but one file for everything |
| Locking / contention | One SQLite file per store: each store has its own write lock | Same | Same if there is one DB per store. One shared DB would put every store's reindex behind one writer lock (sqlite-write-contention) |
| Backup / move / delete a store | Copy or `rm -r` one directory | Same, except for the default store | Edit the config and delete files |
| git-ignore | One `.kb/.gitignore` with two patterns; configs are committable | Same | Same |
| Migration from today | none needed (D8) | — | — |

B only existed to keep today's layout working, and D8 removes that reason. C fights 1.3's schema and makes every override and error message store-qualified. A keeps `config.Config` exactly as 1.3 builds it, and it makes a store a directory that can be copied.

## 3. Addressing

**Ref grammar** (`internal/workspace/refs.go`, 3.1):

```
REF   := NAME              a store in the current location
       | ALIAS             from [aliases] of the current location, then the global location
       | global            the global location's default store
       | NAME@LOC          a named store of another location
LOC   := global | PATH     PATH: absolute, ~/..., or relative to cwd; a location root
```

A path-only ref (`~/knowledge`) is allowed in `[aliases]` values and in `--stores`. It means that location's default store.

**Bare word resolution:** a local store name comes first, then an alias, then `global`. If a local name shadows an alias, kb prints a one-line warning on stderr. `kb stores` marks shadowed aliases.

**Flags.** All three are persistent flags:

| Flag | Meaning |
|---|---|
| `--root DIR` | The location at DIR (no discovery) |
| `--global` | The global location |
| `--store REF` | The store to work on. A local NAME applies within the location chosen by `--root`/`--global`/discovery. An ALIAS or a `NAME@LOC` names its own location, so combining one with `--root`/`--global` is a Usage error. `--global` is `--store global`. |

When `--store` is not given, the store is `default_store` of the location.

Resolution order: location (`--root` > `--global` > discovery of `.kb/kb.toml`), then store (`--store` > `kb.toml default_store`). Errors:

- no location: NotFound, the message 1.1 gives today;
- a location with no such store: NotFound, `no store "adr" in /p (stores: default, docs); create it with kb init --name adr`;
- a location whose `default_store` does not exist: NotFound naming `kb.toml`.

**Commands:**

| Command | Behaviour |
|---|---|
| `kb init [--name NAME] [--model ID\|none] [--source GLOB]...` | Creates the location (`kb.toml`, `.gitignore`) if it is missing, then creates the store (default name `default`). The first store created becomes `default_store`. An existing store is "already initialised" (exit 0). The `$HOME` and `/` guard (DEFAULTS #121) applies to creating a *location*. |
| `kb stores [--json]` | Lists the stores of the current location (name, default marker, model, entries count, DB size, "not indexed"), then the aliases (location, then global) with their resolved targets and whether each resolves. |
| `kb add / edit / rm / show / list / reindex / doctor / config / feedback / stats` | Work on exactly one store, the resolved one. They never fan out: writing to several stores at once has no clear meaning. |
| `kb search [--stores REF,...]` | The primary store is the resolved one. Extra stores come from `--stores`, or from the primary's `search.stores` when the flag is absent. `--stores ''` turns them off. `--stores all` means every store of the current location. |
| `kb show REF:path` | A store-qualified path. REF is a name, an alias or `global` only (no `@`, which keeps parsing unambiguous). |

There is no `kb store add/rm` in v1: `kb init --name` creates a store, and `rm -r .kb/stores/<name>` deletes one. `kb stores` is enough to inspect them. This can be revisited after v1.

## 4. Cross-store queries

Phase 3 generalises without new machinery. "Another store" can now be in the same location or in another one.

1. **Open.** The primary store opens read-write (it gets the log). Every other store opens read-only (`sqlite.Open{ReadOnly}`). A store that is missing, has a newer schema, or is unindexed gives a warning on stderr and is skipped. Fan-out is capped at 8 extra stores (Usage error above that). The leverage risk is open file handles and latency.
2. **Per-store search.** Each store runs the normal `search.Searcher` with **its own** `Retriever` and embed model, and with the **primary's** `[search]` settings, so results are comparable and the logged `settings_hash` stays meaningful. The query is embedded once per distinct `(model, query prefix)`, through `embed.Cached`. The shared cache is keyed by model, so mixed models never mix vectors. Each distinct model is one loaded model in memory, about 70–300 MB. kb warns when more than 2 distinct models are loaded.
3. **Merge on ranks only.** The merged score is `Σ 1/(rrf_k + rank_in_store_s)`. Raw scores (bm25, cosine) are never compared across stores. This is what makes "different paradigms" work: a store with FTS only, a different model, or a future backend only has to return **an ordered list of hits** that carry `(path, ord, heading, text, updated)`. That is today's `search.Hit` plus a store tag. Nothing else crosses the boundary.
4. **Identity.** A hit's identity is `(location root, path, ord)`.
   - Two stores in **the same location** that index the same file produce the same identity. Their RRF terms add up, which is an ensemble (for example the same corpus under bge-small and e5). The hit is tagged with the best-ranked store.
   - Stores in **different locations** never share an identity: `data/x.md` in two projects are two different files, and they are capped separately (3.2's rule).
   - Tie-break: primary first, then the order of `--stores`.
5. **Cap and k.** The per-entry cap applies per `(location, path)`, then k is applied.
6. **Log.** Only the primary store's log records the search. `search_results.store` and `search_candidates.store` (migration v5, 3.2) hold the **canonical ref** `NAME@ABSROOT` for non-primary hits (NULL = primary). An alias is not logged, because aliases change. `kb feedback` and `kb show REF:path` write to the primary log.

## 5. Corpora and sources per store

- `[[sources]]` belongs to the store's `config.toml`. Paths and globs are **relative to the location root**, not to the store directory. Two stores in one repo can therefore index `docs/**` and `adr/*.md`, or the same files.
- **Explicit sources only.** `kb init` writes the sources into the config:
  - the default store gets `[[sources]] path = "data", layout = "entries"`;
  - a named store with no `--source` gets `path = "data-<name>"`, entries layout. It does not get `data/<name>`: the default store's entries layout would read `data/<name>/README.md` as one of its own directory entries.
  - A config with no `[[sources]]` indexes nothing, and `kb doctor` warns about it. There is no name-dependent implicit `data/`.
- **Overlap:** inside one store, two sources that match the same path is a config error (1.5 step 5). Across stores, overlap is allowed and intended (same corpus, different model, or a subset).
- `kb add --store adr x.md` copies into that store's only `entries` source. If the store has none or several, it is a Usage error.
- **Entry identity** inside a store is the root-relative path, as today. Across stores it is `(location root, path)` (§4).

## 6. Impact on the plan

| Item | Change |
|---|---|
| §1 | New D8 (no users before v1; breaking changes are free until then; migrations matter after it). New D9 (named stores: locations hold stores). The "global store and machine config" planner decision now says "global location's `kb.toml`". |
| §2.1, §2.3 | `workspace` resolves location + store. §2.3 adds the location file, and `[stores]` leaves the store config. |
| §2.6 (new) | C4 container sketch (below). |
| 1.1 (done) | Superseded in part: the legacy `.kb/kb.sqlite` marker and the interim store creation (DEFAULTS #110/#111) are removed by 1.7b. |
| 1.3 (W5, done) | "Action for 1.3 now" above, all done in 1.3. |
| **1.7a (new, W6, first in lane A)** | `internal/workspace/location.go` (new file only): `ResolveLocation`, `Location.Store(name)`, `ValidName`, `ListStores`. The cli is not switched yet. |
| **1.7b (new, W6, lane A after 1.7a, before 1.4)** | `config.Location` (+ `ReadLocation`); the cli moves to location+store; `--store`; `kb stores`; removes the legacy marker, interim creation and old `Resolve`. |
| 1.4 (W6) | `kb init --name`; location creation plus the home guard; explicit sources; no legacy adoption step. |
| 1.5 (W7) | Sources are root-relative; overlap rules across stores; `kb add --store` targets the entries source. |
| 3.1 (W6 → **W7**) | Ref grammar (`NAME`, alias, `global`, `NAME@LOC`, path); `--stores all`; aliases from location then global `kb.toml`. It moves one wave because it needs 1.7b's `config.Location`. It is not on the critical path (3.2 is W8). |
| 3.2 (W8) | Identity `(root, path, ord)` with same-location ensemble; canonical `NAME@ABSROOT` in the v5 `store` column; fan-out cap; distinct-model warning. |
| 3.3b (W7) | No change. The cache is machine-wide and keyed by model. |
| DEFAULTS #121 | Unchanged in spirit: it guards *location* creation (`kb init` at `$HOME` or `/`). Since only `kb init` creates locations after 1.7b, the H28 trap (`--root ~ add`) disappears. |
| Schema | No migration. The layout is all that changes. v5's `store TEXT` is reused with the canonical ref. |

### C4 container sketch

Before (today):

```
[ kb CLI process ] --discovers--> <root>/.kb/{config.toml, kb.sqlite}   (one store per root)
        |                                    ^ index + search log
        +--> embed/local (rembed, in-process) --> ~/Library/Caches/kb/models
        +--> embed cache (Phase 3)            --> ~/Library/Caches/kb/embed-cache.sqlite
```

After:

```
[ kb CLI process ]
   | workspace: Location (.kb/kb.toml) -> Store(name)
   |
   +--rw--> <root>/.kb/stores/<primary>/{config.toml, kb.sqlite}   index + search log (logs here only)
   +--ro--> <root>/.kb/stores/<other>/kb.sqlite                    same-location stores  } fan-out
   +--ro--> <other-root>/.kb/stores/<name>/kb.sqlite               aliases / NAME@LOC    } (3.x)
   +--ro--> $XDG_DATA_HOME/kb/.kb/{kb.toml [aliases], stores/default/}  global location
   |
   +--> store.Store (sqlite today; backend key later) per store
   +--> embed/local, one loaded model per distinct store model --> models cache (machine-wide)
   +--> embed cache keyed (model, dim, hash) --> embed-cache.sqlite (machine-wide, shared)
```

Components inside the CLI: `workspace` (location and store resolution, refs), `config` (Config per store, Location per location), `search` (per-store Searcher, then `search/multi.go` RRF over ranks), `store/sqlite` (one handle per store).

## 7. Risks and leverage

| Risk | Mitigation | Leverage entry / harness |
|---|---|---|
| Fan-out opens many DBs and loads several models | Cap of 8 stores; warning above 2 models; read-only opens; skip with a warning | search-latency-and-scale, embedding-model-lifecycle; harness: `--stores` with 3 stores and 2 models, measure p95 |
| Contention | One file per store, so reindexing `adr` never blocks a search logging into `default`. A cross-store search writes only the primary | sqlite-write-contention: add "reindex store A while searching A+B" |
| Discovery ambiguity: a nested location inside another | Nearest `.kb/kb.toml` wins (as today); `--debug` names location and store (DEFAULTS #118 line gains the store) | filesystem-boundary; harness `workspace-discovery`: nested locations, `--store` unknown, `default_store` missing, alias shadowed by a local name |
| User confusion: store vs location vs alias | One word per concept in help and errors; `kb stores` shows all three; the `[default]` hit tag only appears in multi-store output | README section |
| A name that is a path trap (`../x`, `A` vs `a`) | `ValidName` regexp; fuzz target `FuzzValidName` + property "valid ⇒ `StoreDir` is a child of `.kb/stores`" | filesystem-boundary |
| The same file in two same-location stores is double-counted | That is intended (ensemble). Identity is by `(root, path, ord)`, and an eval run can measure it | — |

New harness scenarios (3.x and 1.7b acceptance): `stores-layout` (init default and named, list, unknown store, missing default_store, name validation), `stores-alias` (alias, `NAME@LOC`, `global`, shadowing, an unresolvable alias warns), `stores-fanout` (3 stores, mixed `none` and model, merged tags, log rows only in the primary).

## 8. Decision and rollout

Design A is chosen, rolled out in four steps:

1. **W5 (done):** 1.3 did "Action for 1.3 now".
2. **W6:** lane A runs 1.7a → 1.7b → 1.4. The cli switches to location+store, `--store` and `kb stores` arrive, and `kb init [--name]` follows. 2.1 runs alongside; 1.7b must not edit `cli/search.go` beyond the `ws` plumbing (2.1 owns it this wave).
3. **W7:** 1.5 (root-relative sources, overlap across stores) and 3.1 (refs, aliases).
4. **W8:** 3.2 (fan-out merge, identity, canonical refs in the log).

Multiple stores per location are usable after W6 (one at a time, with `--store`). Cross-store search arrives in W8, as before.

### Decisions (owner decisions, decided with these defaults)

| # | Decision | Chosen | Rejected alternatives |
|---|---|---|---|
| N1 | Layout | A: `.kb/kb.toml` + `.kb/stores/<name>/{config.toml,kb.sqlite}` | B: default at `.kb/` top level; C: one config with `[store.<name>]` and `.kb/<name>.sqlite`; one shared DB for all stores |
| N2 | Default store name | `default`, recorded as `default_store` in `kb.toml` | `main`; the directory's basename; no default (always `--store`) |
| N3 | Location marker | `.kb/kb.toml` only (regular file) | Also `.kb/stores/*/config.toml`; keeping `.kb/kb.sqlite` / `.kb/config.toml` (legacy) |
| N4 | Where aliases live | `[aliases]` in `kb.toml` of the location, then of the global location | `[stores]` in each store's config (as PLAN §2.3 had it); a separate `~/.config/kb` |
| N5 | Store name rule | `^[a-z0-9][a-z0-9_-]{0,31}$`, `global` reserved | Any filename-safe string; case-preserving |
| N6 | `--store` scope | Accepts a full REF; an alias or `NAME@LOC` with `--root`/`--global` is a Usage error | NAME only (aliases in `--stores` only); a separate `--location` flag |
| N7 | Bare-word resolution | Local store name, then alias, then `global`; warn when a name shadows an alias | Alias first; an error on collision |
| N8 | Search settings in a fan-out | The primary's `[search]`; each store's own embed model | Each store's own `[search]` (incomparable ranks, meaningless settings_hash) |
| N9 | Cross-store identity | `(location root, path, ord)`: same location adds up RRF (ensemble), different locations stay separate | Always separate (3.2 as written); always dedupe by path |
| N10 | Logged store id | Canonical `NAME@ABSROOT` in v5's `store` column | The alias or ref as typed; the store name only |
| N11 | Sources | Always explicit in config. A named store's default entries dir is `data-<name>/`. Overlap across stores is allowed; within a store it is an error | Implicit `data/` when there are no sources; `data/<name>/` (collides with the default store's dir entries); forbid overlap across stores |
| N12 | Write commands and fan-out | Single store only; fan-out is search only | `reindex --stores`; `add` to several stores |
| N13 | Store management commands | `kb init --name` + `kb stores` (list); delete with `rm -r` | `kb store add/rm/mv` in v1 |
| N14 | Fan-out limits | At most 8 extra stores (Usage above that); warn at more than 2 distinct models | No limit; a config key |
| N15 | Legacy layout | None (D8): no legacy marker, no adoption, no interim creation by `add`/`reindex` | Keep `.kb/kb.sqlite` discovery and adoption in `kb init` |
