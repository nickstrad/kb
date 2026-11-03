# kb: STATE

**Goal:** kb, the knowledge-store CLI (see README.md). Current work item: port the embedding
provider plugin onto kb v2, from the brief at
[docs/embedding-plugin-v2-brief.md](docs/embedding-plugin-v2-brief.md). Design and the
kept-vs-adopted list: [docs/embedding-plugin.md](docs/embedding-plugin.md). This replaces the
first plugin branch (`feat/embedding-plugin` @ 72ee067, built on the pre-v2 base).

**Status:** LANDED on main 2026-11-03 at Nick's direction: merged at 31a77c5 (merge of
feat/embedding-plugin-v2 @ 35d2fa5) and pushed. Astra confirm review PASS (all six checks).
Suite on the merged main on this VM shows only the six accepted Linux baseline failures
(identical on untouched 898addb). Target-platform (darwin/arm64) verification remains open.

## Done

- `embed:` public `github.com/nickstrad/kb/embed` (contract + `Registry`), `embed/httpembed`
  (`http`), `embed/command` (`command`), `embed/builtin`. No Hugging Face or rembed import.
- `embed:` kb resolves through the registry: `internal/embed` aliases the public contract,
  `internal/embed/local.Provider` makes v2's local embedder the `local` provider (Default),
  `internal/embed/provider` is kb's glue (`NewRegistry`, `Explicit`, `Open`, `Kinded`,
  `Register`), `[embed] provider` plus `[embed.http]` / `[embed.command]` in config.
- `docs:` docs/embedding-plugin.md rewritten for v2, embed/README.md, README "Other embedding
  providers", CLI help line, config package doc.
- Commit SHAs: see `git log main..feat/embedding-plugin-v2`.

### Kept from the first plugin branch

- The public `embed` package and `Registry` (`Provider`, `Option`, `Config`, `Env`,
  `Resolve`/`Open`/`OpenEnv`, `NoneProvider`).
- The `http` provider (the OpenAI-shape client with configurable path, auth header and scheme)
  and the `command` provider with its JSON stdin/stdout protocol and error classification.
- Astra regression 1 (registry env isolation), in substance:
  `embed/builtin.TestRegistryIgnoresKBEnvironment` (an `OTHER_` registry ignores every `KB_*`
  variable for every built-in provider) plus `internal/cli.TestKBIgnoresEmbedderEnvironment`.
- Astra regression 2: `embed/command.TestMissingAbsoluteProgramIsAConfigurationError`, unchanged,
  plus the CLI check that a missing program is a Usage error (`TestProviderFailuresKeepKBExitCodes`).
- The provider-switching CLI test (`TestSwitchingBetweenCommandAndHTTPProviders`), now driven by
  the config file.
- The `TestExtraProviderRegisteredInKB` idea (`provider.Register`).

### Adopted from v2 instead (rule (c))

1. Default provider: v2's in-process `local` embedder (rembed, `BAAI/bge-small-en-v1.5`), wrapped
   as the registry Default, not rewritten. The branch's Ollama default and 768 `DefaultDim` dropped.
2. Configuration source: v2's config file only. kb resolves with `embed.NoEnv()`; the branch's
   `KB_EMBEDDER` / `KB_EMBED_*` variables and `--embedder` / `--embed-model` flags are not ported.
3. Secrets: `[embed.http] api_key_env` names a variable instead of kb reading `KB_EMBED_API_KEY`.
4. Batching: v2's `embed.batch`; no kb-level `KB_EMBED_BATCH`.
5. Prefixes: v2's rule (as-is unless the embedder has `Prefixer`); the nomic fallback is gone.
6. Error kinds: v2's `errs.Unavailable` / `errs.Usage`, via `provider.Kinded`.
7. Caching: v2's shared embedding cache wraps every provider (keyed by `Model()`).
8. `none`: v2's `model = "none"`, FTS only with any provider.
9. Test double: v2's `internal/embed/fake`; the branch's public `embed/fake` not ported.
10. Dropped: `ollama`, `openai`, `openrouter` providers and registry `Auto`. `http` reaches all
    three services; `Auto` existed only so an env key could take over the default, which v2's
    config-only selection rules out.

### Judgment calls (for Nick's review)

- `Model()` identity strings: `http "<endpoint>" model="<m>"[ send_dimensions][ truncate][ doc_prefix=".."][ query_prefix=".."]`
  and `command ["prog" "arg"...] model="<m>"[...]`, every free-form field Go-quoted. Key, auth
  header, batch, timeouts excluded.
- Provider needs (`dim`, `http.url`, `command.program`) are checked at build time
  (`config.Embed.Needs`), not in `Validate`, so a switch works one `--set` at a time.
- `embed.command.args` must be single words (passed to the provider as a space-joined option).
- `kb model pull` / `kb model path` with a non-local provider: Usage error.
- `embedderSet` keyed by the `[embed]` table's JSON (the struct now holds pointers and a slice).

