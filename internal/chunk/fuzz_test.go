package chunk

import (
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/nickstrad/kb/internal/entry"
)

// splitDeadline is far above the microseconds a real entry takes; only a hang or a blow-up trips it.
const splitDeadline = 2 * time.Second

// FuzzSplit runs Split over arbitrary file bodies and checks the chunk invariants the index
// relies on:
//
//  1. it never panics, and the only error it returns is the unclosed front-matter fence;
//  2. chunk 0 is the summary chunk: heading "" and text "title: summary";
//  3. Ord counts up from 0 with no gaps;
//  4. TextHash is the sha256 of Text;
//  5. every later chunk starts with its "title › heading" context line and has a non-blank body;
//  6. it finishes within splitDeadline, so no input can hang `kb reindex`.
//
// Run: go test ./internal/chunk/ -run '^$' -fuzz FuzzSplit -fuzztime 60s -fuzzminimizetime 2s
// (the default 60s minimize time stalls on the long seeds and reports 0 execs/sec, not a hang).
func FuzzSplit(f *testing.F) {
	for _, seed := range []string{
		"---\ntitle: T\nsummary: S\n---\nlede\n\n## One\n\ntext\n\n### Deeper\n\nmore\n",
		"## Fenced\n\n```bash\n## not a heading\n```\n\n~~~\n## nor this\n~~~\n",
		"## \n## ##\n##\n###### six\n",
		"## Long\n\n" + strings.Repeat("word ", 400) + "\n\n" + strings.Repeat("more ", 400),
		"```\nunclosed fence\n## still inside\n",
		"---\nunclosed front matter\n",
		"",
	} {
		f.Add(seed)
	}

	const title, summary = "Fuzz Entry", "One line."
	e := entry.Entry{
		Path:  "data/fuzz.md",
		Kind:  entry.KindFile,
		Files: []string{"data/fuzz.md"},
		Meta:  entry.FrontMatter{Title: title, Summary: summary},
	}

	f.Fuzz(func(t *testing.T, body string) {
		var chunks []Chunk
		var err error
		done := make(chan struct{})
		go func() {
			defer close(done)
			chunks, err = Split(e, map[string][]byte{"data/fuzz.md": []byte(body)})
		}()
		select {
		case <-done:
		case <-time.After(splitDeadline):
			t.Fatalf("Split did not finish within %s on a %d-byte body", splitDeadline, len(body))
		}
		if err != nil {
			var ve *entry.ValidationError
			if errors.As(err, &ve) || !strings.Contains(err.Error(), "front matter is not closed") {
				t.Fatalf("unexpected error: %v", err)
			}
			return
		}

		if len(chunks) == 0 {
			t.Fatal("no chunks; want at least the summary chunk")
		}
		if c := chunks[0]; c.Heading != "" || c.Text != title+": "+summary {
			t.Fatalf("chunk 0 = %+v, want the summary chunk", c)
		}
		for i, c := range chunks {
			if c.Ord != i {
				t.Fatalf("chunk %d has Ord %d", i, c.Ord)
			}
			if c.TextHash != Hash(c.Text) {
				t.Fatalf("chunk %d: TextHash does not match Text", i)
			}
			if i == 0 {
				continue
			}
			prefix := title + " › " + c.Heading + "\n"
			if !strings.HasPrefix(c.Text, prefix) {
				t.Fatalf("chunk %d text %q lacks context line %q", i, c.Text, prefix)
			}
			if strings.TrimSpace(strings.TrimPrefix(c.Text, prefix)) == "" {
				t.Fatalf("chunk %d has a blank body", i)
			}
		}
	})
}

