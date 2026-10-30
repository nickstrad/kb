package embed_test

import (
	"testing"

	"github.com/nickstrad/kb/internal/embed"
	"github.com/nickstrad/kb/internal/embed/embedtest"
	"github.com/nickstrad/kb/internal/embed/fake"
)

func TestNoneContract(t *testing.T) {
	embedtest.Run(t, func(*testing.T) embed.Embedder { return embed.None{} }, embedtest.Caps{FTSOnly: true})
}

// prefixed is a fake embedder that also implements embed.Prefixer.
type prefixed struct {
	*fake.Embedder
	doc, query string
}

func (p prefixed) Prefixes() (string, string) { return p.doc, p.query }

func TestDocAndQueryText(t *testing.T) {
	plain := fake.New("plain", 4)
	cases := []struct {
		name      string
		e         embed.Embedder
		wantDoc   string
		wantQuery string
	}{
		{"no Prefixer: text as is", plain, "body", "q"},
		{"Prefixer with both", prefixed{plain, "D: ", "Q: "}, "D: body", "Q: q"},
		{"Prefixer with query only", prefixed{plain, "", "Q: "}, "body", "Q: q"},
		{"Prefixer with neither", prefixed{plain, "", ""}, "body", "q"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			doc := ""
			if p, ok := c.e.(embed.Prefixer); ok {
				doc, _ = p.Prefixes()
			}
			// Empty text: the prefix alone (or nothing), never a dropped or doubled prefix.
			if got := embed.DocText(c.e, ""); got != doc {
				t.Errorf("DocText(empty) = %q, want %q", got, doc)
			}
			if got := embed.DocText(c.e, "body"); got != c.wantDoc {
				t.Errorf("DocText = %q, want %q", got, c.wantDoc)
			}
			if got := embed.QueryText(c.e, "q"); got != c.wantQuery {
				t.Errorf("QueryText = %q, want %q", got, c.wantQuery)
			}
		})
	}
}

func TestIsNone(t *testing.T) {
	cases := []struct {
		name string
		e    embed.Embedder
		want bool
	}{
		{"nil", nil, true},
		{"None", embed.None{}, true},
		{"fake", fake.New("f", 4), false},
	}
	for _, c := range cases {
		if got := embed.IsNone(c.e); got != c.want {
			t.Errorf("IsNone(%s) = %v, want %v", c.name, got, c.want)
		}
	}
	if (embed.None{}).Model() != embed.NoneModel {
		t.Errorf("None.Model() = %q, want %q", embed.None{}.Model(), embed.NoneModel)
	}
}
