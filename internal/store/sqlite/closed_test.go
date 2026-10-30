package sqlite

import (
	"context"
	"github.com/nickstrad/kb/internal/store"
	"path/filepath"
	"testing"
)

// TestClosedStoreReturnsErrors reaches the driver-error return of every public method by calling
// it on a closed handle: each must return an error (no panic, no silent success). This is the
// cheap way to reach those lines; the messages themselves are checked where a real failure is
// reproducible (mismatch, missing table, bad input).
func TestClosedStoreReturnsErrors(t *testing.T) {
	s, err := Open(context.Background(), filepath.Join(t.TempDir(), "kb.sqlite"), Options{})
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	if err := s.EnsureEmbedMeta(context.Background(), store.EmbedMeta{Model: "m", Dim: 4}); err != nil {
		t.Fatalf("EnsureEmbedMeta: %v", err)
	}
	if s.Path() == "" {
		t.Error("Path is empty")
	}
	s.Close()
	ctx := context.Background()
	for name, call := range map[string]func() error{
		"EnsureEmbedMeta":    func() error { return s.EnsureEmbedMeta(context.Background(), store.EmbedMeta{Model: "m", Dim: 4}) },
		"EmbedMeta":          func() error { _, _, err := s.EmbedMeta(context.Background()); return err },
		"HasVectors":         func() error { _, err := s.HasVectors(ctx); return err },
		"ResetForReindex":    func() error { return s.ResetIndex(ctx) },
		"GetEntryByPath":     func() error { _, err := s.Entry(context.Background(), "a.md"); return err },
		"ListEntries":        func() error { _, err := s.Entries(context.Background()); return err },
		"ListChunks":         func() error { _, err := s.Chunks(context.Background(), 1); return err },
		"ExistingEmbeddings": func() error { _, err := s.Embeddings(ctx, 1); return err },
		"ReplaceEntryChunks": func() error { _, err := s.ReplaceEntry(ctx, sampleEntry(), nil); return err },
		"DeleteEntry":        func() error { return s.DeleteEntry(ctx, "a.md") },
	} {
		if err := call(); err == nil {
			t.Errorf("%s on a closed store returned nil", name)
		}
	}
	var nilStore *Store
	if err := nilStore.Close(); err != nil {
		t.Errorf("Close on a nil store = %v, want nil", err)
	}
}

// TestDecodeHelpersRejectCorruptRows: a tags column that is not a JSON array and a vector blob
// that is not whole float32s are reported, not misread.
func TestDecodeHelpersRejectCorruptRows(t *testing.T) {
	if _, err := decodeTags("not json"); err == nil {
		t.Error("decodeTags accepted corrupt JSON")
	}
	if _, err := deserializeFloat32([]byte{1, 2, 3}); err == nil {
		t.Error("deserializeFloat32 accepted a 3-byte blob")
	}
	s, emb := newTestStore(t)
	if _, err := s.ReplaceEntry(context.Background(), sampleEntry(), chunkInputs(t, emb, "one")); err != nil {
		t.Fatalf("ReplaceEntryChunks: %v", err)
	}
	if _, err := s.DB().Exec("UPDATE entries SET tags = 'corrupt'"); err != nil {
		t.Fatalf("corrupt tags: %v", err)
	}
	if _, err := s.Entry(context.Background(), "sample.md"); err == nil {
		t.Error("GetEntryByPath read a corrupt tags column without error")
	}
	if _, err := s.Entries(context.Background()); err == nil {
		t.Error("ListEntries read a corrupt tags column without error")
	}
	if err := s.DeleteEntry(context.Background(), "sample.md"); err == nil {
		t.Error("DeleteEntry read a corrupt tags column without error")
	}
}
