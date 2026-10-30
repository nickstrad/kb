# Ctrl-C only works where ctx is honoured

> cmd/kb wraps SIGINT/SIGTERM in signal.NotifyContext, so any blocking call that ignores ctx makes Ctrl-C do nothing.

**Fact:** `main` uses `signal.NotifyContext`, which replaces Go's default "exit on SIGINT". A call
that never checks ctx (rembed.Load downloading a model, a long loop) keeps running after Ctrl-C.
`local.Pull` runs `rembed.Load` in a goroutine and selects on `ctx.Done()`; abandoning it is safe
because rembed downloads to a temp file and renames.

**Why it matters:** downloads take minutes; an uncancellable one looks like a hang.

**How to apply:** every new long-running call in a command takes ctx; if a dependency has no ctx
API, wrap it in a goroutine + select and check that abandoning it cannot corrupt state. Add a test
with a blocking seam (see `TestPullCancelMidDownload`).

**Seen in:** internal/embed/local/local.go, cmd/kb/main.go · 2026-10-29
