package embed_test

import (
	"context"
	"math"
	"path/filepath"
	"strings"
	"testing"

	"github.com/nickstrad/kb/internal/embed"
	"github.com/nickstrad/kb/internal/embed/cache"
	"github.com/nickstrad/kb/internal/embed/fake"
)

// FuzzCachedMatchesInner: for any batch, Cached(e) returns exactly what e returns, bit for bit,
// in order: on the first call (mostly misses), on a repeat (all hits), and across iterations,
// since the cache persists for the whole fuzz worker. Texts are the input split on '|'.
func FuzzCachedMatchesInner(f *testing.F) {
	f.Add("")
	f.Add("a")
	f.Add("a|b|a||c")
	f.Add("search_query: x|search_document: x")
	f.Add(strings.Repeat("dup|", 40))

	c, err := cache.Open(context.Background(), filepath.Join(f.TempDir(), "fuzz.sqlite"))
	if err != nil {
		f.Fatalf("cache.Open: %v", err)
	}
	f.Cleanup(func() { c.Close() })
	inner := fake.New("fuzz", 8)
	ce := embed.Cached(inner, c)

	f.Fuzz(func(t *testing.T, in string) {
		var texts []string
		if in != "" {
			texts = strings.Split(in, "|")
		}
		ctx := context.Background()
		want, err := inner.Embed(ctx, texts)
		if err != nil {
			t.Fatalf("inner Embed: %v", err)
		}
		for pass := range 2 {
			got, err := ce.Embed(ctx, texts)
			if err != nil {
				t.Fatalf("pass %d: cached Embed: %v", pass, err)
			}
			if len(got) != len(want) {
				t.Fatalf("pass %d: %d vectors, want %d", pass, len(got), len(want))
			}
			for i := range want {
				for j := range want[i] {
					if math.Float32bits(got[i][j]) != math.Float32bits(want[i][j]) {
						t.Fatalf("pass %d: text %d (%q) float %d differs", pass, i, texts[i], j)
					}
				}
			}
		}
		if ce.Err() != nil {
			t.Fatalf("cache failure: %v", ce.Err())
		}
	})
}
