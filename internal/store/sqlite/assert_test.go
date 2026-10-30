//go:build !noassert

package sqlite

import (
	"context"
	"testing"

	"github.com/nickstrad/kb/internal/assert"
	"github.com/nickstrad/kb/internal/store"
)

func mustViolate(t *testing.T, kind string, f func()) {
	t.Helper()
	defer func() {
		v, ok := recover().(assert.Violation)
		if !ok || v.Kind != kind {
			t.Fatalf("recovered %#v, want a %s Violation", v, kind)
		}
	}()
	f()
}

// TestRowSetInvariantFires: inside a transaction, a chunk row without its FTS row, or without its
// vector row, trips the invariant ReplaceEntry checks before commit.
func TestRowSetInvariantFires(t *testing.T) {
	ctx := context.Background()
	for _, tc := range []struct {
		name string
		dim  int
		sql  []string
	}{
		{"chunk without fts row", 0, []string{
			"INSERT INTO chunks (id, entry_id, ord, source_file, heading, text, text_hash) VALUES (7, 1, 0, 'a', '', 't', 'h')"}},
		{"chunk without vec row", 4, []string{
			"INSERT INTO chunks (id, entry_id, ord, source_file, heading, text, text_hash) VALUES (7, 1, 0, 'a', '', 't', 'h')",
			"INSERT INTO chunks_fts (rowid, text, heading, title) VALUES (7, 't', '', 'T')"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			st := openFaulty(t, tc.dim)
			tx, err := st.DB().BeginTx(ctx, nil)
			if err != nil {
				t.Fatal(err)
			}
			defer tx.Rollback()
			stmts := append([]string{"INSERT INTO entries (id, path, kind, title, summary, tags, body_hash, indexed_at) VALUES (1, 'a', 'file', 'T', 'S', '[]', '', '')"}, tc.sql...)
			for _, q := range stmts {
				if _, err := tx.ExecContext(ctx, q); err != nil {
					t.Fatal(err)
				}
			}
			mustViolate(t, "invariant", func() { assertRowSets(ctx, tx, 1, 1, tc.dim) })
			mustViolate(t, "invariant", func() { assertRowSets(ctx, tx, 1, 2, 0) }) // count differs from what was written
		})
	}
}

// TestRetrieverPreconditions: a non-positive limit or k is a caller bug.
func TestRetrieverPreconditions(t *testing.T) {
	st := openFaulty(t, 4)
	mustViolate(t, "precondition", func() { st.FTS(context.Background(), store.FTSQuery{Terms: []string{"x"}}) })
	mustViolate(t, "precondition", func() { st.KNN(context.Background(), store.KNNQuery{Vec: []float32{1}}) })
}
