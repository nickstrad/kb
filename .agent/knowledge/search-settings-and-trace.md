# Search settings, the trace, and the defaults golden

> [search] reaches kb search as search.Settings (Params + EffectiveJSON + hash); a zero Settings means the defaults; testdata/golden/search_defaults.golden pins the default ranking bit for bit.

**Fact.** `search.NewSettings(cfg)` builds `Settings{Params, JSON, Hash}` from one validated
`config.Config`, so the params a search ran with and its `settings_hash` cannot disagree. The CLI
sets `Searcher.Settings`; a Searcher whose `Settings.Hash` is "" uses `DefaultSettings()` (every
older test builds Searchers that way). `Outcome` carries the trace: `Settings`, `FTSMatch` (the
expression that produced the FTS list, "" if none ran), `FTSFallbackToOr`, and per-candidate
`CapDropped` (set by `applyCap` only for chunks it visited and skipped for the cap, never for
unmapped chunks or those past k). `store.FTSQuery` gained `Weights *BM25Weights` (nil =
`store.DefaultBM25()`) and `ChunkIDs []int64` (nil = all, empty non-nil = nothing, still reports
Match); `store.ChunkMeta` gained EntryID, EntryUpdated, Ord, SourceFile. bm25 weights are bound
parameters (`bm25(chunks_fts, ?, ?, ?)`), which gives the same doubles as the old literals.
config bounds: list_limit and vec_overfetch ≤ 4096 (sqlite-vec's `SQLITE_VEC_VEC0_K_MAX`; 4097 is a
SQL error, pinned by `TestListLimitParamBoundary`), candidate_limit ≤ 8192, rrf_k ≤ 100000,
weights ≤ 1000, so no config value can produce an Inf score (which would make `--json` exit 3).
list_weights.exact/memory stay reserved: there is no such list yet.

**Why it matters.** Anything that changes ranking must show up as a diff of the golden. The golden
was written by the pre-2.1 code and passed unchanged after, and `TestSearchGoldenDefaults` also
checks that zero Settings and `NewSettings(config.Default())` render identically.

**How to apply.** A new ranking knob: add it to `config.Search` (+ Validate bounds, doc.go wired
list, a `TestEffectiveJSON` hash row), to `Params`/`ParamsFrom`, then a boundary row in
`params_boundary_test.go` and, if it is fusion arithmetic, a property in `FuzzFuse`. An intended
ranking change: `go test -tags fts5 ./internal/search -run TestSearchGoldenDefaults -update` and
review the diff. 2.5 should log `Outcome.Settings.Hash`/`JSON`; 2.2's single-column bm25 is
`FTSQuery{Weights: &{w,0,0}, ChunkIDs: ids}`.

**Seen in:** internal/search/settings.go, internal/search/golden_test.go, internal/store/sqlite/retrieve.go, internal/config/defaults.go · 2026-10-30
