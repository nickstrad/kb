package links

import (
	"reflect"
	"testing"

	"github.com/nickstrad/kb/internal/entry"
)

func md(target, anchor string, line int) Link {
	return Link{Target: target, Anchor: anchor, Rel: RelLinks, Kind: KindMarkdown, Line: line}
}

func wiki(target, anchor string, line int) Link {
	return Link{Target: target, Anchor: anchor, Rel: RelLinks, Kind: KindWikilink, Line: line}
}

func TestExtract(t *testing.T) {
	const src = "data/docker/README.md"
	for _, tc := range []struct {
		name string
		src  string
		body string
		want []Link
	}{
		{"empty body", src, "", nil},
		{"garbage", src, "[[[ ]( ]]( ))( [](", nil},
		{"same dir", src, "[t](x.md)", []Link{md("data/docker/x.md", "", 1)}},
		{"dot slash", src, "[t](./x.md)", []Link{md("data/docker/x.md", "", 1)}},
		{"parent dir", src, "[t](../x.md)", []Link{md("data/x.md", "", 1)}},
		{"nested", src, "[t](docs/a/b.md)", []Link{md("data/docker/docs/a/b.md", "", 1)}},
		{"up and down", src, "[t](docs/../../y/z.md)", []Link{md("data/y/z.md", "", 1)}},
		{"anchor", src, "[t](../x.md#sec-1)", []Link{md("data/x.md", "sec-1", 1)}},
		{"empty anchor", src, "[t](x.md#)", []Link{md("data/docker/x.md", "", 1)}},
		{"query dropped", src, "[t](x.md?v=1#a)", []Link{md("data/docker/x.md", "a", 1)}},
		{"url-encoded space", src, "[t](my%20note.md)", []Link{md("data/docker/my note.md", "", 1)}},
		{"bad escape kept raw", src, "[t](100%.md)", []Link{md("data/docker/100%.md", "", 1)}},
		{"angle brackets with space", src, "[t](<my note.md>)", []Link{md("data/docker/my note.md", "", 1)}},
		{"title ignored", src, `[t](x.md "the title")`, []Link{md("data/docker/x.md", "", 1)}},
		{"parens in dest", src, "[t](a(b).md)", []Link{md("data/docker/a(b).md", "", 1)}},
		{"directory link", src, "[t](../other/)", []Link{md("data/other", "", 1)}},
		{"file at root", "a.md", "[t](b.md)", []Link{md("b.md", "", 1)}},
		{"root: parent escapes", "a.md", "[t](../b.md)", nil},
		{"exactly to root", "data/a.md", "[t](../b.md)", []Link{md("b.md", "", 1)}},
		{"one above root", "data/a.md", "[t](../../b.md)", nil},
		{"far above root", src, "[t](../../../../b.md)", nil},
		{"link to own dir", "data/a.md", "[t](.)", []Link{md("data", "", 1)}},
		{"link to the root itself", "a.md", "[t](.)", nil},
		{"link to parent dir", "data/a.md", "[t](..)", nil},
		{"leading slash source", "/data/a.md", "[t](b.md)", []Link{md("data/b.md", "", 1)}},
		{"http", src, "[t](http://x.com/a.md)", nil},
		{"https mixed case", src, "[t](HTTPS://x.com)", nil},
		{"mailto", src, "[t](mailto:a@b.c)", nil},
		{"file scheme", src, "[t](file:///etc/passwd)", nil},
		{"drive letter", src, `[t](C:\a.md)`, nil},
		{"protocol relative", src, "[t](//host/a.md)", nil},
		{"absolute path", src, "[t](/data/a.md)", nil},
		{"backslash absolute", src, `[t](\a.md)`, nil},
		{"pure anchor", src, "[t](#x)", nil},
		{"query only", src, "[t](?x=1)", nil},
		{"empty dest", src, "[t]()", nil},
		{"colon after slash is not a scheme", src, "[t](a/b:c.md)", []Link{md("data/docker/a/b:c.md", "", 1)}},
		{"digit-leading is not a scheme", src, "[t](1a:b.md)", []Link{md("data/docker/1a:b.md", "", 1)}},
		{"leading colon is not a scheme", src, "[t](:b.md)", []Link{md("data/docker/:b.md", "", 1)}},
		{"image skipped", src, "![alt](pic.png)", nil},
		{"image inside link: outer kept", src, "[![alt](pic.png)](x.md)", []Link{md("data/docker/x.md", "", 1)}},
		{"escaped bang is not an image", src, `\![t](x.md)`, []Link{md("data/docker/x.md", "", 1)}},
		{"embed skipped", src, "![[note]]", nil},
		{"escaped bracket", src, `\[t](x.md)`, nil},
		{"nested brackets in text", src, "[a [b] c](x.md)", []Link{md("data/docker/x.md", "", 1)}},
		{"unclosed text", src, "[t(x.md)", nil},
		{"text not followed by paren", src, "[t] (x.md)", nil},
		{"unterminated dest", src, "[t](x.md", nil},
		{"junk after dest", src, "[t](x.md junk)", nil},
		{"unterminated angle dest", src, "[t](<x.md)", nil},
		{"two on a line, two lines, line numbers", src, "[a](a.md) and [b](b.md)\n\n[c](c.md)", []Link{
			md("data/docker/a.md", "", 1), md("data/docker/b.md", "", 1), md("data/docker/c.md", "", 3)}},
		{"CRLF", src, "x\r\n[a](a.md)\r\n", []Link{md("data/docker/a.md", "", 2)}},
		// Rows from the W4 mutation pass: each kills a surviving mutant.
		{"adjacent links", src, "[a](a.md)[b](b.md)", []Link{md("data/docker/a.md", "", 1), md("data/docker/b.md", "", 1)}},
		{"space before dest", src, "[t](\t x.md)", []Link{md("data/docker/x.md", "", 1)}},
		{"escaped bracket inside text", src, `[a\]b](x.md)`, []Link{md("data/docker/x.md", "", 1)}},
		{"anchor is percent-decoded", src, "[t](x.md#a%20b)", []Link{md("data/docker/x.md", "a b", 1)}},
		{"bad anchor escape kept raw", src, "[t](x.md#100%)", []Link{md("data/docker/x.md", "100%", 1)}},
		{"escaped NUL dropped", src, "[t](%00.md)", nil},
		{"escaped leading slash dropped", src, "[t](%2Fetc/passwd)", nil},
		{"scheme letters and digits at range edges", src,
			"[a](a:x) [b](z:x) [c](A:x) [d](Z:x) [e](x0:y) [f](x9:y) [g](x+:y) [h](x-:y) [i](x.:y)", nil},
		{"unclosed text at line start before a paren", src, "[(a.md)", nil},
		{"wikilink then adjacent markdown", src, "[[a]][b](b.md)", []Link{wiki("a", "", 1), md("data/docker/b.md", "", 1)}},
		// Wikilinks.
		{"wikilink", src, "see [[Docker Setup]]", []Link{wiki("Docker Setup", "", 1)}},
		{"wikilink with label", src, "[[docker-setup|the setup]]", []Link{wiki("docker-setup", "", 1)}},
		{"wikilink with anchor and label", src, "[[a#h|l]]", []Link{wiki("a", "h", 1)}},
		{"wikilink trimmed", src, "[[  a  ]]", []Link{wiki("a", "", 1)}},
		{"wikilink empty", src, "[[]] [[ ]] [[|x]]", nil},
		{"wikilink anchor only", src, "[[#h]]", nil},
		{"wikilink unclosed", src, "[[a]", nil},
		{"wikilink with inner bracket", src, "[[a[b]]", nil},
		{"wikilink and markdown on one line", src, "[[a]] [b](b.md)", []Link{wiki("a", "", 1), md("data/docker/b.md", "", 1)}},
		{"wikilink path is not resolved", src, "[[../x]]", []Link{wiki("../x", "", 1)}},
		// Fences.
		{"backtick fence", src, "```\n[a](a.md)\n```\n[b](b.md)", []Link{md("data/docker/b.md", "", 4)}},
		{"tilde fence", src, "~~~\n[a](a.md)\n~~~\n[b](b.md)", []Link{md("data/docker/b.md", "", 4)}},
		{"fence with info string", src, "```go\n[a](a.md)\n```", nil},
		{"unclosed fence swallows the rest", src, "[a](a.md)\n```\n[b](b.md)\n[[c]]", []Link{md("data/docker/a.md", "", 1)}},
		{"longer fence needs a longer close", src, "````\n```\n[a](a.md)\n```\n````\n[b](b.md)", []Link{md("data/docker/b.md", "", 6)}},
		{"tilde does not close backtick fence", src, "```\n~~~\n[a](a.md)\n```\n[b](b.md)", []Link{md("data/docker/b.md", "", 5)}},
		{"fence close with trailing text is not a close", src, "```\n``` x\n[a](a.md)\n```\n[b](b.md)", []Link{md("data/docker/b.md", "", 5)}},
		{"fence indented 3 spaces", src, "   ```\n[a](a.md)\n   ```", nil},
		{"4 spaces is not a fence", src, "    ```\n[a](a.md)", []Link{md("data/docker/a.md", "", 2)}},
		{"two backticks is not a fence", src, "``\n[a](a.md)", []Link{md("data/docker/a.md", "", 2)}},
		{"backtick fence info with backtick is inline code", src, "```a```\n[a](a.md)", []Link{md("data/docker/a.md", "", 2)}},
		{"inline-code line is still scanned", src, "```a``` [b](b.md)", []Link{md("data/docker/b.md", "", 1)}},
		{"fence opening line is not scanned", src, "~~~ [a](a.md)\n~~~\n", nil},
		// Inline code.
		{"inline code", src, "`[a](a.md)` [b](b.md)", []Link{md("data/docker/b.md", "", 1)}},
		{"double-backtick span", src, "``[a](a.md) ` x`` [b](b.md)", []Link{md("data/docker/b.md", "", 1)}},
		{"unmatched backtick is literal", src, "` [a](a.md)", []Link{md("data/docker/a.md", "", 1)}},
		{"mismatched run lengths", src, "`` [a](a.md) `", []Link{md("data/docker/a.md", "", 1)}},
		{"wikilink in code", src, "`[[a]]`", nil},
		{"link between two code spans", src, "`a` [x](x.md) `b`", []Link{md("data/docker/x.md", "", 1)}},
		{"link between two double-backtick spans", src, "``a`` [x](x.md) ``b``", []Link{md("data/docker/x.md", "", 1)}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got := Extract(tc.src, []byte(tc.body))
			if !reflect.DeepEqual(got, tc.want) {
				t.Fatalf("Extract(%q, %q)\n got %+v\nwant %+v", tc.src, tc.body, got, tc.want)
			}
		})
	}
}

