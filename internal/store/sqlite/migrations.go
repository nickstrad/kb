package sqlite

import (
	"context"
	"database/sql"
	_ "embed"

	"github.com/nickstrad/kb/internal/assert"
	"github.com/nickstrad/kb/internal/migrate"
)

// schemaSQL is the baseline step, applied verbatim to a fresh database. Never edit it once a
// database exists in the wild: an edit would reach brand-new databases only.
//
//go:embed schema.sql
var schemaSQL string

// artefactName is the index DB's name in migration IDs and kb migrate output.
const artefactName = "index"

// indexSteps[v] takes a database from PRAGMA user_version v to v+1. Append-only: to change the
// schema, append a step (its ID pre-assigned in the plan's migration table) and leave every
// earlier one alone; SchemaVersion follows. Each Up runs inside the caller's BEGIN IMMEDIATE.
// It is never reassigned: a test that needs an older or a longer list passes its own through
// Options.steps or Artefact.steps, so tests in this package may run in parallel.
var indexSteps = migrate.Steps[execer]{
	{
		Info: migrate.Info{From: 0, ID: "index/0001-baseline", Desc: "create the index, FTS, vector and search-log tables"},
		Up: func(ctx context.Context, db execer) error {
			_, err := db.ExecContext(ctx, schemaSQL)
			return err
		},
	},
	// searches.kb_version held the corpus repo's git hash, so it becomes corpus_rev and a new
	// kb_version records the binary. search_opens holds the implicit signal from `kb show`.
	{
		Info: migrate.Info{From: 1, ID: "index/0002-log-provenance", Desc: "rename searches.kb_version to corpus_rev, add kb_version and search_opens"},
		Up: func(ctx context.Context, db execer) error {
			_, err := db.ExecContext(ctx, `
			ALTER TABLE searches RENAME COLUMN kb_version TO corpus_rev;
			ALTER TABLE searches ADD COLUMN kb_version TEXT;
			CREATE TABLE search_opens (
			  search_id  INTEGER NOT NULL REFERENCES searches(id) ON DELETE CASCADE,
			  entry_path TEXT NOT NULL,
			  ts         TEXT NOT NULL,
			  PRIMARY KEY (search_id, entry_path)
			);`)
			return err
		},
	},
	// chunks_vec's width becomes the model's, fixed when EnsureEmbedMeta creates it. The old
	// vectors go with their table and embed_meta, and every body_hash is cleared so `kb reindex`
	// re-embeds. FTS keeps working in between; the search log is untouched.
	{
		Info: migrate.Info{From: 2, ID: "index/0003-vec-width", Desc: "drop the fixed-width vector table; vectors are rebuilt at the model's width", Effect: migrate.EffectReembed},
		Up: func(ctx context.Context, db execer) error {
			_, err := db.ExecContext(ctx, `
			DROP TABLE IF EXISTS chunks_vec;
			DELETE FROM embed_meta;
			UPDATE entries SET body_hash = '';`)
			return err
		},
	},
	// Each search records the effective [embed]/[search] settings it ran with (canonical JSON and
	// its hash, config.EffectiveJSON) and the FTS MATCH expression, so `kb stats settings` can
	// compare configs and `kb explain` can say what ran. Older rows keep NULL: it is not known.
	{
		Info: migrate.Info{From: 3, ID: "index/0004-search-settings", Desc: "add searches.settings, settings_hash and fts_query"},
		Up: func(ctx context.Context, db execer) error {
			_, err := db.ExecContext(ctx, `
			ALTER TABLE searches ADD COLUMN settings TEXT;
			ALTER TABLE searches ADD COLUMN settings_hash TEXT;
			ALTER TABLE searches ADD COLUMN fts_query TEXT;`)
			return err
		},
	},
	// A cross-store search (PLAN 3.2) logs every hit in the primary store's log. store is NULL
	// for a hit of the primary itself and the canonical ref NAME@ABSROOT of the store that
	// returned it otherwise; chunk_id is that store's. Older rows keep NULL: they are all primary.
	{
		Info: migrate.Info{From: 4, ID: "index/0005-hit-store", Desc: "add search_results.store and search_candidates.store"},
		Up: func(ctx context.Context, db execer) error {
			_, err := db.ExecContext(ctx, `
			ALTER TABLE search_results ADD COLUMN store TEXT;
			ALTER TABLE search_candidates ADD COLUMN store TEXT;`)
			return err
		},
	},
	// PLAN 4.5. chunks_exact is a second contentless FTS index over chunks whose tokenizer keeps
	// identifiers whole (`foo_bar`, `pkg.Func`, `--flag`, `path/to`, `a:b`: no porter, no
	// diacritic folding), backfilled from chunks, so no reindex is needed. chunks_exact_vocab
	// exposes its terms (PLAN 5.6). The log gains each hit's place in the exact list. search_opens
	// gains the store of the opened entry and its key widens to (search_id, store, entry_path), a
	// table rebuild: an open recorded before v6 gets the store the reader inferred until now (this
	// store when the search returned the path from it, else the smallest other ref).
	{
		Info: migrate.Info{From: 5, ID: "index/0006-exact-list", Desc: "add the identifier index chunks_exact (backfilled) and its vocab, exact ranks in the log, search_opens.store"},
		Up: func(ctx context.Context, db execer) error {
			_, err := db.ExecContext(ctx, `
			CREATE VIRTUAL TABLE chunks_exact USING fts5(
			  text, heading,
			  content='', contentless_delete=1,
			  tokenize="unicode61 remove_diacritics 0 tokenchars '_-.:/'"
			);
			INSERT INTO chunks_exact (rowid, text, heading) SELECT id, text, heading FROM chunks ORDER BY id;
			CREATE VIRTUAL TABLE chunks_exact_vocab USING fts5vocab(chunks_exact, 'row');
			ALTER TABLE search_results ADD COLUMN exact_rank INTEGER;
			ALTER TABLE search_results ADD COLUMN exact_score REAL;
			ALTER TABLE search_candidates ADD COLUMN exact_rank INTEGER;
			ALTER TABLE search_candidates ADD COLUMN exact_score REAL;
			CREATE TABLE search_opens_v6 (
			  search_id  INTEGER NOT NULL REFERENCES searches(id) ON DELETE CASCADE,
			  store      TEXT,
			  entry_path TEXT NOT NULL,
			  ts         TEXT NOT NULL
			);
			INSERT INTO search_opens_v6 (search_id, store, entry_path, ts)
			SELECT o.search_id,
			  CASE WHEN EXISTS (SELECT 1 FROM search_results AS r
			                    WHERE r.search_id = o.search_id AND r.entry_path = o.entry_path AND r.store IS NULL)
			       THEN NULL
			       ELSE (SELECT min(r.store) FROM search_results AS r
			             WHERE r.search_id = o.search_id AND r.entry_path = o.entry_path)
			  END,
			  o.entry_path, o.ts
			FROM search_opens AS o ORDER BY o.rowid;
			DROP TABLE search_opens;
			ALTER TABLE search_opens_v6 RENAME TO search_opens;
			CREATE UNIQUE INDEX search_opens_key ON search_opens (search_id, ifnull(store, ''), entry_path);`)
			return err
		},
	},
	// PLAN 5.2. edges holds each entry's outgoing links (body links rel 'links', front-matter
	// lists rel = the key), kind 'path' (a root-relative path) or 'wikilink' (a name as written);
	// dst_entry_id is the entry the target resolves to, NULL while it dangles, and becomes NULL
	// when that entry goes. entry_tags is the tag filter's table: normalised tags, backfilled
	// here from entries.tags with SQL lower() (ASCII only; the reindex this step declares rewrites
	// them with the full NormTag fold). A tags value that is not a JSON array backfills nothing.
	// entries gains aliases (JSON array). Edges and aliases need the Markdown, so every body_hash
	// is cleared: a plain `kb reindex` fills them, reusing every embedding by text_hash.
	{
		Info: migrate.Info{From: 6, ID: "index/0007-edges-tags-aliases", Desc: "add edges, entry_tags (backfilled) and entries.aliases; edges and aliases fill on the next reindex", Effect: migrate.EffectReindex},
		Up: func(ctx context.Context, db execer) error {
			_, err := db.ExecContext(ctx, `
			CREATE TABLE edges (
			  src_entry_id INTEGER NOT NULL REFERENCES entries(id) ON DELETE CASCADE,
			  dst_path     TEXT NOT NULL,
			  dst_entry_id INTEGER REFERENCES entries(id) ON DELETE SET NULL,
			  rel          TEXT NOT NULL,
			  kind         TEXT NOT NULL CHECK (kind IN ('path', 'wikilink')),
			  PRIMARY KEY (src_entry_id, dst_path, rel, kind)
			);
			CREATE INDEX edges_dst ON edges (dst_entry_id);
			CREATE TABLE entry_tags (
			  entry_id INTEGER NOT NULL REFERENCES entries(id) ON DELETE CASCADE,
			  tag      TEXT NOT NULL,
			  PRIMARY KEY (entry_id, tag)
			);
			INSERT OR IGNORE INTO entry_tags (entry_id, tag)
			SELECT e.id, lower(trim(j.value))
			FROM entries AS e,
			     json_each(CASE WHEN json_valid(e.tags) THEN
			                 CASE WHEN json_type(e.tags) = 'array' THEN e.tags END
			               END) AS j
			WHERE j.type = 'text' AND trim(j.value) != ''
			ORDER BY e.id;
			ALTER TABLE entries ADD COLUMN aliases TEXT NOT NULL DEFAULT '[]';
			UPDATE entries SET body_hash = '';`)
			return err
		},
	},
	// PLAN 6.2. The feedback prior multiplies a hit's fused score; the multiplier is logged on the
	// result and candidate rows (rrf_score already includes it). NULL = the prior was inactive for
	// that search (off, --no-prior, too few judged searches), which every older row is.
	{
		Info: migrate.Info{From: 7, ID: "index/0008-prior", Desc: "add search_results.prior and search_candidates.prior"},
		Up: func(ctx context.Context, db execer) error {
			_, err := db.ExecContext(ctx, `
			ALTER TABLE search_results ADD COLUMN prior REAL;
			ALTER TABLE search_candidates ADD COLUMN prior REAL;`)
			return err
		},
	},
	// PLAN 4.6 + Q135. fts_tier is the tier of the main full-text list that placed a hit
	// (phrase, near, and, or; NULL = the list ran untiered, which every older row did, or the hit
	// was not in it). memory_rank and memory_score are the hit's place in the query-memory list
	// and its u; NULL = not in it. All additive and nullable, so nothing is cleared.
	{
		Info: migrate.Info{From: 8, ID: "index/0009-tier-memory", Desc: "add fts_tier, memory_rank and memory_score to search_results and search_candidates"},
		Up: func(ctx context.Context, db execer) error {
			_, err := db.ExecContext(ctx, `
			ALTER TABLE search_results ADD COLUMN fts_tier TEXT CHECK (fts_tier IN ('phrase', 'near', 'and', 'or'));
			ALTER TABLE search_results ADD COLUMN memory_rank INTEGER;
			ALTER TABLE search_results ADD COLUMN memory_score REAL;
			ALTER TABLE search_candidates ADD COLUMN fts_tier TEXT CHECK (fts_tier IN ('phrase', 'near', 'and', 'or'));
			ALTER TABLE search_candidates ADD COLUMN memory_rank INTEGER;
			ALTER TABLE search_candidates ADD COLUMN memory_score REAL;`)
			return err
		},
	},
}

func init() {
	if assert.Enabled {
		err := indexSteps.Check(artefactName)
		assert.Invariant(err == nil, "index migrations: %v", err)
	}
}

// SchemaVersion is the PRAGMA user_version this build reads and writes: one per step.
func SchemaVersion() int { return indexSteps.Target() }

// stepsOr is steps, or this build's indexSteps when nil (the test seam's zero value).
func stepsOr(steps migrate.Steps[execer]) migrate.Steps[execer] {
	if steps == nil {
		return indexSteps
	}
	return steps
}

// execer is what a migration step needs: *sql.Conn and *sql.Tx both satisfy it. Steps run inside
// the caller's transaction, so they must not BEGIN or COMMIT.
type execer interface {
	ExecContext(ctx context.Context, query string, args ...any) (sql.Result, error)
}
