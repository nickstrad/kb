package embed_test

import (
	"context"
	"testing"

	"github.com/nickstrad/kb/embed"
)

type prefixed struct{ embed.None }

func (prefixed) Prefixes() (string, string) { return "d: ", "q: " }

// An embedder without Prefixer gets the text as is (v2's rule); one with it gets its own prefixes.
func TestDocAndQueryText(t *testing.T) {
	cases := []struct {
		name       string
		e          embed.Embedder
		doc, query string
	}{
		{"no prefixer", embed.None{}, "x", "x"},
		{"prefixer", prefixed{}, "d: x", "q: x"},
	}
	for _, c := range cases {
		if got := embed.DocText(c.e, "x"); got != c.doc {
			t.Errorf("%s: DocText = %q, want %q", c.name, got, c.doc)
		}
		if got := embed.QueryText(c.e, "x"); got != c.query {
			t.Errorf("%s: QueryText = %q, want %q", c.name, got, c.query)
		}
	}
}

func TestNone(t *testing.T) {
	var n embed.None
	if n.Model() != embed.NoneModel || n.Dim() != 0 || !embed.IsNone(n) || !embed.IsNone(nil) {
		t.Fatal("None identity")
	}
	if _, err := n.Embed(context.Background(), []string{"x"}); err == nil {
		t.Fatal("None.Embed must fail")
	}
	e, err := embed.NoneProvider().New(embed.Config{})
	if err != nil || !embed.IsNone(e) {
		t.Fatalf("NoneProvider: %v %v", e, err)
	}
}
