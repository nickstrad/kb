package entry

import (
	"bytes"
	"errors"
	"strings"
	"testing"
	"unicode"
	"unicode/utf8"
)

// FuzzParseFrontMatter checks ParseFrontMatter's documented contract on arbitrary bytes:
//
//  1. it never panics;
//  2. without a leading "---" fence (after an optional BOM) there is no front matter: a zero
//     FrontMatter, the source minus the BOM as body, and no error;
//  3. an error that is not a *ValidationError (the unclosed fence) returns the whole original
//     source as body;
//  4. a nil error means title and summary are both set and each is one line;
//  5. a nil error means title and summary are valid UTF-8 with no control characters, within
//     MaxTitleRunes and MaxSummaryRunes;
//  6. a nil error means aliases number at most MaxAliases, each valid and within MaxAliasRunes.
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
		"---\ntitle: T\nsummary: S\nrelated: [a.md, \"[[b]]\"]\nsupersedes:\n  - c.md\ndepends_on: []\naliases: [x, y]\n---\n",
		"---\ntitle: T\nsummary: S\naliases:\n  - \"\\x01\"\n  - " + strings.Repeat("a", MaxAliasRunes+1) + "\n---\n",
		"",
		"---\ntitle: \x00\x01\x02\nsummary: s\n---\n",
		"---\ntitle: a\xffb\nsummary: s\n---\n",
		"---\ntitle: " + strings.Repeat("T", MaxTitleRunes+1) + "\nsummary: s\n---\n",
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
			if fm.Title != "" || fm.Summary != "" || fm.Tags != nil || fm.Updated != "" || fm.Verified != "" ||
				fm.Related != nil || fm.Supersedes != nil || fm.DependsOn != nil || fm.Aliases != nil {
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
				if !utf8.ValidString(v) || strings.IndexFunc(v, unicode.IsControl) >= 0 {
					t.Fatalf("nil error but %s has invalid UTF-8 or a control character: %q", name, v)
				}
			}
			if len(fm.Aliases) > MaxAliases {
				t.Fatalf("nil error but %d aliases", len(fm.Aliases))
			}
			for _, a := range fm.Aliases {
				if !utf8.ValidString(a) || strings.IndexFunc(a, unicode.IsControl) >= 0 || utf8.RuneCountInString(a) > MaxAliasRunes {
					t.Fatalf("nil error but alias %q is invalid", a)
				}
			}
			if utf8.RuneCountInString(fm.Title) > MaxTitleRunes || utf8.RuneCountInString(fm.Summary) > MaxSummaryRunes {
				t.Fatalf("nil error but title (%d) or summary (%d) is over its cap",
					utf8.RuneCountInString(fm.Title), utf8.RuneCountInString(fm.Summary))
			}
		}
	})
}
