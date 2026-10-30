package render

import (
	"strings"
)

// Expand modes for --expand and [output] expand.
const (
	ExpandNone    = "none"
	ExpandSection = "section"
	ExpandEntry   = "entry"
)

// introHeading is the heading the chunker gives the text before a file's first "##" line.
const introHeading = "(intro)"

// Chunk is one stored chunk of an entry as expansion needs it. Chunks are given in ord order.
type Chunk struct {
	SourceFile string
	Heading    string // "" for chunk 0 (the title and summary), "(intro)" for a file's lede
	Text       string // the stored text, which starts with a context line "Title › Heading"
}

// StripContext removes the chunk's own context line, the first line of Text when it ends with
// " › " plus the chunk's heading: the hit header already shows it. Text that has no such first
// line (chunk 0, or text written by hand) is returned unchanged.
func StripContext(heading, text string) string {
	if heading == "" {
		return text
	}
	first, rest, found := strings.Cut(text, "\n")
	if found && strings.HasSuffix(first, " › "+heading) {
		return rest
	}
	return text
}

// Section returns the text of the section the chunk at index at belongs to: every chunk of the
// same source file and heading, in order, each without its context line, joined by a blank line
// (a long section is stored as several chunks). Chunk 0 (no heading) is its own section, and an
// index out of range expands to "".
func Section(chunks []Chunk, at int) string {
	if at < 0 || at >= len(chunks) {
		return ""
	}
	hit := chunks[at]
	var parts []string
	for _, c := range chunks {
		if c.SourceFile == hit.SourceFile && c.Heading == hit.Heading {
			parts = append(parts, strings.TrimSpace(StripContext(c.Heading, c.Text)))
		}
	}
	return strings.Join(parts, "\n\n")
}

// Entry returns the whole entry: the chunks in order without their context lines, a section's
// pieces joined by a blank line, and each section after the first introduced by a "## heading"
// line (a lede, "(intro)", and chunk 0 have none).
func Entry(chunks []Chunk) string {
	var b strings.Builder
	prev := chunkKey{}
	for i, c := range chunks {
		if i > 0 {
			b.WriteString("\n\n")
		}
		if k := (chunkKey{c.SourceFile, c.Heading}); i == 0 || k != prev {
			if c.Heading != "" && c.Heading != introHeading {
				b.WriteString("## " + c.Heading + "\n")
			}
			prev = k
		}
		b.WriteString(strings.TrimSpace(StripContext(c.Heading, c.Text)))
	}
	return b.String()
}

type chunkKey struct{ file, heading string }

// Dedupe merges the hits that share a non-empty key (one entry, or one section of it) into the
// first of them, the best rank, whose Also lists the ranks it absorbed. Hits with an empty key
// are kept as they are. Order is preserved. keys has one key per hit.
func Dedupe(hits []Hit, keys []string) []Hit {
	first := map[string]int{}
	out := make([]Hit, 0, len(hits))
	for i, h := range hits {
		k := keys[i]
		if k == "" {
			out = append(out, h)
			continue
		}
		if at, ok := first[k]; ok {
			out[at].Also = append(out[at].Also, h.Rank)
			continue
		}
		first[k] = len(out)
		out = append(out, h)
	}
	return out
}
