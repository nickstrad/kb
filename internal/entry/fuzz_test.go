package entry

import (
	"bytes"
	"errors"
	"strings"
	"testing"
)

// FuzzParseFrontMatter checks ParseFrontMatter's documented contract on arbitrary bytes:
//
//  1. it never panics;
//  2. without a leading "---" fence (after an optional BOM) there is no front matter: a zero
//     FrontMatter, the source minus the BOM as body, and no error;
//  3. an error that is not a *ValidationError (the unclosed fence) returns the whole original
//     source as body;
//  4. a nil error means title and summary are both set and each is one line.
//
// Run: go test ./internal/entry/ -run '^$' -fuzz FuzzParseFrontMatter -fuzztime 60s
func FuzzParseFrontMatter(f *testing.F) {
	for _, seed := range []string{
		"---\ntitle: T\nsummary: S\ntags: [a, b]\nupdated: 2026-10-03\n---\n# Body\n",
		"\ufeff---\r\ntitle: \"Quoted: with colon\"\r\nsummary: 'one line'\r\n---\r\nbody",
		"---\ntitle: T\nsummary: S\ntags:\n  - a\n  - b\n# comment\n---\n",
		"---\ntitle: |\n  multi\nsummary: S\n---\n",
		"---\ntitle: T\nsummary: first\n  continuation\n---\n",
		"---\ntitle: T\nsummary: S\n",
		"no front matter at all\n---\n",
		"---\n---\n",
		"",
	} {
		f.Add([]byte(seed))
	}

	f.Fuzz(func(t *testing.T, src []byte) {
		fm, body, err := ParseFrontMatter(src)

		rest := bytes.TrimPrefix(src, []byte("\ufeff"))
		fenced := bytes.HasPrefix(rest, []byte("---\n")) || bytes.HasPrefix(rest, []byte("---\r\n"))
		if !fenced {
			if err != nil {
				t.Fatalf("unfenced source: err = %v, want nil", err)
			}
			if fm.Title != "" || fm.Summary != "" || fm.Tags != nil || fm.Updated != "" || fm.Verified != "" {
				t.Fatalf("unfenced source: fm = %+v, want zero", fm)
			}
			if !bytes.Equal(body, rest) {
				t.Fatalf("unfenced source: body = %q, want %q", body, rest)
			}
			return
		}

		var ve *ValidationError
		if err != nil && !errors.As(err, &ve) && !bytes.Equal(body, src) {
			t.Fatalf("structural error %v: body = %q, want the whole source", err, body)
		}
		if err == nil {
			for name, v := range map[string]string{"title": fm.Title, "summary": fm.Summary} {
				if strings.TrimSpace(v) == "" {
					t.Fatalf("nil error but %s is empty: %+v", name, fm)
				}
				if strings.Contains(v, "\n") {
					t.Fatalf("nil error but %s spans lines: %q", name, v)
				}
			}
		}
	})
}
