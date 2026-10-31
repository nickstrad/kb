// Edges, tags and aliases (PLAN 5.2). ReplaceEntry writes an entry's edges and normalised tags
// in the transaction that writes its chunks; DeleteEntry and ResetIndex remove them with it.
// ResolveEdges sets every edge's dst_entry_id by store.EdgeResolver, against the entries as they
// are when it runs.

package sqlite

import (
	"context"
	"database/sql"
	"fmt"
	"slices"
	"strings"

	"github.com/nickstrad/kb/internal/assert"
	"github.com/nickstrad/kb/internal/errs"
	"github.com/nickstrad/kb/internal/store"
)

// edgeKey is an edge's identity within its source entry: the edges primary key without the
// source.
type edgeKey struct{ dst, rel, kind string }

// checkEdges refuses an edge with an empty field or an unknown kind before anything is written:
// the caller (reindex) builds them, so a bad one is a kb bug, not bad data.
func checkEdges(op, path string, edges []store.EdgeInput) error {
	for i, e := range edges {
		if e.DstPath == "" || e.Rel == "" || (e.Kind != store.EdgePath && e.Kind != store.EdgeWikilink) {
			return errs.New(errs.Internal, op, "replace entry %s: edge %d %+v needs a target, a rel and kind %s or %s", path, i, e, store.EdgePath, store.EdgeWikilink).With("path", path)
		}
	}
	return nil
}

// writeLinks replaces the entry's tags and edges. An edge the entry had before with the same key
// keeps its resolved target (so an unchanged link never dangles between ReplaceEntry and
// ResolveEdges); a new one starts dangling.
func writeLinks(ctx context.Context, tx *sql.Tx, entryID int64, e store.EntryInput) error {
	old := map[edgeKey]sql.NullInt64{}
	rows, err := tx.QueryContext(ctx, "SELECT dst_path, rel, kind, dst_entry_id FROM edges WHERE src_entry_id = ?", entryID)
	if err != nil {
		return fmt.Errorf("read old edges of %s: %w", e.Path, err)
	}
	for rows.Next() {
		var k edgeKey
		var dst sql.NullInt64
		if err := rows.Scan(&k.dst, &k.rel, &k.kind, &dst); err != nil {
			rows.Close()
			return fmt.Errorf("read old edges of %s: %w", e.Path, err)
		}
		old[k] = dst
	}
	err = rows.Err()
	rows.Close()
	if err != nil {
		return fmt.Errorf("read old edges of %s: %w", e.Path, err)
	}
	if err := clearLinks(ctx, tx, entryID, e.Path); err != nil {
		return err
	}
	for _, tag := range store.NormTags(e.Tags) {
		if _, err := tx.ExecContext(ctx, "INSERT INTO entry_tags (entry_id, tag) VALUES (?, ?)", entryID, tag); err != nil {
			return fmt.Errorf("write tag %q of %s: %w", tag, e.Path, err)
		}
	}
	for _, g := range store.UniqueEdges(e.Edges) {
		dst := old[edgeKey{g.DstPath, g.Rel, g.Kind}]
		if _, err := tx.ExecContext(ctx, "INSERT INTO edges (src_entry_id, dst_path, dst_entry_id, rel, kind) VALUES (?, ?, ?, ?, ?)",
			entryID, g.DstPath, dst, g.Rel, g.Kind); err != nil {
			return fmt.Errorf("write edge %s -> %s (%s) of %s: %w", e.Path, g.DstPath, g.Rel, e.Path, err)
		}
	}
	return nil
}

// clearLinks removes an entry's tags and outgoing edges.
func clearLinks(ctx context.Context, tx *sql.Tx, entryID int64, path string) error {
	if _, err := tx.ExecContext(ctx, "DELETE FROM entry_tags WHERE entry_id = ?", entryID); err != nil {
		return fmt.Errorf("delete tags of %s: %w", path, err)
	}
	if _, err := tx.ExecContext(ctx, "DELETE FROM edges WHERE src_entry_id = ?", entryID); err != nil {
		return fmt.Errorf("delete edges of %s: %w", path, err)
	}
	return nil
}

// assertLinkSets checks, inside the transaction that wrote them, that the entry's entry_tags rows
// are exactly NormTags(e.Tags) and its edges exactly UniqueEdges(e.Edges).
func assertLinkSets(ctx context.Context, tx *sql.Tx, entryID int64, e store.EntryInput) {
	tags, err := strings1(ctx, tx, "SELECT tag FROM entry_tags WHERE entry_id = ? ORDER BY tag", entryID)
	if err != nil {
		return // a driver failure is reported by Commit, not by an assertion
	}
	edges, err := strings1(ctx, tx, "SELECT dst_path || char(0) || rel || char(0) || kind FROM edges WHERE src_entry_id = ? ORDER BY 1", entryID)
	if err != nil {
		return
	}
	var want []string
	for _, g := range store.UniqueEdges(e.Edges) {
		want = append(want, g.DstPath+"\x00"+g.Rel+"\x00"+g.Kind)
	}
	slices.Sort(want)
	assert.Invariant(slices.Equal(tags, store.NormTags(e.Tags)), "ReplaceEntry: entry %d entry_tags %q != NormTags %q", entryID, tags, store.NormTags(e.Tags))
	assert.Invariant(slices.Equal(edges, want), "ReplaceEntry: entry %d edges %q != input %q", entryID, edges, want)
}

