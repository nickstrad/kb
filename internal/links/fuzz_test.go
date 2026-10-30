package links

import (
	"reflect"
	"strings"
	"testing"
)

// FuzzExtract checks Extract on arbitrary input: it never panics; it is deterministic; no
// Markdown target escapes the root (resolve's L1 assertion owns "clean, relative, non-empty");
// every wikilink target is non-empty, trimmed and holds no bracket or pipe; lines are within the
// body. Two metamorphic properties: (1) a leading blank line shifts every link down one line and
// changes nothing else; (2) the body wrapped in a tilde fence longer than any tilde run in it
// yields no links.
//
// Run: go test -tags fts5 ./internal/links/ -run '^$' -fuzz FuzzExtract -fuzztime 20s
func FuzzExtract(f *testing.F) {
	for _, s := range []string{
		"[a](b.md#c) [[d|e]] `[x](y.md)`\n```\n[z](z.md)\n```\n",
		"[a](../../../../x.md)", "[a](%2e%2e/%2e%2e/x.md)", "[a](..%2f..%2fx.md)", "[a](<../ x>)",
		"[[[[a]]]]", "![[a]] ![b](c)", "~~~\n[a](b)", "[a](", "[a](a\\)", "``` `", "[a](a b)",
		"[a](./././b/../c.md?q#h)", "[a](a//b///c.md)", "[a](%00.md)", "\r\n[[\r\n]]",
	} {
		f.Add("data/x/README.md", []byte(s))
	}
	f.Add("a.md", []byte("[a](../b.md)"))
	f.Add("../../a.md", []byte("[a](../b.md)"))
	f.Add("", []byte("[a](b.md)"))

	f.Fuzz(func(t *testing.T, src string, body []byte) {
		got := Extract(src, body)
		if again := Extract(src, body); !reflect.DeepEqual(got, again) {
			t.Fatalf("not deterministic: %+v vs %+v", got, again)
		}
		nlines := strings.Count(string(body), "\n") + 1
		for _, l := range got {
			if l.Line < 1 || l.Line > nlines || l.Rel != RelLinks {
				t.Fatalf("bad line/rel: %+v (lines %d)", l, nlines)
			}
			switch l.Kind {
			case KindMarkdown:
				if l.Target == "." || l.Target == ".." || strings.HasPrefix(l.Target, "../") {
					t.Fatalf("unclean markdown target %q from src %q", l.Target, src)
				}
			case KindWikilink:
				if l.Target == "" || l.Target != strings.TrimSpace(l.Target) || strings.ContainsAny(l.Target, "[]|\n") {
					t.Fatalf("bad wikilink target %q", l.Target)
				}
			default:
				t.Fatalf("unknown kind %q", l.Kind)
			}
		}

		// (1) Shift: a blank first line moves every link down by one.
		shifted := Extract(src, append([]byte("\n"), body...))
		if len(shifted) != len(got) {
			t.Fatalf("leading blank line changed the link count: %d -> %d", len(got), len(shifted))
		}
		for i := range got {
			want := got[i]
			want.Line++
			if shifted[i] != want {
				t.Fatalf("leading blank line: link %d = %+v, want %+v", i, shifted[i], want)
			}
		}

		// (2) Fence: nothing inside a fenced block is a link.
		run, longest := 0, 0
		for _, c := range body {
			if c == '~' {
				run++
				longest = max(longest, run)
			} else {
				run = 0
			}
		}
		fence := strings.Repeat("~", max(3, longest+1))
		wrapped := fence + "\n" + string(body) + "\n" + fence + "\n"
		if in := Extract(src, []byte(wrapped)); len(in) != 0 {
			t.Fatalf("links found inside a fence: %+v", in)
		}
	})
}
