# Schema migrations
> Migrations run automatically on any open and are one-way; the search log they carry is the only irreplaceable data in a store.

- **Category:** data integrity
- **Status:** watch
- **Where:** `internal/store/sqlite/migrations.go` (`migrations`), `internal/store/sqlite/sqlite.go` (`migrate` under BEGIN IMMEDIATE, `checkVersion`: the one version check, also for `Options.ReadOnly`); planned v4–v8 (docs/PLAN.md §2.5).
- **Why it matters:** a bad migration silently corrupts or drops history on every user's next command, including read-only ones. Old binaries refuse upgraded databases.
- **Evidence:** v2 (column rename) and v3 (vector table drop) have tests that compare every search-log row before and after (`TestMigration2KeepsSearchLog`, `migrate3_test.go`). `kb stats`, which opens read-only and cannot migrate, accepted a newer schema (Q19); fixed in the W2 pass: `stats.Open` refuses any user_version but `store.SchemaVersion()` (older ones with "run kb doctor once to upgrade"). Harness `newer-schema` passes for search, reindex, doctor, stats and feedback. 1.2 (W3): `sqlite.Open(ctx, path, Options{ReadOnly})` is now the only version check (stats uses it; `stats.Open`'s own check is gone); a failed migration rolls back and the next open retries (`TestOpenFaults`, fault driver, incl. `PRAGMA user_version = 1` and COMMIT).
- **Coverage today:** per-migration fixture tests (`migrate3_test.go`); `readonly_test.go` (read-only at/±1 versions, missing file NotFound); `TestOpenFaults` (every migration step failing); harness `newer-schema`, `corrupt-db` (reindex only). W3 mutate on `migrate`: only message-text and post-error sentinel survivors. Not covered black-box: the upgraded-but-not-reindexed v3 state (H18).
- **Next checks:** harness fixture of a v2 database upgraded by the binary (H18, reachable without a model); a single "replay every migration from v1 fixtures with realistic data" test that each new migration extends; Ctrl-C during a migration (fault-driver test can cancel at `CREATE TABLE`); a backup-before-migrate option (`.kb/kb.sqlite.bak-vN`) as a design decision before v4.
- **Last reviewed:** 2026-10-30 · W3 wave-end pass
