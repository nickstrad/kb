# Brief (Opus): port the embedding plugin onto kb v2 (branch feat/embedding-plugin-v2)

You are in the worktree /root/Software/kb-work/embedding-plugin-v2 on branch feat/embedding-plugin-v2, based on main 898addb (kb v2). Nick's direction, verbatim: "Have opus plug-in work merge and push and have an opus agent do whatever to update kb repo in vm-1 to work with latest state and merge and commit too."

## What exists

- The plugin was built earlier on branch feat/embedding-plugin @ 72ee067 (worktree /root/Software/kb-work/embedding-plugin — read its code and docs/embedding-plugin.md there). It was built against the STALE pre-v2 base (ad5c811), so it does not apply to v2 directly. It adds: a public `embed` package (contract + provider Registry), providers ollama / openai / http / command / fake, kb resolving through the registry with a KB_ env prefix, docs/embedding-plugin.md, and a STATE.md. Astra reviewed it: PASS after one repair round that fixed (1) a registry-built provider leaking another env prefix's variables (KB_EMBED_MODEL leaking into a non-KB_ registry — fixed via newClient reading no environment; regression test TestRegistryIgnoresKBEnvironment) and (2) the command provider classifying a nonexistent program path as ErrUnavailable instead of a configuration error (regression test TestMissingAbsoluteProgramIsAConfigurationError). Neither bug may regress in the port.
- kb v2 (your base, 898addb) restructured embeddings: internal/embed holds the Embedder interface, None, and a caching wrapper; internal/embed/local is an in-process embedder using rembed (pure Go) with Hugging Face model ids (default BAAI/bge-small-en-v1.5, 384-dim), models pulled once into a shared per-machine cache and only by index-building commands (reindex/add/init) — search/show/doctor never download. Selection is config-driven: config.Embed.Model is a supported model id or "none" (FTS only); internal/cli/setup.go buildEmbedder constructs local.New or embed.None and wraps with the embed cache. embed_meta records Model()+Dim(), and Model() must name every setting that changes vectors (see internal/embed/cached.go).

## Port rules (binding)

(a) kb v2's current behavior is the baseline — defaults after the port must be exactly v2's defaults (provider selection, env names, embed_meta no-mixing guard). A config that names nothing must behave byte-for-byte as v2 does today: local rembed embedder, default model BAAI/bge-small-en-v1.5, same pull-only-on-index-commands and cache semantics; model "none" stays FTS-only.
(b) Land the plugin's value in v2's idiom: the public embed package + provider registry usable by other projects, plus the http and command providers.
(c) Where v2 already built an equivalent mechanism, adopt v2's and drop the duplicate from the port, noting each such decision in STATE.md and your final summary.
(d) No Hugging Face dependency anywhere in what the port adds: a user on a locked-down machine must be able to select the http or command provider with zero HF involvement. (v2's existing local provider is baseline under (a) and stays as it is; the port simply must not add new HF requirements.)
(e) Full suite `go test -tags fts5 ./...` green and `go vet ./...` clean on the ported branch.

## Design expectations

- Public package at github.com/nickstrad/kb/embed exposing the Embedder contract and a provider Registry (providers register by name; other projects can import it and register their own). kb's CLI resolves its embedder through the registry. Keep internal/embed working as v2 has it where rule (c) applies — e.g. v2's local embedder becomes the registry's built-in "local" provider rather than being rewritten.
- New providers: "http" (generic API embeddings: base URL + API key + model, configurable in v2's config idiom) and "command" (runs a user-supplied program: JSON texts on stdin, JSON vectors on stdout; kb never loads model weights in-process). Their Model() strings must satisfy the embed_meta rule: name every setting that changes vectors (endpoint + model for http; program + args for command).
- Provider selection must extend v2's config (config.Embed) in v2's idiom — existing config files and flags behave identically; the new selection defaults to the v2 behavior. The old branch's ollama/openai providers: carry them over as additional registered providers ONLY if they fit the v2 contract cleanly without disturbing any default; otherwise drop them and note it under rule (c).
- Tests: port/keep the old branch's provider-switching tests adapted to v2, keep both Astra regression tests working in substance (registry env isolation; command missing-program classification), and add coverage that default resolution equals v2's local embedder.

## Finish

Commit on feat/embedding-plugin-v2 and push the branch. Create STATE.md on this branch (carry the old branch's STATE.md forward, rewritten for the port) recording: status, what was kept from the plugin, what was adopted from v2 (rule (c) decisions), test counts. Print a final summary: commits, files changed, the kept-vs-adopted list, `go test -tags fts5 ./...` totals, go vet result.
