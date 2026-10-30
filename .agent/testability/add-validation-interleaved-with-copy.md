# kb add interleaves its validation with store, embedder and filesystem effects
> `runAddFile` (125 lines) and `runAddDir` (153) check names, front matter, secrets, the embedder and the destination between opening the store and copying files, so every rule and the "fails before copy" ordering are tested end to end.

- **Smell:** decision+effect mixed
- **Status:** watch
- **Where:** `internal/cli/add.go:runAddFile`, `runAddDir`, `checkAddDestination`, `rollbackAdded`
- **Symptoms in tests:**
  - Every rule is a full CLI run on a made location: `TestAddBadNameIsRejected`, `TestAddMissingSummaryIsRejected`, `TestAddSecretPatternIsRejected`, `TestAddDuplicateIsRejected`, `TestAddDirWithoutReadmeIsRejected`, and others (16 add tests in `add_test.go`/`add_regression_test.go`).
  - The ordering property ("nothing is copied when X fails") has to be proven per X, end to end: `TestAddRejectsMissingFrontMatterBeforeCopy`, `TestAddRejectsBinaryCompanionBeforeCopy`, `TestAddFileFailsBeforeCopyWhenEmbedderUnusable`. Q11 was the bug this shape allowed: the Ping came after the copy.
  - Only `scanSecrets` was pulled out as a pure function, and it is the one with direct table rows (`TestAddSecretPatternsAllowLocationProse`).
- **Why it happens:** the handler is one sequence, so the copy step's position relative to each check is implicit in line order.
- **Remedy:** split into `planAdd(src, files, cfg) (addPlan, error)`, which is pure: name rules, front matter, secrets, companion files and destination path. `applyAdd(ctx, plan, st, embedder)` then pings, copies, rolls back and indexes. Table-test `planAdd` per rule. Keep two or three CLI tests for wiring, plus one test that an apply failure rolls back. → **1.5** owns `add.go` in W7 (in-place sources adds a second path through it). If 1.5 lands without it, the next item that touches `add.go`, or a **new item**.
- **Cost/benefit:** medium (two handlers). Each new add rule becomes one table row, and "before copy" holds by construction instead of by a test per rule.
- **Last reviewed:** 2026-10-30 · testability first pass (1.5 was editing add.go; re-check)
