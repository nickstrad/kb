# Edges, entry_tags and aliases (PLAN 5.2, in the baseline schema)

> The baseline schema has `edges` (kind path|wikilink, dst_entry_id NULL = dangling), `entry_tags` (NormTag: trimmed lowercase; the tag filter joins it) and `entries.aliases` (folded into the FTS title column); reindex writes links in ReplaceEntry and resolves them with ResolveEdges after every All/One.

**Fact.**
- Schema: `edges(src_entry_id, dst_path, dst_entry_id, rel, kind)`, PK `(src_entry_id, dst_path, rel, kind)`,
  `dst_entry_id REFERENCES entries ON DELETE SET NULL`, index `edges_dst`. `kind` is not in the PLAN §2.5
  statement: it is needed so a wikilink `[[x.md]]` and a path link `x.md` with one rel do not collide, and so
  5.4 can list broken *path* links (`kind = 'path' AND dst_entry_id IS NULL`).
- Resolution is one pure rule, `store.EdgeResolver` (internal/store/edges.go): path → entry path, then
  `<dir>/README.md`, then the entry whose chunks came from that file; wikilink → those for T and T+".md",
  then basename (case-insensitive, no `/` in T; a README.md entry is named by its dir), then alias
  (case-insensitive). Ties: smallest path. `sqlite.ResolveEdges` reads entries + DISTINCT chunk source files
  and rewrites only changed rows.
- ReplaceEntry rewrites an entry's tags and edges in the chunk transaction; an edge whose key it had before
  keeps its resolved target, a new one starts dangling. DeleteEntry nulls inbound edges explicitly (FK SET
  NULL too). `assertLinkSets` checks tags = NormTags(input) and edges = UniqueEdges(input).
- Tag filter: `JOIN entry_tags t ON t.entry_id = c.entry_id AND t.tag = NormTag(q.Tag)` (FTS and the KNN
  post-filter). `entries.tags` keeps the case as written; entry_tags is
  written only by ReplaceEntry with Go's full fold (the pre-release SQL backfill is gone with the squash).
- Aliases go into `chunks_fts.title` (title + " " + aliases), not into `chunks_exact`.
- reindex.Links builds edges: front-door front-matter lists (against the front door's dir), then each file's
  body links (against its own dir), links.Dedupe, path self-links (own files or dir) dropped.
- `reindex.Options.Discover` is the seam for All's two discoveries; `pruneSet` is the pure prune rule.

**Why it matters.** 5.3 (`kb related`, supersede demotion) and 5.4 (`kb stale` broken links) read these
tables; a new write path must keep tags/edges in step like the five chunk tables.

**How to apply.** Read edges through `Index.Edges` (or a new sqlite reader for 5.3's CTE). Change resolution
only in EdgeResolver + `TestEdgeResolver`. The storetest model (`RunModel`) checks tags and edges against a
model after every op; keep its seeds green.

**Seen in:** internal/store/edges.go, internal/store/sqlite/{edges,index,retrieve,migrations}.go, internal/reindex/reindex.go · 2026-10-30
