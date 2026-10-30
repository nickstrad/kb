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

**More equivalent shapes (W3, 2026-10-30).** (1) A `>`→`>=` or `<`→`<=` inside a branch already
guarded by `a != b` (`search.fusedBefore`): the two differ only on equality, which the guard
excludes. (2) The sentinel values in an error return (`return 0, err`, `return false, err`) and the
format arguments of an error message (`"rank %d", i+1`): about a third of the store/sqlite
survivors on the 1.2 diff (H26). (3) An `err-check` whose skipped failure makes the very next
call fail anyway (`MkdirAll` before `OpenFile`). (4) The `if assert.Enabled` negation: behaviour
must not change with assertions off. Real gaps found the same run: tie-break levels of a total
order reachable only by calling the comparator directly, and rank/flag columns written to the
search log that no test read back.

(5) Hand-written scanners (links.go, W4): a run counter that starts at 0 → 1 when the first
character is already known to match; a `-1` "not found" sentinel → `-2` or `end < 0` → `<= 0`;
a resume index `i += 2+j+2` → `+1` that only re-reads a `]`. All 30 links survivors after the W4
pass are of these kinds. The real gaps there were adjacent constructs (`[a](a.md)[b](b.md)`,
`[[a]][b](b.md)`, two code spans around a link) and edges of character ranges (`a:`, `z:`,
`A:`, `Z:`, `x0:`, `x9:` as schemes): add rows with constructs packed side by side.

(6) W12: an `ORDER BY` that an index already yields (`ORDER BY c.ord LIMIT 1` in
`sqlite.LocateChunks`, where `UNIQUE (entry_id, ord)` makes the planner walk ord order) survives
even a test that writes rows out of order; keep the contract row anyway, it catches a schema or
index change. `make(..., len(a)+len(b))` capacity arithmetic (`+`→`-`) is equivalent. Not
equivalent, and cheap since Q125: `rows.Scan`/`rows.Err` err-checks in a store reader. Add the
query to a `failRows(t, match, 1, rowsIter|rowsScan)` table (`TestJudgedDriverFailures`).

**Seen in:** internal/chunk/chunk.go:36,101,284; internal/chunk/boundary_test.go · 2026-10-29; internal/search/search.go (fusedBefore), internal/store/sqlite/log.go · 2026-10-30; internal/links/links.go, internal/entry/entry.go (ParseFrontMatter) · 2026-10-30 W4 pass; internal/store/sqlite/{locate,judged}.go, internal/search/search.go (fuse) · 2026-10-30 W12 pass

**Tool caveat (fixed 2026-10-29).** Before the W2 pass, `mutate -diff` applied a different mutant
from the one it reported (the sandbox re-enumerated without the line filter), so `-diff` results
from before then are unreliable: re-run before triaging old survivor lists. `pickMutant` now
refuses any mismatch. Also: a driver-only error path (an `err != nil` after a sqlite call that
cannot fail in tests) survives every run; triage it once as "driver-only" (H20), not as a gap. Same for `sql.Open` with the registered sqlite3 driver (never errors) and `rows.Columns`/`rows.Scan(*any)` after a successful query.
