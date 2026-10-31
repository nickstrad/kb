-- The index DB baseline: schema v1, step index/0001-baseline (migrations.go). It is the squash of
-- every pre-release step (D8): a fresh store is created from this file and nothing else. FROZEN
-- from the first release on: a schema change is a new step appended in migrations.go, never an
-- edit here (TestBaselineSchemaGolden fails on one).

CREATE TABLE entries (
  id          INTEGER PRIMARY KEY,
  path        TEXT NOT NULL UNIQUE,          -- 'droplet/README.md' or 'foo.md'
  kind        TEXT NOT NULL CHECK (kind IN ('file','dir')),
  title       TEXT NOT NULL,
  summary     TEXT NOT NULL,
  tags        TEXT NOT NULL DEFAULT '[]',    -- JSON array, as written in the front matter
  updated     TEXT,                          -- from front matter, verbatim
  verified    TEXT,                          -- from front matter, verbatim (may be NULL)
  body_hash   TEXT NOT NULL,                 -- sha256 of all indexed files, concatenated in path order;
                                             -- '' forces the next kb reindex to rebuild the entry
  indexed_at  TEXT NOT NULL,                 -- ISO-8601 UTC
  aliases     TEXT NOT NULL DEFAULT '[]'     -- JSON array of the entry's other names (wikilink targets)
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
-- join to entry_tags, not an FTS column.
CREATE VIRTUAL TABLE chunks_fts USING fts5(
  text, heading, title,
  content='', contentless_delete=1,
  tokenize='porter unicode61'
);
-- NOTE: contentless means SQLite stores no copy of the column values, so the columns are
-- WRITE-ONLY: MATCH and bm25() work, SELECT text FROM chunks_fts does not. The store code keeps
-- this table (and chunks_exact) in step with chunks by hand -- one insert per new chunk and
-- `DELETE FROM chunks_fts WHERE rowid = ?` per removed chunk (contentless_delete=1 is what makes
-- that delete legal), inside the same transaction as the chunks write.

-- The identifier index: a second contentless FTS index over chunks (rowid = chunks.id) whose
-- tokenizer keeps identifiers whole (`foo_bar`, `pkg.Func`, `--flag`, `path/to`, `a:b`: no porter,
-- no diacritic folding). chunks_exact_vocab exposes its terms.
CREATE VIRTUAL TABLE chunks_exact USING fts5(
  text, heading,
  content='', contentless_delete=1,
  tokenize="unicode61 remove_diacritics 0 tokenchars '_-.:/'"
);
CREATE VIRTUAL TABLE chunks_exact_vocab USING fts5vocab(chunks_exact, 'row');

-- The vector table chunks_vec is not here: EnsureEmbedMeta creates it at the model's width
-- (vec0, chunk_id = chunks.id, cosine) when it records the model in embed_meta.

CREATE TABLE embed_meta (
  id         INTEGER PRIMARY KEY CHECK (id = 1),
  model      TEXT NOT NULL,
  dim        INTEGER NOT NULL,
  created_at TEXT NOT NULL
);                                           -- model/dim mismatch at startup => refuse, tell user to `kb reindex --all`

-- An entry's outgoing links: body links (rel 'links') and front-matter lists (rel = the key), kind
-- 'path' (a root-relative path) or 'wikilink' (a name as written). dst_entry_id is the entry the
-- target resolves to, NULL while it dangles, and becomes NULL when that entry goes.
CREATE TABLE edges (
  src_entry_id INTEGER NOT NULL REFERENCES entries(id) ON DELETE CASCADE,
  dst_path     TEXT NOT NULL,
  dst_entry_id INTEGER REFERENCES entries(id) ON DELETE SET NULL,
  rel          TEXT NOT NULL,
  kind         TEXT NOT NULL CHECK (kind IN ('path', 'wikilink')),
  PRIMARY KEY (src_entry_id, dst_path, rel, kind)
);
CREATE INDEX edges_dst ON edges (dst_entry_id);

-- The tag filter's table: the entry's tags normalised (store.NormTag).
CREATE TABLE entry_tags (
  entry_id INTEGER NOT NULL REFERENCES entries(id) ON DELETE CASCADE,
  tag      TEXT NOT NULL,
  PRIMARY KEY (entry_id, tag)
);

-- The search log: the only data in the file that cannot be rebuilt from the Markdown. Every
-- migration preserves its rows.
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
  corpus_rev   TEXT,                         -- git short hash of the corpus repo at search time
  kb_version   TEXT,                         -- the kb binary's version
  settings      TEXT,                        -- effective [embed]/[search]/[prior] settings, canonical JSON
  settings_hash TEXT,                        -- sha256 of settings, for kb stats settings
  fts_query     TEXT                         -- the FTS MATCH expression that ran, NULL if none
);

