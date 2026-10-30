# AGENTS.md

Entry point for any agent (Claude Code, Codex, …) working on this repo. `CLAUDE.md` is a symlink
to this file.

## Hard rule: always use `.agent/` and keep it in sync

Every agent, on every task, MUST:

1. **State log.** For any non-trivial work (more than a quick read or a one-line fix), create a
   log in `.agent/state/` at the start (copy `.agent/state/TEMPLATE.md`), list the files you own,
   and update `updated`/`status` and the log as you go.
2. **Scripts.** Put any reusable helper in `.agent/scripts/` and add it to `scripts/index.md`
   (plus a folder `README.md` when the script has its own folder).
3. **Knowledge.** Record durable insights in `.agent/knowledge/` and add them to
   `knowledge/index.md`.
4. **Fold and delete.** When work is done, fold the finished state log into knowledge (and
   docs/DEFAULTS.md), then delete the log.

**Before finishing any task, run this checklist:**

- [ ] State log: `status: done`, final `done:` entry with tests run and testing approach.
- [ ] Insights moved to `.agent/knowledge/` + `index.md`; judgment calls listed in your report (only the orchestrator writes `docs/DEFAULTS.md` and assigns row numbers; when working alone, append the next free number).
- [ ] New or changed scripts listed in `.agent/scripts/index.md` (folder README if needed).
- [ ] Small out-of-scope fixes you noticed are appended to `.agent/queue/low-findings.md`.
- [ ] `go run ./.agent/scripts/index-check` prints "indexes in sync".
- [ ] `bash .agent/scripts/sandbox.sh list` shows none of your sandboxes (`sandbox.sh rm <name>`).
- [ ] `bash .agent/scripts/state-status.sh` shows no log of yours still `active`; done log deleted
      (by you if you run alone, otherwise by the orchestrator).

## What kb is

A Go CLI knowledge store: Markdown entries indexed with SQLite FTS5 + sqlite-vec, hybrid search,
in-process embeddings via rembed (`internal/embed/local`), a search log, feedback and `kb stats`.
It is mid-way through a v2 rework.

| Doc | What it holds |
|---|---|
| `docs/PLAN.md` | v2 phased work plan (items, files, tests, accept criteria) and model/concurrency assignments: who does what, what can run in parallel |
| `docs/TESTING.md` | test layers, assertions, table tests, fuzzing, coverage gates, build tags |
| `docs/CODING_STYLE.md` | errors, packages, naming, SQL, logging, CLI output, review checklist |
| `docs/DEFAULTS.md` | **local only, git-excluded, never commit.** Judgment calls pending owner review |
| `README.md` | user-facing install and usage |

