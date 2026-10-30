# Migrations as a first-class part of kb

Status: **accepted** (2026-10-30). Folded into docs/PLAN.md as items 1.8a and 1.8b and into the §2.5 table. Sibling of [named-stores.md](named-stores.md): a "store" and a "location" are the terms defined there.

## 1. What is versioned today

| Artefact | Version lives in | Steps | On open today |
|---|---|---|---|
| Store index DB (`stores/<name>/kb.sqlite`) | `PRAGMA user_version` | `internal/store/sqlite/migrations.go`: `[]func(execer) error`, v1–v3 | Read-write opens migrate automatically, all pending steps in one `BEGIN IMMEDIATE` with the version re-read under the lock. Read-only opens refuse an older DB. Every open refuses a newer one. |
| Embedding cache (`<user cache>/kb/embed-cache.sqlite`) | `PRAGMA user_version` | Inline schema v1 in `cache.go` `init` | Creates the file; a newer schema gives `ErrNewerSchema`, which callers treat as "no cache" |
| Store config (`stores/<name>/config.toml`) | `version` key | none (v1) | Any version other than 1 is a Usage error |
| Location file (`.kb/kb.toml`, named stores 1.7b) | `version` key | none (v1) | (new) |

Each artefact has its own version check, error text and test style. There is no backup, no `kb` command that shows versions, and no record of which steps need a reindex. The only signal is v3 clearing `body_hash`, which doctor notices.

## 2. Design

### 2.1 Package `internal/migrate` (no SQL, no TOML)

The consumer is the runner and the `kb migrate` command, so the interface is defined there (CODING_STYLE §2, "consumer-defined interfaces"). Each artefact package implements it in a small adapter file.

```go
package migrate

// Effect is what a step leaves for the user to do after it runs.
type Effect uint8
const (
	EffectNone    Effect = iota
	EffectReindex        // derived rows were cleared; a plain `kb reindex` rebuilds them (embeddings reused)
	EffectReembed        // vectors were dropped; `kb reindex` re-embeds (model needed)
)

// Info describes one step. ID is stable forever: "<artefact>/<NNNN>-<slug>", e.g. "index/0003-vec-width".
type Info struct {
	From   int    // the step takes the artefact from version From to From+1
	ID     string
	Desc   string // one line, shown by kb migrate status
	Effect Effect
}

// Step is one forward migration over a backend-specific handle T
// (an execer for SQLite, a map[string]any tree for TOML).
type Step[T any] struct {
	Info
	Up func(ctx context.Context, h T) error
}

// Steps is an append-only list: Steps[i] takes version i to i+1. Check verifies From == i and that
// IDs are unique and well-formed; every artefact calls it in a test (and in an init-time assert).
type Steps[T any] []Step[T]
func (s Steps[T]) Target() int
func (s Steps[T]) Pending(current int) []Info
func (s Steps[T]) Check(artefact string) error

// Status is one artefact's position.
type Status struct {
	Artefact string // "index", "embed-cache", "config", "location"
	Path     string
	Current  int    // 0 = does not exist yet
	Target   int
	Pending  []Info // empty when Current >= Target
}
func (s Status) Newer() bool // Current > Target: written by a newer kb; nothing may touch it

// Artefact is anything kb versions. Implementations: sqlite.Store (index), cache.Cache,
// config.File (store config), config.LocationFile.
type Artefact interface {
	Status(ctx context.Context) (Status, error)
	// Backup writes a consistent copy to dst (sqlite: VACUUM INTO; toml: copy). Called before Apply.
	Backup(ctx context.Context, dst string) error
	// Apply runs every pending step. Atomic where the backend allows (all steps in one transaction,
	// version stamped inside it); otherwise per step, stamping after each, so a rerun resumes.
	Apply(ctx context.Context) ([]Info, error)
}

// Verifier is optional: an artefact that can check itself after Apply (and for kb migrate verify).
type Verifier interface{ Verify(ctx context.Context) error }

// Run applies pending steps to each artefact in order: lock, re-read status, backup, apply, verify.
func Run(ctx context.Context, arts []Artefact, o Options) (Report, error)
type Options struct {
	DryRun   bool          // report the plan only
	NoBackup bool
	LockWait time.Duration // how long to wait for another migrator (default 10s)
	Now      func() time.Time
}
```

There is no `Down`. Migrations are **forward-only**, and the backup is the rollback (decision M3).

### 2.2 When migrations run (decision M1)

The rule depends on whether losing the artefact costs anything:

| Artefact | Policy | Why |
|---|---|---|
| **Fresh artefact** (version 0: `kb init`, first cache open) | Created at the target automatically | Nothing to lose; this is creation, not migration |
| **Embedding cache** | **Automatic** on open, with no backup | It is disposable. Any failure already degrades to "no cache", and `kb model cache --clear` rebuilds it. Making the user migrate a cache on every machine would only cost speed |
| **Store index DB, store config, location file** | **Explicit**: `kb migrate up` | They hold the search log (the only irreplaceable data) or user-written settings |

