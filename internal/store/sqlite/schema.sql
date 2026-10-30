CREATE TABLE entries (
  id          INTEGER PRIMARY KEY,
  path        TEXT NOT NULL UNIQUE,          -- 'droplet/README.md' or 'foo.md'
  kind        TEXT NOT NULL CHECK (kind IN ('file','dir')),
  title       TEXT NOT NULL,
  summary     TEXT NOT NULL,
  tags        TEXT NOT NULL DEFAULT '[]',    -- JSON array
  updated     TEXT,                          -- from front matter, verbatim
  verified    TEXT,                          -- from front matter, verbatim (may be NULL)
  body_hash   TEXT NOT NULL,                 -- sha256 of all indexed files, concatenated in path order
  indexed_at  TEXT NOT NULL                  -- ISO-8601 UTC
);

CREATE TABLE chunks (
  id          INTEGER PRIMARY KEY,
  entry_id    INTEGER NOT NULL REFERENCES entries(id) ON DELETE CASCADE,
  ord         INTEGER NOT NULL,
  source_file TEXT NOT NULL,                 -- repo-relative
  heading     TEXT NOT NULL DEFAULT '',      -- '' for the summary chunk
  text        TEXT NOT NULL,
  text_hash   TEXT NOT NULL,
  UNIQUE (entry_id, ord)
);

-- Contentless FTS index over chunks: rowid = chunks.id. title is denormalised in for the bm25
-- weight; text and heading are read back from chunks by joining on rowid, and the tag filter is a
-- join to entries.tags, not an FTS column.
CREATE VIRTUAL TABLE chunks_fts USING fts5(
  text, heading, title,
  content='', contentless_delete=1,
  tokenize='porter unicode61'
);
-- NOTE: contentless means SQLite stores no copy of the column values, so the columns are
-- WRITE-ONLY: MATCH and bm25() work, SELECT text FROM chunks_fts does not. The store code keeps
-- this table in step with chunks by hand -- one insert per new chunk and
-- `DELETE FROM chunks_fts WHERE rowid = ?` per removed chunk (contentless_delete=1 is what makes
-- that delete legal), inside the same transaction as the chunks write.
-- The original external-content shape (content='chunks' with path/tags UNINDEXED) could not be
-- read or filtered at all: any non-MATCH access raised `no such column: T.title`, because chunks
-- has no title column.

CREATE VIRTUAL TABLE chunks_vec USING vec0(
  chunk_id  INTEGER PRIMARY KEY,
  embedding FLOAT[768] distance_metric=cosine
);

CREATE TABLE embed_meta (
  id         INTEGER PRIMARY KEY CHECK (id = 1),
  model      TEXT NOT NULL,
  dim        INTEGER NOT NULL,
  created_at TEXT NOT NULL
);                                           -- model/dim mismatch at startup => refuse, tell user to `kb reindex --all`

CREATE TABLE searches (
  id           INTEGER PRIMARY KEY,
  ts           TEXT NOT NULL,                -- ISO-8601 UTC
  query        TEXT NOT NULL,
  mode         TEXT NOT NULL,                -- hybrid | fts | vec | fts-fallback
  k            INTEGER NOT NULL,
  tag_filter   TEXT,
  embed_model  TEXT,
  n_fts        INTEGER NOT NULL DEFAULT 0,   -- candidates returned by FTS
  n_vec        INTEGER NOT NULL DEFAULT 0,   -- candidates returned by vec0
  n_returned   INTEGER NOT NULL,
  fts_ms       INTEGER, vec_ms INTEGER, embed_ms INTEGER, total_ms INTEGER,
  caller       TEXT NOT NULL DEFAULT 'unknown',
  kb_version   TEXT                          -- git short hash of the repo at search time
);

CREATE TABLE search_results (                -- what was shown to the caller
  search_id    INTEGER NOT NULL REFERENCES searches(id) ON DELETE CASCADE,
  rank         INTEGER NOT NULL,
  chunk_id     INTEGER,                      -- NOT a reliable join key after a reindex: chunks.id is
                                             -- an autoincrement-free rowid, so a reindex both drops
                                             -- ids and REUSES them for unrelated chunks. Keep it for
                                             -- forensics only; entry_path and heading below are the
                                             -- columns to trust.
  entry_path   TEXT NOT NULL,
  heading      TEXT NOT NULL,
  fts_rank     INTEGER, fts_score REAL,      -- bm25() value (negative, lower is better), NULL if not in FTS list
  vec_rank     INTEGER, vec_distance REAL,   -- NULL if not in vec list
  rrf_score    REAL NOT NULL,
  PRIMARY KEY (search_id, rank)
);

CREATE TABLE search_candidates (             -- D5: full fused list before truncation (top 40)
  search_id    INTEGER NOT NULL REFERENCES searches(id) ON DELETE CASCADE,
  fused_rank   INTEGER NOT NULL,
  chunk_id     INTEGER,                      -- same warning as search_results.chunk_id: reindexing
                                             -- reuses chunk ids, so this may point at a different
                                             -- chunk than the one that was ranked.
  entry_path   TEXT NOT NULL,
  heading      TEXT NOT NULL,
  fts_rank     INTEGER, fts_score REAL,
  vec_rank     INTEGER, vec_distance REAL,
  rrf_score    REAL NOT NULL,
  returned     INTEGER NOT NULL DEFAULT 0,   -- 1 if it made it into search_results
  PRIMARY KEY (search_id, fused_rank)
);

CREATE TABLE search_feedback (
  search_id  INTEGER NOT NULL REFERENCES searches(id) ON DELETE CASCADE,
  rank       INTEGER NOT NULL,
  useful     INTEGER NOT NULL CHECK (useful IN (0,1)),
  note       TEXT,
  ts         TEXT NOT NULL,
  PRIMARY KEY (search_id, rank)
);
