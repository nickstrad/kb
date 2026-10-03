// Package chunk turns an entry's Markdown files into the chunks the indexer stores and searches.
//
// The full rules live in plan.md ("Chunking rules"), all six implemented here:
//
//  1. strip front matter (every file in the entry, not only the front door — see stripFrontMatter);
//  2. chunk 0 is "<title>: <summary>" with an empty heading;
//  3. one chunk per "##" section of each indexed file, text prefixed with a context line
//     "<title> › <heading>"; text before the first "##" (the lede) is its own chunk with
//     heading "(intro)"; a section — the lede or any "##" section — whose body is blank after
//     trimming produces no chunk at all (a heading with nothing before the next heading, or at
//     EOF, is not indexed just to carry its own context line); "###" and deeper stay inside their
//     parent "##" section; a "##" line inside a fenced code block is not a heading at all;
//  4. a section whose final text (context line included) is longer than MaxChunkChars is split
//     at blank-line paragraph boundaries into pieces at most MaxChunkChars long, each piece
//     repeating the context line; a fenced code block is never split internally; see
//     splitOversized for the documented exception when a single paragraph or code block alone
//     will not fit;
//  5. Ord runs in the order chunks are produced: 0 for the summary chunk, then sequentially
//     across e.Files in order (README.md first, then docs/*.md by name) and, within a file,
//     in document order;
//  6. TextHash is the sha256 of Text.
package chunk

import (
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"strings"

	"github.com/nickstrad/kb/internal/entry"
)

// MaxChunkChars is the size above which a section is split at paragraph boundaries (rule 4).
const MaxChunkChars = 1500

// introHeading is the Chunk.Heading of the lede of a file: the text before its first "##" line.
const introHeading = "(intro)"

// Chunk is one indexed piece of an entry. SourceFile is repo-relative, Heading is "" for the
// summary chunk and "(intro)" for the lede of a file.
type Chunk struct {
	Ord        int
	SourceFile string
	Heading    string
	Text       string
	TextHash   string
}

// Hash returns the sha256 of a chunk's text, hex-encoded, as stored in chunks.text_hash.
func Hash(text string) string {
	sum := sha256.Sum256([]byte(text))
	return hex.EncodeToString(sum[:])
}

// Split produces the chunks of one entry. files maps every repo-relative path in e.Files to its
// raw bytes, front matter included; a file listed in e.Files but missing from files is an error.
// Split requires e.Meta to already carry a title and summary (Discover leaves Meta zero-valued
// and sets Entry.Err instead when the front door's own front matter did not validate — callers
// should check Err before calling Split); a missing title or summary here is reported as a
// *entry.ValidationError so a caller such as `kb reindex` can warn-and-skip by type.
func Split(e entry.Entry, files map[string][]byte) ([]Chunk, error) {
	for _, f := range e.Files {
		if _, ok := files[f]; !ok {
			return nil, fmt.Errorf("chunk %s: file %s not provided", e.Path, f)
		}
	}
	title := strings.TrimSpace(e.Meta.Title)
	summary := strings.TrimSpace(e.Meta.Summary)
	if title == "" || summary == "" {
		return nil, &entry.ValidationError{Path: e.Path, Reason: "front matter needs both a title and a summary"}
	}

	chunks := []Chunk{newChunk(0, e.Path, "", title+": "+summary)}
	ord := 1

	for _, f := range e.Files {
		body, err := stripFrontMatter(files[f])
		if err != nil {
			return nil, fmt.Errorf("chunk %s: strip front matter of %s: %w", e.Path, f, err)
		}
		for _, sec := range splitSections(string(body)) {
			text := strings.TrimSpace(sec.body)
			if text == "" {
				continue // rule 3: a blank lede, or a "##" section with nothing in it, produces no chunk at all
			}

			contextLine := title + " › " + sec.heading
			full := contextLine + "\n" + text

			if len(full) <= MaxChunkChars {
				chunks = append(chunks, newChunk(ord, f, sec.heading, full))
				ord++
				continue
			}
			for _, piece := range splitOversized(contextLine, text) {
				chunks = append(chunks, newChunk(ord, f, sec.heading, piece))
				ord++
			}
		}
	}

	return chunks, nil
}

// stripFrontMatter removes a leading YAML front-matter block from one file's raw bytes, for use
// while chunking a single file. Only the entry's front door (README.md, or the lone file of a
// file entry) is required to carry front matter and have a valid title/summary; a secondary file
// (docs/*.md) is stripped the same way when it happens to have its own front matter — real
// docs/*.md files in this repo do — but a *entry.ValidationError from that secondary file's own
// content (e.g. it has no title) is not a chunking failure and is ignored here: the body was
// still split off correctly. Any other error (a malformed, unclosed fence) is real and propagates.
func stripFrontMatter(raw []byte) ([]byte, error) {
	_, body, err := entry.ParseFrontMatter(raw)
	if err != nil {
		var ve *entry.ValidationError
		if !errors.As(err, &ve) {
			return nil, err
		}
	}
	return body, nil
}

// rawSection is one "##" section of a file (or its lede) before paragraph-level splitting.
type rawSection struct {
	heading string
	body    string
}

