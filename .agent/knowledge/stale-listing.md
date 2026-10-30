# kb stale: date rule and reasons

> `kb stale` reads the index (Entries + Edges) against the injected clock; an entry is old when its coalesce(verified, updated) date is at least `--days` whole UTC days before now.

**Fact.**
- Pure core `buildStale(now, days, entries, edges)` in internal/cli/stale.go; the command only opens the index read-only and passes `d.clock()`.
- Date: verified when non-blank, else updated (blank = unset, as NULL in the index). Accepted: YYYY-MM-DD (midnight UTC) and RFC 3339. Anything else is "no date" with the raw value shown.
- Age = whole UTC calendar days from the date's UTC midnight to now's; a future date counts as 0. Old when `age >= days`, so `--days 0` lists every dated entry too.
- Reasons, each listing an entry separately: `no_date`, `old`, `superseded` (dst of a `supersedes` edge from another entry; a self-supersede is ignored, as in the search demotion), `broken_link` (`kind = 'path'` and no target, whatever the rel, so a dangling `supersedes` is a broken link). Wikilinks that dangle are not reported.
- Default threshold: `[stale] days`, default 180 (config). Exit 0 whether or not anything is listed.

**Why it matters.** The same rule decides what the J14 journey and any future stale report show; changing "at least" to "more than" moves every boundary entry.

**How to apply.** Keep the date rule in parseStaleDate/staleAgeDays and test changes in TestParseStaleDate and TestBuildStaleBoundary. Do not use time.Now (use the clock).

**Seen in:** internal/cli/stale.go, internal/cli/stale_test.go, internal/config/config.go, journey `links-and-stale` (J14), harness `links-stale` · 2026-10-30
