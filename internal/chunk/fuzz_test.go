package chunk

import (
	"errors"
	"strings"
	"testing"
	"time"

	"knowledge/kb/internal/entry"
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
