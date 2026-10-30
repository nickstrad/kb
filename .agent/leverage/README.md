# Leverage points

The few places in kb where a weakness costs the most: integration points under contention,
resource management that drives performance, data-integrity boundaries, and external boundaries
(filesystem, network, process lifetime). Agents point them out as the project grows, so they get
validated, tested (including adversarially), performance-tested, improved, or pivoted on
deliberately rather than by accident.

**Curated, not exhaustive:** at most about 10 active entries. Adding one means it outranks
something already here, or that something here is retired.

## Entry format (`<kebab-name>.md`)

```
# <Title>
> one-line summary: what it is and why it's high leverage

- **Category:** contention | resource/performance | data integrity | external boundary | process lifetime
- **Status:** watch | needs work | validated (<date>) | retired (<reason>)
- **Where:** code paths
- **Why it matters:** what breaks, for whom, and how badly
- **Evidence:** measurements, findings, incidents (Q/H queue ids, knowledge entries)
- **Coverage today:** which tests, harness scenarios and mutation runs probe it
- **Next checks:** the concrete validations or experiments worth doing (perf benchmark,
  harness scenario, load test, design review), each a candidate PLAN item or H entry
- **Last reviewed:** <date> · <wave or item>
```

`index.md` lists every entry with its status and one-line summary, and is kept in sync
(`go run ./.agent/scripts/index-check`).

## Retiring entries

When a leverage point is refactored or redesigned so the weakness is gone, stripped out of the
code, or outranked, it doesn't stay active. Move it to `archive/` following
`archive/README.md`: status `retired (<reason>, <date>)` plus a **Resolution** line. If only part
of it was resolved, update the entry instead and narrow its scope. The `leverage-check` skill
(`.claude/skills/leverage-check/`) validates the folder against the code and proposes these
moves.