// splitSections walks one (already front-matter-stripped) file body and returns the lede
// ("(intro)") followed by one rawSection per top-level "##" heading, in document order. "###" and
// deeper headings never start a new section — their lines stay in the body of the enclosing "##"
// section — and neither does any "##" line inside a fenced code block (``` or ~~~), tracked here
// so that heading detection never fires inside one.
func splitSections(body string) []rawSection {
	lines := strings.Split(body, "\n")
	var sections []rawSection
	var cur []string
	heading := introHeading
	inFence := false
	var fenceMarker string

	flush := func() {
		sections = append(sections, rawSection{heading: heading, body: strings.Join(cur, "\n")})
		cur = nil
	}

	for _, line := range lines {
		trimmed := strings.TrimSpace(line)
		if inFence {
			cur = append(cur, line)
			if strings.HasPrefix(trimmed, fenceMarker) {
				inFence = false
			}
			continue
		}
		if marker, ok := fenceOpen(trimmed); ok {
			inFence = true
			fenceMarker = marker
			cur = append(cur, line)
			continue
		}
		if h, ok := Heading(line); ok {
			flush()
			heading = h
			continue
		}
		cur = append(cur, line)
	}
	flush()
	return sections
}

// Heading reports the section heading a line introduces, and false if the line is not really a
// section boundary. A line only starts a new "##" section when it begins with exactly two "#"
// characters (not three or more — "###" and deeper stay inside their parent section) and the text
// after them, trimmed, is not empty and not made only of further "#" characters — the fallback
// rule for a stray line like a bare "##" or a leftover "## ##" from some other tool, which is
// ordinary content, not a heading. Heading does not itself account for fenced code blocks; a
// caller walking a whole document (splitSections) must track fences separately and skip calling
// Heading on a line inside one.
func Heading(line string) (string, bool) {
	trimmed := strings.TrimSpace(line)
	if !strings.HasPrefix(trimmed, "##") || strings.HasPrefix(trimmed, "###") {
		return "", false
	}
	h := strings.TrimSpace(trimmed[2:])
	if h == "" || isHashesOnly(h) {
		return "", false
	}
	return h, true
}

// isHashesOnly reports whether s is non-empty and made only of "#" characters.
func isHashesOnly(s string) bool {
	for _, r := range s {
		if r != '#' {
			return false
		}
	}
	return true
}

// fenceOpen reports whether a trimmed line opens a fenced code block, and its marker ("```" or
// "~~~") if so, so the matching close can be recognized by the same prefix.
func fenceOpen(trimmed string) (string, bool) {
	if strings.HasPrefix(trimmed, "```") {
		return "```", true
	}
	if strings.HasPrefix(trimmed, "~~~") {
		return "~~~", true
	}
	return "", false
}

// splitOversized breaks one section's body into pieces of at most MaxChunkChars, each prefixed
// with contextLine and a newline, splitting only between paragraphs and never inside a fenced
// code block (rule 4). Those two constraints can conflict: a single paragraph, or a single fenced
// code block, that together with contextLine already exceeds MaxChunkChars has no split point
// that honors both "≤1500 chars" and "never split a paragraph or fence" at once. This is the
// documented exception rule 4 calls for: such a block is kept whole as its own oversized piece
// rather than being cut.
func splitOversized(contextLine, body string) []string {
	blocks := paragraphBlocks(body)
	if len(blocks) == 0 {
		return []string{contextLine + "\n" + body}
	}
	var pieces []string
	var cur []string
	for _, b := range blocks {
		trial := append(append([]string{}, cur...), b)
		candidate := contextLine + "\n" + strings.Join(trial, "\n\n")
		if len(cur) > 0 && len(candidate) > MaxChunkChars {
			pieces = append(pieces, contextLine+"\n"+strings.Join(cur, "\n\n"))
			cur = []string{b}
			continue
		}
		cur = trial
	}
	if len(cur) > 0 {
		pieces = append(pieces, contextLine+"\n"+strings.Join(cur, "\n\n"))
	}
	return pieces
}

// paragraphBlocks splits body into blank-line-delimited paragraphs, except that a fenced code
// block (``` or ~~~) is always isolated into its own block regardless of the blank lines around
// it — including the blank lines a fenced block's neighbors may lack — so that splitOversized can
// never place a cut inside one.
func paragraphBlocks(body string) []string {
	lines := strings.Split(body, "\n")
	var blocks []string
	var cur []string
	inFence := false
	var fenceMarker string

	flush := func() {
		text := strings.Trim(strings.Join(cur, "\n"), "\n")
		cur = nil
		if text != "" {
			blocks = append(blocks, text)
		}
	}

	for _, line := range lines {
		trimmed := strings.TrimSpace(line)
		if inFence {
			cur = append(cur, line)
			if strings.HasPrefix(trimmed, fenceMarker) {
				inFence = false
				flush()
			}
			continue
		}
		if marker, ok := fenceOpen(trimmed); ok {
			flush()
			inFence = true
			fenceMarker = marker
			cur = append(cur, line)
			continue
		}
		if trimmed == "" {
			flush()
			continue
		}
		cur = append(cur, line)
	}
	flush()
	return blocks
}

// newChunk builds a chunk with its hash filled in.
func newChunk(ord int, sourceFile, heading, text string) Chunk {
	return Chunk{Ord: ord, SourceFile: sourceFile, Heading: heading, Text: text, TextHash: Hash(text)}
}
