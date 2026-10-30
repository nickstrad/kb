package entry

import (
	"path"
	"strings"
	"unicode"
	"unicode/utf8"
)

// FallbackSummaryRunes caps a summary taken from a file's first paragraph: it is a one-line
// description, not the paragraph itself.
const FallbackSummaryRunes = 200

// fallbackMeta fills the title and summary an in-place file (files layout) does not set in front
// matter. Precedence:
//
//   - title: the front-matter title, else the first "# " heading outside a code fence, else the
//     file name humanised ("install-guide.md" → "Install guide"), else the path;
//   - summary: the front-matter summary, else the first paragraph that is not a heading, a fence,
//     HTML or an image/badge line, collapsed to one line and cut to FallbackSummaryRunes, else "".
//
// Every value it derives passes checkText: control characters and invalid UTF-8 are dropped and
// the length is capped, so a fallback can never make Discover report a ValidationError.
func fallbackMeta(fm FrontMatter, body []byte, relPath string) FrontMatter {
	if strings.TrimSpace(fm.Title) == "" {
		h1, _ := firstHeadingAndParagraph(body)
		fm.Title = clean(h1, MaxTitleRunes)
		if fm.Title == "" {
			fm.Title = clean(humanize(relPath), MaxTitleRunes)
		}
		if fm.Title == "" {
			fm.Title = clean(relPath, MaxTitleRunes)
		}
	}
	if strings.TrimSpace(fm.Summary) == "" {
		_, para := firstHeadingAndParagraph(body)
		fm.Summary = clean(para, FallbackSummaryRunes)
	}
	return fm
}

// firstHeadingAndParagraph scans a Markdown body once and returns the text of its first "# "
// heading and its first plain paragraph (joined lines), both outside fenced code blocks.
func firstHeadingAndParagraph(body []byte) (h1, para string) {
	inFence := false
	fence := ""
	var cur []string
	done := false
	flush := func() {
		if len(cur) > 0 && !done {
			p := strings.Join(cur, " ")
			if !skippedParagraph(p) {
				para, done = p, true
			}
		}
		cur = nil
	}
	for _, line := range strings.Split(string(body), "\n") {
		t := strings.TrimSpace(line)
		if inFence {
			if strings.HasPrefix(t, fence) {
				inFence = false
			}
			continue
		}
		switch {
		case strings.HasPrefix(t, "```") || strings.HasPrefix(t, "~~~"):
			flush()
			inFence, fence = true, t[:3]
		case t == "":
			flush()
		case strings.HasPrefix(t, "#"):
			flush()
			if h1 == "" && strings.HasPrefix(t, "# ") {
				if h := strings.TrimSpace(t[2:]); !isHashesOnly(h) {
					h1 = h
				}
			}
		default:
			cur = append(cur, t)
		}
		if h1 != "" && done {
			return h1, para
		}
	}
	flush()
	return h1, para
}

// skippedParagraph reports whether a paragraph is markup rather than prose: HTML, an image or a
// row of badges, a table, a block quote of front matter leftovers ("---"), or a list marker alone.
func skippedParagraph(p string) bool {
	for _, prefix := range []string{"<", "![", "[![", "|", "---", "==="} {
		if strings.HasPrefix(p, prefix) {
			return true
		}
	}
	return false
}

// humanize turns a file name into a title: the base name without ".md", "-" and "_" as spaces,
// the first letter upper-cased. "docs/install-guide.md" → "Install guide".
func humanize(relPath string) string {
	name := strings.TrimSuffix(path.Base(relPath), ".md")
	name = strings.Join(strings.FieldsFunc(name, func(r rune) bool { return r == '-' || r == '_' || unicode.IsSpace(r) }), " ")
	r, size := utf8.DecodeRuneInString(name)
	if size == 0 || r == utf8.RuneError {
		return name
	}
	return string(unicode.ToUpper(r)) + name[size:]
}

// clean makes s a valid one-line display value: invalid UTF-8 and control characters dropped,
// whitespace runs collapsed, at most max runes (a cut value ends in "…").
func clean(s string, max int) string {
	s = strings.ToValidUTF8(s, "")
	s = strings.Map(func(r rune) rune {
		if unicode.IsControl(r) {
			if unicode.IsSpace(r) {
				return ' '
			}
			return -1
		}
		return r
	}, s)
	s = strings.Join(strings.Fields(s), " ")
	if utf8.RuneCountInString(s) <= max {
		return s
	}
	runes := []rune(s)
	return strings.TrimSpace(string(runes[:max-1])) + "…"
}

// isHashesOnly reports whether s is non-empty and made only of "#" characters ("# ##" is not a
// heading with text).
func isHashesOnly(s string) bool {
	return s != "" && strings.Trim(s, "#") == ""
}
