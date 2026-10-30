# kb context budgets the whole output, not just the hits

> `render.Render` sizes only the hits; `kb context` adds a heading and footer, so it re-renders with a smaller budget until the whole Markdown fits.

**Fact.** `cli/context.go` `fitContext` calls `render.Render(hits, b)`, composes the Markdown frame (heading, hits, footer) and measures the frame plus body with `render.EstimateTokens`. If it overshoots the budget, `b` drops by the overshoot and it repeats; it stops at budget 1, where only the frame and the "raise --budget" note remain. Budget 0 is unlimited. `kb search` does not do this: its `search N` line and `search_id=` footer are not counted.

**Why it matters.** The exact-fit case (budget = size of the unlimited output) must return the unlimited output with no tail, which is tested. Below the frame's own size the output can exceed the budget; that is the one documented exception.

**How to apply.** Change the context layout in `contextBlock`/`contextFooter` and the budget still holds. Do not port `render`'s block format into context; its level logic is unexported, so `contextBody` mirrors the level numbers 1–3 and the tail wording.

**Seen in:** internal/cli/context.go, internal/cli/context_test.go · 2026-10-30
