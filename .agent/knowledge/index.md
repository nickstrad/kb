# Agent knowledge

Durable project insights for future agents, one entry per line. Keep this list in sync with the
folder (check with `go run ./.agent/scripts/index-check`). Not here: judgment calls awaiting the
owner (docs/DEFAULTS.md, local only) and work to do (docs/PLAN.md).

- [rembed-spike-numbers.md](rembed-spike-numbers.md) — bge-small via rembed: ~0.2 s per search, ~33 ms/chunk to index; int8 is slower on Apple Silicon.
- [rembed-model-cache-and-downloads.md](rembed-model-cache-and-downloads.md) — rembed downloads only for hub ids; kb loads from `<cache>/<org>--<name>` so search never downloads.
- [sqlite-round-midpoints.md](sqlite-round-midpoints.md) — use `round(x*10^d)/10^d` in stats SQL; SQLite `round(x,d)` differs from DuckDB at midpoints.
- [macos-bsd-sed-in-tests.md](macos-bsd-sed-in-tests.md) — no GNU `sed -i` (or other GNU flags) in tests; use the POSIX-sh fake editor or Go.
- [go-dot-dirs-ignored.md](go-dot-dirs-ignored.md) — `./...` skips `.agent/`; run and test helpers by explicit path.
- [sqlite-vec-cgo-warnings.md](sqlite-vec-cgo-warnings.md) — sqlite-vec's `sqlite3_auto_extension` deprecation warnings on macOS are harmless.
- [go-no-mcdc-tool.md](go-no-mcdc-tool.md) — no Go MC/DC tool; MC/DC dropped 2026-10-29 for statement coverage + branch-outcome tables + fuzz properties; reqproof (closed beta) evaluated, not adopted.
- [vec0-width-is-lazy.md](vec0-width-is-lazy.md) — chunks_vec exists only after EnsureEmbedMeta(dim>0) at FLOAT[dim]; handle "no such table"; tests may use any width.
- [signal-notifycontext-needs-ctx-everywhere.md](signal-notifycontext-needs-ctx-everywhere.md) — NotifyContext swallows SIGINT; calls that ignore ctx make Ctrl-C a no-op.
- [wave-end-pass-boundary-audit.md](wave-end-pass-boundary-audit.md) — limits were tested only above, never at; grep constants + cover profile to find gaps.
- [sqlite-deferred-tx-locked.md](sqlite-deferred-tx-locked.md) — deferred `BeginTx` bypasses busy_timeout in WAL; concurrent `kb add` gets "database is locked"; use `_txlock=immediate`.
- [mutation-survivor-patterns.md](mutation-survivor-patterns.md) — symbol-relative constants, fast-path `<=` checks and driver-only error paths survive mutation without being test gaps; `-diff` results before 2026-10-29 are unreliable.
- [reindex-prune-rediscover.md](reindex-prune-rediscover.md) — `reindex --all` prunes only rows that a second Discover after ListEntries also misses; a concurrent `kb add` is otherwise deleted.
