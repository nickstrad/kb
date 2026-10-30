# Process interruption
> Ctrl-C, kill or a crash in the middle of a reindex, download, migration or add must leave the store consistent and the next command working.

- **Category:** process lifetime
- **Status:** watch
- **Where:** `cmd/kb/main.go` (`NotifyContext`), every transaction in `internal/store`, `local.Pull` (ctx-aware since W1), `add.go` rollback, migrations.
- **Why it matters:** agents get cancelled often. A half-written state that `kb doctor` can't explain stops all further work in that store.
- **Evidence:** before W1, Ctrl-C was ignored during downloads (fixed). `kb reindex` cut short by SIGINT printed the driver's "context canceled" (Q21); fixed in the W2 pass: "kb reindex: interrupted (N entries reindexed before it); finished entries are saved, rerun kb reindex to finish", exit 1 (also when the cancel lands in Ping or ResetForReindex). Other commands (add, edit, search, show) still flatten a cancel into "… context canceled" (PLAN 1.2 carried note). Panics exit 3 by design (CODING_STYLE §1.5, not yet implemented).
- **Coverage today:** harness `sigint-mid-reindex` (only the 80 ms SIGINT lands mid-run on a 400-entry FTS-only corpus; later ones arrive after it finished); `TestReindexInterrupted` (cli, pre-cancelled ctx); `TestPullCancelMidDownload`; transaction rollback tests.
- **Next checks:** SIGKILL (not just SIGINT) mid-reindex and mid-migration, then `kb doctor` and a re-run; make `sigint-mid-reindex` scale its corpus so every delay lands mid-run; interrupting `kb add --dir` mid-copy; one "interrupted" rule in `report` and the exit-3 incident path once the errs package lands (1.2).
- **Last reviewed:** 2026-10-29 · W2 wave-end pass
