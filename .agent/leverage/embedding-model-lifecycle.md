# Embedding model lifecycle
> Every embedding command pays a ~0.2 s model load, and embedding peaks near 700 MB; downloads and the model cache are an external boundary.

- **Category:** resource/performance · external boundary
- **Status:** watch
- **Where:** `internal/embed/local` (lazy load, RWMutex, `Pull` with ctx), `internal/cli/setup.go` (`loadConfig`: the model comes from `[embed] model`, the `--embedder`/`--embed-model` flags are gone; which commands may download), rembed v0.3.0 (sets `REMBED_CACHE` in-process).
- **Why it matters:** the load cost lands on every hybrid search. Memory peaks limit concurrent agents on one machine. A corrupt or partial cache must fall back, never crash or download during search.
- **Evidence:** spike (docs/DEFAULTS.md "Spike results"): warm load 185–205 ms, query 3.3 ms, 33 ms/chunk, about 700 MB RSS while embedding batches of 16; int8 slower on Apple Silicon. Q10 Close/Embed race (fixed).
- **Coverage today:** local unit and concurrency tests (93%); `-tags model` contract tests; harness `no-model-search` and `no-model-index`.
- **Next checks:** memory profile at batch sizes 4/8/16/32 (pick a default; `embed.batch` is now a config key, default 16); RSS with 3–5 concurrent kb processes; whether a long-lived helper process is ever worth it (an architecture change, so ask the owner); upgrading rembed with pinned-model parity tests.
- **Last reviewed:** 2026-10-30 · W5 wave-end pass (model choice moved to config; reindex precondition now a Conflict on a changed embed_meta)
