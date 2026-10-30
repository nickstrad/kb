# Fault injection exists only inside internal/store/sqlite
> The fault-injecting driver is unexported and test-only, so other packages fake disk errors with tricks or leave their OS/driver error paths as "unreachable" survivors.

- **Smell:** untestable error path
- **Status:** needs work
- **Where:** `internal/store/sqlite/fault_test.go` (`faultDriver`, `fail(t, sql, nth)`), `sqlite.go:Options.driver` (unexported), `internal/embed/cache/cache.go:Open` (own SQLite file, no seam), `internal/config/config.go:readCapped`
- **Symptoms in tests:**
  - `cli/report_test.go:TestDiskFullIsExit1` produces SQLITE_FULL by pinning `PRAGMA max_page_count` through `st.DB()` with one connection. It works for plain tables only, so the sqlite-vec full-disk path (Q47) has no test at all.
  - Embedding cache mutation (W2): the survivors are `Open`/`init` error paths ("directory is a file", schema create). The leverage entry `embedding-cache-shared-file` lists an `Open` error-path table as a next check.
  - Q62: `readCapped`'s limited-read branch (a file that grows between stat and read) is "defensive, unreached by tests; cover it only if a test seam for the reader appears".
  - `mutation-survivor-patterns.md` and H26 triage driver-only error paths outside sqlite as non-gaps by default, so real gaps there go unseen.
  - H20 shows what a seam buys: the fault driver gave 63 fault cases inside sqlite.
  - Fixed (Q125): the driver failed statements, not rows, so the `rows.Scan` and `rows.Err` branches of `Related` and `Superseded` survived mutation. `failRows(t, match, nth, rowsIter|rowsScan)` now fails the nth row of a query; `TestDriverFaultsOnRows` covers Related, Superseded, Edges, Entries and Chunks (mutation on those readers: 61 killed, 0 survived). Other scan loops (FTS, KNN, log reads) can join that table by adding a row.
- **Why it happens:** H20 built the fault driver as a `_test.go` file behind an unexported option, so only that package can reach it.
- **Remedy:** (1) Move the fault driver into an internal test-support package (`internal/store/sqlite/sqlitetest`, or `internal/sqlfault` shared by sqlite and cache). (2) Let `cache.Open` take an options value with the same driver hook. (3) Leave `readCapped` as triaged unless an `open func` seam comes almost free. (4) DONE: the driver fails the nth `Rows.Next` or Scan of a matched query (a `driver.Rows` wrapper). Remaining: 1-3. → **new item** (Sonnet), or the cache's leverage next-check. Do it before 3.2 adds more log writes.
- **Cost/benefit:** small to medium. The cache's degrade-never-corrupt claim becomes testable per statement, and cli tests stop needing PRAGMA tricks.
- **Last reviewed:** 2026-10-30 · queue drain (remedy 4 landed; Q126 closed through the cli `openStore` seam, which needed no driver)
