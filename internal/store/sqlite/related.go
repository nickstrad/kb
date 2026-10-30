// Related and Superseded: reads over the edges table (schema v7). The walk is a recursive CTE
// over resolved edges in both directions; UNION plus the depth bound make it cycle-safe.

package sqlite

import (
	"context"

	"github.com/nickstrad/kb/internal/errs"
	"github.com/nickstrad/kb/internal/store"
)

// relatedSQL walks nodes (walk), keeps each node's shortest distance (near), picks the edges with
// an end in near closer than the depth (cand), then lists them. Each step is an index lookup
// (the primary key leads with src_entry_id; edges_dst covers dst_entry_id), so the cost grows
// with the neighbourhood, not the whole edges table. Parameters: ?1 origin id, ?2 depth.
const relatedSQL = `
WITH RECURSIVE walk(id, depth) AS (
	SELECT ?1, 0
	UNION
	SELECT g.dst_entry_id, w.depth + 1
	FROM walk AS w JOIN edges AS g ON g.src_entry_id = w.id
	WHERE w.depth < ?2 AND g.dst_entry_id IS NOT NULL
	UNION
	SELECT g.src_entry_id, w.depth + 1
	FROM walk AS w JOIN edges AS g ON g.dst_entry_id = w.id
	WHERE w.depth < ?2
), near(id, depth) AS (
	SELECT id, MIN(depth) FROM walk GROUP BY id
), cand(rid) AS (
	SELECT g.rowid FROM near AS n JOIN edges AS g ON g.src_entry_id = n.id WHERE n.depth < ?2
	UNION
	SELECT g.rowid FROM near AS n JOIN edges AS g ON g.dst_entry_id = n.id WHERE n.depth < ?2
), reach AS (
	SELECT g.rel, g.kind, g.dst_path, src.path AS src_path, coalesce(dst.path, '') AS dst_entry_path,
		coalesce(ns.depth, ` + farSQL + `) AS ds,
		CASE WHEN g.dst_entry_id IS NULL THEN ` + farSQL + ` ELSE coalesce(nd.depth, ` + farSQL + `) END AS dd
	FROM cand AS c
	JOIN edges AS g ON g.rowid = c.rid
	JOIN entries AS src ON src.id = g.src_entry_id
	LEFT JOIN entries AS dst ON dst.id = g.dst_entry_id
	LEFT JOIN near AS ns ON ns.id = g.src_entry_id
	LEFT JOIN near AS nd ON nd.id = g.dst_entry_id
)
SELECT rel, kind,
	CASE WHEN ds <= dd THEN 'out' ELSE 'in' END,
	min(ds, dd) + 1,
	CASE WHEN ds <= dd THEN src_path ELSE dst_entry_path END,
	CASE WHEN ds <= dd THEN dst_entry_path ELSE src_path END,
	CASE WHEN ds <= dd THEN dst_path ELSE src_path END
FROM reach
WHERE min(ds, dd) < ?2`

// farSQL is the distance of an end the walk did not reach; it equals store.RelatedModel's.
const farSQL = "1048576"

// Related is the contract of store.Relater.
func (s *Store) Related(ctx context.Context, path string, depth int) ([]store.RelatedRow, error) {
	const op = "sqlite.Related"
	if depth < 1 || depth > store.MaxRelatedDepth {
		return nil, errs.New(errs.Usage, op, "depth %d is out of range: use 1 to %d", depth, store.MaxRelatedDepth)
	}
	e, err := s.Entry(ctx, path)
	if err != nil {
		return nil, err
	}
	if e == nil {
		return nil, errs.New(errs.NotFound, op, "no entry %q in the index", path).With("path", path)
	}
	rows, err := s.db.QueryContext(ctx, relatedSQL, e.ID, depth)
	if err != nil {
		return nil, wrap(ctx, err, s.path, op, "could not walk the links of %s", path)
	}
	defer rows.Close()
	var out []store.RelatedRow
	for rows.Next() {
		var r store.RelatedRow
		if err := rows.Scan(&r.Rel, &r.Kind, &r.Direction, &r.Depth, &r.Via, &r.Path, &r.Target); err != nil {
			return nil, wrap(ctx, err, s.path, op, "could not walk the links of %s", path)
		}
		out = append(out, r)
	}
	if err := rows.Err(); err != nil {
		return nil, wrap(ctx, err, s.path, op, "could not walk the links of %s", path)
	}
	store.SortRelated(out)
	return out, nil
}

// Superseded is the contract of store.Retriever: the entries among paths that another entry's
// `supersedes` edge names.
func (s *Store) Superseded(ctx context.Context, paths []string) (map[string]bool, error) {
	const op = "sqlite.Superseded"
	out := make(map[string]bool)
	if len(paths) == 0 {
		return out, nil
	}
	args := make([]any, len(paths))
	for i, p := range paths {
		args[i] = p
	}
	rows, err := s.db.QueryContext(ctx, `SELECT DISTINCT d.path FROM edges AS g
		JOIN entries AS d ON d.id = g.dst_entry_id
		WHERE g.rel = 'supersedes' AND g.src_entry_id != g.dst_entry_id AND d.path IN (`+placeholders(len(args))+`)`, args...)
	if err != nil {
		return nil, wrap(ctx, err, s.path, op, "could not read which entries are superseded")
	}
	defer rows.Close()
	for rows.Next() {
		var p string
		if err := rows.Scan(&p); err != nil {
			return nil, wrap(ctx, err, s.path, op, "could not read which entries are superseded")
		}
		out[p] = true
	}
	if err := rows.Err(); err != nil {
		return nil, wrap(ctx, err, s.path, op, "could not read which entries are superseded")
	}
	return out, nil
}
