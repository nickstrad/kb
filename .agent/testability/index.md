# Testability index

Rules: README.md and AGENTS.md "testability/". About 10–15 active entries, ranked: cheapest
remedy for the most test pain first. Each line: status · one-line summary → suggested PLAN home.

1. [tests-read-sqlite-schema](tests-read-sqlite-schema.md) — needs work · 14 `.DB()` uses outside sqlite run raw SQL (even `ALTER TABLE` to fault the log); the read side (`store.JudgedLog`) landed in W8 → T1 step 5
2. [cli-shared-test-deps](cli-shared-test-deps.md) — needs work · one package-level `testDeps` (96 field swaps) and the process cwd (54 `t.Chdir`, 81 `useRoot`) make every CLI test serial → new item
3. [cli-outcomes-only-as-text](cli-outcomes-only-as-text.md) — needs work · CLI tests see only exit code + stderr: 397 output-text checks vs 26 kind checks → new item (with 5)
4. [doctor-checks-print-text](doctor-checks-print-text.md) — needs work · doctor checks print ok/FAIL text directly; inline checks untestable (Q60) → with Q55/Q60
5. [fault-seams-only-in-sqlite](fault-seams-only-in-sqlite.md) — needs work · the fault driver is unexported test code; cache Open paths, vec full-disk and row-scan errors (Q125) untestable → new item
6. [harness-timing-and-model](harness-timing-and-model.md) — needs work · SIGINT scenarios timed by sleeps (the `fake:<dim>` model landed in W8; H2/H14/H18 now writable) → H23, 5.2
7. [duplicated-rule-tables](duplicated-rule-tables.md) — watch · Q76 landed (one reserved-name table in config, `cache.DefaultPath` gone); left: an add-accepts ⇒ Discover-finds property → 1.5 follow-up
8. [add-validation-interleaved-with-copy](add-validation-interleaved-with-copy.md) — watch · kb add's rules and "fails before copy" ordering are tested only end to end → 1.5 or new item
9. [test-doubles-break-error-contract](test-doubles-break-error-contract.md) — watch · `errFakeCache` returns a bare error where production returns Usage (Q75); Q61, Q39 landed → Q75 + TESTING §7
10. [binary-clock-not-pinnable](binary-clock-not-pinnable.md) — partly resolved · `KB_HARNESS_NOW` (E1) pins search/open/feedback stamps; `kb stale` (5.4) uses it; `indexed_at`, backup names and `kb index` dates still read the wall clock → Q96

## Archive

- [archive](archive/README.md) — resolved entries, with what made tests hard and how it was fixed
