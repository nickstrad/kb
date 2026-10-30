# Durable files migrate only through `kb migrate up`

> sqlite.Open creates a version-0 database and refuses an older one before writing anything; only Artefact.Apply upgrades; migrate.Run backs up through a .partial file; the harness makes old databases with its loadfixture helper.

**Fact.** (PLAN 1.8b, D10.) `Store.init` reads `user_version` first and calls `checkVersion(v, false)`:
newer is refused, `0 < v < target` is refused ("store database P is at schema vN; this kb needs vM.
Run: kb migrate up"), version 0 (a new file) is created at the target by `Store.migrate(ctx, false)`.
This check runs **before** `enableWAL`: a refused open used to switch an older file to WAL, which
changes the bytes of a database kb must leave alone (`TestRefusedOpenLeavesFileAlone`).
`Artefact.Apply` opens with `noMigrate` and calls `Store.migrate(ctx, true)`, the only path that
upgrades. There is no `Options.Create`: creation is implicit in a read-write open of a missing or
version-0 file (kb reindex and kb add rely on it), so `kb init` needed no switch.
`migrate.Run` writes the backup to `<dst>.partial` and renames it on success, and removes the partial
on any failure: an interrupted `VACUUM INTO` otherwise left an invalid "backup" that pruning would
count (found by harness `migrate-interrupt` on its first run).
The config and location files are `config.NewArtefact` / `NewLocationArtefact` (`internal/config/migrate.go`):
decode to `map[string]any`, run steps, re-check with Parse/ParseLocation, temp file + rename. A
missing file is version 0 with nothing pending (kb migrate never creates them); a file without
`version` reads as current, as Parse does.
`kb migrate` resolves the location without `config.ReadLocation` (strict), so an older kb.toml can
still be migrated; the cache is listed but never touched (`Info` items). It resolves `--store` with
the same `workspace.Resolver` as every command (NAME@LOC, PATH, aliases), only with
`Read = rawLocationInfo` (lenient); `TestMigrateScopeMatchesStoreRefs` checks it lists the store
`kb config --path` resolves (the W7 pass found it reading only the NAME). An older index is a
Conflict wrapping `sqlite.ErrSchemaOlder`: branch on `errors.Is`, never on "migrate up" text.
Tests that need an older database: `loadFixture(t, v)` then `NewArtefact(path).Apply`, or
`openAtVersion(t, path, v)` (which now upgrades an existing file with Apply, pinned to v); in the
cli package `installIndex(t, path, v)` loads the checked-in SQL fixture. The harness has no SQLite
driver, so `harness/loadfixture` (built next to kb) loads fixtures, fills rows, sets
`user_version` and checks integrity.

**Why it matters.** An agent's `kb search` can no longer migrate a store behind the user's back, and
a newer binary cannot silently pin a store.

**How to apply.** A new step (v5...): append to `migrations.go`, add its row to `TestIndexSteps` and
`TestStepEffects`, write the fixture with `-write-fixtures`; scenario `migrate-refusal` uses
`index-v2.sql` and needs no change. Doctor reports pending steps as `FAIL migrate` and does not open
the file. `kb stores` shows the refusal text in the store's row.

**Seen in:** internal/store/sqlite/sqlite.go, internal/migrate/run.go, internal/config/migrate.go, internal/cli/migrate_cmd.go, .agent/scripts/adversarial/harness/migrate_scenarios.go · 2026-10-30
