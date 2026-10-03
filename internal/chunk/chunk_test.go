package chunk

import (
	"errors"
	"strings"
	"testing"

	"github.com/nickstrad/kb/internal/entry"
)

// TestSplitProducesSummaryChunk pins rule 2 and rule 6, and that a missing summary is reported as
// a *entry.ValidationError so a caller like `kb reindex` can warn-and-skip by type.
func TestSplitProducesSummaryChunk(t *testing.T) {
	e := entry.Entry{
		Path:  "thing.md",
		Kind:  entry.KindFile,
		Files: []string{"thing.md"},
		Meta:  entry.FrontMatter{Title: "A thing", Summary: "What it does."},
	}
	files := map[string][]byte{"thing.md": []byte("---\ntitle: A thing\nsummary: What it does.\n---\n\nbody\n")}

	chunks, err := Split(e, files)
	if err != nil {
		t.Fatalf("Split: %v", err)
	}
	if len(chunks) == 0 {
		t.Fatal("Split returned no chunks")
	}
	first := chunks[0]
	if first.Ord != 0 || first.Heading != "" || first.Text != "A thing: What it does." {
		t.Errorf("chunk 0 = %+v", first)
	}
	if first.TextHash != Hash(first.Text) {
		t.Errorf("text hash %q does not match Hash(text)", first.TextHash)
	}

	if _, err := Split(e, map[string][]byte{}); err == nil {
		t.Error("expected an error when an indexed file is missing")
	}
	noMeta := e
	noMeta.Meta.Summary = ""
	_, err = Split(noMeta, files)
	if err == nil {
		t.Fatal("expected an error when the summary is missing")
	}
	var ve *entry.ValidationError
	if !errors.As(err, &ve) {
		t.Errorf("error is %T, not *entry.ValidationError: %v", err, err)
	}
}

// baseEntry returns a minimal file entry named thing.md with the given title/summary, for tests
// that only care about section splitting.
func baseEntry() entry.Entry {
	return entry.Entry{
		Path:  "thing.md",
		Kind:  entry.KindFile,
		Files: []string{"thing.md"},
		Meta:  entry.FrontMatter{Title: "Thing", Summary: "A summary."},
	}
}

func mustSplit(t *testing.T, body string) []Chunk {
	t.Helper()
	e := baseEntry()
	files := map[string][]byte{"thing.md": []byte(body)}
	chunks, err := Split(e, files)
	if err != nil {
		t.Fatalf("Split: %v", err)
	}
	return chunks
}

// TestSplitSections pins rule 3: the lede is "(intro)", one chunk per "##" section, "###" and
// deeper stay inside their parent, and every section chunk starts with the context line.
func TestSplitSections(t *testing.T) {
	body := "# Thing\n\nIntro text.\n\n## First\n\nFirst body.\n\n### Sub\n\nNested text.\n\n## Second\n\nSecond body.\n"
	chunks := mustSplit(t, body)

	if len(chunks) != 4 { // summary, intro, First, Second
		t.Fatalf("got %d chunks, want 4: %+v", len(chunks), chunks)
	}
	if chunks[1].Heading != "(intro)" || !strings.Contains(chunks[1].Text, "Intro text.") {
		t.Errorf("chunk 1 = %+v", chunks[1])
	}
	if chunks[2].Heading != "First" || chunks[2].Text != "Thing › First\nFirst body.\n\n### Sub\n\nNested text." {
		t.Errorf("chunk 2 = %+v", chunks[2])
	}
	if chunks[3].Heading != "Second" || chunks[3].Text != "Thing › Second\nSecond body." {
		t.Errorf("chunk 3 = %+v", chunks[3])
	}
}

// TestSplitBlankLedeSkipped pins the "skip if blank" half of rule 3.
func TestSplitBlankLedeSkipped(t *testing.T) {
	body := "\n\n## Only\n\nBody.\n"
	chunks := mustSplit(t, body)
	if len(chunks) != 2 { // summary, Only (no intro chunk)
		t.Fatalf("got %d chunks, want 2 (no intro): %+v", len(chunks), chunks)
	}
	if chunks[1].Heading != "Only" {
		t.Errorf("chunks[1].Heading = %q", chunks[1].Heading)
	}
}

// TestSplitBlankSectionSkipped pins the extended rule 3: a "##" section with nothing before the
// next heading (or, here, before EOF) produces no chunk at all, the same as a blank lede.
func TestSplitBlankSectionSkipped(t *testing.T) {
	body := "Intro.\n\n## Tail\n"
	chunks := mustSplit(t, body)
	if len(chunks) != 2 { // summary, intro; "Tail" has no body and is skipped
		t.Fatalf("got %d chunks, want 2 (blank trailing section skipped): %+v", len(chunks), chunks)
	}
	for _, c := range chunks {
		if c.Heading == "Tail" {
			t.Errorf("a chunk was produced for the blank \"Tail\" section: %+v", c)
		}
	}

	// Same rule mid-document: a heading immediately followed by another heading, with nothing but
	// whitespace between them, is skipped too.
	body2 := "Intro.\n\n## Empty\n\n \n\n## Real\n\nReal body.\n"
	chunks2 := mustSplit(t, body2)
	if len(chunks2) != 3 { // summary, intro, Real (no chunk for Empty)
		t.Fatalf("got %d chunks, want 3: %+v", len(chunks2), chunks2)
	}
	for _, c := range chunks2 {
		if c.Heading == "Empty" {
			t.Errorf("a chunk was produced for the blank \"Empty\" section: %+v", c)
		}
	}
}