## Tests (this VM: linux/amd64, root)

- `go vet ./...` and `go vet -tags fts5 ./...`: clean (also `-tags fts5,harnessfake`).
- `go test -tags fts5 ./...` on the port: 3554 pass, 7 skip, 6 fail. Untouched base 898addb on
  the same VM: 3505 pass, 7 skip, the same 6 fail. The port adds 49 passing tests and no failure.
- The 6 are environmental and pre-existing: 4 permission tests (`TestReadFoundFlag`,
  `TestReadLocation`, `TestDiscoverFilesNonRegular`, `TestFanOutHardErrors`) cannot fail as root;
  run as `nobody` on a copy they pass (non-root full suite: 3565 pass, 2 fail). The other 2
  (`TestExactListImproves`, `TestSearchGoldenDefaults`) are search goldens recorded on
  darwin/arm64 that differ in the last float digit on amd64. The repo targets macOS; a green run
  there is still to be confirmed.
- E2E journeys (`-tags fts5,e2e`, `GOFLAGS=-buildvcs=false` in a worktree): fail identically on
  the base and the port (macOS `Library/Caches` paths and the version string in the goldens).

## Repair round (Astra's findings on 17192aa)

Review evidence (Astra): `/tmp/plugin-review-probe_test.go` and
`/tmp/plugin-review-probes.log` reproduce identity collisions and invalid response indexes.
`/tmp/plugin-review-targeted.log` records passing provider-switching, default-resolution,
environment-isolation, missing-program and timeout tests. `go vet ./...` exited 0.
Full `go test -tags fts5 ./...` runs on 17192aa and base 898addb both failed the same six
tests documented above (`/tmp/plugin-review-tests.log`, `/tmp/plugin-review-base-tests.log`).
An external Go module successfully imported the public registry/built-ins; its dependency
list contains no kb internal, rembed, or Hugging Face packages. Review artifacts are temporary.

- Finding 1, identity: `httpembed.Client.Model()` and `command.Client.Model()` Go-quote the
  endpoint, model and prefixes (command argv was already quoted), and HTTP adds `send_dimensions`.
- Finding 3, HTTP indexes: `embedBatch` rejects a duplicate, negative or out-of-range index
  (each of `0..n-1` exactly once) before reordering.
- Regression tests: `embed/httpembed.TestSendDimensionsChangesIdentity`,
  `embed/httpembed.TestModelIdentityIsCollisionFree`,
  `embed/httpembed.TestEmbedRejectsInvalidResponseIndexes` (7 subtests),
  `embed/command.TestModelIdentityIsCollisionFree`. All four fail on 17192aa (checked in a
  scratch worktree with only the new test files copied in) and pass after. Astra's probe file
  (`/tmp/plugin-review-probe_test.go`) fails 4/5 before and passes 5/5 after.
- Existing identity assertions updated to the quoted form (`provider_test.go`, `command_test.go`,
  `internal/cli/embed_provider_test.go`); docs/embedding-plugin.md updated.
- After repair: `go vet ./...` and `go vet -tags fts5[,harnessfake] ./...` clean;
  `go test -tags fts5 ./...` fails exactly the six pre-existing tests listed under Tests.
- Third verdict ground (suite not green): the same six fail on untouched base 898addb; out of
  scope (root permissions, darwin float goldens).

## Confirm review (Codex, 2026-11-03)

- Original independent probes: all five pass, including HTTP timeout/cancellation.
- Committed repair tests pass; all four fail when copied onto pre-repair 17192aa.
- Default local resolution, none/FTS, automatic download boundaries, environment isolation,
  missing-program classification and provider switching pass.
- External module imports public registry/built-ins and runs; dependency graph has no kb internal,
  rembed or Hugging Face packages (`-buildvcs=false` needed for external dependency listing here).
- Fresh `go test -count=1 -tags fts5 -json ./...`: 3565 pass, 7 skip, exactly the six accepted
  baseline failures above; `go vet ./...` clean. darwin/arm64 remains unverified.
- Evidence: `/tmp/plugin-confirm-evidence/{suite.json,targeted.log,before.log,vet.log,external-deps.txt}`.
  Temporary artifacts; no product changes, commits or merges made by this review.

## In progress

Nothing running.

## Next steps

1. Run `go test -tags fts5 ./...` on darwin/arm64 to confirm green on the target platform.
2. Retire the old `feat/embedding-plugin` branch (superseded by this port; Nick's call).

## Open decisions / blockers

- Merge to main: Nick, after review.
- Whether the old `KB_*` variables should keep working for one release, which needs
  an env layer on top of the config file. Not done; Nick decides.

## Resume

- Worktree `/root/Software/kb-work/embedding-plugin-v2`, branch `feat/embedding-plugin-v2`
  (pushed to origin).
- Verify: `go vet -tags fts5 ./... && go test -tags fts5 ./...`.
- No background runs belong to this item.
