# The binary's clock cannot be pinned from outside
> `deps.now` pins search, open and feedback stamps only inside Go tests. A black-box run of the binary (harness, E2E) has no way to fix time, and several stamps bypass `deps.now` entirely.

- **Smell:** hard-wired I/O/time/env
- **Status:** partly resolved (E1 landed remedy 1; remedy 2 open)
- **Where:** `internal/cli/root.go:deps.now`/`clock` (productionDeps leaves it nil); `internal/store/sqlite/index.go:nowUTC` (entries.indexed_at, embed_meta created_at, and `stamp` for a zero time); `internal/migrate/run.go:backupPath(…, o.Now())`; `internal/cli/report.go` (incident time); `internal/cli/doctor.go` (timings)
- **Symptoms in tests:**
  - Remedy 1 is in use: `first-run` and `agent-workflow` pin `KB_HARNESS_NOW`, so search, open and feedback stamps and `stats --since`/open_window are deterministic in journeys (`TestHarnessClock*` in harnessfake_test.go).
  - Still unpinned: `entries.indexed_at` and embed_meta `created_at` (`sqlite.nowUTC`), migrate backup names (`.bak-v<N>-<time>`, checked by glob only), and `kb index`'s "Last reviewed" date (Q96: `entry-lifecycle` greps one row instead of comparing index.md). `kb stale` (5.4) landed on `d.clock()` and reads `verified`/`updated` from front matter, not `indexed_at`, so it did not need remedy 2: the `links-and-stale` journey (J14) moves the clock with `env KB_HARNESS_NOW=+200d` and asserts exact ages.
- **Why it happens:** the clock is injected per `deps` for in-process CLI tests, but nothing outside the process can set it, and the store and migrate layers read `time.Now` themselves.
- **Remedy:** (1) DONE in E1 (2026-10-30, `harnessClock` in harnessfake.go, `harnessNow` in root.go; journeys pin and advance it with `env KB_HARNESS_NOW=...`): in a harnessfake build only, `KB_HARNESS_NOW` (RFC3339) sets `productionDeps().now` (one line in root.go via a `harnessNow` var set in harnessfake.go). Release builds never read it, so D4 holds. (2) Later, if E2E needs `indexed_at` or backup names exact: pass `deps.clock` into `sqlite.Open` options and `migrate.Options.Now`. Until then, E2E normalises those stamps.
- **Cost/benefit:** (1) is about 20 lines and unlocks deterministic time-window journeys and harness scenarios. (2) is a small store-option change, and only worth it if a test needs it.
- **Last reviewed:** 2026-10-30 · W11 wave-end pass (5.4 needed only remedy 1; remedy 2 → Q96 or the first test that needs `indexed_at`)