// TestSplitHeadingFallback pins the documented Heading fallback rule: a "##" line with nothing,
// or only more "#" characters, after it is not a heading and does not start a new section.
func TestSplitHeadingFallback(t *testing.T) {
	cases := []string{
		"##",
		"## ",
		"## ##",
		"## ###",
	}
	for _, line := range cases {
		if _, ok := Heading(line); ok {
			t.Errorf("Heading(%q) = ok, want not-a-heading", line)
		}
	}
	if h, ok := Heading("## Real Heading"); !ok || h != "Real Heading" {
		t.Errorf("Heading(%q) = %q, %v", "## Real Heading", h, ok)
	}
	if _, ok := Heading("### Not top-level"); ok {
		t.Error(`Heading("### ...") should not be a heading (rule: "##" only)`)
	}

	body := "Intro.\n\n## ##\n\nThis stays inside the intro because '## ##' is not a real heading.\n\n## Real\n\nReal body.\n"
	chunks := mustSplit(t, body)
	if len(chunks) != 3 { // summary, intro, Real
		t.Fatalf("got %d chunks, want 3: %+v", len(chunks), chunks)
	}
	if chunks[1].Heading != "(intro)" || !strings.Contains(chunks[1].Text, "## ##") {
		t.Errorf("chunk 1 = %+v, want the fallback line folded into the intro", chunks[1])
	}
}

// TestSplitIgnoresHeadingsInFencedCode pins the rule that a "##" line inside a fenced code block
// (``` or ~~~) never starts a new section.
func TestSplitIgnoresHeadingsInFencedCode(t *testing.T) {
	body := "Intro.\n\n## Real\n\nBefore.\n\n```markdown\n## Not a real heading\n### also not\n```\n\nAfter.\n\n~~~\n## also inside a tilde fence\n~~~\n\nEnd.\n"
	chunks := mustSplit(t, body)
	if len(chunks) != 3 { // summary, intro, Real
		t.Fatalf("got %d chunks, want 3 (fenced ## lines must not start sections): %+v", len(chunks), chunks)
	}
	if chunks[2].Heading != "Real" {
		t.Fatalf("chunks[2].Heading = %q", chunks[2].Heading)
	}
	if !strings.Contains(chunks[2].Text, "## Not a real heading") {
		t.Error("the fenced ## line should remain, verbatim, inside the Real section's body")
	}
	if !strings.Contains(chunks[2].Text, "## also inside a tilde fence") {
		t.Error("the ~~~-fenced ## line should remain inside the Real section's body")
	}
}

// TestSplitStripsFrontMatterFromEachFile pins that docs/*.md (a second file) has its own front
// matter stripped too, tolerating one that is missing a title/summary of its own — that is not a
// chunking failure since only the front door's title/summary are required.
func TestSplitStripsFrontMatterFromEachFile(t *testing.T) {
	e := entry.Entry{
		Path:  "topic/README.md",
		Kind:  entry.KindDir,
		Dir:   "topic",
		Files: []string{"topic/README.md", "topic/docs/extra.md"},
		Meta:  entry.FrontMatter{Title: "Topic", Summary: "A topic."},
	}
	files := map[string][]byte{
		"topic/README.md":     []byte("---\ntitle: Topic\nsummary: A topic.\n---\n\n## Main\n\nMain body.\n"),
		"topic/docs/extra.md": []byte("---\ntitle: Extra\n---\n\n## Extra section\n\nExtra body.\n"), // no summary of its own
	}
	chunks, err := Split(e, files)
	if err != nil {
		t.Fatalf("Split: %v", err)
	}
	var sawExtra bool
	for _, c := range chunks {
		if c.SourceFile == "topic/docs/extra.md" {
			sawExtra = true
			if strings.Contains(c.Text, "title: Extra") {
				t.Errorf("front matter was not stripped from docs file: %+v", c)
			}
		}
	}
	if !sawExtra {
		t.Fatal("no chunk came from topic/docs/extra.md")
	}
}

