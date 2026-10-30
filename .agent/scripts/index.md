# Agent scripts

Every script or script folder here, with a one-line purpose and usage. Keep this list in sync
with the folder: add a line when you add a script, remove it when you delete one. Check with
`go run ./.agent/scripts/index-check`. Run everything from the repo root.

- [state-status.sh](state-status.sh) — list `.agent/state/` logs (agent, item, status, updated),
  flag logs not updated in N minutes and paths owned by two active logs.
  Usage: `bash .agent/scripts/state-status.sh [stale-minutes]` (default 30).
- [index-check](index-check/README.md) — report entries missing from or extra in `scripts/index.md`,
  `knowledge/index.md` and `leverage/index.md`; exit 1 when out of sync.
  Usage: `go run ./.agent/scripts/index-check`.
- [sandbox.sh](sandbox.sh) — throwaway snapshots of the working tree (uncommitted and untracked
  files included) under `.agent/sandbox/` for anything that mutates code or builds; refuses paths
  outside it. Usage: `bash .agent/scripts/sandbox.sh new <name> | list | rm <name|path>... |
  prune [--older-than 24h]` (protocol: `.agent/sandbox/README.md`).
- [adversarial](adversarial/README.md) — adversarial testing (docs/ADVERSARIAL_TESTING.md):
  `mutate` (mutation testing), `harness` (hostile CLI scenarios), `boundaries` (§2.1 limits in
  tests), `coverage-gaps.sh`, `flaky.sh`; all sandboxed. Usage:
  `go run ./.agent/scripts/adversarial/mutate -pkg ./internal/<pkg> [-diff main]`,
  `go run ./.agent/scripts/adversarial/harness [-run re]`.
