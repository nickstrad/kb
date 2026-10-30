// Package stats runs the `kb stats` analytic queries against the search log.
//
// Inputs: a Querier (the store, opened read-only by the CLI), a Filter and a Query from the
// registry (or ad-hoc SQL). Output: a store.Table of plain Go values. The SQL is SQLite dialect;
// a future backend would supply its own registry.
package stats

import (
	"context"
	"encoding/json"
	"fmt"
	"time"

	"github.com/nickstrad/kb/internal/errs"
	"github.com/nickstrad/kb/internal/store"
)

// Filter narrows which searches the named queries see. The zero value keeps every search.
type Filter struct {
	Since  time.Time // zero = no lower bound; keeps searches with ts >= Since
	Caller string    // "" = every caller
}

// args renders f as the two positional parameters every named query takes: ?1 is the lower bound
// on ts as RFC3339 text ("" matches every row), ?2 the caller ("" means any). The log writes ts
// with time.RFC3339 in UTC, so text order is time order.
func (f Filter) args() []any {
	since := ""
	if !f.Since.IsZero() {
		since = f.Since.UTC().Format(time.RFC3339)
	}
	return []any{since, f.Caller}
}

// Table is one query's output. Values are int64, float64, string or nil.
type Table = store.Table

// Querier runs analytic SQL against the log (store.SearchLog has it).
type Querier interface {
	QueryLog(ctx context.Context, sql string, args ...any) (*store.Table, error)
}

// DB runs the registry's queries through a Querier.
type DB struct {
	q Querier
}

// New wraps q.
func New(q Querier) *DB { return &DB{q: q} }

// Run executes a named query under f and fills in its percentile columns. The registry's SQL is
// kb's own, so a query failure keeps the store's kind: Internal for a driver or SQL error, Conflict
// for a locked or damaged database, Interrupted for a cancel. A bad percentile cell is Internal.
func (d *DB) Run(ctx context.Context, q Query, f Filter) (*Table, error) {
	t, err := d.q.QueryLog(ctx, q.SQL, f.args()...)
	if err != nil {
		return nil, errs.Wrap(err, errs.KindOf(err), "stats.Run", "%s: %v", q.Name, err).With("query", q.Name)
	}
	if err := applyPercentiles(t, q.Percentiles); err != nil {
		return nil, errs.Wrap(err, errs.Internal, "stats.Run", "%s: %v", q.Name, err).With("query", q.Name)
	}
	return t, nil
}

// SQL runs ad-hoc SQL against the real tables. It takes no filter. A failure is Usage: the user
// wrote the SQL, and the message carries SQLite's complaint.
func (d *DB) SQL(ctx context.Context, text string) (*Table, error) {
	t, err := d.q.QueryLog(ctx, text)
	if err != nil {
		if errs.KindOf(err) == errs.Conflict {
			return nil, err // a damaged database, not the user's SQL
		}
		return nil, errs.Wrap(err, errs.Usage, "stats.SQL", "%v", err)
	}
	return t, nil
}

// SearchesMatching counts the searches f keeps.
func (d *DB) SearchesMatching(ctx context.Context, f Filter) (int64, error) {
	t, err := d.q.QueryLog(ctx, filterCTE+"SELECT count(*) FROM fs", f.args()...)
	if err != nil {
		return 0, errs.Wrap(err, errs.KindOf(err), "stats.SearchesMatching", "count searches: %v", err)
	}
	if len(t.Rows) != 1 || len(t.Rows[0]) != 1 {
		return 0, errs.New(errs.Internal, "stats.SearchesMatching", "count searches: got %d rows", len(t.Rows))
	}
	n, ok := t.Rows[0][0].(int64)
	if !ok {
		return 0, errs.New(errs.Internal, "stats.SearchesMatching", "count searches: got %T", t.Rows[0][0])
	}
	return n, nil
}

// applyPercentiles replaces each percentile column's JSON array of samples with the rounded
// continuous quantile of its non-null numbers, or nil when there are none.
func applyPercentiles(t *Table, pcts []Pct) error {
	for _, p := range pcts {
		col := -1
		for i, c := range t.Columns {
			if c == p.Column {
				col = i
			}
		}
		if col < 0 {
			return fmt.Errorf("percentile column %q not in result", p.Column)
		}
		for _, row := range t.Rows {
			s, ok := row[col].(string)
			if !ok {
				return fmt.Errorf("percentile column %q: got %T, want a JSON array", p.Column, row[col])
			}
			var raw []*float64
			if err := json.Unmarshal([]byte(s), &raw); err != nil {
				return fmt.Errorf("percentile column %q: %w", p.Column, err)
			}
			xs := make([]float64, 0, len(raw))
			for _, x := range raw {
				if x != nil {
					xs = append(xs, *x)
				}
			}
			if v, ok := quantileCont(xs, p.P); ok {
				row[col] = roundTo(v, p.Digits)
			} else {
				row[col] = nil
			}
		}
	}
	return nil
}
