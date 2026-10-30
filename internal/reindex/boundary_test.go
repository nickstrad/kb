package reindex

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/nickstrad/kb/internal/embed"
	"github.com/nickstrad/kb/internal/embed/fake"
)

// writeEntryWithChunks writes data/e.md so that chunk.Split yields exactly n chunks: the summary
// chunk plus n-1 "##" sections.
func writeEntryWithChunks(t *testing.T, root string, n int) {
	t.Helper()
	var b strings.Builder
	b.WriteString("---\ntitle: E\nsummary: S.\n---\n")
	for i := 1; i < n; i++ {
		fmt.Fprintf(&b, "\n## Section %d\n\nbody %d\n", i, i)
	}
	path := filepath.Join(root, "data", "e.md")
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(b.String()), 0o644); err != nil {
		t.Fatal(err)
	}
}

// TestBatchSizeBoundary: one embedder call per BatchSize texts, at and around the limit, and the
// default (16) when BatchSize is not positive.
func TestBatchSizeBoundary(t *testing.T) {
	for _, tc := range []struct {
		name      string
		chunks    int
		batchSize int
		wantCalls int
	}{
		{"one chunk", 1, defaultBatchSize, 1},
		{"defaultBatchSize-1", defaultBatchSize - 1, defaultBatchSize, 1},
		{"at defaultBatchSize", defaultBatchSize, defaultBatchSize, 1},
		{"defaultBatchSize+1", defaultBatchSize + 1, defaultBatchSize, 2},
		{"2*defaultBatchSize", 2 * defaultBatchSize, defaultBatchSize, 2},
		{"BatchSize 0 means default, at it", defaultBatchSize, 0, 1},
		{"BatchSize 0 means default, above it", defaultBatchSize + 1, 0, 2},
		{"BatchSize -1 means default", defaultBatchSize + 1, -1, 2},
		{"BatchSize 1", 3, 1, 3},
	} {
		t.Run(tc.name, func(t *testing.T) {
			root := t.TempDir()
			writeEntryWithChunks(t, root, tc.chunks)
			emb := &countingEmbedder{Embedder: fake.New("fake", testDim)}
			s := newTestStore(t, emb)
			sum, err := All(context.Background(), Options{Root: root, Store: s, Embedder: emb, BatchSize: tc.batchSize})
			if err != nil {
				t.Fatal(err)
			}
			if sum.Chunks != tc.chunks {
				t.Fatalf("fixture gave %d chunks, want %d", sum.Chunks, tc.chunks)
			}
			if sum.EmbedCalls != tc.wantCalls || emb.calls != tc.wantCalls {
				t.Fatalf("EmbedCalls=%d embedder calls=%d, want %d", sum.EmbedCalls, emb.calls, tc.wantCalls)
			}
		})
	}
}

// brokenEmbedder returns err, or (when err is nil) one vector too few.
type brokenEmbedder struct {
	embed.Embedder
	err error
}

func (b brokenEmbedder) Embed(ctx context.Context, texts []string) ([][]float32, error) {
	if b.err != nil {
		return nil, b.err
	}
	vecs, err := b.Embedder.Embed(ctx, texts)
	return vecs[:len(vecs)-1], err
}

// TestEmbedFailureAbortsRun: an embedder error aborts the run, keeps embed.ErrUnavailable
// reachable with errors.Is (the CLI's exit-2 decision), and writes nothing for the entry; a wrong
// vector count is an error too, never a partial write.
func TestEmbedFailureAbortsRun(t *testing.T) {
	unavailable := fmt.Errorf("model missing: %w", embed.ErrUnavailable)
	for _, tc := range []struct {
		name   string
		err    error
		wantIs error // nil: only require an error
	}{
		{"unavailable: wrapped, Is ErrUnavailable", unavailable, embed.ErrUnavailable},
		{"other error: wrapped", errors.New("boom"), nil},
		{"one vector short: error", nil, nil},
	} {
		t.Run(tc.name, func(t *testing.T) {
			root := t.TempDir()
			writeEntryWithChunks(t, root, 3)
			emb := brokenEmbedder{fake.New("fake", testDim), tc.err}
			s := newTestStore(t, emb)
			_, err := All(context.Background(), Options{Root: root, Store: s, Embedder: emb})
			if err == nil {
				t.Fatal("want an error")
			}
			if tc.wantIs != nil && !errors.Is(err, tc.wantIs) {
				t.Fatalf("err = %v, want one wrapping %v", err, tc.wantIs)
			}
			if tc.err != nil && !errors.Is(err, tc.err) {
				t.Fatalf("err = %v, chain lost the embedder's error", err)
			}
			if !strings.Contains(err.Error(), "data/e.md") {
				t.Errorf("err = %v, want it to name the entry", err)
			}
			if rows, lerr := s.Entries(context.Background()); lerr != nil || len(rows) != 0 {
				t.Fatalf("entries after a failed embed = %v (%v), want none", rows, lerr)
			}
		})
	}
}