// FuzzSplitDocs is FuzzSplit's sibling for a directory entry. It fuzzes the body of one
// docs/*.md file, which is the only path that builds the three-part context line
// "<title> › <file title> › <heading>" (see fileTitle). When withH1 is set the file opens with
// "# Docs Title", so every chunk from that file must carry that title in its context line.
// README.md is fixed and always has a section, so the front door is always present.
//
// Invariants, on top of those FuzzSplit checks (never panic, Ord sequential, TextHash, deadline):
//
//  1. chunk 0 is still the summary chunk;
//  2. every chunk after README's chunks starts with its context line on the first line, and
//     the body after it is non-blank;
//  3. a README chunk's context line is "<title> › <heading>";
//  4. a docs chunk's context line is "<title> › <file title> › <heading>", and with withH1 the
//     file title is "Docs Title", so the line is exact;
//  5. no docs chunk comes before the README chunks.
//
// Run: go test -tags fts5 ./internal/chunk/ -run '^$' -fuzz '^FuzzSplitDocs$' -fuzztime 60s -fuzzminimizetime 2s
func FuzzSplitDocs(f *testing.F) {
	for _, seed := range []struct {
		body   string
		withH1 bool
	}{
		{"# Docs Title\n\nLede.\n\n## One\n\ntext\n\n### Deeper\n\nmore\n", true},
		{"Lede with no H1 of its own.\n\n## Sec\n\nBody.\n", false},
		{"## Sec\n\n```\n## not a heading\n```\n\n## \n", true},
		{"---\ntitle: Front\n---\n# Other\n\nLede.\n\n## Sec\n\nBody.\n", false},
		{"", true},
		{"# ###\n\nhashes-only H1 is not a title\n", false}, // fileTitle boundary
		{"\n  \n", false}, // all-blank docs file
	} {
		f.Add(seed.body, seed.withH1)
	}

	const title, summary = "Fuzz Entry", "One line."
	const docsTitle = "Docs Title"
	const docsPath = "fuzz/docs/a.md"
	e := entry.Entry{
		Path:  "fuzz/README.md",
		Kind:  entry.KindDir,
		Dir:   "fuzz",
		Files: []string{"fuzz/README.md", docsPath},
		Meta:  entry.FrontMatter{Title: title, Summary: summary},
	}
	const readme = "Lede.\n\n## Main\n\nMain body.\n"

	f.Fuzz(func(t *testing.T, body string, withH1 bool) {
		docs := body
		if withH1 {
			docs = "# " + docsTitle + "\n\n" + body
		}
		files := map[string][]byte{
			e.Path:   []byte(readme),
			docsPath: []byte(docs),
		}

		var chunks []Chunk
		var err error
		done := make(chan struct{})
		go func() {
			defer close(done)
			chunks, err = Split(e, files)
		}()
		select {
		case <-done:
		case <-time.After(splitDeadline):
			t.Fatalf("Split did not finish within %s on a %d-byte docs body", splitDeadline, len(docs))
		}
		if err != nil {
			var ve *entry.ValidationError
			if errors.As(err, &ve) || !strings.Contains(err.Error(), "front matter is not closed") {
				t.Fatalf("unexpected error: %v", err)
			}
			return
		}

		if len(chunks) < 2 {
			t.Fatalf("got %d chunks; want the summary chunk and at least one README chunk", len(chunks))
		}
		if c := chunks[0]; c.Heading != "" || c.Text != title+": "+summary {
			t.Fatalf("chunk 0 = %+v, want the summary chunk", c)
		}
		sawDocs := false
		for i, c := range chunks {
			if c.Ord != i {
				t.Fatalf("chunk %d has Ord %d", i, c.Ord)
			}
			if c.TextHash != Hash(c.Text) {
				t.Fatalf("chunk %d: TextHash does not match Text", i)
			}
			if i == 0 {
				continue
			}
			first, rest, _ := strings.Cut(c.Text, "\n")
			if c.SourceFile == e.Path {
				if sawDocs {
					t.Fatalf("chunk %d from README after a docs chunk", i)
				}
				if want := title + " › " + c.Heading; first != want {
					t.Fatalf("README chunk %d context line %q, want %q", i, first, want)
				}
			} else {
				sawDocs = true
				if !strings.HasPrefix(first, title+" › ") || !strings.HasSuffix(first, " › "+c.Heading) {
					t.Fatalf("docs chunk %d context line %q lacks %q … %q", i, first, title+" › ", " › "+c.Heading)
				}
				if withH1 && first != title+" › "+docsTitle+" › "+c.Heading {
					t.Fatalf("docs chunk %d context line %q, want the H1 title in it", i, first)
				}
			}
			if strings.TrimSpace(rest) == "" {
				t.Fatalf("chunk %d has a blank body", i)
			}
		}
	})
}
