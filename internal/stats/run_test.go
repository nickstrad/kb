package stats

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"github.com/nickstrad/kb/internal/errs"
	"io"
	"os"
	"strings"
	"testing"
	"time"
)

func mustParse(t *testing.T, s string) time.Time {
	t.Helper()
	ts, err := time.Parse(time.RFC3339, s)
	if err != nil {
		t.Fatal(err)
	}
	return ts
}

// TestFilterSinceReachesChildTables: with Since at search 6's own ts (inclusive), only searches
// 6-9 remain, and the in-scope sources rows are search 7's three and search 8's one. Search 1's
// verdicts, which dominate the unfiltered sources table, must be gone.
func TestFilterSinceReachesChildTables(t *testing.T) {
	db := openFixture(t)
	f := Filter{Since: mustParse(t, at(14, 9, 0))}

	n, err := db.SearchesMatching(context.Background(), f)
	if err != nil {
		t.Fatal(err)
	}
	if n != 4 {
		t.Errorf("SearchesMatching = %d, want 4 (searches 6-9; the bound is inclusive)", n)
	}
	checkQuery(t, db, "sources", f,
		[]string{"source", "results", "share_pct", "judged", "useful", "useful_rate_pct", "avg_rank", "with_exact"},
		[][]any{
			{"both", int64(2), 50.0, int64(1), int64(0), 0.0, 1.0, int64(0)},
			{"fts-only", int64(1), 25.0, int64(0), int64(0), nil, 2.0, int64(0)},
			{"vec-only", int64(1), 25.0, int64(0), int64(0), nil, 3.0, int64(0)},
		})
}

// TestFilterCaller: codex asked searches 2, 4, 7 and 8. "duckdb appender" shrinks to search 8
// alone (one verdict, one caller), and "postgres tuning" drops out because search 2 was useful.
func TestFilterCaller(t *testing.T) {
	db := openFixture(t)
	f := Filter{Caller: "codex"}

	n, err := db.SearchesMatching(context.Background(), f)
	if err != nil {
		t.Fatal(err)
	}
	if n != 4 {
		t.Errorf("SearchesMatching = %d, want 4", n)
	}
	checkQuery(t, db, "gaps", f,
		[]string{"query", "times_asked", "last_asked", "callers", "judged", "not_useful",
			"zero_results", "top_entry", "last_note"},
		[][]any{
			{"duckdb appender", int64(1), at(16, 11, 0), "codex", int64(1), int64(1),
				int64(0), "duckdb.md", "still not the appender docs"},
			{"ssh daemon dies", int64(1), at(15, 10, 0), "codex", int64(0), int64(0),
				int64(0), "ssh.md", nil},
			{"ollama offline", int64(1), at(10, 12, 0), "codex", int64(0), int64(0),
				int64(0), "ollama.md", nil},
		})
}

func TestFilterSinceAndCaller(t *testing.T) {
	db := openFixture(t)
	n, err := db.SearchesMatching(context.Background(),
		Filter{Since: mustParse(t, at(14, 9, 0)), Caller: "codex"})
	if err != nil {
		t.Fatal(err)
	}
	if n != 2 {
		t.Errorf("SearchesMatching = %d, want 2 (searches 7 and 8)", n)
	}
}

func TestSQLBadSyntax(t *testing.T) {
	db := openFixture(t)
	_, err := db.SQL(context.Background(), "SELEC * FORM nonsense_table")
	if err == nil || !strings.Contains(err.Error(), "syntax error") {
		t.Fatalf("err = %v, want SQLite's syntax error", err)
	}
}

// TestReadOnly: ad-hoc SQL cannot write, and opening and querying leave the file's bytes alone.
func TestReadOnly(t *testing.T) {
	db := openFixture(t)
	path := storeOf(db).Path()
	before := hashFile(t, path)

	ctx := context.Background()
	if _, err := db.SQL(ctx, "DELETE FROM searches"); err == nil {
		t.Error("DELETE through SQL succeeded on a read-only database")
	}
	tbl, err := db.SQL(ctx, "SELECT count(*) AS n FROM searches")
	if err != nil {
		t.Fatal(err)
	}
	if tbl.Rows[0][0] != int64(9) {
		t.Errorf("searches = %v after a rejected DELETE, want 9", tbl.Rows[0][0])
	}
	if err := storeOf(db).Close(); err != nil {
		t.Fatal(err)
	}
	if after := hashFile(t, path); after != before {
		t.Errorf("database bytes changed: %s -> %s", before, after)
	}
}

