---
name: adversarial-testing
description: This skill should be used when the user asks for "adversarial testing", "mutation testing", to "test the tests", "attack the CLI", "run the harness", "find gaps the tests miss", "break kb", "are my tests strong enough", "check survivors", "flaky tests", or an "adversarial coverage verdict" for a wave or diff. Runs kb's sandboxed mutation tester, hostile CLI harness and test-validation helpers against a diff or package, triages survivors and failures to the TESTING.md layer that should own them, and reports a table. Reports by default; fixes only when asked.
---

# Adversarial testing

**Source of truth:** `docs/ADVERSARIAL_TESTING.md`: tools (§3), coverage verdict (§4), finding → test (§5), safety (§2). Read §2–§5 before a run. For layer placement, cite `docs/TESTING.md` §2.1 and §10 rather than restating them. Tool flags: `.agent/scripts/adversarial/README.md`.

This skill **reports**. Do not edit code or tests unless the user explicitly asks.

## Safety (always)

- Every tool works in a sandbox from `.agent/scripts/sandbox.sh` (under `.agent/sandbox/`, timestamped, removed on exit). Never run a mutating tool against the live tree, never copy files back from a sandbox, never use `~/knowledge`.
- No network and no model downloads; the tools enforce this, so don't work around them.
- Before you finish, `bash .agent/scripts/sandbox.sh list` must show none of your sandboxes (`sandbox.sh rm <name>`).

## 1. Scope

- **No argument:** the diff against main. `git diff --name-only main...HEAD -- '*.go'` plus uncommitted `git diff --name-only -- '*.go'`. Group by package; non-test files only for mutation.
- **A package or path:** that package.
- **"coverage verdict" / wave-end:** the wave's items from `docs/PLAN.md` §6 and their files. Use mode B.

## 2. Mode A: find gaps (default)

1. For each changed package, run mutate on the changed lines (whole package when asked or when the package is new):
   `go run ./.agent/scripts/adversarial/mutate -pkg ./internal/<pkg> -diff main -j 4`
2. If any limits or constants changed: `go run ./.agent/scripts/adversarial/boundaries internal/<pkg>`.
3. If a command, flag, exit code, schema, locking or paths changed: run the harness, filtered with `-run` to the scenarios mapped in ADVERSARIAL_TESTING.md §4 (or all of them; it takes about 5 s).
   `go run ./.agent/scripts/adversarial/harness [-run 'regexp']`
4. Optionally: `bash .agent/scripts/adversarial/coverage-gaps.sh internal/<pkg>` for fuzzed packages; `bash .agent/scripts/adversarial/flaky.sh -n 10 internal/<pkg>` when concurrency changed.

## 3. Mode B: coverage verdict (wave-end pass)

Follow ADVERSARIAL_TESTING.md §4. For each item, list what changed (packages, commands, flags, exit codes, parsers, integration points). Map each to an operator or scenario and run them. Give one verdict per item:

| Item | Changed | Probed by (result) | Verdict |
|---|---|---|---|
| 0.5a | `kb model pull`, `--embedder` | harness `exit-codes`, `no-model-index` (FAIL: exit 1) | gaps: H1, H2 |

Then append each gap to `.agent/queue/harness-improvements.md` as `H<n>` (re-read the file first; the format is in its header).

## 4. Triage

A survivor or failure is a **lead**. Read the code before you report it.

- **Survivor:** decide which it is: a missing assertion on a result, an untested error path, a missing boundary row, a weak fuzz property or missing seed, an equivalent mutant, or a symbol-relative constant (ADVERSARIAL_TESTING.md §3.1 table). Equivalent mutants are listed in the report as such, not filed.
- **Harness FAIL:** a product bug, or a stale expectation in the scenario? Reproduce by hand with `-keep` and the binary at `<sandbox>/_adv/kb` (use the same env: `HOME` in the run dir, and `--root <run>/root`). Find the root-cause layer (TESTING.md §10). The suggested test goes there, not in the CLI.
- **Harness note (`·`):** report it only if it needs an owner decision (a DEFAULTS.md candidate).

## 5. Severity

- **High:** data loss or corruption; a write to a file outside the root; a panic, an internal error (exit 3) or an exit code outside 0/1/2/3; a download or network use; concurrent kb processes failing; a survivor in a §4.2 decision-heavy function.
- **Medium:** a survivor on a §2.1 boundary or on an error path the user can trigger; a contract the harness shows broken (wrong exit code, an unfriendly error); a fuzz property that a mutant survives.
- **Low:** a survivor on a message or rounding detail; an untested defensive branch; an unfriendly but correct message.

## 6. Report

First line: the scope, the tools run, mutation score per package (killed/survived/compile-error), and the harness result (passed/failed). Then a table sorted by severity:

| Sev | Location | Tool | Finding | Suggested test |
|---|---|---|---|---|
| High | internal/store/entries.go:175 | harness `concurrent-fresh-db` | concurrent `kb add` gets `database is locked`: deferred tx bypasses busy_timeout | store test: two Stores write concurrently; fix with `_txlock=immediate` |
| Medium | internal/chunk/chunk.go:319 | mutate `loop-drop` | fence isolation in paragraphBlocks untested | splitOversized row: oversized section with a fence holding blank lines |

- Location is `file:line` (for a scenario: the code the evidence points to, or the scenario name).
- One row per finding. Merge identical survivors of one function into one row.
- After the table, at most three lines: equivalent mutants seen, counts by severity, sandboxes removed.
- Low findings go to `.agent/queue/low-findings.md` (`Q<n>` format in its header); tool ideas go to `harness-improvements.md`.
- **If asked to fix:** High first, each at the layer TESTING.md §10 assigns. Then rerun the tool to show the mutant is killed or the scenario passes.
