package sqlite

import (
	"context"
	"database/sql"
	_ "embed"
	"fmt"

	"github.com/nickstrad/kb/internal/assert"
	"github.com/nickstrad/kb/internal/migrate"
)

// schemaSQL is the baseline (schema v1), applied verbatim to a fresh database. It is the squash of
// every pre-release step (design D8, decision M6). Never edit it: from the first release on, an
// edit would reach brand-new databases only; TestBaselineSchemaGolden fails on one.
//
//go:embed schema.sql
var schemaSQL string

// artefactName is the index DB's name in migration IDs and kb migrate output.
const artefactName = "index"

// indexSteps[v] takes a database from PRAGMA user_version v to v+1. Append-only: to change the
// schema, append a step (ID index/<NNNN>-<slug>, NNNN = its From+1, pre-assigned in the plan's
// migration table) and leave every earlier one alone; SchemaVersion follows. Each Up runs inside
// the caller's BEGIN IMMEDIATE. It is never reassigned: a test that needs an older or a longer
// list passes its own through Options.Steps or Artefact.steps, so tests in this package may run
// in parallel. Adding a step: see the checklist in migrations_test.go.
var indexSteps = Steps{
	{
		Info: migrate.Info{From: 0, ID: "index/0001-baseline", Desc: "create the index, FTS, link, vector-metadata and search-log tables"},
		Up: func(ctx context.Context, db execer) error {
			_, err := db.ExecContext(ctx, schemaSQL)
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

// Steps is a list of index migration steps: this build's (IndexSteps), or a test's.
type Steps = migrate.Steps[execer]

// SchemaVersion is the PRAGMA user_version this build reads and writes: one per step.
func SchemaVersion() int { return indexSteps.Target() }

// IndexSteps is a copy of this build's steps.
func IndexSteps() Steps { return append(Steps(nil), indexSteps...) }

// ProbeSteps is this build's steps plus one synthetic step, index/<NNNN>-probe, that plays the
// next release: it adds an empty table and clears every body_hash, declaring EffectReindex. Tests
// and harnessfake builds pass it as Options.Steps and NewArtefactSteps so that a store at this
// build's version is one release older, which drives the refusal, kb migrate up, backup and
// "needs kb reindex" paths end to end. Release builds never use it.
func ProbeSteps() Steps {
	v := SchemaVersion()
	return append(IndexSteps(), migrate.Step[execer]{
		Info: migrate.Info{From: v, ID: fmt.Sprintf("%s/%04d-probe", artefactName, v+1),
			Desc: "test probe: add the table probe and clear every body_hash", Effect: migrate.EffectReindex},
		Up: func(ctx context.Context, db execer) error {
			_, err := db.ExecContext(ctx, `
			CREATE TABLE probe (id INTEGER PRIMARY KEY);
			UPDATE entries SET body_hash = '';`)
			return err
		},
	})
}

// stepsOr is steps, or this build's indexSteps when nil (the seam's zero value).
func stepsOr(steps Steps) Steps {
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
