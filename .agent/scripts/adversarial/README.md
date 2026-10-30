# adversarial

**Purpose:** tools that test kb's tests and attack its integration points. Background, triage
and cadence are in `docs/ADVERSARIAL_TESTING.md`. The skill is `.claude/skills/adversarial-testing/`.

Every tool works in a sandbox (`.agent/scripts/sandbox.sh`, protocol in `.agent/sandbox/README.md`)
and removes it on exit unless `-keep`/`-k` is given. None of them writes to the live tree,
touches `~/knowledge` or uses the network.

| Tool | Language | What it does |
|---|---|---|
| `mutate/` | Go | Mutation testing: one source change at a time, runs the package's tests, reports survivors and a score |
| `harness/` | Go | Builds kb once and runs hostile CLI scenarios against temp roots; reports pass/fail with evidence |
| `boundaries/` | Go | Lists numeric limits and whether tests name them at, -1 and +1 (docs/TESTING.md §2.1) |
| `coverage-gaps.sh` | Bash | Functions below a coverage threshold, measured with the package's own tests and seeds |
| `flaky.sh` | Bash | Runs tests N times with `-race -shuffle=on` and reports tests that ever failed |

## Usage (from the repo root)

```bash
go run ./.agent/scripts/adversarial/mutate -pkg ./internal/stats              # whole package
go run ./.agent/scripts/adversarial/mutate -pkg ./internal/search -func '^ClampK$|applyCap' -j 4
go run ./.agent/scripts/adversarial/mutate -pkg ./internal/chunk -diff main   # changed lines only
go run ./.agent/scripts/adversarial/mutate -pkg ./internal/chunk -list        # enumerate, run nothing
go run ./.agent/scripts/adversarial/harness                                   # every scenario
go run ./.agent/scripts/adversarial/harness -run 'concurrent|sigint' -keep
go run ./.agent/scripts/adversarial/harness -list
go run ./.agent/scripts/adversarial/boundaries internal/search internal/chunk
bash .agent/scripts/adversarial/coverage-gaps.sh internal/entry internal/chunk
bash .agent/scripts/adversarial/flaky.sh -n 20 internal/store internal/cli
go test ./.agent/scripts/adversarial/...                                      # the tools' own tests
```

**mutate flags:** `-pkg` (required), `-func` regexp on `Func` or `Recv.Func`, `-ops` (comma list
of `boundary, negate-rel, logical, if-negate, const, arith, err-check, bool-return, loop-drop`;
default all), `-max N` (an even sample), `-timeout` per mutant (default 3× baseline + 15 s), `-j`
workers (default 2, one sandbox each), `-diff REF` (lines changed against REF plus untracked
files), `-tags` (default `fts5`; use `fts5,model` for model-tagged tests), `-json`, `-list`,
`-keep`. Exit 0 when it ran (survivors are findings, not errors), 2 when it could not run. It also
exits 2 when the baseline tests fail in the sandbox.

**Output:** survivors (`file:line op func before -> after`), timeouts, then the counts and
`score = (killed+timeout)/(killed+timeout+survived)`. Compile-error mutants are left out of the
score. `-json` gives every mutant with `status` and `killed_by` (the first failing test).

**harness flags:** `-run` regexp, `-bin` (test a prebuilt binary), `-keep`, `-json`, `-list`.
Exit 0 all passed, 1 some failed, 2 it could not run. Each scenario gets
`<sandbox>/_adv/runs/<name>/{root,home,outside,tmp}`. `HOME`, `KB_ROOT`, `TMPDIR` and
`REMBED_CACHE` point there. Every HTTP(S) proxy variable points at a dead port, and the default
model's cache dir holds a garbage `config.json` that rembed parses before fetching anything. That
is two independent barriers against a download, and a downloaded `*.safetensors` fails the
scenario. Universal checks on every run: no Go panic or runtime trace, exit code in {0,1,2}, no
hang over 60 s.

**Adding things:** a mutation operator is a case in `mutate/ops.go`. Add its name to `allOps`,
give it reversible `apply`/`undo` closures, and add a row in `TestOperatorRewrites`. A harness
scenario is a `{Name, Desc, Run}` row in `harness/scenarios.go`. It uses `h.write`, `h.kb`,
`h.expect`, `h.fail` and `h.note`, and `withNone(...)` keeps a command on FTS.

**Dependencies:** Go standard library, bash, git, rsync (macOS base system). The Go tools are
`package main` folders in the main module; `./...` skips `.agent`, so run and test them by path.
