# Mutation survivors that are not test gaps

> Two survivor shapes recur in kb and are usually not gaps: constants that tests only ever use symbolically, and `<=`/`<` at a fast-path check whose fallback gives the same answer.

**Fact.** In `internal/chunk` (2026-10-29), `MaxChunkChars 1500→1501/1499` survived even though
`boundary_test.go` has rows at MaxChunkChars-1, at it and +1. The rows compute their inputs from
the symbol, so they move with it. `chunk.go:101 len(full) <= MaxChunkChars` → `<` also survived.
At exactly the limit the section falls through to `splitOversized`, which returns the same single
piece. So the mutant is equivalent, and the at-limit row is still right. The final
`if len(cur) > 0` flush after a non-empty loop (`>`→`>=`) is equivalent too.

**Why it matters.** Filing these as gaps wastes a fix-up. Missing them hides real ones.

**How to apply.** For a `const` survivor, ask whether the value itself is a contract (documented
to users, stored on disk). If it is, pin it once (golden or a doc test); if not, mark it
symbol-relative. For a `boundary` survivor, check whether both branches give the same result at
that point. If they do, it is equivalent. Otherwise it is a missing row. Pair `boundaries` (are
rows there?) with `mutate -ops const,boundary` (do they discriminate?).

**Seen in:** internal/chunk/chunk.go:36,101,284; internal/chunk/boundary_test.go · 2026-10-29

**Tool caveat (fixed 2026-10-29).** Before the W2 pass, `mutate -diff` applied a different mutant
from the one it reported (the sandbox re-enumerated without the line filter), so `-diff` results
from before then are unreliable: re-run before triaging old survivor lists. `pickMutant` now
refuses any mismatch. Also: a driver-only error path (an `err != nil` after a sqlite call that
cannot fail in tests) survives every run; triage it once as "driver-only" (H20), not as a gap. Same for `sql.Open` with the registered sqlite3 driver (never errors) and `rows.Columns`/`rows.Scan(*any)` after a successful query.