func hashFile(t *testing.T, path string) string {
	t.Helper()
	f, err := os.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	h := sha256.New()
	if _, err := io.Copy(h, f); err != nil {
		t.Fatal(err)
	}
	return hex.EncodeToString(h.Sum(nil))
}

// TestDBErrors: every way Run, SQL and SearchesMatching can fail returns an error of the right
// kind instead of an empty result.
func TestDBErrors(t *testing.T) {
	ctx := context.Background()
	db := openFixture(t)
	for _, tc := range []struct {
		name    string
		run     func() error
		wantErr string
	}{
		{"SQL bad statement", func() error { _, err := db.SQL(ctx, "SELEKT 1"); return err }, "syntax error"},
		{"SQL bad statement is Usage", func() error {
			_, err := db.SQL(ctx, "SELEKT 1")
			if errs.KindOf(err) != errs.Usage {
				return fmt.Errorf("kind %v", errs.KindOf(err))
			}
			return errors.New("ok usage")
		}, "ok usage"},
		{"Run bad SQL is Internal", func() error {
			_, err := db.Run(ctx, Query{Name: "broken", SQL: "SELEKT 1"}, Filter{})
			if errs.KindOf(err) != errs.Internal {
				return fmt.Errorf("kind %v", errs.KindOf(err))
			}
			return errors.New("ok internal")
		}, "ok internal"},
		{"Run bad SQL names the query", func() error {
			_, err := db.Run(ctx, Query{Name: "broken", SQL: "SELEKT 1"}, Filter{})
			return err
		}, "broken:"},
		{"Run percentile column absent", func() error {
			_, err := db.Run(ctx, Query{Name: "nopct", SQL: "SELECT 1 AS x",
				Percentiles: []Pct{{"p50", 0.5, 1}}}, Filter{})
			return err
		}, `nopct: percentile column "p50" not in result`},
		{"Run non-JSON percentile cell", func() error {
			_, err := db.Run(ctx, Query{Name: "badjson", SQL: "SELECT 'nope' AS p50",
				Percentiles: []Pct{{"p50", 0.5, 1}}}, Filter{})
			return err
		}, `badjson: percentile column "p50"`},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if err := tc.run(); err == nil || !strings.Contains(err.Error(), tc.wantErr) {
				t.Fatalf("err = %v, want %q", err, tc.wantErr)
			}
		})
	}

	t.Run("closed DB", func(t *testing.T) {
		c := openFixture(t)
		if err := storeOf(c).Close(); err != nil {
			t.Fatal(err)
		}
		n, err := c.SearchesMatching(ctx, Filter{})
		if err == nil || !strings.Contains(err.Error(), "count searches") {
			t.Fatalf("SearchesMatching err = %v", err)
		}
		if n != 0 {
			t.Fatalf("SearchesMatching count on error = %d, want 0", n)
		}
		if _, err := c.SQL(ctx, "SELECT 1"); err == nil {
			t.Fatal("SQL on a closed DB succeeded")
		}
	})
}

// kindQuerier fails every query with a well-formed store error of a chosen kind.
type kindQuerier struct{ kind errs.Kind }

func (q kindQuerier) QueryLog(context.Context, string, ...any) (*Table, error) {
	return nil, errs.New(q.kind, "sqlite.QueryLog", "the database stayed locked")
}

// TestRunKeepsStoreKind: a named query that fails because of the database (locked, damaged,
// interrupted) keeps that kind, so kb stats exits 1 with the store's message instead of logging
// an incident; only a plain driver/SQL failure is Internal.
func TestRunKeepsStoreKind(t *testing.T) {
	for _, k := range []errs.Kind{errs.Conflict, errs.Interrupted, errs.Internal} {
		_, err := New(kindQuerier{k}).Run(context.Background(), Query{Name: "overview", SQL: "x"}, Filter{})
		if got := errs.KindOf(err); got != k {
			t.Errorf("store kind %v: Run kind = %v (%v)", k, got, err)
		}
	}
}
