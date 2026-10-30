// The store.ChunkLocator implementation: log (path, heading) pairs back to chunk ids.

package sqlite

import (
	"context"
	"database/sql"
	"errors"

	"github.com/nickstrad/kb/internal/store"
)

var _ store.ChunkLocator = (*Store)(nil)

// LocateChunks resolves each ref with one indexed lookup (the callers pass at most a list's worth).
func (s *Store) LocateChunks(ctx context.Context, refs []store.ChunkRef, tag string) (map[store.ChunkRef]int64, error) {
	const op = "sqlite.LocateChunks"
	out := make(map[store.ChunkRef]int64, len(refs))
	for _, r := range refs {
		q := `SELECT c.id FROM chunks c JOIN entries e ON e.id = c.entry_id WHERE e.path = ?`
		args := []any{r.Path}
		if !r.AnyHeading {
			q += ` AND c.heading = ?`
			args = append(args, r.Heading)
		}
		if tag != "" {
			q += ` AND EXISTS (SELECT 1 FROM entry_tags t WHERE t.entry_id = c.entry_id AND t.tag = ?)`
			args = append(args, store.NormTag(tag))
		}
		q += ` ORDER BY c.ord LIMIT 1`
		var id int64
		switch err := s.db.QueryRowContext(ctx, q, args...).Scan(&id); {
		case errors.Is(err, sql.ErrNoRows):
		case err != nil:
			return nil, wrap(ctx, err, s.path, op, "could not look up a remembered chunk")
		default:
			out[r] = id
		}
	}
	return out, nil
}
