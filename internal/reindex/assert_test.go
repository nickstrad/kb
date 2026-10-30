//go:build !noassert

package reindex

import (
	"context"
	"path/filepath"
	"testing"

	"github.com/nickstrad/kb/internal/assert"
	"github.com/nickstrad/kb/internal/embed/fake"
	"github.com/nickstrad/kb/internal/entry"
	"github.com/nickstrad/kb/internal/store/sqlite"
)

// TestEmbedMetaPrecondition: running before embed_meta was recorded is a caller bug (the CLI always
// runs EnsureEmbedMeta first), for All and One alike. A recorded but different embed_meta is a
// Conflict error instead (TestEmbedMetaMismatchIsConflict).
func TestEmbedMetaPrecondition(t *testing.T) {
	ctx := context.Background()
	st, err := sqlite.Open(ctx, filepath.Join(t.TempDir(), "kb.sqlite"), sqlite.Options{})
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	o := Options{Root: t.TempDir(), Store: st, Embedder: fake.New("m", 8)}
	for name, run := range map[string]func(){
		"All": func() { All(ctx, o) },
		"One": func() { One(ctx, o, entry.Entry{Path: "data/a.md", Kind: entry.KindFile}) },
	} {
		func() {
			defer func() {
				if v, ok := recover().(assert.Violation); !ok || v.Kind != "precondition" {
					t.Errorf("%s: recovered %#v, want a precondition Violation", name, v)
				}
			}()
			run()
		}()
	}
}
