package reindex

import (
	"context"
	"errors"
	"path/filepath"
	"testing"

	"github.com/nickstrad/kb/internal/embed/fake"
	"github.com/nickstrad/kb/internal/entry"
	"github.com/nickstrad/kb/internal/errs"
	"github.com/nickstrad/kb/internal/store"
	"github.com/nickstrad/kb/internal/store/sqlite"
)

// TestEmbedMetaMismatchIsConflict: another process (or an edited config) can change embed_meta
// between the caller's EnsureEmbedMeta and the run. That is a Conflict that names the fix, in
// every build, for All and One alike, and it happens before anything is written.
func TestEmbedMetaMismatchIsConflict(t *testing.T) {
	ctx := context.Background()
	st, err := sqlite.Open(ctx, filepath.Join(t.TempDir(), "kb.sqlite"), sqlite.Options{})
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	if err := st.EnsureEmbedMeta(ctx, store.EmbedMeta{Model: "other", Dim: 4}); err != nil {
		t.Fatal(err)
	}
	o := Options{Root: t.TempDir(), Store: st, Embedder: fake.New("m", 8)}
	for name, run := range map[string]func() error{
		"All": func() error { _, err := All(ctx, o); return err },
		"One": func() error {
			_, err := One(ctx, o, entry.Entry{Path: "data/a.md", Kind: entry.KindFile})
			return err
		},
	} {
		err := run()
		if errs.KindOf(err) != errs.Conflict || !errors.Is(err, store.ErrEmbedMetaMismatch) {
			t.Errorf("%s: err = %v (kind %v), want a Conflict wrapping ErrEmbedMetaMismatch", name, err, errs.KindOf(err))
		}
	}
}
