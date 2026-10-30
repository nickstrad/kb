# Archived testability entries

Entries whose design problem is gone: the remedy landed, the code was removed, or something else
outranked it. They're kept as a record of what made tests hard, and how it was fixed, so the same
shape is recognised next time.

To archive: move the file here with `git mv` (or `mv` while it's untracked), set **Status** to
`resolved (<date>)` (or `retired (<reason>, <date>)` when outranked or removed), add a
**Resolution** line (what changed, and the PR, item or wave, plus the test that now shows it),
remove its line from `../index.md` and add it below. An archived entry that becomes relevant
again is moved back and re-reviewed.

## Archived entries

- [package-level-test-seams](package-level-test-seams.md) — resolved (2026-10-29) · `var newEmbedder`, `hubLoad` and a sh+sed `$EDITOR` were swapped by tests; now fields of `deps` and `local.puller`
- [reindex-phases-hooked-through-embedder](reindex-phases-hooked-through-embedder.md) — resolved (2026-10-30) · the Q22 race test hooked a second process into a fake `Embed`; now `Options.Discover` and a pure `pruneSet` (5.2)
- [search-explain-duplicated-setup](search-explain-duplicated-setup.md) — resolved (2026-10-30) · search and explain each carried their own mode/embedder/meta setup; now one `deps.openSearch`/`searchEmbedder` (Q63). `renderHits` still takes `store.Store` (PLAN 3.2)
- [sqlite-migrations-package-vars](sqlite-migrations-package-vars.md) — resolved (2026-10-30) · tests reassigned package-level `migrations`/`schemaVersion`, banning `t.Parallel`; now `Options.steps`/`Artefact.steps`
- [store-ref-resolution-copied](store-ref-resolution-copied.md) — resolved (2026-10-30) · `--store` grammar copied three times plus doctor's own kb.toml read; now one `resolveRefWith` taking the reader (3.2, Q80)
- [store-stamps-its-own-time](store-stamps-its-own-time.md) — resolved (2026-10-30) · sqlite stamped searches, verdicts and opens with `time.Now`; now the caller's clock (`Searcher.Now`, `deps.now`, `Feedback(..., at)`)
- [cli-shared-test-deps](cli-shared-test-deps.md) — resolved (2026-10-30) · one package-level `testDeps` and the process cwd made every CLI test serial; now a per-test `world` (own deps, home, cwd), 317 of 321 tests parallel
- [doctor-checks-print-text](doctor-checks-print-text.md) — resolved (2026-10-30) · doctor printed ok/warn/FAIL text per check; now it records `[]doctorCheck` and prints once, tests assert (Name, Level) as fields
- [fault-seams-only-in-sqlite](fault-seams-only-in-sqlite.md) — resolved (2026-10-30) · the fault driver was unexported test code inside the sqlite package; now `internal/sqlfault`, shared by the store and `cache.OpenWith`
