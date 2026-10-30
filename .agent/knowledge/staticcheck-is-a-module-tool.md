# staticcheck is a pinned module tool
> Run `go tool staticcheck -tags fts5 ./...` from the repo root; never `go run …staticcheck@latest`.

**Fact.** staticcheck v0.8.2 is pinned as a `tool` directive in `go.mod` (Q34). `go tool staticcheck` uses that version from the module cache, offline. Several agents reported "staticcheck not installed" and fell back to `go run honnef.co/go/tools/cmd/staticcheck@latest`, which fetches a different, unpinned version (or fails offline and prints nothing, which looks like a clean run).

**Why it matters.** An unpinned run can disagree with CI, and an offline `go run` that silently does nothing reads as "clean".

**How to apply.** Always `go tool staticcheck -tags fts5 ./...` (add `,harnessfake` or `,noassert` when checking those builds). If it errors, report the error instead of switching to `@latest`.

**Seen in:** go.mod `tool` directive; W9 agent reports · 2026-10-30
