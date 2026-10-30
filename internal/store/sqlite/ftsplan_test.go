package sqlite

import (
	"context"
	"path/filepath"
	"strings"
	"testing"

	"github.com/nickstrad/kb/internal/store"
)

// TestFTSChunkIDsPlan guards the W13-pass planner fix in ftsSQL: a ChunkIDs restriction must not
// reach FTS5 as a rowid constraint (idxStr "=…"), which makes SQLite run the MATCH once per id,
// 10x slower for a tier rescoring of 20 ids (BenchmarkTiers). Dropping the unary + is otherwise
// invisible: the rows and scores are the same. With and without a tag join, both tables.
func TestFTSChunkIDsPlan(t *testing.T) {
	ctx := context.Background()
	s, err := Open(ctx, filepath.Join(t.TempDir(), "kb.sqlite"), Options{})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { s.Close() })
	for _, q := range []store.FTSQuery{
		{Terms: []string{"a", "b"}, Limit: 2, ChunkIDs: []int64{1, 2}},
		{Terms: []string{"a", "b"}, Limit: 2, ChunkIDs: []int64{1, 2}, Tag: "t"},
		{Terms: []string{"a_b"}, Limit: 2, ChunkIDs: []int64{1, 2}, Exact: true},
	} {
		query, args := ftsSQL(matchFor(q), store.DefaultBM25(), q)
		rows, err := s.DB().QueryContext(ctx, "EXPLAIN QUERY PLAN "+query, args...)
		if err != nil {
			t.Fatal(err)
		}
		var plan []string
		for rows.Next() {
			var id, parent, unused int
			var detail string
			if err := rows.Scan(&id, &parent, &unused, &detail); err != nil {
				t.Fatal(err)
			}
			plan = append(plan, detail)
		}
		rows.Close()
		joined := strings.Join(plan, " | ")
		if !strings.Contains(joined, "VIRTUAL TABLE INDEX 0:M") || strings.Contains(joined, "INDEX 0:=") {
			t.Errorf("%+v: plan %q: want the MATCH run once (INDEX 0:M…), not per chunk id (INDEX 0:=…)", q, joined)
		}
	}
}
