# Store seam, errs and the top-level report: where things live

> Services use small interfaces in internal/store; all SQL is in internal/store/sqlite; errors become *errs.Error at module boundaries and cli/report.go alone decides messages and exit codes 0/1/2/3.

**Fact.** `store.Index`, `store.Retriever`, `store.SearchLog` (+ `store.Store`) are the seam;
`sqlite.Open(ctx, path, sqlite.Options{ReadOnly})` is the only schema-version check. Every sqlite
method wraps once via `wrap()` (known SQLite codes → Conflict/Usage/Interrupted, else Internal).
The CLI gets everything replaceable through `deps` (embedder, model cache, pull, openStore,
editor, userCacheDir); tests use the package-level `testDeps` in main_test.go. `report` prefixes
every well-formed message with the command path (`kb add: …`), so `usageErr` messages must not
include it. Commands that already printed their diagnostics return `exitSilently(code)`.

**Why it matters.** A raw error that reaches `report` is an incident and exit 3. New code that
returns plain `fmt.Errorf` across a package boundary turns user mistakes into "please report it".

**How to apply.** User-input failures in the CLI: `usageErr(...)` (it keeps the first error arg as
the cause). Service errors: return them as they are. Driver failures are reachable in tests
through the fault driver in internal/store/sqlite/fault_test.go (`fail(t, sqlSubstring, nth)`).
Backend-neutral behaviour belongs in internal/store/storetest.
Services (search, reindex, stats) wrap with `errs.KindOf(err)`, never a fixed kind: a fixed
`Internal` (stats.Run before the W3 pass) turns a locked or damaged database (Conflict) into an
incident and exit 3. Fixed kinds belong only where the service itself knows the cause (stats.SQL:
the user wrote the SQL → Usage, except a Conflict). A message passed to `usageErr` must not start
with "kb <cmd>:" (secretsErr did, printing the prefix twice). Test the decision of a CLI helper
with a scripted `store.Index` (embed the interface, override the methods the helper calls), as
`TestEnsureEmbedMetaForReindexDecision` does.

**Seen in:** internal/store, internal/store/sqlite, internal/errs, internal/cli/report.go · 2026-10-29; internal/stats/run.go, internal/cli/add.go · 2026-10-30
