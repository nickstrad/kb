# kb adversarial testing

Audience: contributors, people and agents alike. Read time: about 10 minutes. [TESTING.md](TESTING.md) says where each test belongs. This document covers checking those tests: are they strong enough, and what do they miss at the seams between packages and processes?

## 1. Purpose

Adversarial testing is **a check on the tests, not a new test layer**. It does three things:

1. **Validate the tests themselves.** Mutation testing changes the code in small, plausible ways (a `<` becomes `<=`, a limit moves by one, an error check disappears). A change that every test still passes on is a *survivor*: the tests cannot tell the right code from the wrong code there.
2. **Probe the integration points.** The CLI harness runs the real binary as separate processes against temp stores: concurrent writers on one database, SIGINT mid-write, corrupt and future-schema databases, hostile queries and paths, no model and no network. Unit tests cannot reach these, by design.
3. **Attack assumptions.** Each scenario states an assumption ("SQLite's busy_timeout serialises writers", "a symlink can't leave the root", "search never downloads") and tries to break it.

Nothing found here is fixed here. A survivor or a failed scenario becomes a test, an assertion or a fix at the layer TESTING.md §10 assigns (§5 below). The tools only find gaps; the layers own the claims.

| TESTING.md layer | What adversarial testing asks of it | Tool |
|---|---|---|
| L1 assertions, L2 unit/table, L3 properties | Does some test fail when this line is wrong? Is each §2.1 boundary pinned at, below and above? Do fuzz targets plus seeds reach every line? | `mutate`, `boundaries`, `coverage-gaps.sh` |
| L4 contract suites | Run `mutate` on a backend or embedder: a contract survivor means the suite is too weak | `mutate` |
| L5 integration (CLI) | Do the commands keep the exit-code contract and fail well-formed under hostile conditions across processes? | `harness` |
| all | Are any tests order-dependent or racy? | `flaky.sh` |

## 2. Safety rules

- **Sandbox only.** Every tool snapshots the working tree with `.agent/scripts/sandbox.sh new <name>` into `.agent/sandbox/<YYYYMMDD-HHMM>-<name>/`. It mutates, builds and runs only there, and removes the sandbox on exit (`-keep`/`-k` keeps it). The protocol is in `.agent/sandbox/README.md`. Other agents are editing the live tree, so never point a mutating tool at it.
- **Temp roots.** Harness scenarios use `<sandbox>/_adv/runs/<scenario>/{root,home,outside,tmp}` with `HOME`, `TMPDIR` and `REMBED_CACHE` redirected. Never `~/knowledge`, never the repo.
- **No network, no model downloads.** Go runs with `GOPROXY=off`. kb processes get a dead HTTP(S) proxy, plus a garbage cached `config.json` that makes `rembed` fail before fetching anything. Any `*.safetensors` appearing in a scenario's HOME fails that scenario.
- **Report by default.** Fix only when asked. Small findings go to `.agent/queue/low-findings.md`. Ideas for the tools go to `.agent/queue/harness-improvements.md`.

## 3. The tools

All live in `.agent/scripts/adversarial/` (README there: flags, outputs, exit codes). Run from the repo root.

### 3.1 `mutate`: mutation testing

```bash
go run ./.agent/scripts/adversarial/mutate -pkg ./internal/stats -j 4
go run ./.agent/scripts/adversarial/mutate -pkg ./internal/search -func 'applyCap|ClampK' -ops boundary,const
go run ./.agent/scripts/adversarial/mutate -pkg ./internal/chunk -diff main     # wave mode: changed lines only
```

Operators: `boundary` (`<`↔`<=`, `>`↔`>=`), `negate-rel` (`==`↔`!=`), `logical` (`&&`↔`||`), `if-negate`, `const` (integer literal ±1: this is how `MaxChunkChars`, `PerEntryCap` and `ListLimit` get attacked), `arith` (`+`↔`-`, `+=`↔`-=`, `++`↔`--`), `err-check` (`if err != nil` → `if false`), `bool-return` (negate a returned bool), `loop-drop` (remove one statement from a loop body). Each mutant runs `go test -tags fts5 -count=1 -vet=off -failfast ./<pkg>/` with a timeout.

**Reading the result.** `killed`: a test failed, good. `survived`: a gap, or an equivalent mutant. `timeout`: counted as killed, but check for a hang the tests should name. `compile-error`: ignored (left out of the score). Score = (killed+timeout)/(killed+timeout+survived). There is no gate on the score. The survivor list is what matters.

**Triage each survivor into one of:**

