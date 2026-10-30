# Dumping a kb SQLite file as a text fixture: virtual-table quirks

> Contentless FTS5 can't be read back, sqlite-vec preallocates multi-MB blobs and doesn't mark all its shadow tables, and sqlite_sequence moves; the fixture dumpers handle each.

**Fact.**
- `chunks_fts` is contentless: its columns can't be SELECTed. Dump its shadow tables
  (`chunks_fts_data/_idx/_docsize/_config`) raw, after `DELETE FROM` (the CREATE seeds `_config`).
- sqlite-vec preallocates `chunks_vec_vector_chunks00` blobs (768 dims × 1024 rows ≈ 3 MB, 6 MB as
  hex), and does **not** mark that table `shadow` in `pragma_table_list` (only `_info`, `_chunks`,
  `_rowids`). Dump a vec0 table through its own columns (`chunk_id, embedding`) and skip its
  `<vtab>_*` tables and its `sqlite_sequence` rows (vec0 rebuilds them; copying them breaks
  inserts with "Error opening vector blob").
- `sqlite_sequence`'s position in `sqlite_schema` differs between a file and its reload, so the
  data section is ordered by table name, and it is emitted only when it has rows.
- `quote()` gives exact SQL literals (`X'…'` for blobs). `VACUUM INTO` is refused on a
  `_query_only` connection: back up through a read-write handle.

**Why it matters.** Per-version migration fixtures must be small, exact and reloadable, or the
"fixture == steps at its version" check is noise.

**How to apply.** Reuse `dumpFixture`/`ownedTables` in internal/store/sqlite (migrations_test.go,
artefact.go). Schema comparisons leave out tables a virtual table owns (`ownedTables`: shadow, or
`<vtab>_` prefix). A new artefact with a virtual table needs the same treatment.

**Seen in:** internal/store/sqlite/migrations_test.go, internal/store/sqlite/artefact.go · 2026-10-30
