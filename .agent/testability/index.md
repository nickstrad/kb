# Testability index

Rules: README.md and AGENTS.md "testability/". About 10–15 active entries, ranked: cheapest
remedy for the most test pain first. Each line: status · one-line summary → suggested PLAN home.

1. [tests-read-sqlite-schema](tests-read-sqlite-schema.md) — partly resolved · `.DB()` outside sqlite is down to 1 (stats fixture); the door is `sqlitetest`; search log tests still run raw SQL through it → T1 follow-up
2. [cli-outcomes-only-as-text](cli-outcomes-only-as-text.md) — partly resolved · `executeErr`/`runErr`/`wantKind` landed; 436 text checks vs 32 kind checks, migrate as files are touched → T1 follow-up
3. [harness-timing-and-model](harness-timing-and-model.md) — partly resolved · reindex SIGINT waits on `--debug` progress lines; migrate SIGINT scenarios still sleep → H23
4. [duplicated-rule-tables](duplicated-rule-tables.md) — watch · Q76 landed (one reserved-name table in config, `cache.DefaultPath` gone); left: an add-accepts ⇒ Discover-finds property → 1.5 follow-up
5. [add-validation-interleaved-with-copy](add-validation-interleaved-with-copy.md) — watch · kb add's rules and "fails before copy" ordering are tested only end to end → 1.5 or new item
6. [test-doubles-break-error-contract](test-doubles-break-error-contract.md) — watch · `errFakeCache` returns a bare error where production returns Usage (Q75); Q61, Q39 landed → Q75 + TESTING §7
7. [binary-clock-not-pinnable](binary-clock-not-pinnable.md) — partly resolved · `KB_HARNESS_NOW` (E1) pins search/open/feedback stamps; `kb stale` (5.4) uses it; `indexed_at`, backup names and `kb index` dates still read the wall clock → Q96

## Archive

- [archive](archive/README.md) — resolved entries, with what made tests hard and how it was fixed
