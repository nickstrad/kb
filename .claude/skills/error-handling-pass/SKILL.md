---
name: error-handling-pass
description: This skill should be used when the user asks for an "error handling pass", to "check error handling", "review errors", "standards pass on errors", "audit error propagation", "are my errors well-formed", or wants kb code (the current diff vs main, or a given path/package) reviewed against the error-handling rules in docs/CODING_STYLE.md section 1. Reports gaps as a table; does not fix them unless asked.
---

# Error-handling standards pass

**Source of truth:** `docs/CODING_STYLE.md` §1 (Error handling), plus §4 (context), §8 (logging) and §9 (CLI output). The assertion rules are in `docs/TESTING.md` §3. Read §1 before every pass. Do not restate the rules here or in the report; cite them by section.

This skill **reports** findings. Do not edit code unless the user explicitly asks for fixes.

## 1. Scope

- If the user gives no argument, review the Go files changed against main: `git diff --name-only main...HEAD -- '*.go'` plus uncommitted changes from `git diff --name-only -- '*.go'`. Review only the changed hunks (`git diff main -U5`), but read enough of each function around them to judge it.
- If the user gives a path or package (`internal/search`, `./internal/store/...`), review every non-test `.go` file in it.
- Skip `_test.go` files, except for goroutine and `_ =` checks inside test helpers.
- Note the package of each file. A **module boundary** is any exported function or method of a package called by another kb package. `internal/cli` is the top level.

## 2. Scan

Run these with `rg -n` (or `grep -rn`) over the files in scope. A hit is a **lead**, not a finding: read the code before reporting it.

| # | Pattern | Look for | Rule |
|---|---|---|---|
| 1 | `return .*\berr\b$` and `return nil, err` in exported funcs | a raw lower-level error (driver, os, http, another kb module) crosses a boundary without being wrapped | §1.3, §1.5 |
| 2 | `fmt.Errorf\([^)]*%v[^)]*err` and `fmt.Errorf` with an `err` argument but no `%w` | the chain is broken, so callers cannot use `errors.Is`/`As` | §1.5 |
| 3 | `_ = ` and `_, _ = ` and calls whose error result is ignored (`\.Close\(\)$`, `\.Exec\(` without assignment) | a swallowed error with no justifying comment, especially on write paths | §1.5 |
| 4 | `strings\.(Contains\|HasPrefix\|TrimPrefix)\(.*(err\|Error\(\))` and `err.Error\(\) ==` | branching on, or rewriting, error strings | §1.5 |
| 5 | `panic\(` and `assert\.` used with flags, args, file contents, config or DB rows | panic or assert on user-influenced input | §1.7, docs/TESTING.md §3 |
| 6 | `recover\(\)` | any `recover` other than the single top-level one in `cli.Execute` (which logs an incident and exits 3) | §1.5, docs/TESTING.md §3.1 |
| 7 | `go func` and `errgroup` | a goroutine that logs, prints, drops or `os.Exit`s on an error instead of returning `Result{Value, Err}` or using errgroup; ignores `ctx.Done()` | §1.6 |
| 8 | `log\.\|slog\.\|fmt\.Fprint.*os\.Stderr` outside `internal/cli` | a library package logging instead of returning | §8 |
| 9 | `usageErr\(\|embedderErr\(` with `%s", err` or `err.Error()` | the CLI flattens an unexpected error into a usage error (it should be a bug or exit 3) or loses its kind | §1.5 |
| 10 | `errors.New\(` / `var Err` | a new sentinel where a `Kind` would do; check it is documented and needed across packages | §1.5 |
| 11 | messages in `Wrap`/`usageErr`/`Fprintf(stderr` containing SQL, `sqlite`, HTTP status bodies or `%+v` | user-facing text that leaks internals | §1.2, §9 |
| 12 | `context.Background\(\)\|context.TODO\(\)` below `cmd/kb` | cancellation lost; caller cancel misclassified as `Unavailable` | §4 |

Also check by reading, since grep cannot find these:

- **Missing context.** The error at the boundary should name the entry path, search id, database path or model. If it doesn't, the user cannot act on it (§1.2 item 2).
- **Partial success.** A function that returns `(value, err)` with both set must say so in its doc comment, and its callers must print the value and warn (§1.5, the `ErrLogFailed` pattern).
- **Failures told apart only by text.** Two outcomes that callers or tests must distinguish share a kind and differ only in `Msg` (for example several `usageErr` cases that tests match by substring). Give them a typed error, a result field or a distinct kind (§1.5; CODING_STYLE §12.3). Report as Low unless a caller branches on the text (then check 4).
- **Top level.** `report` in `internal/cli/root.go` must show the `Msg` of well-formed errors, record bugs in the incident log with an ID and a stack trace, and map kinds to the exit codes in §1.5. Flag any new path that writes errors to stderr and bypasses `report`.

## 3. Classify each finding

- **Class**
  - `bug-path`: an error that can reach `report` without being well-formed. It would show as an internal error, or worse, be mislabelled as a usage error.
  - `well-formed gap`: the error is wrapped, but it is missing context, the friendly message, `%w`, or the right kind.
  - `misuse`: the wrong mechanism for the job (assert or panic on user input, string matching, a goroutine that decides).
- **Severity**
  - **High**: lost or swallowed errors on a write path, a wrong exit code, a panic on user input, a goroutine that drops errors, or leaked internals.
  - **Medium**: broken chain (`%v`), a missing boundary wrap, branching on error strings.
  - **Low**: missing context fields, wording, an undocumented sentinel.
- If a finding is debatable (for example, whether a function is really a boundary), report it as **Low** and say why.

## 4. Report format

Start with one line giving the scope reviewed and the file count. Then give a table sorted by severity:

| Sev | Class | Location | Rule | What's wrong | Suggested fix |
|---|---|---|---|---|---|
| High | bug-path | internal/cli/search.go:161 | CODING_STYLE §1.5 | Unexpected search error flattened into `usageErr`, exits 1, chain lost | `return err`; let `report` classify via `errs.KindOf` |

- Use `file:line` paths relative to the repo root. Give one row per finding; group identical patterns in one file into a single row listing the lines.
- Keep the suggested fix to one sentence or one short snippet.
- After the table, write at most three lines: counts by severity, and any systemic issue (for example, "the `errs` package does not exist yet, so every boundary finding depends on it").
- If nothing is found, say so and list the checks you ran.
- Do not fix anything unless the user asks. If they do, fix High items first and run `go vet -tags fts5 ./...` and `go test -tags fts5 ./...` on the touched packages.
