# Embedding model lifecycle
> Every embedding command pays a ~0.2 s model load, and embedding peaks near 700 MB; downloads and the model cache are an external boundary.

- **Category:** resource/performance · external boundary
- **Status:** watch
- **Where:** `internal/embed/local` (lazy load, RWMutex, `Pull` with ctx), `internal/cli/setup.go` (`loadConfig`: the model comes from `[embed] model`, the `--embedder`/`--embed-model` flags are gone; which commands may download), rembed v0.3.0 (sets `REMBED_CACHE` in-process).
- **Why it matters:** the load cost lands on every hybrid search. Memory peaks limit concurrent agents on one machine. A corrupt or partial cache must fall back, never crash or download during search.
- **Evidence:** spike (docs/DEFAULTS.md "Spike results"): warm load 185–205 ms, query 3.3 ms, 33 ms/chunk, about 700 MB RSS while embedding batches of 16; int8 slower on Apple Silicon. Q10 Close/Embed race (fixed).
- **W6 (1.4) + wave-end pass:** `kb init` downloads the model up front (exit 2 leaves the store created; `kb model pull` finishes); doctor's `model` check loads the model and embeds "ping" with timings (`TestCheckEmbedderModelCheck`); the model cache path has one rule, `workspace.ModelsDirOf` (`local.DefaultCacheDir` deleted, Q54). Fixed in the pass: inside a location, `kb model pull|path` with a `--store` or `default_store` that names no store silently used the default model; now NotFound (`TestModelCommandsNeedTheNamedStore`).
- **Coverage today:** local unit and concurrency tests (93%); `-tags model` contract tests; harness `no-model-search` and `no-model-index`.
- **Next checks:** memory profile at batch sizes 4/8/16/32 (pick a default; `embed.batch` is now a config key, default 16); RSS with 3–5 concurrent kb processes; whether a long-lived helper process is ever worth it (an architecture change, so ask the owner); upgrading rembed with pinned-model parity tests.
- **Last reviewed:** 2026-10-30 · W7 wave-end pass (no model-loading change in W7; cache wiring is tracked in embedding-cache-shared-file)
