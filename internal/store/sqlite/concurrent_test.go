package sqlite

import (
	"context"
	"fmt"
	"github.com/nickstrad/kb/internal/store"
	"path/filepath"
	"sync"
	"testing"
)

// TestConcurrentWritersFromTwoHandles is the regression test for "database is locked" under
// concurrent `kb add`: two Store handles (two processes, as far as SQLite is concerned) on one file
// write entries at the same time. With deferred transactions the read-to-write lock upgrade
// failed at once with SQLITE_BUSY, bypassing busy_timeout; with _txlock=immediate every writer
// waits its turn and all succeed.
func TestConcurrentWritersFromTwoHandles(t *testing.T) {
	path := filepath.Join(t.TempDir(), "kb.sqlite")
	const handles, perHandle = 2, 15
	stores := make([]*Store, handles)
	for i := range stores {
		s, err := Open(context.Background(), path, Options{})
		if err != nil {
			t.Fatalf("open handle %d: %v", i, err)
		}
		defer s.Close()
		stores[i] = s
	}
	if err := stores[0].EnsureEmbedMeta(context.Background(), store.EmbedMeta{Model: "m", Dim: 4}); err != nil {
		t.Fatalf("EnsureEmbedMeta: %v", err)
	}

	ctx := context.Background()
	errs := make(chan error, handles*perHandle)
	var wg sync.WaitGroup
	start := make(chan struct{})
	for h, s := range stores {
		wg.Add(1)
		go func(h int, s *Store) {
			defer wg.Done()
			<-start
			for i := 0; i < perHandle; i++ {
				p := fmt.Sprintf("h%d-%d.md", h, i)
				// Read first, then write: the shape that made the deferred upgrade fail.
				if _, err := s.Entry(context.Background(), p); err != nil {
					errs <- err
					continue
				}
				in := store.ChunkInput{Ord: 0, SourceFile: p, Text: "text " + p, TextHash: p, Embedding: []float32{1, 0, 0, float32(i)}}
				if _, err := s.ReplaceEntry(ctx, store.EntryInput{Path: p, Kind: "file", Title: p, Summary: "s", BodyHash: p}, []store.ChunkInput{in}); err != nil {
					errs <- fmt.Errorf("handle %d entry %d: %w", h, i, err)
				}
			}
		}(h, s)
	}
	close(start)
	wg.Wait()
	close(errs)
	for err := range errs {
		t.Error(err)
	}
	if n := countRows(t, stores[0], "SELECT count(*) FROM entries"); n != handles*perHandle {
		t.Errorf("entries = %d, want %d", n, handles*perHandle)
	}
}
