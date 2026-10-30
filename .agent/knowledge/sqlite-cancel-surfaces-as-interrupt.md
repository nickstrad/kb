# A cancelled ctx may not surface as context.Canceled

> Mid-statement it is sqlite3.ErrInterrupt; at Tx.Commit it can be sql.ErrTxDone (database/sql already rolled back). Decide by ctx.Err(), not by the error.

**Fact.** database/sql returns `ctx.Err()` when the cancel is seen before or between calls, but a
statement already running inside SQLite is stopped with `sqlite3_interrupt`, and the error is
`sqlite3.ErrInterrupt` ("interrupted") with no `context.Canceled` in its chain. The harness
`sigint-mid-reindex` (SIGINT after 80 ms) hit this on the first run after the errs package
landed: the error reached `report` as Internal and kb exited 3 with an incident.

A second form: when ctx is cancelled during a transaction, database/sql's background goroutine
rolls the transaction back, and the following `Tx.Commit` can return `sql.ErrTxDone`. That made
`sigint-mid-reindex` exit 3 about one run in four even after ErrInterrupt was classified.

**Why it matters.** Ctrl-C is a known edge case (exit 1, "interrupted"), not a bug. Any check
written as `errors.Is(err, context.Canceled)` alone misses it.

**How to apply.** `sqlite.wrap(ctx, err, …)` returns `errs.Interrupted` whenever `ctx.Err() != nil`,
whatever the error; `classify` also maps ErrInterrupt and context.Canceled. Above the store, check
`errs.KindOf(err) == errs.Interrupted || errors.Is(err, context.Canceled)`. A pre-cancelled ctx in a
unit test reaches only the `ctx.Err()` path; `TestCancelMidWriteIsInterrupted` (fault driver hook
that cancels at a chosen statement) covers the others deterministically.

**Seen in:** internal/store/sqlite/sqlite.go (classify), internal/cli/report.go · 2026-10-29
