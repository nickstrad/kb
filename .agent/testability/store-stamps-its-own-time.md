# The store and the CLI read the wall clock themselves
> `sqlite` stamps searches, feedback and entries with `time.Now` internally, and `parseSince` and `kb show` call it too, so tests fix timestamps after the fact or allow slack.

- **Smell:** hard-wired I/O/time/env
- **Status:** needs work
- **Where:** `internal/store/sqlite/index.go:nowUTC` (used by `log.go` LogSearch/Feedback/FeedbackNone, `index.go` embed_meta and indexed_at), `internal/cli/stats.go:parseSince`, `internal/cli/show.go` (`RecordOpen(ctx, path, time.Now(), …)`)
- **Symptoms in tests:**
  - `internal/cli/stats_test.go` logs searches through `Searcher.Record` and then rewrites `searches.ts` with raw `UPDATE searches SET ts = ?` (lines 81, 99) to place them 30 days or 1 hour ago.
  - `TestParseSince` allows a one-minute `slack` because the bound is computed from `time.Now` inside the function.
  - H5: the `>=` vs `>` mutant on the `--since` bound needs a search logged exactly at the bound, which no test can arrange while both stamps come from the wall clock.
  - `RecordOpen` already takes `now` (the right shape: storetest checks the window edge exactly), but `LogSearch` does not, so the two halves of the open window are tested differently.
- **Why it happens:** the backend decides "now" for the rows it writes. Callers cannot pass a time.
- **Remedy:** (1) a `TS time.Time` field on `store.SearchRecord`, set by `search.Searcher` from a `Now func() time.Time` field (default `time.Now`), and the same for feedback. (2) `parseSince(s, now)`, with `deps.now` for the CLI. (3) Drop the `UPDATE … SET ts` fixups. → **3.2** (W8 rewrites `LogSearch`/`SearchRecord` for cross-store logging, mig v5), or **2.3** (eval replays need searches at chosen times).
- **Cost/benefit:** small (one field, one func param). It makes time-window tests exact, kills the H5 since-bound survivor, and 2.3/6.x (time-based priors) will need it anyway.
- **Last reviewed:** 2026-10-30 · testability first pass
