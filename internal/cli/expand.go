// `kb search --expand section|entry`: widen each hit to its section or whole entry.
package cli

import (
	"context"

	"github.com/nickstrad/kb/internal/render"
	"github.com/nickstrad/kb/internal/search"
	"github.com/nickstrad/kb/internal/store"
)

// chunkLookup is the second thing expansion needs from a store: one entry's chunks in ord order.
// A lookup that lacks it expands nothing.
type chunkLookup interface {
	Chunks(ctx context.Context, entryID int64) ([]store.ChunkRow, error)
}

// expansion is one hit's expanded text ("" = not expanded) and its dedupe key ("" = never merged).
type expansion struct {
	Text string
	Key  string
}

// expandHits expands every hit through its own store's lookup (the Store tag: a cross-store hit's
// chunks live in that store, never the primary's). The hit's chunk is found by heading and text in
// its entry's chunks; a hit whose store has no lookup, whose entry is gone or whose chunk no
// longer matches (reindexed since the search) is left unexpanded. A store error is returned.
// mode "none" or "" returns no expansions without reading anything.
//
// Dedupe keys: entry mode keys a hit by its entry (store and path), section mode by its section
// (entry, source file and heading), so two hits of one section render once.
func expandHits(ctx context.Context, lookups map[string]entryLookup, hits []search.Hit, mode string) ([]expansion, error) {
	out := make([]expansion, len(hits))
	if mode != render.ExpandSection && mode != render.ExpandEntry {
		return out, nil
	}
	rows, err := entryRows(ctx, lookups, hits)
	if err != nil {
		return nil, err
	}
	type entryKey struct{ store, path string }
	cache := map[entryKey][]render.Chunk{}
	for i, h := range hits {
		row, ok := rows[h.Store][h.Path]
		cl, hasChunks := lookups[h.Store].(chunkLookup)
		if !ok || !hasChunks {
			continue
		}
		k := entryKey{h.Store, h.Path}
		chunks, seen := cache[k]
		if !seen {
			rs, err := cl.Chunks(ctx, row.ID)
			if err != nil {
				return nil, err
			}
			for _, r := range rs {
				chunks = append(chunks, render.Chunk{SourceFile: r.SourceFile, Heading: r.Heading, Text: r.Text})
			}
			cache[k] = chunks
		}
		at := -1
		for j, c := range chunks {
			if c.Heading == h.Heading && c.Text == h.Text {
				at = j
				break
			}
		}
		if at < 0 {
			continue
		}
		key := h.Store + "\x00" + h.Path
		if mode == render.ExpandEntry {
			out[i] = expansion{Text: render.Entry(chunks), Key: key}
		} else {
			out[i] = expansion{Text: render.Section(chunks, at), Key: key + "\x00" + chunks[at].SourceFile + "\x00" + chunks[at].Heading}
		}
	}
	return out, nil
}
