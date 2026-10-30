# Go has no MC/DC tool; the MC/DC requirement was dropped

> No Go tool measures MC/DC; decision 2026-10-29: use statement coverage plus table tests per branch outcome plus fuzz properties.

**Fact.** No mainstream Go tool measures MC/DC. `rillig/gobco` v1.3.4 measures condition
coverage (each condition seen true and false), runs one package at a time, takes `go test` flags
via `-test` (e.g. `-test -tags=fts5`), and copies the package to a temp dir, so it is slow.
Condition coverage does not prove each condition independently flips the decision.

**Decision 2026-10-29:** MC/DC requirement dropped (owner); use statement coverage + property tests.
gobco and `make cond` are no longer part of the tooling. docs/TESTING.md §4.2 now asks for a
table-driven test covering each branch outcome of decision-heavy functions, plus a fuzz property
where the input space is large. MC/DC may be revisited if a Go tool appears.

**Why it matters.** Without a tool, MC/DC could only be checked by hand in review, which is weak
evidence for the cost.

**How to apply.** For functions on the docs/TESTING.md §4.2 list, read the coverage profile to confirm
every branch ran, and write one table row per branch outcome. Do not reintroduce gobco unless the
owner reopens the question.

**Seen in:** docs/TESTING.md §4.1–4.2; docs/DEFAULTS.md rows 19, 21 · 2026-10-29

## reqproof evaluation (2026-10-29)

Source: https://reqproof.com/topics/mcdc-coverage-go (an article, not a repo or docs page).

- **What it is.** "Proof CLI" from a commercial vendor (reqproof.com, software assurance for agentic
  engineering). Version in the article: `0.1.0-dev`; the site says "Private beta". Command:
  `proof mcdc measure ./... --engine go` (instruments decisions, runs tests, reports independence
  pairs and short-circuit skips; JSON via `--format json`). So it does measure MC/DC for Go, by
  its own claim. The article's ZIP holds the example and a recorded report, not the CLI.
- **Unknowns.** No public repo, licence, price, install steps or source. Commercial engagements are
  quoted case by case. Not stated: cgo, `-tags fts5`, per-function gating, whether code or results
  go to a hosted portal (portal.reqproof.com exists). The only Go evidence is vendor-written (the
  author's dev.to post and PRs to jsonparser and tyk); no independent review found. The free
  "spec-lab" generator works on a decision spec, not on Go code.
- **Verdict: do not adopt now; revisit.** A closed, pre-1.0 beta cannot be pinned in `go.mod`,
  cannot be vetted for code egress, and its macOS, cgo and build-tag behaviour is unknown.
  The owner's rule (architecture-level or heavy new dependency: ask) also applies.
- **Revisit when** all hold: a public binary or source with a licence; runs offline; accepts
  `-tags fts5` and cgo packages on darwin/arm64 (and the GitHub macOS runner); gives per-function
  output we can threshold; a stable release.
- **Trial sketch if so.** Pilot on `internal/search` (`Search`, `ClampK`, `SanitizeFTS`) and
  `cli.runFeedback`; later `store.EnsureEmbedMeta`. A non-gating `make mcdc` target writes the JSON
  and a small `.agent/scripts` helper lists decisions without independence pairs; the gate
  stays code review of the table tests until the numbers are stable. Table rows (TESTING.md 4.3)
  stay the way to close gaps; `testing-standards-pass` would read the report for the 4.2 list.
