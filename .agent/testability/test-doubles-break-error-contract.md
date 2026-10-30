# Some test doubles return errors production never returns
> Fakes inject bare errors or sentinels where the real dependency returns an `*errs.Error` with a kind, so production code keeps re-wrapping branches that matter only in tests, and real classification bugs hide.

- **Smell:** hidden coupling
- **Status:** watch
- **Where:** `internal/cli/setup_test.go:TestNewEmbedderKinds` (`errFakeCache`, a plain `errors.New` returned by `deps.modelCacheDir`); TESTING.md §7 (doubles). Landed, for the record: `internal/cli/model_test.go:TestModelCommandsCacheDirFailure` (Q61), `internal/cli/add_regression_test.go:unavailableAddEmbedder` (Q39)
- **Symptoms in tests:**
  - Q75 (open, queue): `errFakeCache` is a plain error where production `modelCacheDir` returns Usage, so the test checks a branch production cannot reach. The queue entry is the fix (`errs.New(errs.Usage, …)`) plus the TESTING.md §7 line.
  - Landed: Q61 (W6) changed `TestModelCommandsCacheDirFailure` to inject an `errs.Usage` error. Q39 (W3) made `unavailableAddEmbedder` wrap `embed.ErrUnavailable` with the Unavailable kind, as the real embedder does.
  - The good counter-example is `embed/embedtest`, a contract suite every embedder runs, so `fake` and `local` agree on kinds.
- **Why it happens:** `deps` functions and small stubs have no contract stating which error kinds they may return, so each test picks whatever is handy.
- **Remedy:** (1) Q75, then the TESTING.md §7 line: a double returns only errors the real implementation can return (same kind). Build them with `errs.New`/`errs.Wrap`. (2) A `testability-check` / `testing-standards-pass` signal: a stub returning `errors.New` for a dependency whose doc names a kind. → **queue Q75** + TESTING.md §7 at the next wave-end pass; the signal is a future check, not a PLAN item.
- **Cost/benefit:** tiny. It prevents tests that pass on paths users never take.
- **Last reviewed:** 2026-10-30 · W7 pass
