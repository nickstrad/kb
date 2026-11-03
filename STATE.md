# STATE.md — kb

## Goal and spec

kb is the knowledge-store CLI (Go, SQLite FTS5 + sqlite-vec hybrid search, DuckDB-backed
`kb stats`). The current unit of work is the GraphDuck evaluation requested by Nick:
brief at `docs/graphduck-eval-brief.md`, deliverable at `docs/graphduck-eval.md`.

## Status

GraphDuck evaluation written (branch `eval/graphduck`) and checked against DuckDB 2.0 (branch
`eval/graphduck-duckdb2-check`, `docs/graphduck-eval-duckdb2-check.md`). Overall verdict of the
2.0 check: the no-pivot recommendation STANDS WITH QUALIFICATIONS. Awaiting Nick's read.

## Done

- `docs/graphduck-eval.md`: pivot analysis (a), dual-approach analysis (b), recommendation
  (c), four book-based changes (d). DuckDB claims verified on 2026-11-03 against the DuckDB
  2.0 docs and the local CLI 1.5.5 (HNSW persistence flag, FTS index not maintained on
  write, one-process file lock). Commit SHAs are in `git log eval/graphduck`.

- `docs/graphduck-eval-duckdb2-check.md` (branch `eval/graphduck-duckdb2-check`): per-claim
  check of the eval's DuckDB claims on DuckDB 2.0.0 (tested with the official CLI, plus docs).
  C1 (single writer) and C3 (FTS maintenance) WEAKENED by Quack (beta) and FTS
  `incremental = 1`; C2 (HNSW persistence experimental) and the rest HOLD.

## In progress

Nothing. No background runs.

## Next steps

1. Nick reads the evaluation and decides which of the four proposed changes, if any, to plan.
2. If accepted, each change becomes a plan item built by a fresh agent in its own worktree
   per the global protocol; 6.2 (validity windows, S) is the natural first item, 6.1 (typed
   relations) unlocks 6.3 and 6.4.
3. Delete the temporary research clone at `/root/Software/research/graphduck` and the
   extracted text `/root/Software/research/graphduck.txt` when no longer needed (they are
   outside the repo and not committed).

4. Whatever the pivot decision, `kb stats` depends on the archived `marcboeker/go-duckdb/v2`;
   moving to `github.com/duckdb/duckdb-go/v2` is a candidate maintenance item (see the 2.0
   check, C6).

## Open decisions

- Owner: Nick. Whether to adopt any of the proposed changes, and whether `related:` front
  matter should be part of the knowledge-repo contract (`/root/Raw/knowledge/AGENTS.md`).
- Owner: Nick. Whether the eval should be revised per the 2.0 check's qualifications
  (restate the concurrency and FTS arguments, update the Go driver facts).
- Owner: Nick. Whether to merge `eval/graphduck` to main as documentation (the brief says do
  not merge without asking).

## Resume instructions

- Branch `eval/graphduck`, worktree `/root/Software/kb-work/graphduck-eval` (main checkout
  at `/root/Software/kb`). The 2.0 check is on branch `eval/graphduck-duckdb2-check`, worktree
  `/root/Software/kb-work/graphduck-duckdb2`. No detached runs; nothing to relaunch.
- Research inputs: the PDF at `/root/Software/kb/research/graphduck.pdf`, text extraction at
  `/root/Software/research/graphduck.txt`, companion repo clone at
  `/root/Software/research/graphduck`.
