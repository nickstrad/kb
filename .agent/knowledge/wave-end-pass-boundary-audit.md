# Wave-end boundary audit: what to look for

> W1 pass: most kb limits were tested only above the limit, never at it or one below; `go tool cover -func` plus a grep for constants finds them fast.

**Fact:** In the W1 pass, `MaxChunkChars`, `CandidateLimit`, `PerEntryCap`, `ListLimit`,
`vecOverFetch`, reindex batch size and `--since` all lacked an "at the limit" row; ListLimit,
vecOverFetch and the batch size had no test at all. Two real bugs hid at edges: `--since -1h` and
`--since 106752d` (Duration overflow) silently emptied every stat.

**Why it matters:** TESTING §2.1 requires at/below/above rows; off-by-one mutants survive without them.

**How to apply:** grep each package for `const` limits and `<=`/`>=`/`>` against them; write one
table with rows named after the boundary; for parsers of numbers, add the overflow edge. List
unreached blocks with `awk -F'[: ,]' '$NF==0' cover.out`.

**Seen in:** internal/{chunk,search,reindex,entry}/boundary_test.go, internal/cli/stats_since_test.go · 2026-10-29
