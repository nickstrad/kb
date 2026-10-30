# CLI decisions are observable only as stderr text and an exit code
> CLI tests can't see the error `execute` reported, so they check kinds and branches by substring. That pins wording to logic, and many distinct failures share one Usage kind.

- **Smell:** output-format coupling
- **Status:** needs work
- **Where:** `internal/cli/root.go:execute` (reports, then drops the error), `internal/cli/report.go:usageErr` (flattens every well-formed failure to `errs.Usage` + a formatted string), the test helpers `run` and `runKB`
- **Symptoms in tests:**
  - CLI tests have 244 `strings.Contains` checks on stdout, stderr or an output buffer (364 `strings.Contains` calls in all) against 17 `errs.KindOf`/`errors.Is`/`errors.As` lines (W7 count; was 195 vs 19 at the first pass). The gap is widening, not closing.
  - Wording changes ripple into decision tests: Q48 (a `--debug` line, `TestShowNoLog` adjusted), Q29 (plural, `TestNoVectorsMessages` updated), Q26 (`TestStatsErrorHasOneStatsPrefix`).
  - `TestSearchFTSMode` pins the whole human layout in a single regexp. That is a fair contract test, but it is also the only check of the hit count and order.
  - Bad name, reserved name, missing summary and secret found in `kb add` are all `Usage`, so the tests (`TestAddBadNameIsRejected`, `TestAddMissingSummaryIsRejected`, `TestAddSecretPatternIsRejected`) can only tell them apart by message.
- **Why it happens:** the tests' only entry point is `execute(…) int`, which writes the message and returns a code. The error value, with its kind and its chain, never reaches the test.
- **Remedy:** (1) Split `execute` so that a test variant returns `(code int, err error)` alongside the buffers. (2) Decision tests assert the kind, `errors.As` the typed cause (`*entry.ValidationError`, `*store.EmbedMetaMismatchError`) and the exit code. (3) Keep one wording test per message family, in `report_test.go`. (4) Where a branch matters to callers, give it a typed cause, not a new kind. → **new item** (Sonnet, test infra), paired with `cli-shared-test-deps`. New commands (2.3 eval, 3.2) should use the new helper from the start.
- **Cost/benefit:** medium (a helper, then gradual migration as files are touched). Wording edits stop breaking logic tests, and kind regressions (Usage↔Internal, exit 1↔3) fail with a precise message.
- **Last reviewed:** 2026-10-30 · W7 pass