func TestFromFrontMatter(t *testing.T) {
	const src = "data/docker/README.md"
	fm := entry.FrontMatter{
		Related:    []string{"../a.md#h", "[[Other Note|x]]", "https://x.com", "/abs.md", "../../../out.md", " "},
		Supersedes: []string{"old.md"},
		DependsOn:  []string{"[[a[b]]", "[[ ]]", " [[dep]] ", "[[half", "half]]"},
	}
	want := []Link{
		{Target: "data/a.md", Anchor: "h", Rel: "related", Kind: KindMarkdown},
		{Target: "Other Note", Rel: "related", Kind: KindWikilink},
		{Target: "data/docker/old.md", Rel: "supersedes", Kind: KindMarkdown},
		{Target: "dep", Rel: "depends_on", Kind: KindWikilink},
		{Target: "data/docker/[[half", Rel: "depends_on", Kind: KindMarkdown},
		{Target: "data/docker/half]]", Rel: "depends_on", Kind: KindMarkdown},
	}
	if got := FromFrontMatter(src, fm); !reflect.DeepEqual(got, want) {
		t.Fatalf("got %+v\nwant %+v", got, want)
	}
	if got := FromFrontMatter(src, entry.FrontMatter{}); got != nil {
		t.Fatalf("empty front matter: %+v", got)
	}
}

func TestDedupe(t *testing.T) {
	a := md("x.md", "one", 1)
	b := md("x.md", "two", 2)
	w := wiki("x.md", "", 3)
	r := Link{Target: "x.md", Rel: RelRelated, Kind: KindMarkdown}
	got := Dedupe([]Link{a, b, w, r, a})
	want := []Link{a, w, r}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("got %+v want %+v", got, want)
	}
	if Dedupe(nil) != nil {
		t.Fatal("nil in, non-nil out")
	}
}
