# rembed spike numbers (darwin/arm64)

> In-process bge-small costs ~0.2 s per search and ~33 ms per chunk to index; int8 is slower on Apple Silicon.

**Fact.** PLAN 0.1 spike, rembed v0.3.0, bge-small-en-v1.5, M-series Mac: warm load 185–205 ms,
one query embed ~3.3 ms, cold load with download 3.7 s; indexing ~33 ms/chunk in batches of 16
(~33 s per 1,000 chunks), peak ~700 MB while embedding a batch; model cache 129 MB. int8
quantisation was *slower* (39 vs 33 ms/chunk) and saved only ~60 MB: its speedups need x86
AVX-VNNI, which Apple Silicon lacks. rembed links fine beside mattn/go-sqlite3 + sqlite-vec.

**Why it matters.** These are the baselines for performance budgets and the reason int8 is off.

**How to apply.** Compare new timings against these before claiming a regression or a win; don't
re-propose int8 for macOS without new evidence; if reindex memory bites, try batches of 8.

**Seen in:** docs/DEFAULTS.md "Spike results" and row 6 · 2026-10-29
