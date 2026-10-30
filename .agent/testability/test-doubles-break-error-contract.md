# Some test doubles return errors production never returns
> Fakes inject bare errors or sentinels where the real dependency returns an `*errs.Error` with a kind, so production code keeps re-wrapping branches that matter only in tests, and real classification bugs hide.

- **Smell:** hidden coupling
- **Status:** watch
- **Where:** `internal/cli/model_test.go:TestModelCommandsCacheDirFailure` (fake `deps.modelCacheDir`), `internal/cli/model.go:112,131` (`usageErr("%s", err)`), the embedder stubs in cli tests (`add_regression_test.go:unavailableAddEmbedder`); TESTING.md §7 (doubles)
- **Symptoms in tests:**
  - Q61: production `modelCacheDir` already returns Usage, but the test injects a plain error, so the `usageErr` flattening is load-bearing only in tests.
  - Q39 (W3): `unavailableAddEmbedder` returned a bare `embed.ErrUnavailable` sentinel and went Internal (exit 3), while the real embedder returns the Unavailable kind. The test checked behaviour production could never reach.
  - The good counter-example is `embed/embedtest`, a contract suite every embedder runs, so `fake` and `local` agree on kinds.
- **Why it happens:** `deps` functions and small stubs have no contract stating which error kinds they may return, so each test picks whatever is handy.
- **Remedy:** (1) A TESTING.md §7 line: a double returns only errors the real implementation can return (same kind). Build them with `errs.New`/`errs.Wrap`. (2) Fix Q61 as queued. (3) Add `testing-standards-pass`/`testability-check` signal: a stub returning `errors.New` for a dependency whose doc names a kind. → **queue Q61** + a TESTING.md edit at the next wave-end pass.
- **Cost/benefit:** tiny. It prevents tests that pass on paths users never take.
- **Last reviewed:** 2026-10-30 · testability first pass