| Survivor kind | Example (real, 2026-10-29) | Action |
|---|---|---|
| Missing assertion on a result | `stats/queries.go:69` `Digits 1→2` survives: nothing checks percentile rounding | add the check to an existing table row (Q15) |
| Untested error path | `stats/run.go` `err-check` survivors in Open/Run/query | table test with one row per error (Q13) |
| Missing boundary row | a `const` or `boundary` survivor on a §2.1 limit | rows "at X", "X-1", "X+1" |
| Weak fuzz property or missing seed | `chunk.go:196` fence-close negation survives `FuzzSplit` | `f.Add` seed with the shape that kills it (Q17) |
| Equivalent mutant (no behaviour change) | `chunk.go:101` `<=`→`<`: at exactly the limit the fast path and the general path give the same piece | note it in the report as equivalent, no test |
| Symbol-relative constant | `MaxChunkChars 1500→1501` survives because every test uses the symbol | fine unless the value is a contract; if it is, pin it once (golden or doc test) |

### 3.2 `harness`: hostile CLI scenarios

```bash
go run ./.agent/scripts/adversarial/harness                 # all scenarios, ~5 s plus the build
go run ./.agent/scripts/adversarial/harness -run 'concurrent|corrupt' -keep
```

Scenarios (in `harness/*_scenarios.go` and `scenarios.go`; `harness -list` prints all 28 with what each attacks), among them `exit-codes`, `fts-roundtrip`, `no-model-search`, `no-model-index`, `hostile-queries`, `markdown-edges`, `front-matter-hostile`, `path-traversal`, `feedback-hostile`, `concurrent-fresh-db`, `sigint-mid-reindex`, `corrupt-db`, `newer-schema`, `workspace-discovery` (the only scenario that runs kb without `--root`), `links-stale` (kb related, search --neighbors and kb stale on a hostile link graph and hostile dates). Every kb run is checked for a Go panic trace, an exit code outside {0,1,2} (exit 3, an internal error, is a contract code but always a finding under hostile input), a hang over 60 s, and a model download.

**Reading the result.** `PASS`/`FAIL` per scenario. Each `✗` line carries the command, exit code, stderr and stdout as evidence. `·` lines are notes: observed behaviour that is not a contract violation but may need a decision (for example, that a symlink under `data/` is followed). A FAIL is either a product bug or a stale expectation in the scenario. Read the evidence and the code before filing it.

**Harness vs E2E.** Both run the real binary in temp roots, but they answer different questions, and neither copies the other's cases.

| | Harness (this section) | E2E journeys (TESTING.md §11, planned: PLAN E1–E5) |
|---|---|---|
| Question | Does kb keep its contracts (exit codes 0/1/2/3, no panic, no hang, no download) under hostile conditions? | Do the commands work together as README and `kb help` say? |
| Inputs | Hostile, malformed, edge: concurrent writers, SIGINT, corrupt and newer-schema files, path traversal, odd queries | Realistic: a small corpus, the documented flags, the documented recovery steps |
| Checks | Invariants plus targeted expectations, written in Go | Whole transcripts (stdout, stderr, exit code, file effects) that read as documentation |
| Lives in | `.agent/scripts/adversarial/harness/` (agent tooling, run at wave end and nightly) | `internal/e2e/` (product tests, every PR) |

Put a new probe in the harness if its point is to *break* an assumption, and in a journey if its point is that a *documented flow works*. When a harness failure is fixed, the regression test goes to the owning layer (§5), not into a journey. When a journey exposes a hostile case, add a harness scenario instead of extending the journey.

### 3.3 Test-validation helpers

- `boundaries <pkg dir>...`: lists numeric limits and counts test lines naming each at `X`, `X-1` and `X+1` (`ok` / `part` / `none`). It finds leads for the §2.1 boundary rule. Pair it with `mutate -ops const,boundary`: a limit marked `ok` whose `boundary` mutant survives has rows that don't discriminate.
- `coverage-gaps.sh [-t pct] <pkg dir>...`: functions below the threshold, measured with the package's own tests (seeds and crashers replay). Use it for §2.1 "what fuzzing doesn't reach gets a unit test".
- `flaky.sh [-n N] <pkg dir>...`: runs `go test -race -shuffle=on -count=N`; prints each test that ever failed, the shuffle seed and the race count.

## 4. Coverage-verdict mode (wave-end passes)

docs/PLAN.md §6.4 rule 4: every wave-end pass states, per item, whether the adversarial tools already probe what changed. Procedure:

