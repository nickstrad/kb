# The index schema is one frozen baseline; "older store" tests use a probe build

> Pre-release index steps v1–v9 were squashed into `index/0001-baseline` (schema.sql, v1), frozen by a semantic schema golden; tests that need an older store run kb one release ahead with `sqlite.ProbeSteps`.

**Fact.** `internal/store/sqlite/schema.sql` is the full index schema (the old v9, minus `chunks_vec`,
which `EnsureEmbedMeta` creates at the model's width), applied by the only step
`index/0001-baseline`; `SchemaVersion()` is 1. `TestBaselineSchemaGolden` compares a fresh store's
semantic schema (`semanticSchema`: `pragma_table_list`/`table_xinfo`/`foreign_key_list`/
`index_list`/`index_xinfo`, CHECK expressions from the SQL, normalised virtual-table and index SQL)
with `testdata/schema-baseline.golden`. The golden was written from the old 9-step chain before the
squash, so it is the equivalence proof. `sqlite_schema` text cannot be compared across a squash:
ALTER TABLE ADD/RENAME rewrites the stored CREATE text (appended columns, quoted renamed names).
With no real older schema, `sqlite.ProbeSteps()` = this build's steps + `index/<N+1>-probe`
(empty table `probe`, every `body_hash` cleared, Effect reindex). It reaches a store through
`sqlite.Options.Steps`, `sqlite.NewArtefactSteps`, the CLI's `deps.indexSteps` (tests:
`useProbeBuild`, `installOlderIndex`, `buildTarget`) and `KB_HARNESS_INDEX_PROBE=1` in a harnessfake
binary (e2e `upgrade` journey, harness `loadOld`/`h.probe`). The newest fixture is then "one
release older".

**Why it matters.** Editing `schema.sql` would change only new stores and silently fork the schema;
the golden fails instead. The probe keeps the refusal, `kb migrate up`, backup, effect and
interrupt paths tested end to end while there is only one real step, and it keeps working after
real steps land.

**How to apply.** Never edit `schema.sql` or the golden. A schema change is `index/0002-<slug>`:
follow the checklist at the top of `migrations_test.go` (effect row, `stepEffectChecks` entry, seed
and `-write-fixtures` for v2). Under a probe build, every store a test creates must be created by
the probe build too (`sqlite.Options{Steps: depsFor(t).indexSteps}`), or it reads as older; the
harness common setup's default store must be deleted before a probe scenario re-inits it.

**Seen in:** internal/store/sqlite/{schema.sql,migrations.go,schema_test.go,migrations_test.go}, internal/cli/root.go, internal/cli/harnessfake.go, internal/e2e/testdata/journeys/upgrade.txt, .agent/scripts/adversarial/harness/{migrate,fanout}_scenarios.go · 2026-10-30
