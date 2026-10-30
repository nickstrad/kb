# Testability index

Rules: README.md and AGENTS.md "testability/". About 10–15 active entries, ranked: cheapest
remedy for the most test pain first. Each line: status · one-line summary → suggested PLAN home.

1. [sqlite-migrations-package-vars](sqlite-migrations-package-vars.md) — needs work · tests reassign package-level `migrations`/`schemaVersion`, so the sqlite package bans `t.Parallel` → 3.2 / W7 wave-end
2. [store-ref-resolution-copied](store-ref-resolution-copied.md) — needs work · `--store` resolution exists three times (`resolveRef`, `resolveRefIn`, `migrateRefStore`) plus `isDefaultStore` reading kb.toml; the W7 High came from one copy → 3.2 (one resolver taking `Read`)
3. [store-stamps-its-own-time](store-stamps-its-own-time.md) — needs work · sqlite stamps rows with `time.Now`; tests `UPDATE searches SET ts` or allow slack; since-bound survivor unkillable → 3.2 / 2.3
4. [tests-read-sqlite-schema](tests-read-sqlite-schema.md) — needs work · 15 `.DB()` uses outside sqlite run raw SQL (even `ALTER TABLE` to fault the log), so migrations break cli/search tests → 2.3 + new item
5. [cli-shared-test-deps](cli-shared-test-deps.md) — needs work · one package-level `testDeps` (73 field swaps) and the process cwd (51 `t.Chdir`, 58 `useRoot`) make every CLI test serial → new item
6. [cli-outcomes-only-as-text](cli-outcomes-only-as-text.md) — needs work · CLI tests see only exit code + stderr: 244 output-text checks vs 17 kind checks → new item (with 5)
7. [doctor-checks-print-text](doctor-checks-print-text.md) — needs work · doctor checks print ok/FAIL text directly; inline checks untestable (Q60) → with Q55/Q60
8. [fault-seams-only-in-sqlite](fault-seams-only-in-sqlite.md) — needs work · the fault driver is unexported test code; cache Open paths and vec full-disk untestable → new item
9. [harness-timing-and-model](harness-timing-and-model.md) — needs work · SIGINT scenarios timed by sleeps; no deterministic embedder for vector paths → H23/H14, 5.2
10. [duplicated-rule-tables](duplicated-rule-tables.md) — watch · reserved names/dirs copied across packages (Q76, claimed); `cache.DefaultPath` still present, test-only → 1.5 / Q76
11. [add-validation-interleaved-with-copy](add-validation-interleaved-with-copy.md) — watch · kb add's rules and "fails before copy" ordering are tested only end to end → 1.5 or new item
12. [test-doubles-break-error-contract](test-doubles-break-error-contract.md) — watch · `errFakeCache` returns a bare error where production returns Usage (Q75); Q61, Q39 landed → Q75 + TESTING §7
13. [reindex-phases-hooked-through-embedder](reindex-phases-hooked-through-embedder.md) — watch · the Q22 race test plays a second process inside a fake Embed; no Discover seam → 5.2

## Archive

- [archive](archive/README.md) — resolved entries, with what made tests hard and how it was fixed
