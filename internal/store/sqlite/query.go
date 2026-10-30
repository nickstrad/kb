package sqlite

import (
	"context"
	"time"

	"github.com/nickstrad/kb/internal/errs"
	"github.com/nickstrad/kb/internal/store"
)

// QueryLog runs analytic SQL (SQLite dialect) and returns its rows as plain Go values: int64,
// float64, string or nil. The SQL is the caller's, so on failure Msg carries the driver's own
// complaint (for example a syntax error), which is what a user who wrote the SQL needs.
func (s *Store) QueryLog(ctx context.Context, query string, args ...any) (*store.Table, error) {
	t, err := s.queryLog(ctx, query, args...)
	if err != nil {
		if errs.KindOf(err) != errs.Internal || classify(err) != errs.Internal {
			return nil, wrap(err, s.path, "sqlite.QueryLog", "query failed")
		}
		return nil, errs.Wrap(err, errs.Internal, "sqlite.QueryLog", "%s", err.Error()).With("db", s.path)
	}
	return t, nil
}

func (s *Store) queryLog(ctx context.Context, query string, args ...any) (*store.Table, error) {
	rows, err := s.db.QueryContext(ctx, query, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	cols, err := rows.Columns()
	if err != nil {
		return nil, err
	}
	t := &store.Table{Columns: cols}
	for rows.Next() {
		row := make([]any, len(cols))
		ptrs := make([]any, len(cols))
		for i := range row {
			ptrs[i] = &row[i]
		}
		if err := rows.Scan(ptrs...); err != nil {
			return nil, err
		}
		for i, v := range row {
			row[i] = normalizeValue(v)
		}
		t.Rows = append(t.Rows, row)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	return t, nil
}

// normalizeValue maps the driver's scan types onto store.Table's: BLOB bytes become a string, a
// column the driver parses as a time comes back as RFC3339 text, and a bool as 0 or 1.
func normalizeValue(v any) any {
	switch x := v.(type) {
	case []byte:
		return string(x)
	case time.Time:
		return x.UTC().Format(time.RFC3339)
	case bool:
		if x {
			return int64(1)
		}
		return int64(0)
	default:
		return v
	}
}
