---
name: testing-standards-pass
description: This skill should be used when the user asks for a "testing standards pass", "test pass", "testing pass", to "check my tests", "review test coverage", "are my tests at the right layer", "audit assertions", "check branch tables", "missing fuzz targets", "find duplicate tests", "is this tested properly", or wants kb code and tests (the current diff vs main, or a given path/package) reviewed against docs/TESTING.md: assertions, decision tables, fuzz/property targets, layer placement and duplication, contract suites, E2E journeys, hermeticity and coverage thresholds. Reports gaps as a table; does not fix them unless asked.
---

# Testing standards pass

**Source of truth:** `docs/TESTING.md`. Read §2 (layers and placement rules), §3.1 and §3.5 (assertions), §4.2 to §4.4 (decision-heavy list, coverage gates), §5.2 (properties), §6 (contract suites), §7 (determinism) and §10 (decision checklist) before every pass. Do not restate the rules here or in the report; cite them by section. Where assertions meet errors, `docs/CODING_STYLE.md` §1.7 also applies.

This skill **reports** findings. Do not edit code or tests unless the user explicitly asks.

## 1. Scope

- If the user gives no argument: the Go files changed against main, `git diff --name-only main...HEAD -- '*.go'` plus uncommitted `git diff --name-only -- '*.go'`. Review the changed hunks (`git diff main -U5`), and for each changed function, also read its tests.
- If the user gives a path or package, review every `.go` file in it, test files included.
- For each changed non-test function, find its tests: `rg -n '<FuncName>' <pkg>/*_test.go`.
- For check 15, also list changed journeys and user-facing surfaces: `git diff --name-only main -- internal/e2e/testdata/journeys README.md internal/cli/root.go`, plus new `cobra.Command`s in the diff (`git diff main -- internal/cli | rg '^\+.*&cobra.Command'`).

## 2. Checks

A grep hit is a **lead**, not a finding. Read the code before you report it.

| # | Check | How | docs/TESTING.md |
|---|---|---|---|
| 1 | Missing assertion | Function is in the §3.5 table (or is new and has a local invariant: ordering, bounds, uniqueness, hash/ord consistency) and has no `assert.Pre/Post/Invariant` | §3.5, §10 step 1 |
| 2 | Assertion misuse | `assert.` on flags, config, file contents, DB rows read back, embedder output or HTTP responses; or an expensive argument (function call, loop) outside `if assert.Enabled`; or an assertion with side effects | §3.1, §3.3 |
| 3 | `recover(` outside the top-level `report` and tests that prove a check fires | `rg -n 'recover\(' --glob '!*_test.go'` | §3.1 |
| 4 | Decision table gap | Changed function is on the §4.2 list, or is new with a decision of 2 or more conditions (`&&`, `\|\|`) that picks a mode, error or fallback. Report it if it lacks a table-driven test covering each branch outcome, or lacks the fuzz property where docs/TESTING.md lists one. Check the coverage profile for unrun branches. | §4.2, §4.3 |
| 5 | Missing property or seed | A changed function in the §5.2 table without its property; a new parser, sanitiser, merger or serialiser with no fuzz target; a bug fix in fuzzed code with no new `f.Add` seed or committed `testdata/fuzz/` crasher | §5.2, §5.3 |
| 6 | Weak property | A fuzz target that re-implements the algorithm as its oracle, or re-checks something an L1 assertion already checks | §5.1, §2.1 |
| 7 | Boundary or unreached gap | A limit, range edge or mode switch point (§2.1 "Every boundary is tested") with no table row or `f.Add` seed at, below and above it; or a line in fuzzed code that the fuzz targets don't reach and no unit test covers (read `go test -cover` output for the package) | §2.1, §10 step 3 |
| 8 | Duplication | The same claim checked at two layers: a fixed-input `Test` that repeats a fuzz property the fuzzer already reaches (should be a seed); a test that re-checks an assertion's condition; a CLI test that checks service logic; a suite case repeated in a backend's own tests | §2.1 |
| 9 | Wrong layer | A regression test above the layer that owns the root cause; integration tests beyond one success and one failure per command | §2.1, §10 |
| 10 | Contract wiring | A new storage backend or `Embedder` whose package has no test calling `storetest.Run` / `embedtest.Run`; backend-neutral behaviour tested only in one backend | §6 |
| 11 | Host tools | `rg -n 'exec.Command\|"sh", "-c"\|sed \|awk \|EDITOR' --glob '*_test.go'`. Tests must not depend on host tools (hygiene, not portability). | §7 |
| 12 | Nondeterminism | `http.Get`, real hosts or ports other than `httptest`; model loading without `//go:build model`; `time.Sleep`; exact duration asserts; unseeded `rand`; output that depends on map order; files outside `t.TempDir()` | §7 |
| 13 | Build tags | Test or command missing `-tags fts5`; a new tag not in the §8 table; behaviour that changes under `noassert` | §8 |
| 14 | Coverage | Run the §9 commands (below) and compare against the §4.4 floors | §4.4 |
| 15 | E2E journeys (TESTING §11; from PLAN E1, before that note it on the E-item) | (a) A new user-facing command, cross-command flow (a REF form, an upgrade step, a new log signal) or changed README workflow in the diff with no new or extended journey in `internal/e2e/testdata/journeys/`. (b) A journey that re-tests unit logic (asserts a ranking, clamp, validation table or metric that a lower layer owns), or carries hostile, concurrent or interrupt cases (harness). (c) A non-hermetic journey: a real model id or download, a host tool, a path outside `$WORK`, a dependency on another journey, an unpinned time window, or an output block that is mostly `...`. (d) A transcript diff from `-update` that the change under review does not explain. | §10 step 7, §11 |