-- Columns shared by search_results and search_candidates:
--   chunk_id   NOT a reliable join key after a reindex: chunks.id is an autoincrement-free rowid,
--              so a reindex both drops ids and REUSES them for unrelated chunks. Forensics only;
--              entry_path and heading are the columns to trust.
--   fts_score  bm25() value (negative, lower is better); fts_rank/fts_score NULL if not in the FTS list
--   vec_*      NULL if not in the vector list
--   store      NULL = a hit of this store; else the canonical ref NAME@ABSROOT of the store that
--              returned it (a cross-store search), whose chunk id chunk_id is
--   exact_*    the hit's place in the identifier (chunks_exact) list, NULL if not in it
--   prior      the feedback-prior multiplier (already folded into rrf_score), NULL = prior inactive
--   fts_tier   the tier of the main full-text list that placed the hit, NULL = untiered / not in it
--   memory_*   the hit's place in the query-memory list and its score, NULL if not in it
CREATE TABLE search_results (                -- what was shown to the caller
  search_id    INTEGER NOT NULL REFERENCES searches(id) ON DELETE CASCADE,
  rank         INTEGER NOT NULL,
  chunk_id     INTEGER,
  entry_path   TEXT NOT NULL,
  heading      TEXT NOT NULL,
  fts_rank     INTEGER, fts_score REAL,
  vec_rank     INTEGER, vec_distance REAL,
  rrf_score    REAL NOT NULL,
  store        TEXT,
  exact_rank   INTEGER, exact_score REAL,
  prior        REAL,
  fts_tier     TEXT CHECK (fts_tier IN ('phrase', 'near', 'and', 'or')),
  memory_rank  INTEGER, memory_score REAL,
  PRIMARY KEY (search_id, rank)
);

CREATE TABLE search_candidates (             -- D5: full fused list before truncation (top 40)
  search_id    INTEGER NOT NULL REFERENCES searches(id) ON DELETE CASCADE,
  fused_rank   INTEGER NOT NULL,
  chunk_id     INTEGER,
  entry_path   TEXT NOT NULL,
  heading      TEXT NOT NULL,
  fts_rank     INTEGER, fts_score REAL,
  vec_rank     INTEGER, vec_distance REAL,
  rrf_score    REAL NOT NULL,
  returned     INTEGER NOT NULL DEFAULT 0,   -- 1 if it made it into search_results
  store        TEXT,
  exact_rank   INTEGER, exact_score REAL,
  prior        REAL,
  fts_tier     TEXT CHECK (fts_tier IN ('phrase', 'near', 'and', 'or')),
  memory_rank  INTEGER, memory_score REAL,
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

-- The implicit signal from `kb show`: an entry opened after a search. store is NULL for an entry
-- of this store, else the canonical ref of the store it was opened from.
CREATE TABLE search_opens (
  search_id  INTEGER NOT NULL REFERENCES searches(id) ON DELETE CASCADE,
  store      TEXT,
  entry_path TEXT NOT NULL,
  ts         TEXT NOT NULL
);
CREATE UNIQUE INDEX search_opens_key ON search_opens (search_id, ifnull(store, ''), entry_path);
