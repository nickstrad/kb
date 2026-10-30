---
name: testability-check
description: This skill should be used when the user asks for a "testability check", "testability pass", "what's hard to test", "why is this hard to test", "brittle tests", "flaky or fragile tests because of the design", "coupling", "test seams", "is this code testable", to "validate the testability folder", "should this testability entry be archived", or at every PLAN wave-end pass. Validates .agent/testability/ entries against the current code (still true? remedy landed → archive), finds new candidates in a diff or package (package-level seams, wall clock/env/rand without injection, handlers mixing decisions with I/O, tests using t.Chdir/sleep/host tools, tests asserting human text for logic, raw-schema test setup, "unreachable" mutation survivors), and reports a table. Edits entries only when asked or as part of a wave-end pass.
---

# Testability check

**Source of truth:** `.agent/testability/README.md` (what an entry is, the format, the 10–15
cap, rules, lifecycle), `.agent/testability/archive/README.md` (how to archive), docs/TESTING.md
§1 and §7 (hermetic tests, doubles, determinism), docs/CODING_STYLE.md §2, §4 and §5 (consumer
interfaces, ctx, `deps` instead of package state) and §12 (designing for testability: domain vs
infrastructure, controllability, observability, field vs parameter, cohesion and coupling). Cite
them; don't restate them. Each finding names the §12 rule it breaks (README's smell table).

**Default mode is report only.** Edit, archive or add entries only if the user asks, or when you
run as part of a wave-end pass (docs/PLAN.md §6.4 rule 4).

## 1. Scope

- Every active entry in `.agent/testability/index.md`.
- Code changed since each entry's **Last reviewed** date: `git log --since=<date> --name-only --
  <paths in Where>`, plus the uncommitted `git diff --name-only`.
- For candidate discovery: the diff against main, or the package the user names.

## 2. Validate each active entry

A grep hit is a lead. Read the code and the tests before you conclude anything.

| # | Check | How |
|---|---|---|
| 1 | **Where resolves** | Every file and symbol in **Where** exists (`rg -n '<Symbol>'`). If one was renamed, follow it. If it was removed, the entry may be resolved. |
| 2 | **Smell still present** | Re-read the code. Did the remedy (or part of it) land: a field instead of a package var, an injected clock, a pure function split out? |
| 3 | **Symptoms still true** | Re-count the cited evidence (`rg -c`): test names, workaround code, swaps, chdirs, substring checks. Queue ids still open? Survivors still survive (`mutate -pkg`, sandboxed)? |
| 4 | **Remedy home still right** | Has the named PLAN item landed, moved wave, or dropped the files? Name the next item that touches the area. |
| 5 | **Status** | `watch` (contained) or `needs work` (new tests keep paying, or a gap hides behind it). |

Decide per entry: **keep** (bump Last reviewed), **update** (counts, paths, home), **narrow**
(part fixed), **archive** (remedy landed or code removed: `resolved (<date>)` + **Resolution**
naming the test that now shows it), or **merge**.

## 3. Look for new candidates

Signals (each is a lead, not a finding):

| Signal | Search |
|---|---|
| Package-level var used as a seam | `rg -n '^var ' internal --type go -g '!*_test.go'`, then `rg -n '<name> = ' -g '*_test.go'` |
| Wall clock, env, randomness, cwd, std streams not injected | `rg -n 'time\.Now\(\)\|os\.Getenv\|os\.Getwd\|rand\.\|os\.Std(in\|out\|err)' internal -g '!*_test.go'` |
| Tests moving process state | `rg -c 't\.Chdir\|t\.Setenv' -g '*_test.go'`; any `t.Parallel` ban comment |
| Shared mutable test fixture | a package-level test var whose fields tests swap (`rg -n 'testDeps\.\w+ = '`) |
| Sleeps and host tools in tests or the harness | `rg -n 'time\.Sleep\|exec\.Command\|"sh"\|sed ' -g '*_test.go' .agent/scripts/adversarial` |
| Logic asserted through human text | ratio of `strings.Contains(stderr\|stdout` to `errs.KindOf\|errors.As` in a package's tests |
| Tests reaching past an interface | `rg -n '\.DB\(\)\|\.\(\*sqlite\.Store\)' -g '*_test.go'` outside `internal/store/sqlite` |
| Wide interface for a narrow need | a param typed `store.Store` (or another wide interface) that calls one or two methods |
| Decision mixed with effect (§12.1) | a handler or func over ~80 lines that validates and does I/O; tests named `…BeforeCopy`/`…BeforeWrite`; a pure-looking package (README table in §12.1) importing `os`, `database/sql` or `net/http` (`rg -l '"os"\|"database/sql"' internal/{chunk,glob,links,prior,render,search,reindex}`) |
| Clock passed as a func into domain code (§12.4) | a pure function taking `func() time.Time` or a collaborator, where a `time.Time`/value would do (`rg -n 'func\(\) time\.Time' internal -g '!*_test.go'` outside `deps`/`Options`/struct fields) |
| Effect with no observable result (§12.3) | an exported func that writes and returns only `error`, whose tests then read the effect back with raw SQL or file reads |
| Duplicated rule | comments saying "mirrors", "same as", "keep in sync"; a test whose only job is comparing two copies |
| Untestable error path | survivors triaged "driver-only", "unreachable", "defensive" (`.agent/knowledge/mutation-survivor-patterns.md`, queue) |
| Doubles off contract | stubs returning `errors.New` or a bare sentinel for a dependency whose doc names an `errs` kind |

Propose a candidate only with concrete evidence (TESTING.md-level symptoms, not taste), and only
if it outranks the weakest active entry or the folder is under about 12 entries. Say which entry
it displaces. A one-file fix goes to `.agent/queue/low-findings.md` instead.

## 4. Verify (when editing)

`go run ./.agent/scripts/index-check` prints "indexes in sync". `index.md` stays ranked and
lists only active entries plus the archive link. Every archived entry has a Resolution line.

## 5. Report

Start with one line: the number of entries checked and how many to keep, update, narrow, archive
or merge, plus the number of candidates. Then one table, with archives and **needs work** first:

| Entry | Status (claimed → actual) | Finding | Action | Evidence |
|---|---|---|---|---|
| sqlite-migrations-package-vars | needs work → resolved | `Artefact.steps` field landed in 3.2; no test reassigns `migrations` | Archive | `rg 'migrations = ' -g '*_test.go'` empty |
| <candidate> | — → needs work | <smell> | Add (displaces <entry>) | <tests, counts> |

After the table, at most three lines: which PLAN item should absorb which remedy now, any queue
entries you added, and anything the owner should decide.
