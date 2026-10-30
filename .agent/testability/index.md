# Testability index

Rules: README.md and AGENTS.md "testability/". About 10–15 active entries, ranked: cheapest
remedy for the most test pain first. Each line: status · one-line summary → suggested PLAN home.

1. [search-explain-duplicated-setup](search-explain-duplicated-setup.md) — needs work · search and explain each copy ~45 lines of mode/embedder/meta setup; each copy has a different untested branch → 3.2
2. [sqlite-migrations-package-vars](sqlite-migrations-package-vars.md) — needs work · tests reassign package-level `migrations`/`schemaVersion`, so the sqlite package bans `t.Parallel` → 3.2 / W7 wave-end
3. [store-stamps-its-own-time](store-stamps-its-own-time.md) — needs work · sqlite stamps rows with `time.Now`; tests `UPDATE searches SET ts` or allow slack; since-bound survivor unkillable → 3.2 / 2.3
4. [tests-read-sqlite-schema](tests-read-sqlite-schema.md) — needs work · 15 `.DB()` uses outside sqlite run raw SQL (even `ALTER TABLE` to fault the log), so migrations break cli/search tests → 2.3 + new item
5. [cli-shared-test-deps](cli-shared-test-deps.md) — needs work · one package-level `testDeps` (62 field swaps) and the process cwd (108 chdirs) make every CLI test serial → new item
6. [cli-outcomes-only-as-text](cli-outcomes-only-as-text.md) — needs work · CLI tests see only exit code + stderr: 195 substring checks vs 19 kind checks → new item (with 5)
7. [doctor-checks-print-text](doctor-checks-print-text.md) — needs work · doctor checks print ok/FAIL text directly; inline checks untestable (Q60) → with Q55/Q60
8. [fault-seams-only-in-sqlite](fault-seams-only-in-sqlite.md) — needs work · the fault driver is unexported test code; cache Open paths and vec full-disk untestable → new item
9. [harness-timing-and-model](harness-timing-and-model.md) — needs work · SIGINT scenarios timed by sleeps; no deterministic embedder for vector paths → H23/H14, 5.2
10. [duplicated-rule-tables](duplicated-rule-tables.md) — watch · reserved names, never-walked dirs and the cache path are copied between packages → 1.5 / W7 wave-end
11. [add-validation-interleaved-with-copy](add-validation-interleaved-with-copy.md) — watch · kb add's rules and "fails before copy" ordering are tested only end to end → 1.5 or new item
12. [test-doubles-break-error-contract](test-doubles-break-error-contract.md) — watch · fakes return bare errors production never returns (Q61, Q39) → Q61 + TESTING §7
13. [reindex-phases-hooked-through-embedder](reindex-phases-hooked-through-embedder.md) — watch · the Q22 race test plays a second process inside a fake Embed; no Discover seam → 5.2

## Archive

- [archive](archive/README.md) — resolved entries, with what made tests hard and how it was fixed