// ResolveEdges sets every edge's dst_entry_id by store.EdgeResolver in one transaction, writing
// only the rows whose target changed.
func (s *Store) ResolveEdges(ctx context.Context) error {
	if err := s.resolveEdges(ctx); err != nil {
		return wrap(ctx, err, s.path, "sqlite.ResolveEdges", "could not resolve the links between entries")
	}
	return nil
}

func (s *Store) resolveEdges(ctx context.Context) error {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("begin edge resolution: %w", err)
	}
	defer tx.Rollback()
	targets, err := edgeTargets(ctx, tx)
	if err != nil {
		return err
	}
	r := store.NewEdgeResolver(targets)

	type edge struct {
		rowid    int64
		dst      string
		kind     string
		resolved sql.NullInt64
	}
	var edges []edge
	rows, err := tx.QueryContext(ctx, "SELECT rowid, dst_path, kind, dst_entry_id FROM edges ORDER BY rowid")
	if err != nil {
		return fmt.Errorf("read edges: %w", err)
	}
	for rows.Next() {
		var g edge
		if err := rows.Scan(&g.rowid, &g.dst, &g.kind, &g.resolved); err != nil {
			rows.Close()
			return fmt.Errorf("read edges: %w", err)
		}
		edges = append(edges, g)
	}
	err = rows.Err()
	rows.Close()
	if err != nil {
		return fmt.Errorf("read edges: %w", err)
	}
	for _, g := range edges {
		id, ok := r.Resolve(g.dst, g.kind)
		want := sql.NullInt64{Int64: id, Valid: ok}
		if want == g.resolved {
			continue
		}
		if _, err := tx.ExecContext(ctx, "UPDATE edges SET dst_entry_id = ? WHERE rowid = ?", want, g.rowid); err != nil {
			return fmt.Errorf("resolve edge to %s: %w", g.dst, err)
		}
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	if err := tx.Commit(); err != nil {
		return fmt.Errorf("commit edge resolution: %w", err)
	}
	return nil
}

// edgeTargets reads every entry with its aliases and the files its chunks came from.
func edgeTargets(ctx context.Context, tx *sql.Tx) ([]store.EdgeTarget, error) {
	rows, err := tx.QueryContext(ctx, "SELECT id, path, aliases FROM entries ORDER BY id")
	if err != nil {
		return nil, fmt.Errorf("read entries for edge resolution: %w", err)
	}
	var out []store.EdgeTarget
	byID := map[int64]int{}
	for rows.Next() {
		var t store.EdgeTarget
		var aliases string
		if err := rows.Scan(&t.ID, &t.Path, &aliases); err != nil {
			rows.Close()
			return nil, fmt.Errorf("read entries for edge resolution: %w", err)
		}
		if t.Aliases, err = decodeList("aliases", aliases); err != nil {
			rows.Close()
			return nil, fmt.Errorf("entry %s: %w", t.Path, err)
		}
		byID[t.ID] = len(out)
		out = append(out, t)
	}
	err = rows.Err()
	rows.Close()
	if err != nil {
		return nil, fmt.Errorf("read entries for edge resolution: %w", err)
	}
	files, err := tx.QueryContext(ctx, "SELECT DISTINCT entry_id, source_file FROM chunks ORDER BY entry_id, source_file")
	if err != nil {
		return nil, fmt.Errorf("read indexed files for edge resolution: %w", err)
	}
	defer files.Close()
	for files.Next() {
		var id int64
		var f string
		if err := files.Scan(&id, &f); err != nil {
			return nil, fmt.Errorf("read indexed files for edge resolution: %w", err)
		}
		if i, ok := byID[id]; ok {
			out[i].Files = append(out[i].Files, f)
		}
	}
	if err := files.Err(); err != nil {
		return nil, fmt.Errorf("read indexed files for edge resolution: %w", err)
	}
	return out, nil
}

// Edges returns every edge with both ends named, sorted by source path, rel, kind and target.
func (s *Store) Edges(ctx context.Context) ([]store.EdgeRow, error) {
	out, err := s.edges(ctx)
	if err != nil {
		return nil, wrap(ctx, err, s.path, "sqlite.Edges", "could not read the links between entries")
	}
	return out, nil
}

func (s *Store) edges(ctx context.Context) ([]store.EdgeRow, error) {
	rows, err := s.db.QueryContext(ctx, `
		SELECT g.src_entry_id, src.path, g.dst_path, g.rel, g.kind, coalesce(g.dst_entry_id, 0), coalesce(dst.path, '')
		FROM edges AS g
		JOIN entries AS src ON src.id = g.src_entry_id
		LEFT JOIN entries AS dst ON dst.id = g.dst_entry_id
		ORDER BY src.path, g.rel, g.kind, g.dst_path`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []store.EdgeRow
	for rows.Next() {
		var g store.EdgeRow
		if err := rows.Scan(&g.SrcEntryID, &g.SrcPath, &g.DstPath, &g.Rel, &g.Kind, &g.DstEntryID, &g.DstEntryPath); err != nil {
			return nil, err
		}
		out = append(out, g)
	}
	return out, rows.Err()
}

// ftsTitle is what the FTS title column holds: the title, then each alias, so an alias is found
// like a title word (with the title's bm25 weight). The identifier index has no title column and
// gets no aliases.
func ftsTitle(title string, aliases []string) string {
	if len(aliases) == 0 {
		return title
	}
	return title + " " + strings.Join(aliases, " ")
}
