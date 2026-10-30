package sqlite

import (
	"context"
	"fmt"
	"path/filepath"
	"testing"

	"github.com/nickstrad/kb/internal/store"
)

// BenchmarkResolveEdges measures the ResolveEdges that ends every reindex All and One (kb add,
// kb edit, kb reindex <entry>): a full read of entries, chunk source files and edges, then the
// resolver. Each entry has 3 chunks and 5 edges (3 path links, 2 wikilinks), a fifth of them
// dangling. "steady" is the usual case (nothing changed, no UPDATE); "all-new" resolves every
// edge from NULL. Leverage entry reindex-throughput cites the numbers.
func BenchmarkResolveEdges(b *testing.B) {
	for _, n := range []int{1000, 5000} {
		st := benchStore(b, n)
		b.Run(fmt.Sprintf("steady/%d", n), func(b *testing.B) {
			ctx := context.Background()
			if err := st.ResolveEdges(ctx); err != nil {
				b.Fatal(err)
			}
			b.ResetTimer()
			for b.Loop() {
				if err := st.ResolveEdges(ctx); err != nil {
					b.Fatal(err)
				}
			}
		})
		b.Run(fmt.Sprintf("all-new/%d", n), func(b *testing.B) {
			ctx := context.Background()
			for b.Loop() {
				b.StopTimer()
				if _, err := st.DB().ExecContext(ctx, "UPDATE edges SET dst_entry_id = NULL"); err != nil {
					b.Fatal(err)
				}
				b.StartTimer()
				if err := st.ResolveEdges(ctx); err != nil {
					b.Fatal(err)
				}
			}
		})
	}
}

func benchStore(b *testing.B, n int) *Store {
	b.Helper()
	ctx := context.Background()
	st, err := Open(ctx, filepath.Join(b.TempDir(), "kb.sqlite"), Options{})
	if err != nil {
		b.Fatal(err)
	}
	b.Cleanup(func() { st.Close() })
	if err := st.EnsureEmbedMeta(ctx, store.EmbedMeta{Model: store.FTSOnlyModel}); err != nil {
		b.Fatal(err)
	}
	for i := range n {
		p := fmt.Sprintf("data/d%d/e%d.md", i%50, i)
		e := store.EntryInput{Path: p, Kind: "file", Title: "T", BodyHash: p, Aliases: []string{fmt.Sprintf("alias%d", i)}}
		for j := 1; j <= 3; j++ {
			e.Edges = append(e.Edges, store.EdgeInput{DstPath: fmt.Sprintf("data/d%d/e%d.md", (i+j)%50, (i+j*7)%(n+n/5)), Rel: "links", Kind: store.EdgePath})
		}
		e.Edges = append(e.Edges,
			store.EdgeInput{DstPath: fmt.Sprintf("e%d", (i+11)%n), Rel: "links", Kind: store.EdgeWikilink},
			store.EdgeInput{DstPath: fmt.Sprintf("alias%d", (i+13)%n), Rel: "related", Kind: store.EdgeWikilink})
		var cs []store.ChunkInput
		for j := range 3 {
			text := fmt.Sprintf("entry %d chunk %d", i, j)
			cs = append(cs, store.ChunkInput{Ord: j, SourceFile: p, Text: text, TextHash: text})
		}
		if _, err := st.ReplaceEntry(ctx, e, cs); err != nil {
			b.Fatal(err)
		}
	}
	return st
}
