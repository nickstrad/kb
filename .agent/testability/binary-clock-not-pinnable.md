# The binary's clock cannot be pinned from outside
> `deps.now` pins search, open and feedback stamps only inside Go tests. A black-box run of the binary (harness, E2E) has no way to fix time, and several stamps bypass `deps.now` entirely.

- **Smell:** hard-wired I/O/time/env
- **Status:** needs work
- **Where:** `internal/cli/root.go:deps.now`/`clock` (productionDeps leaves it nil); `internal/store/sqlite/index.go:nowUTC` (entries.indexed_at, embed_meta created_at, and `stamp` for a zero time); `internal/migrate/run.go:backupPath(…, o.Now())`; `internal/cli/report.go` (incident time); `internal/cli/doctor.go` (timings)
- **Symptoms in tests:**
  - A harnessfake binary run prints wall-clock stamps, for example `kb feedback --last` → `(newest search for claude, 2026-10-30T19:23:02Z: …)`. Any transcript must normalise them.
  - Time-window behaviour cannot be driven black-box without real waiting: `log.open_window` (show counts as an open), `stats --since 7d`, `stats trend` weeks, `eval --since`, and later `kb stale` (5.4, `stale.days`). The harness never probes these, and PLAN E1 journeys J1/J14 need them.
  - Migrate backup names (`.bak-v5-<time>`) are only checkable by glob.
- **Why it happens:** the clock is injected per `deps` for in-process CLI tests, but nothing outside the process can set it, and the store and migrate layers read `time.Now` themselves.
- **Remedy:** (1) PLAN **E1**: in a harnessfake build only, `KB_HARNESS_NOW` (RFC3339) sets `productionDeps().now` (one line in root.go via a `harnessNow` var set in harnessfake.go). Release builds never read it, so D4 holds. (2) Later, if E2E needs `indexed_at` or backup names exact: pass `deps.clock` into `sqlite.Open` options and `migrate.Options.Now`. Until then, E2E normalises those stamps.
- **Cost/benefit:** (1) is about 20 lines and unlocks deterministic time-window journeys and harness scenarios. (2) is a small store-option change, and only worth it if a test needs it.
- **Last reviewed:** 2026-10-30 · E2E planning (PLAN Phase 7)
