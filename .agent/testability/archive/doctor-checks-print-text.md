# kb doctor checks write their verdict as text
> Each check prints an `ok`/`warn`/`FAIL` line directly, so tests assert level and wording together, and the checks inline in `runDoctor` can't be unit-tested.

- **Smell:** output-format coupling · decision+effect mixed
- **Status:** resolved (2026-10-30)
- **Where:** `internal/cli/doctor.go:doctorState` (`ok`/`warn`/`fail` print immediately), `runDoctor` (the entries and model checks are inline), `checkSources`, `checkEmbedMeta`
- **Symptoms in tests:**
  - 42 `"FAIL …"`/`"ok   …"`/`"warn …"` string checks across 8 test files (14 in `w6_crosslane_test.go`). `TestCheckEmbedMetaDecisions` is a good table, but every row's `want` is a text prefix such as `"FAIL embed_meta: not set, but 1 entry is indexed"`. So the Q29 plural fix had to update `TestNoVectorsMessages`, a decision test, as well as add its own row.
  - Q60: the `entries` FAIL (Discover error) and the `model` FAIL (embedder factory error) in `runDoctor` are mutation survivors, reachable only through a full `kb doctor` run.
  - Q55: `checkSources` will gain overlap and missing-dir checks when 1.5 lands, and each one adds more text rows.
- **Why it happens:** the verdict (a check name and a level) and its rendering happen in one call, and some checks are not functions at all.
- **Remedy:** (1) `doctorState` records `[]doctorCheck{Name, Level, Detail}` and prints them once at the end. (2) Tests assert `(Name, Level)` and check `Detail` only where the wording is the contract. (3) Lift the inline checks into `checkEntries`/`checkModel` functions with narrow inputs (the Discover result, the factory error). → with **Q55/Q60** when 1.5's sources check lands, else a small **new item**.
- **Cost/benefit:** small (one file). It closes Q60 cheaply, and every future doctor check gets a one-row table test.
- **Resolution:** T1 step 3: `doctorState` records `[]doctorCheck{Name, Level, Detail}` and `print` writes them once, with the summary line; `wantCheck` (init_test.go) asserts level and name as fields and the detail as a prefix (`TestCheckEmbedMetaDecisions`, `TestCheckEmbedder`). The entries and model checks were lifted into functions earlier (Q60).
- **Last reviewed:** 2026-10-30 · T1 (archived)
