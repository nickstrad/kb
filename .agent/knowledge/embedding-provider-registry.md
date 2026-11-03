# Embedding provider registry: how kb picks its embedder

> kb resolves its embedder through the public `embed` registry with `NoEnv`; "local" is the Default and stays internal; a non-local embedder's `Model()` carries endpoint/program, so any change to them needs `kb reindex --all`.

**Fact:** `cli.buildEmbedder` order is: `d.embedder` test hook, `model = "none"` (FTS only, any
provider), `harnessModel` (local only), then `provider.Open(provider.NewRegistry(d.modelCacheDir),
cfg.Embed)`. The registry holds `local` (Default, `internal/embed/local.Provider`), `http`,
`command`, `none`, then `provider.Register` extras. Resolution uses `embed.NoEnv()`, so no `KB_*`
variable selects a provider; only `[embed.http] api_key_env` reads a named variable (through
`embed.Config.LookupEnv`, injectable). Non-local embedders are wrapped by `provider.Kinded`
(ErrUnavailable → `errs.Unavailable`, else `errs.Usage`, caller cancellation as is).
`internal/embed.Embedder` etc. are aliases of the public types; `ErrUnavailable` is the same var.
`config.Embed.HTTP`/`Command` are pointers with omitempty, so `default.toml` and `EffectiveJSON`
are unchanged when no provider is named; `override.clone` deep-copies them and `resolve` derefs
pointer sub-tables for `--set`. `embedderSet` is keyed by the table's JSON (Embed is not a
usable map key any more). What a provider needs (dim, url, program) is checked at build time by
`config.Embed.Needs`, not in `Validate`, so a switch can be made one `--set` at a time.

**Why it matters:** changing any of this can silently change v2's default (a config naming no
provider must behave byte-for-byte as before), or make provider errors exit 3 (Internal) instead
of 1/2.

**How to apply:** test a provider end to end with `internal/cli/embed_provider_test.go` (its
`TestMain` makes the test binary the `command` program; `fakeEmbeddingsAPI` is the `http` API).
A changed URL/path/program is a different `Model()` and trips the embed_meta guard, so a test
that wants a down API toggles one server rather than starting a new one.

**Seen in:** embed/, internal/embed/provider/, internal/cli/setup.go, internal/config/config.go, docs/embedding-plugin.md · 2026-11-03

**Independent review (17192aa, 2026-11-03):** identity completeness is not yet established.
With truncation enabled, changing `send_dimensions` can return different vectors at the same
`Model()` and `Dim()`, bypassing both the shared-cache key and the index compatibility guard.
Unescaped model names also collide with identity suffixes (`model="m truncate"` versus
`model="m", truncate=true` in HTTP; `model='m doc_prefix="x"'` versus `model="m", doc_prefix="x"`
in command). The HTTP decoder sorts indexes but does not validate uniqueness
or range: replies with indexes `[0,0]`, `[-1,1]`, and `[0,9]` for two inputs were accepted.
Future tests must assert these boundaries, not just correctly reordered replies and one-option
identity changes. Independent probe source and output: `/tmp/plugin-review-probe_test.go` and
`/tmp/plugin-review-probes.log` (temporary review artifacts).

**Repair (2026-11-03):** fixed. `Model()` now Go-quotes every free-form field
(`http "<endpoint>" model="<m>"[ send_dimensions][ truncate][ doc_prefix=".."][ query_prefix=".."]`,
`command ["prog" "arg"] model="<m>"[...]`), so no value can imitate a flag or field, and HTTP
includes `send_dimensions`. The HTTP decoder rejects any reply whose indexes are not exactly
`0..n-1`, each once (a server that omits `index` therefore fails as "index 0 more than once").
Guarded by `TestModelIdentityIsCollisionFree` (httpembed and command: exhaustive cross-product
of hostile values, any two distinct settings must differ), `TestSendDimensionsChangesIdentity`
and `TestEmbedRejectsInvalidResponseIndexes`. When adding a vector-changing option to a
provider, add it to `Model()` quoted and to the cross-product in that test.

**Independent confirmation (7284b3a, 2026-11-03):** original review probes pass; all four
committed repair regressions fail on 17192aa and pass on the repair. Full Linux/root suite has
only the six accepted baseline failures; vet is clean. See STATE.md for evidence locations.