## 3. Run

Run these in the repo root. They are safe and read-only, apart from `cover.out`.

```sh
go vet -tags fts5 ./...
go test -tags fts5 ./...                       # must pass first; report failures as High
go test -tags fts5,noassert ./...              # if internal/assert exists
make cover 2>/dev/null || go test -tags fts5 -cover ./...
make e2e 2>/dev/null || go test -tags fts5,e2e ./internal/e2e/   # once internal/e2e exists (PLAN E1)
```

- Do **not** run `-fuzz`, `make fuzz` or `make test-model` unless the user asks. They are long, or they need a model.
- Without `make cover`, read per-package percentages from `go test -cover` and compare them to the §4.4 floors (package 70%, total 80%, or the values in `.testcoverage.yml` if present). Measure each package with its own tests; do not use `-coverpkg`.

## 4. Severity

- **High**: failing tests (journeys included); an assertion on user input; a §4.2 decision with no table test; a host-tool or network dependency; a backend or embedder not wired to its contract suite; a package below its coverage floor; a test that passes vacuously (it passes because of the failure it should detect).
- **Medium**: missing §3.5 assertion; decision table missing a branch outcome; untested boundary or fuzz-unreached line with no unit test (check 7); missing §5.2 property or regression seed; duplication across layers; wrong layer; a new user-facing command or flow with no journey, or a journey that re-tests unit logic (check 15).
- **Low**: weak property, message format, an unguarded but cheap assertion argument, a table whose row names do not say which outcome they prove.

## 5. Report format

Start with one line: the scope, the file count, and test status (pass/fail, with assertions on and off). Then give a table sorted by severity:

| Sev | Location | docs/TESTING.md | Gap | Suggested test / assertion |
|---|---|---|---|---|
| High | internal/cli/p44_test.go:52 | §7 | `EDITOR=sed -i …` uses GNU sed syntax; fails with macOS sed | inject `deps.editor` with a Go func that rewrites the file |
| Medium | internal/search/search.go:613 | §4.3 | fallback decision covered by 3 scattered tests; no row for the hook-says-no outcome | `TestSearchEmbedFailureDecision`, five rows |

- Location is `file:line` for code findings, or the package for coverage and contract findings.
- One row per finding. Merge identical patterns in one file into one row.
- The suggestion is one sentence or a short snippet: the test name and its rows, or the exact `assert.` line.
- After the table, write at most three lines: coverage per package against the floors, counts by severity, and any systemic gap (for example, "`internal/assert` does not exist yet, so all §3.5 rows are open").
- If nothing is found, say so and list the checks you ran.
- If asked to fix: High first. Put each fix at the layer §10 assigns, and rerun the §3 commands.
