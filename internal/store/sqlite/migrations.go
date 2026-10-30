package sqlite

import (
	"context"
	"database/sql"
	_ "embed"
)

// schemaSQL is migrations[0], applied verbatim to a fresh database. Never edit it once a database
// exists in the wild: an edit would reach brand-new databases only.
//
//go:embed schema.sql
var schemaSQL string

// migrations[v] takes a database from PRAGMA user_version v to v+1. To change the schema, append
// a function and leave every earlier element alone; schemaVersion follows.
var migrations = []func(execer) error{
	func(db execer) error {
		_, err := db.Exec(schemaSQL)
		return err
	},
	// 1→2: searches.kb_version held the corpus repo's git hash, so it becomes corpus_rev and a new
	// kb_version records the binary. search_opens holds the implicit signal from `kb show`.
	func(db execer) error {
		_, err := db.Exec(`
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
	// 2→3: chunks_vec's width becomes the model's, fixed when EnsureEmbedMeta creates it. The old
	// vectors go with their table and embed_meta, and every body_hash is cleared so `kb reindex`
	// re-embeds. FTS keeps working in between; the search log is untouched.
	func(db execer) error {
		_, err := db.Exec(`
			DROP TABLE IF EXISTS chunks_vec;
			DELETE FROM embed_meta;
			UPDATE entries SET body_hash = '';`)
		return err
	},
}

// schemaVersion is the PRAGMA user_version this build expects: one per migration.
var schemaVersion = len(migrations)

// SchemaVersion is the PRAGMA user_version this build reads and writes.
func SchemaVersion() int { return schemaVersion }

// execer is what a migration needs. Migrations run inside the caller's transaction, so they must
// not BEGIN or COMMIT.
type execer interface {
	Exec(query string, args ...any) (sql.Result, error)
}

// connExecer adapts *sql.Conn, which only offers the ctx-taking methods, to execer.
type connExecer struct {
	ctx  context.Context
	conn *sql.Conn
}

func (c connExecer) Exec(query string, args ...any) (sql.Result, error) {
	return c.conn.ExecContext(c.ctx, query, args...)
}
