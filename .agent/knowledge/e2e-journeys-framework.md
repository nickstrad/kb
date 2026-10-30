# E2E journeys: in-repo cram-style runner, not testscript
> kb's E2E layer (L7, planned in PLAN Phase 7) uses a small stdlib runner over cram-style transcripts, a harnessfake binary, and a harnessfake-only clock env. testscript was rejected.

**Fact:**
- Journeys live in `internal/e2e/testdata/journeys/<slug>.txt`. Unindented lines are prose, `  $ kb …` is a command, and the indented lines after it are the output: stdout, then `[stderr] `-prefixed lines, then `[exit N]` when N ≠ 0. `...` and `~ RE` are elisions. `-- path --` sections are files placed in `$WORK`.
- The binary is built once per run with `-tags fts5,harnessfake` and uses the `fake:<dim>` model or `none`. The env is hermetic: HOME under `$WORK`, empty PATH, dead proxy, a no-weights guard.
- Time: `deps.now` is pinned via `KB_HARNESS_NOW` (harnessfake builds only). Other stamps (`sqlite.nowUTC`, migrate backup names, incident time) are normalised to `(time)`.
- `rogpeppe/go-internal/testscript` v1.14.1 (BSD-3, maintained) was evaluated and rejected. Its `UpdateScripts` works only for `cmp`, not `cmpenv`, so every path-bearing output needs a custom normalise-to-file step. Expected output sits apart from the command, so a script reads less like a transcript. It is also a new go.mod dependency. It is the fallback if the runner grows past about 800 lines.
- Observed with a harnessfake build (2026-10-30): `kb model pull` refuses `fake:8` ("unknown embedding model"), and `kb model list` does not show it. Real pulls therefore stay in the nightly `model` job. `kb init` with the default model offline exits 2 with "the store is created, but the model could not be loaded", and the warning includes the proxy error text, so journeys elide it with `...`.

**Why it matters:** E2E owns cross-command user flows only. The harness owns hostile, concurrent and interrupt cases, and lower layers own the logic. Copying the harness or unit claims into journeys makes them churn on every ranking change.

**How to apply:** Before writing E1, read docs/PLAN.md Phase 7 and docs/TESTING.md §11. Keep the normaliser list fixed. Assert volatile tables through `--json`. Update one journey at a time with `-update` and review the diff.

**Seen in:** docs/PLAN.md Phase 7, docs/TESTING.md §11, internal/cli/harnessfake.go, internal/cli/root.go (`deps.now`) · 2026-10-30
