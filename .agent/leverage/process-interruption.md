# Process interruption
> Ctrl-C, kill or a crash in the middle of a reindex, download, migration or add must leave the store consistent and the next command working.

- **Category:** process lifetime
- **Status:** watch
- **Where:** `cmd/kb/main.go` (`NotifyContext`), `internal/store/sqlite` (`wrap`/`classify`: any failure after a cancel is Interrupted), `internal/cli/report.go` (one interrupted rule, exit 1), `local.Pull`, `add.go` rollback (`context.WithoutCancel`), migrations.
- **Why it matters:** agents get cancelled often. A half-written state that `kb doctor` can't explain stops all further work in that store.
- **Evidence:** before W1, Ctrl-C was ignored during downloads (fixed). `kb reindex` cut short by SIGINT printed the driver's "context canceled" (Q21); fixed in the W2 pass: "kb reindex: interrupted (N entries reindexed before it); finished entries are saved, rerun kb reindex to finish", exit 1 (also when the cancel lands in Ping or ResetForReindex). 1.2 (W3): `report` has one rule for every command (Interrupted kind or context.Canceled in the chain → "interrupted", exit 1); a cancel may surface as SQLITE_INTERRUPT or sql.ErrTxDone, so the store decides by `ctx.Err()` (knowledge: `sqlite-cancel-surfaces-as-interrupt.md`); panics exit 3 via the single recover in `cli.execute`.
- **Coverage today:** harness `sigint-mid-reindex` (6/6 runs pass in W3; still only the 80 ms SIGINT lands mid-run); `TestReindexInterrupted` and `TestReindexInterruptedKind` (cli: cancelled ctx, Interrupted kind without Canceled); `TestCancelMidWriteIsInterrupted` (fault driver cancels at a chosen statement); `TestWrapAfterCancelIsInterrupted`; `TestEnableWALRetries` (cancel while retrying WAL); `TestPullCancelMidDownload`.
- **Next checks:** SIGKILL (not just SIGINT) mid-reindex and mid-migration, then `kb doctor` and a re-run; make `sigint-mid-reindex` scale its corpus so every delay lands mid-run (H23); SIGINT for every other command (H22); interrupting `kb add --dir` mid-copy.
- **Last reviewed:** 2026-10-30 · W3 wave-end pass