1. List the changes: `git diff --name-only main...HEAD` plus uncommitted. For each item, note every changed **package**, **command**, **flag**, **exit code or error kind**, **parser or sanitiser**, and **integration point** (schema or migration, cross-process locking, files outside the root, embedder or network, signals).
2. Map each to its probe:

   | Change | Probed by |
   |---|---|
   | logic in a package | `mutate -pkg <pkg> -diff main` (run it; survivors are findings) |
   | a new limit or constant | `boundaries <pkg>` + `mutate -ops const,boundary` |
   | a parser, sanitiser or decoder | `markdown-edges`, `front-matter-hostile`, `hostile-queries`, or the package's fuzz target |
   | a command, flag or exit code | `exit-codes` (plus a row for the new flag), the command's own scenario |
   | schema or migration | `newer-schema`, `corrupt-db`, `concurrent-fresh-db` |
   | locking or transactions | `concurrent-fresh-db`, `sigint-mid-reindex`, `flaky.sh` |
   | paths and the root | `path-traversal`; root discovery, `--root`, `--global`: `workspace-discovery` |
   | embedder, model or network | `no-model-search`, `no-model-index` |
   | feedback or the search log | `feedback-hostile` |

3. Run the mapped tools (mutate on changed packages, the mapped harness scenarios).
4. Output, per item: **suffices** (name the tools and scenarios, with their result) or **gaps** (what changed that nothing probes). Each gap becomes an `H<n>` entry in `.agent/queue/harness-improvements.md` (format in its header). A survivor or a failed scenario is a finding like any other: Low ones go to `low-findings.md`, larger ones into the pass report.

## 5. From a finding to a test

Work down docs/TESTING.md §10 and stop at the first yes. In short:

- **A survivor on a local invariant** (order, bounds, hash consistency): an `assert` (L1). The mutant then dies in every test that reaches the code.
- **A survivor on a boundary or branch outcome:** a table row in the owning package, named after the boundary or the outcome.
- **A survivor that a fuzz target should have killed:** strengthen the property or add an `f.Add` seed. Don't add a fixed-input duplicate test.
- **A harness failure:** find the root-cause layer. `database is locked` under concurrent `kb add` is a store bug, so the regression test is a store test with two `Store` handles writing concurrently, not a CLI test. Keep the harness scenario as the end-to-end probe.
- **A harness note that needs a decision** (a symlink policy, a title-length cap): a `docs/DEFAULTS.md` row, then a test once it's decided.

After the fix, rerun the tool: the mutant must now be killed, or the scenario must pass.

## 6. Cadence

| When | What | Cost |
|---|---|---|
| Per item, before review (optional) | `mutate -pkg <changed pkg> -diff main`; harness scenarios mapped in §4 | seconds to a minute |
| Wave-end pass | coverage verdict (§4): mutate on every changed package, full harness, `boundaries` on packages with new limits | a few minutes |
| Nightly (alongside `make fuzz`) | full harness; mutate on every package (`-j 4`); `flaky.sh -n 20` on store, search and cli | about 10–20 min |
| After a crash or a bug report | the scenario or operator that would have caught it, then the test at the right layer | |

## 7. Extending

- **A mutation operator:** add a constant to `allOps` in `mutate/ops.go`, a case in `enumerator.walk` or a helper that calls `e.add(op, pos, before, after, apply, undo)` with reversible closures, and a row in `TestOperatorRewrites` (fixture source, line, expected rewrite). Prefer operators that compile; a mutant that only produces compile errors costs a run and teaches nothing.
- **A harness scenario:** add `{Name, Desc, Run}` to `scenarios` in `harness/scenarios.go`. Inside `Run`: set up files with `h.write`, run `h.kb(...)` (use `withNone(...)` for FTS-only commands), and check with `h.expect(r, codes...)`, `h.fail` and `h.note`. The universal invariants and the download check run automatically. Name it after the assumption it attacks.
- Ideas you can't do now go to `.agent/queue/harness-improvements.md`.

## 8. Why our own mutation tool

Considered: **go-gremlins/gremlins** (coverage-guided, maintained, many operators) and **avito-tech/go-mutesting** (older, file-copy based, blacklist of equivalent mutants). Both are third-party binaries outside `go.mod`; AGENTS.md wants helpers on the standard library only. Neither knows kb's `fts5` tag or the sandbox protocol without wrappers. gremlins mutates the tree it runs in, so it would need the sandbox anyway. Our tool is about 900 lines of `go/ast` code plus tests, and it fits our operators (`err-check`, `loop-drop`, constant ±1 for §2.1 limits). It lacks type information (H10) and an equivalent-mutant list (H11). If the operator set outgrows it, revisit gremlins in a sandbox (an owner decision: it is a new dependency).
