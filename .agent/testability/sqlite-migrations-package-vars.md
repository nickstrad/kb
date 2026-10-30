# sqlite's migration list and schema version are package-level test seams
> Tests reassign `migrations` and `schemaVersion` to pin an older build's view, so the whole sqlite package bans `t.Parallel`.

- **Smell:** global/process state
- **Status:** needs work
- **Where:** `internal/store/sqlite/migrations.go` (`var migrations`, `var schemaVersion`: "A variable … so tests can pin an older build's view"), `sqlite.go:Options`, `artefact.go:Artefact`
- **Symptoms in tests:**
  - Four tests reassign both vars and restore them: `sqlite_test.go:TestMigrateRunsOnlyOutstandingMigrations`, `migrate3_test.go`, `migrations_test.go` (helper around line 503), `artefact_test.go` (around line 207).
  - `sqlite_test.go` carries the banner "NO TEST IN THIS PACKAGE MAY CALL t.Parallel: a parallel test opening a database while this one is running would be migrated against the wrong schema."
  - This breaks CODING_STYLE §5 ("no package-level mutable state"; only `vecOnce`, `Version` and immutable tables are allowed).
  - The fix pattern already exists: `config.tomlArtefact` carries `steps` as a field, and `embed/cache` tests never mutate its `migrations`.
- **Why it happens:** `Open` and `Artefact` read the package table directly, with no per-instance override.
- **Remedy:** give `Artefact` and the unexported `Options` a `steps migrate.Steps[execer]` (zero value = the package table, which becomes effectively immutable). Tests build `newArtefact(path, migrations[:v])`. → **3.2** (W8 adds mig v5 to `migrations.go`), or the W7 wave-end pass once the 1.8b→2.5 lane releases the file.
- **Cost/benefit:** small (one field threaded through `Open`/`Artefact`). It lifts the package-wide `t.Parallel` ban and removes a CODING_STYLE exception nobody approved.
- **Last reviewed:** 2026-10-30 · testability first pass (the 1.8b→2.5 lane was editing these files; re-check line refs)
