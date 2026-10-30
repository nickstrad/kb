//go:build !noassert

package reindex

import (
	"context"
	"path/filepath"
	"testing"

	"github.com/nickstrad/kb/internal/assert"
	"github.com/nickstrad/kb/internal/embed/fake"
	"github.com/nickstrad/kb/internal/entry"
	"github.com/nickstrad/kb/internal/store"
	"github.com/nickstrad/kb/internal/store/sqlite"
)

// TestEmbedMetaPrecondition: running with an embedder embed_meta was not recorded for is a caller
// bug (the CLI always runs EnsureEmbedMeta first), for All and One alike.
func TestEmbedMetaPrecondition(t *testing.T) {
	ctx := context.Background()
	for _, recorded := range []bool{false, true} {
		st, err := sqlite.Open(ctx, filepath.Join(t.TempDir(), "kb.sqlite"), sqlite.Options{})
		if err != nil {
			t.Fatal(err)
		}
		defer st.Close()
		if recorded {
			if err := st.EnsureEmbedMeta(ctx, store.EmbedMeta{Model: "other", Dim: 4}); err != nil {
				t.Fatal(err)
			}
		}
		o := Options{Root: t.TempDir(), Store: st, Embedder: fake.New("m", 8)}
		for name, run := range map[string]func(){
			"All": func() { All(ctx, o) },
			"One": func() { One(ctx, o, entry.Entry{Path: "data/a.md", Kind: entry.KindFile}) },
		} {
			func() {
				defer func() {
					if v, ok := recover().(assert.Violation); !ok || v.Kind != "precondition" {
						t.Errorf("%s (recorded=%v): recovered %#v, want a precondition Violation", name, recorded, v)
					}
				}()
				run()
			}()
		}
	}
}
