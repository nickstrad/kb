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
