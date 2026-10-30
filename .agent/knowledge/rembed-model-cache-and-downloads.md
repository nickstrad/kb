# rembed downloads only for hub ids; kb loads from a local dir

> rembed.Load downloads when given `hf:org/name` (or a missing `org/name`) into `$REMBED_CACHE`; kb passes a local dir so search never downloads.

**Fact.** `rembed.Load(ref)` treats an existing directory as a local model. An `org/name` id that
is not a local dir, or any `hf:` ref, is fetched from the Hugging Face Hub into `$REMBED_CACHE`
(default: the user cache dir), laid out as `<cache>/<org>--<name>`. kb keeps its own cache at
`os.UserCacheDir()/kb/models` with the same layout, calls `rembed.Load(<absolute dir>)` in
`Embed`, and sets `REMBED_CACHE` plus `hf:` only inside `local.Pull`. A model counts as cached
when `config.json` and `model.safetensors` both exist (weights are written last).

rembed's `hub.Ensure` skips files already in the cache and parses `config.json` before fetching
anything else, so `Pull` over a cache holding a garbage `config.json` fails with no network
(`TestPullCorruptCacheFailsOffline`); it also deletes that file as cleanup.

**Why it matters.** Passing a bare id would make a search silently hit the network.

**How to apply.** Never pass a model id to `rembed.Load` outside `Pull`; load from `ModelDir()`.
Tests that need a model use the `model` build tag and fail (not skip) when it is missing.
CLI tests never see the real cache: `internal/cli/main_test.go` (TestMain) points
`modelCacheDir` at an empty temp dir and makes `pullModel` fail; `stubPull` in `model_test.go`
fakes a successful download. Keep that when `deps` replaces these seams.

**Seen in:** internal/embed/local/local.go; internal/cli/setup.go, main_test.go; rembed v0.3.0 rembed.go `Load` · 2026-10-29

**Concurrency (2026-10-29, Q10):** rembed v0.3.0's `Embedder` is safe for concurrent `Embed` and
must not be used after `Close`. `internal/embed/local` holds a read lock across each model call,
and `Close` takes the write lock. A separate `loadMu` serialises the lazy load: taking the write
lock while other goroutines hold read locks deadlocked parallel first use. Tests inject a
blocking fake through the `Embedder.load` seam (`concurrency_test.go`).
