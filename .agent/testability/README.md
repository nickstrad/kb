# Testability

The few places in kb where the design itself makes tests brittle, slow, non-hermetic, or hard or
impossible to write. Agents point them out as the code grows, so the refactor that fixes them
lands in a PLAN item that already touches the area instead of being worked around in every new
test.

A **testability entry** is a design property, not a missing test. Typical shapes:

- **hidden coupling**: a test of A must build B, C and D because A reaches them directly;
- **global or process state**: package-level vars used as seams, the process cwd, env vars,
  a shared `deps` that tests swap fields on (no `t.Parallel`, cleanup-order coupling);
- **wide interfaces**: a function takes `store.Store` to call one method, so a double must
  implement twenty;
- **hard-wired I/O, time, randomness, env**: `time.Now`, `os.Getenv`, `os.Stdin`, a stamp taken
  inside the store, so tests fix values after the fact or allow slack;
- **constructors or handlers doing work**: decisions interleaved with effects, so one rule can
  only be tested end to end;
- **output-format coupling**: a decision is observable only as human text, so tests pin wording;
- **untestable error paths**: an OS or driver failure with no fault seam; mutation survivors
  triaged "unreachable" or "driver-only";
- **duplicated rules**: the same table or decision in two places, each with its own partial tests;
- **shared fixtures and order dependence**: tests that write raw schema SQL, or harness scenarios
  timed with sleeps.

Each shape breaks one of the design rules in docs/CODING_STYLE.md §12 (from Aniche, *Effective
Software Testing*, ch. 7). Cite the rule in the entry's **Why it happens**:

| Smell | CODING_STYLE rule it breaks |
|---|---|
| decision+effect mixed | §12.1 domain vs infrastructure (functional core, imperative shell) |
| global/process state, hard-wired I/O/time/env | §12.2 controllability; §12.4 value in a parameter; §5 |
| output-format coupling | §12.3 observability (structured result, typed error) |
| untestable error path | §12.2 fault hook in the adapter |
| wide interface, hidden coupling | §12.5 coupling; §2 consumer interfaces |
| duplicated rule | §12.1 one rule in one pure function (`store.EdgeResolver`) |
| shared fixture/order dependence | §12.3 narrow reads instead of raw schema; §12.5 a hard test is a design signal |

How it differs from its neighbours:

| Folder | Holds | Example |
|---|---|---|
| `leverage/` | runtime weak points where a failure costs users the most | SQLite write contention |
| `testability/` | design properties that make the *tests* of an area weak or costly | CLI tests share one mutable `deps`, so none can run in parallel |
| `queue/low-findings.md` | one small mechanical fix | a missing table row, a stale comment |

A testability entry often explains a cluster of queue findings (the same survivor shape keeps
coming back). When the remedy is a one-file change, put it in the queue instead.

**Curated, not exhaustive:** about 10–15 active entries. Adding one means it outranks something
already here, or that something here is retired. Entries are **evidence-based, not taste**: each
cites concrete tests, workarounds, mutation survivors or queue ids. "I'd have designed it
differently" is not an entry.

## Entry format (`<kebab-name>.md`)

```
# <Title>
> one-line summary: the design property and what it costs the tests

- **Smell:** hidden coupling | global/process state | wide interface | hard-wired I/O/time/env |
  decision+effect mixed | output-format coupling | untestable error path | duplicated rule |
  shared fixture/order dependence
- **Status:** watch | needs work | resolved (<date>)
- **Where:** file:symbol, one per line or comma-separated
- **Symptoms in tests:** concrete brittle, missing or contorted tests, with evidence: test names,
  mutation survivors, workaround code, queue (Q/H) ids, counts
- **Why it happens:** the coupling or design choice behind the symptoms, and the
  CODING_STYLE §12 rule it breaks
- **Remedy:** the refactor, smallest step first; which PLAN item (or "new item") should absorb it
- **Cost/benefit:** rough size of the change vs what it unlocks (parallel tests, killed survivors,
  fewer fixtures, cheaper new tests)
- **Last reviewed:** <date> · <wave or item>
```

`watch` means the smell is real but contained (few tests pay for it, or the area is stable).
`needs work` means new tests keep paying for it, or it hides a gap. `index.md` lists every active
entry with its status and one-line summary, ranked, and is kept in sync
(`go run ./.agent/scripts/index-check`).

## Rules

- **Evidence or it doesn't go in.** Cite at least one concrete symptom per entry. Re-check the
  evidence when you review it; drop claims that no longer hold.
- **Name a home.** Every remedy names the PLAN item that touches the area next, or says "new
  item". The orchestrator decides; the entry makes the cheap moment visible.
- **Read before you build.** Before designing or reviewing work in an area listed here, read its
  entry and apply the remedy if your item already owns the files.
- **Review every wave.** The wave-end pass runs the `testability-check` skill
  (`.claude/skills/testability-check/`) and updates the entries the wave touched.
- **Retire, don't delete.** When the remedy lands, or the code is removed, archive the entry
  (`archive/README.md`). If only part was fixed, narrow the entry instead.

## Lifecycle

spotted (a pass, a mutation run, an agent's report) → entry `watch` or `needs work` → remedy
absorbed by a PLAN item or fix-up → re-checked by `testability-check` → `resolved (<date>)` with a
**Resolution** line → moved to `archive/`.
