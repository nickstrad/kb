package sqlite

import "context"

// EntryCount returns the number of indexed entries without reading them (kb stores lists one count
// per store, so reading every row would be linear in corpus size for each store listed).
func (s *Store) EntryCount(ctx context.Context) (int, error) {
	var n int
	if err := s.db.QueryRowContext(ctx, `SELECT count(*) FROM entries`).Scan(&n); err != nil {
		return 0, wrap(ctx, err, s.path, "sqlite.EntryCount", "could not count the indexed entries")
	}
	return n, nil
}