// TestSplitOversizedSection pins rule 4: a section over MaxChunkChars is split at blank-line
// paragraph boundaries into pieces that each fit, and each piece repeats the context line.
func TestSplitOversizedSection(t *testing.T) {
	makeParagraph := func(n int) string {
		return strings.Repeat("word ", n)
	}
	var paras []string
	for i := 0; i < 6; i++ {
		paras = append(paras, strings.TrimSpace(makeParagraph(80))+" #"+string(rune('A'+i)))
	}
	body := "## Big\n\n" + strings.Join(paras, "\n\n") + "\n"

	chunks := mustSplit(t, body)
	if len(chunks) < 3 { // summary + at least 2 pieces
		t.Fatalf("got %d chunks, want the Big section split into multiple pieces: %+v", len(chunks), chunks)
	}
	context := "Thing › Big"
	for i, c := range chunks[1:] {
		if c.Heading != "Big" {
			t.Errorf("piece %d: heading = %q, want %q", i, c.Heading, "Big")
		}
		if !strings.HasPrefix(c.Text, context+"\n") {
			t.Errorf("piece %d: does not start with the context line: %.60q", i, c.Text)
		}
		if len(c.Text) > MaxChunkChars {
			t.Errorf("piece %d: %d chars, want <= %d", i, len(c.Text), MaxChunkChars)
		}
	}
	// Reassembling the pieces (minus the repeated context lines) should recover every paragraph,
	// in order, with nothing dropped or duplicated.
	var rebuilt []string
	for _, c := range chunks[1:] {
		body := strings.TrimPrefix(c.Text, context+"\n")
		rebuilt = append(rebuilt, strings.Split(body, "\n\n")...)
	}
	if len(rebuilt) != len(paras) {
		t.Fatalf("reassembled %d paragraphs, want %d", len(rebuilt), len(paras))
	}
	for i, p := range paras {
		if rebuilt[i] != p {
			t.Errorf("paragraph %d = %q, want %q", i, rebuilt[i], p)
		}
	}
}

// TestSplitOversizedSingleParagraphKeptWhole pins the documented exception to rule 4: a single
// paragraph (no blank-line break inside it) that alone exceeds MaxChunkChars is kept whole as one
// oversized chunk rather than being cut mid-paragraph.
func TestSplitOversizedSingleParagraphKeptWhole(t *testing.T) {
	longLine := strings.Repeat("nobreak ", 250) // one paragraph, far over MaxChunkChars, no blank line
	body := "## Huge\n\n" + strings.TrimSpace(longLine) + "\n"

	chunks := mustSplit(t, body)
	if len(chunks) != 2 { // summary + the one oversized piece
		t.Fatalf("got %d chunks, want 2 (the paragraph must stay whole): %+v", len(chunks), chunks)
	}
	if len(chunks[1].Text) <= MaxChunkChars {
		t.Fatalf("test setup broken: piece is only %d chars, not actually oversized", len(chunks[1].Text))
	}
	if !strings.Contains(chunks[1].Text, "nobreak") {
		t.Error("the oversized paragraph's content is missing")
	}
}

// TestSplitOversizedCodeBlockKeptWhole is the same exception for a single fenced code block that
// alone exceeds MaxChunkChars: it must never be split internally.
func TestSplitOversizedCodeBlockKeptWhole(t *testing.T) {
	var lines []string
	for i := 0; i < 200; i++ {
		lines = append(lines, "line of code")
	}
	code := "```text\n" + strings.Join(lines, "\n") + "\n```"
	body := "## Log\n\nBefore.\n\n" + code + "\n\nAfter.\n"

	chunks := mustSplit(t, body)
	var codeChunk *Chunk
	for i := range chunks {
		if strings.Contains(chunks[i].Text, "```text") {
			codeChunk = &chunks[i]
		}
	}
	if codeChunk == nil {
		t.Fatal("no chunk contains the fenced code block")
	}
	if !strings.HasSuffix(strings.TrimRight(codeChunk.Text, "\n"), "```") {
		t.Error("the fenced code block was split: its closing fence is not in the same chunk as its opening fence")
	}
	if len(codeChunk.Text) <= MaxChunkChars {
		t.Fatalf("test setup broken: code chunk is only %d chars, not actually oversized", len(codeChunk.Text))
	}
}

// TestSplitOrdSequentialAcrossFiles pins rule 5: Ord is sequential across the whole entry, and
// docs/*.md chunks come after README chunks.
func TestSplitOrdSequentialAcrossFiles(t *testing.T) {
	e := entry.Entry{
		Path:  "topic/README.md",
		Kind:  entry.KindDir,
		Dir:   "topic",
		Files: []string{"topic/README.md", "topic/docs/a.md", "topic/docs/b.md"},
		Meta:  entry.FrontMatter{Title: "Topic", Summary: "A topic."},
	}
	files := map[string][]byte{
		"topic/README.md": []byte("---\ntitle: Topic\nsummary: A topic.\n---\n\n## R1\n\nr1 body.\n"),
		"topic/docs/a.md": []byte("## A1\n\na1 body.\n"),
		"topic/docs/b.md": []byte("## B1\n\nb1 body.\n"),
	}
	chunks, err := Split(e, files)
	if err != nil {
		t.Fatalf("Split: %v", err)
	}
	for i, c := range chunks {
		if c.Ord != i {
			t.Errorf("chunk %d: Ord = %d, want %d", i, c.Ord, i)
		}
	}
	var order []string
	for _, c := range chunks {
		order = append(order, c.SourceFile)
	}
	want := []string{"topic/README.md", "topic/README.md", "topic/docs/a.md", "topic/docs/b.md"}
	if len(order) != len(want) {
		t.Fatalf("source files = %v, want %v", order, want)
	}
	for i := range want {
		if order[i] != want[i] {
			t.Fatalf("source files = %v, want %v", order, want)
		}
	}
}
