package sqlite

import (
	"context"

	"github.com/nickstrad/kb/internal/store"
)

// FeedbackRows returns every search_feedback row, by search id then rank (store.FeedbackLog).
func (s *Store) FeedbackRows(ctx context.Context) ([]store.FeedbackRow, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT search_id, rank, useful, coalesce(note, ''), ts FROM search_feedback ORDER BY search_id, rank`)
	if err != nil {
		return nil, wrap(ctx, err, s.path, "sqlite.FeedbackRows", "could not read the verdicts")
	}
	defer rows.Close()
	var out []store.FeedbackRow
	for rows.Next() {
		var r store.FeedbackRow
		if err := rows.Scan(&r.SearchID, &r.Rank, &r.Useful, &r.Note, &r.TS); err != nil {
			return nil, wrap(ctx, err, s.path, "sqlite.FeedbackRows", "could not read the verdicts")
		}
		out = append(out, r)
	}
	if err := rows.Err(); err != nil {
		return nil, wrap(ctx, err, s.path, "sqlite.FeedbackRows", "could not read the verdicts")
	}
	return out, nil
}

var _ store.FeedbackLog = (*Store)(nil)
