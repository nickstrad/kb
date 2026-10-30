# SQLite round(x, d) differs from DuckDB at midpoints

> Write `round(x * 10^d) / 10^d` in stats SQL; SQLite's `round(x, d)` rounds the stored double (137/40 → 3.42).

**Fact.** SQLite's `round(x, d)` rounds the exact binary double, so 137/40 = 3.42499… shows 3.42.
DuckDB (the old stats engine) and the Go percentile code show 3.43. Scaling first,
`round(x * 100) / 100`, gives 3.43 and matched the old binary on 225 of 225 runs.

**Why it matters.** Golden outputs and users' expectations break by one in the last digit.

**How to apply.** Use the scaled form for every rounded column in `internal/stats` SQL; keep
`--print-sql` output runnable.

**Seen in:** internal/stats/queries.go header comment; docs/DEFAULTS.md row 31 · 2026-10-29