For an explicit artefact at an older version, every command except `kb migrate`, `kb doctor`, `kb stores`, `kb config --path` and `kb version` refuses with a Conflict (exit 1):

```
kb search: store "default" (/p/.kb/stores/default/kb.sqlite) is at schema v3; this kb needs v5.
  Run: kb migrate up            (backs up to kb.sqlite.bak-v3-20261030-1612 first)
```

Why explicit, for a pre-v1 tool spread over many projects and machines:

- **Agents run kb.** An automatic migration would fire inside an agent's `kb search`, often with several agents racing, and might quietly leave a store needing a reindex. An explicit error is loud, cheap and tells the user what to do.
- **A newer binary should not silently pin a store.** Once a store is migrated, older binaries refuse it. With stores on several machines (synced folders, or a repo checked out on two machines with an older kb on one), an automatic upgrade breaks the other machine without warning.
- **The backup and the `needs reindex` report need a moment where the user is watching.** `kb migrate up` is that moment.
- The cost is one command per store after an upgrade. `kb migrate up --all` covers every store of the location plus the global location. A loop over projects is a shell one-liner, which the README documents.

Rejected:

- automatic on every open (today's behaviour);
- automatic with a backup;
- a per-location `auto_migrate` switch (deferred: it can be added later without changing the design).

### 2.3 `kb migrate`

| Command | Behaviour |
|---|---|
| `kb migrate status [--store REF \| --all] [--json]` | One row per artefact: store, artefact, path, current → target, and the pending IDs with descriptions and effects. Exit 0 when everything is current; exit 1 when anything is pending or newer, so scripts can test for it. |
| `kb migrate up [--store REF \| --all] [--dry-run] [--no-backup]` | For each artefact with pending steps: lock, backup, apply, verify, then report. It ends with the combined effects, e.g. `needs: kb reindex (store default: index/0007-edges)`. A dry run prints the plan and the backup paths it would write. |
| `kb migrate verify [--store REF \| --all]` | Runs each `Verifier` (sqlite: `integrity_check`, `foreign_key_check`, and the schema compared with a fresh database at the same version). Exit 1 on drift. |

Scope:

- `--all` means every store of the resolved location, the location file, and the global location.
- The default scope is the resolved store plus the location file.
- The embedding cache appears in `status` for information; `up` never needs to touch it.

### 2.4 Mechanics

- **Backup:** `<file>.bak-v<N>-<YYYYMMDD-HHMMSS>` beside the file. SQLite uses `VACUUM INTO`, which gives a consistent snapshot without stopping readers; TOML uses a byte copy. The newest 3 backups per artefact are kept and older ones deleted. `.kb/.gitignore` gains `*.bak-v*`. If the backup fails, nothing is applied.
- **Concurrency:** `migrate.Run` takes an advisory `flock` on `<StoreDir>/migrate.lock` (location file: `.kb/migrate.lock`) for the whole lock, backup, apply and verify sequence. A second `kb migrate up` waits up to `LockWait`, re-reads the status, and finds nothing to do. Otherwise it reports Conflict "another kb is migrating …". Inside Apply, SQLite keeps its existing `BEGIN IMMEDIATE` with the version re-read under the lock. Ordinary commands never take the flock. They read the version, and while a migration is running they see the old version and refuse with the message above, which is correct.
- **Ctrl-C:** SQLite DDL is transactional, so all pending steps of a store run in one transaction (as today). A cancel rolls back to the starting version and the backup stays. TOML artefacts write a temp file and rename it. A backend without transactional DDL (a future DuckDB/Turso adapter) stamps the version after each step, so a rerun resumes. The interface doc states which mode an adapter uses.
- **Newer than the binary:** `Status.Newer()` makes every command, `kb migrate up` included, refuse with "written by a newer kb (vN > vM); upgrade kb". The file is never touched. (This keeps today's rule; the embedding cache keeps degrading to no cache.)
- **Data migrations:** a SQL backfill (`json_each` tags, `chunks_exact` from `chunks`) runs inside `Up`. Work that needs the model or the corpus never runs in `Up`. Instead the step clears the derived state and declares `EffectReindex` or `EffectReembed`. `kb migrate up` prints the combined need, and `kb doctor` keeps detecting the cleared state (empty `body_hash`, missing `embed_meta`) after a restart. There is no new "needs" table.
- **TOML artefacts:** steps transform the decoded `map[string]any` tree, and the result is written back with `config.Save`. Comments are lost; the report says so, and the backup keeps the original. `config.Parse` reads `version` **before** the strict decode, so an older file reports "run kb migrate up" and not "unknown key". The W5 pass makes this ordering change; 1.8b only checks that it is there.

### 2.5 Authoring rules (become CODING_STYLE §6.x and TESTING §x via item 1.8a)

1. **Append-only.** Never edit or reorder a released step or `schema.sql`. A fix is a new step.
2. Every step has a stable `ID` (`<artefact>/<NNNN>-<slug>`), a one-line `Desc` and an `Effect`. `Steps.Check` enforces the shape.
3. A step uses only its handle: SQL text, or the map tree. It never calls kb's current Go code (for example `chunk.Split`), because a later change to that code would silently change an old migration. It never begins or commits a transaction, and never touches the network or the model.
4. **Every search-log table is preserved.** A step that alters one has a test that compares every row before and after.
5. **Tests per step:** a fixture at version `From` with realistic rows (built by `testdata/mkfixture` applying steps `0..From-1` and inserting rows; checked in as SQL text, not as binary). The test asserts the step's effect and that the log is preserved.
6. **Replay test (one per artefact):**
   - steps `0..N` applied to an empty artefact give the same normalised `sqlite_schema` as creation at the target;
   - applying every step to the oldest fixture gives a DB that passes `Verify`;
   - every new step must keep both green.
7. **Effects are declared honestly.** A test table lists `(step ID, Effect)`, and a reviewer checks any step that drops or clears derived data.
8. **Migration numbers are pre-assigned** (PLAN §6.3), and no agent adds a step that is not in that table.

At the v1 tag (decision M6), all pre-v1 steps of each artefact are squashed into one baseline (index v1 = the schema at the time), so v1 users start from a single fixture. Until then steps accumulate as planned (v4–v8).

## 3. Leverage change

`.agent/leverage/schema-migrations.md` changes as follows.

- **Where:** `internal/migrate` plus the four adapters.
- **The weakness moves:**
  - from "silent automatic one-way upgrade on any command, including an agent's search";
  - to "the lock, backup and apply sequence is correct, the refusal path is clear, and declared effects match reality".
- **New next checks:**
  - harness `migrate-concurrent` (two `kb migrate up` processes; one wins, the other is a no-op);
  - `migrate-interrupt` (SIGINT during apply rolls back, the backup exists, and a rerun succeeds);
  - `migrate-refusal` (an old fixture plus `kb search` gives exit 1 and the hint; `kb migrate up` then succeeds and search works);
  - a backup restore round trip;
  - `verify` schema-drift detection;
  - backup disk use on large stores.

`embedding-cache-shared-file` is unchanged: the cache stays automatic.

## 4. Plan placement

- **1.8a (W6), framework and adapters, no change in behaviour:**
  - add `internal/migrate`;
  - move the sqlite index and the cache onto `migrate.Steps` with IDs and effects (v1–v3);
  - add Status/Backup/Verify to both;
  - add the replay and fixture test harness;
  - add CODING_STYLE/TESTING text.

  Opens still migrate automatically. Hotspots: `sqlite/sqlite.go`, `sqlite/migrations.go` and `embed/cache/cache.go`; nobody else owns them in W6 (2.1 owns `sqlite/retrieve.go` only).
- **1.8b (W7), the policy switch and the command:**
  - explicit migration for durable artefacts, with the refusal message;
  - `kb migrate status|up|verify`;
  - adapters for the config and location files;
  - the flock and the backups.

  It needs 1.7b (stores and location) and lands **before 2.5's v4** in the same wave. 2.5's step is written as a `migrate.Step` from the start.
- §2.5 table: gains `ID` and `Effect` columns. v3 = `EffectReembed`, v7 = `EffectReindex`, all others `EffectNone`. v5's `store` column holds the canonical `NAME@ABSROOT` (named-stores.md §4).

## 5. Decisions

| # | Decision | Chosen | Rejected |
|---|---|---|---|
| M1 | When to migrate | Explicit `kb migrate up` for the index DB, store config and location file; automatic for the embedding cache and for fresh artefacts | Automatic on every open (today); automatic with a backup; an `auto_migrate` switch now |
| M2 | Interface shape | `migrate.Artefact{Status, Backup, Apply}` + optional `Verifier`; generic `Steps[T]` with `Info{From, ID, Desc, Effect}`; defined in `internal/migrate` | One interface with `Current/Target/Pending/Apply` per artefact and no step type; a migration library (goose, golang-migrate: heavy, file-based SQL, no TOML) |
| M3 | Reversibility | Forward-only; an automatic backup is the rollback | Down steps (double the tests; SQLite ALTER limits) |
| M4 | Backup | `<file>.bak-v<N>-<ts>` beside the file, `VACUUM INTO` for SQLite, keep the newest 3, `--no-backup` to skip | No backup; one backup dir per location; keep all |
| M5 | Concurrency | `flock` on `migrate.lock` around the whole run, plus `BEGIN IMMEDIATE` inside; the loser waits 10 s, then no-op or Conflict | Rely on `BEGIN IMMEDIATE` alone (the backup would be outside the lock) |
| M6 | Pre-v1 history | Keep v1–v8 as planned; squash into one baseline at the v1 tag | Squash now (mid-plan churn, pre-assigned numbers); never squash |
| M7 | Data migrations | SQL backfills inside `Up`; anything that needs the model or corpus becomes a declared `Effect` (reindex/reembed), detected later by doctor | A persisted "needs" table; running reindex inside migrate |
| M8 | TOML migrations | Transform the map tree, rewrite with `config.Save` (comments lost, backup kept); read `version` before the strict decode | Text-preserving edits (fragile); no config migrations (a breaking config change would need manual edits) |
