# Compact search rendering and --budget

> kb search's human output comes from internal/render (greedy elastic levels, ceil(runes/4) tokens); search.Hit carries no summary or updated date, so the CLI looks them up per hit.

**Fact.** `render.Render(hits, budget)` renders the hit section only (the `search N · M hits` line and
`search_id=` footer belong to cli and are not counted). Levels: 1 header (`[r] path › heading ·
updated D`), 2 + entry summary, 3 + chunk text (cap 600 runes; never cheaper than level 2). Budget 0 =
all level 3. Under a budget: all-level-3 if it fits (the only tail-free state), else the largest
header prefix with a tail, then, if every hit is shown, upgrade in rank order to 2 then 3, each pass
stopping at the first miss. Tail: `(+N hits not shown, K condensed; raise --budget)`. Tokens are
measured on the whole composed string (not a sum of parts), so the budget holds exactly and is
monotone in levels. `search.Hit` has no summary/updated; `cli.renderHits` reads them with
`Store.Entry`. RRF score is JSON only. `[output] budget` is the default; `--budget` (even 0) wins.

**Why it matters.** 4.2 (`--expand`) expands text before allocation: feed bigger `Text` into
`render.Hit`. 4.3 (`kb context`) should call `render.Render` with `output.context_budget`.

**How to apply.** Change wording: `go test -tags fts5 ./internal/render -update` and review goldens.

**Seen in:** internal/render/render.go, internal/cli/search.go · 2026-10-30