Standards-pass skills (in `.claude/skills/`; they report, they don't fix):
`error-handling-pass` (CODING_STYLE §1) and `testing-standards-pass` (docs/TESTING.md).
`adversarial-testing` (docs/ADVERSARIAL_TESTING.md) tests the tests: mutation testing, a hostile
CLI harness, and the wave-end coverage verdict.
`leverage-check` validates `.agent/leverage/` against the code: stale paths, resolved weaknesses
to archive, coverage claims that no longer hold, and new candidates.
`testability-check` validates `.agent/testability/` against the code (designs that make tests
brittle or hard to write), archives entries whose remedy landed, and finds new candidates.

## Working rules (agreed with the owner)

- **macOS only.** Develop and test on darwin/arm64. No GNU-only shell (`sed -i`, `date -d`).
- **Build and test with `-tags fts5`**: `go test -tags fts5 ./...`. Without it there is no FTS5.
- **Commits:** item and pass agents never commit or push. The orchestrator commits and pushes `main` once per wave, after the wave-end pass, when everything is green (docs/PLAN.md §6.4 rule 7).
- **Small/medium decisions:** pick a default and log it in `docs/DEFAULTS.md` as a row: decision,
  default picked, alternatives, why, where. Keep going.
- **Architecture-level changes** (anything that would change a C4 diagram: storage backend,
  embedder, process boundaries, heavy new dependencies): stop and ask the owner.
- **Before starting a PLAN phase**, say what work starts and what it delivers.
- **When an item finishes**, report its testing approach per docs/TESTING.md (layers, assertions,
  table tests, properties, coverage), e.g. via the `testing-standards-pass` skill.
- **Wave-end pass:** at the end of every PLAN wave, run `testing-standards-pass` and `error-handling-pass` over the wave's diff and address the findings before the next wave (docs/PLAN.md §6.4 rule 4). Low findings go to `.agent/queue/low-findings.md` and are drained by Haiku sub-agents. The pass also gives an adversarial coverage verdict ("suffices", or gaps with suggestions); suggestions go to `.agent/queue/harness-improvements.md`.
- **Concurrency:** an orchestrator often runs sub-agents on PLAN items in parallel. Edit only
  the files your state log owns; check `state-status.sh` for conflicts before taking a path.

## The `.agent` folder

```
.agent/
  README.md        pointer to this section
  scripts/         reusable helper scripts; index.md lists every one
  knowledge/       durable insights; index.md lists every one
  state/           per-run logs (git-ignored except README.md, TEMPLATE.md)
  sandbox/         git-ignored throwaway snapshots of the tree (scripts/sandbox.sh); anything that
                   mutates code or builds works only in one, never in the live tree
  leverage/        the ~10 highest-leverage weak points to track; index.md lists them
  testability/     the ~10-15 designs that make tests brittle or hard to write; index.md ranks them
  queue/           low-findings.md: small fixes waiting for a Haiku sub-agent
                   harness-improvements.md: suggested adversarial-tool improvements, for later
```

**scripts/** — helpers agents write while working, run from the repo root.

- Language: **Bash** for short glue, one-liners and naturally-shell tasks; **Go** for anything
  with real logic or parsing, or that must be tested. Go helpers use the standard library only.
- A single-file script lives directly in `scripts/`. A script that needs other files (a Go
  package, tests, fixtures, templates) gets its own folder with a `README.md`: purpose, usage,
  inputs/outputs, how to run, dependencies.
- Bash: starts `#!/usr/bin/env bash` then `set -euo pipefail`, one-line header comment, `chmod +x`.
- Go: a `package main` folder in the main module (no own `go.mod`), run as
  `go run ./.agent/scripts/<name>` and tested as `go test ./.agent/scripts/<name>`. Dot-dirs are
  skipped by `./...`, so helpers never affect `go build/test ./...`.
- `scripts/index.md` MUST list every script/folder with a one-line purpose and usage.

**knowledge/** — facts a future agent needs that aren't obvious from the code.

- One kebab-case `.md` per insight: `# Title`, a `> one-line summary`, then **Fact**, **Why it
  matters**, **How to apply**, **Seen in:** path(s) · date. Keep it short. Use a folder with a
  `README.md` when an insight needs supporting files.
- Update an existing entry rather than adding a near-duplicate. Delete entries that become false.
- `knowledge/index.md` MUST list every entry with its one-line summary.
- Not knowledge: pending judgment calls (→ `docs/DEFAULTS.md`) or work to do (→ `docs/PLAN.md`).

**state/** — see `.agent/state/README.md`. One log per agent run, named
`<YYYYMMDD-HHMM>-<agent>-<item>.md` (local time, sortable). Front matter: agent, model, item,
status (`active|blocked|done`), started, updated, owns (comma-separated paths). Body: an
append-only, timestamped log plus an Insights list. Lifecycle: create at start → update as you
go → `done` → insights to knowledge, judgment calls to docs/DEFAULTS.md → delete.

**queue/** — the Low-findings queue (`queue/low-findings.md`), a habit, not an afterthought.

- **Feed it.** After every wave-end standards pass, the orchestrator appends each Low finding
  that isn't fixed on the spot, plus any small mechanical Medium (a missing seed, a stale
  comment, a renamed reference). Any agent that notices a small out-of-scope fix while working
  appends it instead of fixing it outside its owned files.
- **Drain it with Haiku.** The orchestrator dispatches **Haiku** sub-agents in batches: up to
  about 5 entries each, on files no active state log owns (check `state-status.sh`), one batch per
  file set so batches never overlap. Each Haiku agent claims its entries, keeps a state log,
  makes the fix, runs the narrowest test that covers it (`go test -tags fts5 ./<pkg>/`), and
  marks the entries done with that command.
- **Escalate, don't stretch.** If an entry needs design or touches more than a couple of files,
  Haiku marks it `[!] needs: sonnet|opus` and moves on.
- Drain the queue at least once per wave, after the wave-end pass and before the next wave
  starts, or alongside a wave when its files are free.

**leverage/** — the few places where a weakness costs the most: integration points under
contention (SQLite writers, the shared cache), resource management that drives performance
(model load and memory, reindex throughput, search scale), data-integrity boundaries
(migrations), and external boundaries (filesystem, process interruption). Format and rules:
`.agent/leverage/README.md`.

- **Point them out.** When your work reveals or changes a high-leverage weak point, say so in
  your report. Add or update an entry only if it's among the main items worth tracking. It's
  curated, at most about 10 active entries: adding one means it outranks something here, or
  something gets retired.
- **Use them.** Before designing or reviewing work that touches a listed area, read its entry.
  Its "Next checks" are candidates for PLAN items, H entries, perf benchmarks or adversarial
  scenarios.
- **Review them.** Every wave-end pass runs the `leverage-check` skill: it validates entries
  against the code and updates the ones the wave touched (status, evidence, coverage, next
  checks, last reviewed).
- **Retire them.** When a leverage point is refactored or redesigned so the weakness is gone,
  or stripped out of the code, archive it (`.agent/leverage/archive/`, with a Resolution line).
  Don't leave it active or delete it.
- `leverage/index.md` MUST list every entry with its status and one-line summary
  (`index-check` verifies it).

**testability/** — the designs that make tests brittle, slow, non-hermetic or hard to write:
hidden coupling, global or process state, wide interfaces, an un-injected clock or env,
decisions mixed with effects, logic observable only as human text, error paths with no fault
seam. `leverage/` is about runtime weak points; this folder is about the tests. Format and rules:
`.agent/testability/README.md`.

- **Point them out.** When a test is hard to write because of the design, or you work around it
  (a `t.Chdir`, a raw-SQL fixup, a sleep, a survivor triaged "unreachable"), say so in your
  report. Add or update an entry only with concrete evidence. The list is curated, about 10–15
  active entries, ranked.
- **Use them.** Before working in a listed area, read its entry. If your item owns the files,
  apply the remedy (each entry names the PLAN item that should absorb it).
- **Review and retire.** Every wave-end pass runs `testability-check`. It updates the entries the
  wave touched and archives resolved ones (`.agent/testability/archive/`, with a Resolution line).
- `testability/index.md` MUST list every active entry (`index-check` verifies it).

**Harness-improvements queue** (`queue/harness-improvements.md`). Every wave-end pass states
whether the adversarial tools already probe what the wave changed. Each gap becomes an `H<n>`
entry with a value rating. Entries are picked up when a wave has spare capacity, or when a later
item touches that area. A Sonnet agent is usually right; use Opus for new mutation operators or
concurrency scenarios. A done entry records how the improvement was validated, e.g. "the new
scenario fails on the old binary".

Useful commands:

```bash
bash .agent/scripts/state-status.sh       # who is doing what; stale logs; ownership conflicts
go run ./.agent/scripts/index-check       # indexes match folder contents
```
