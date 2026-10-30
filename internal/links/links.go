// Package links extracts the relationships between knowledge entries from a Markdown body and
// from front-matter lists. It is pure: no filesystem access, no store. PLAN 5.2 stores the result
// in the edges table and resolves wikilinks by path, basename or alias.
package links

import (
	"net/url"
	"path"
	"strings"

	"github.com/nickstrad/kb/internal/assert"
	"github.com/nickstrad/kb/internal/entry"
)

// Kind says how a link was written.
type Kind string

const (
	KindMarkdown Kind = "markdown" // [t](path#anchor), or a path in a front-matter list
	KindWikilink Kind = "wikilink" // [[target]], [[target|label]], [[target#anchor]]
)

// Rel values. Body links use RelLinks; front-matter lists use their key.
const (
	RelLinks      = "links"
	RelRelated    = "related"
	RelSupersedes = "supersedes"
	RelDependsOn  = "depends_on"
)

// Link is one outgoing reference.
//
// For KindMarkdown, Target is a root-relative, slash-separated, cleaned path: never empty, never
// absolute, never "." or starting with "..". For KindWikilink, Target is the trimmed name as
// written, not a path; it is never empty. Anchor is the text after "#" (percent-decoded for
// Markdown links), possibly "". Line is 1-based within the body passed to Extract, or 0 for a
// front-matter link.
type Link struct {
	Target string
	Anchor string
	Rel    string
	Kind   Kind
	Line   int
}

// Extract returns the links in body, in document order, for the file at sourcePath (root-relative,
// slash-separated). It does not deduplicate; see Dedupe.
//
// Rules:
//   - Markdown links [t](dest) are kept when dest is a relative path. dest is resolved against the
//     directory of sourcePath, its query string dropped and its percent-escapes decoded.
//   - Dropped: links with a scheme (http:, mailto:, file:, ...), protocol-relative (//host),
//     absolute paths, pure anchors (#x) or query-only links, and links that resolve above the root
//     (a link that escapes the root names nothing in the corpus, so it is dropped, not flagged).
//   - Image links ![a](b) and embeds ![[x]] are not relationships between entries and are skipped.
//   - Fenced code blocks (``` or ~~~, an unclosed fence runs to the end) and inline code spans are
//     ignored. Links must start and end on one line; code spans are matched per line.
//   - Wikilinks are captured unresolved.
//   - Not extracted: reference-style links ([t][ref] with a "[ref]: path" definition) and
//     autolinks (<path>).
func Extract(sourcePath string, body []byte) []Link {
	dir := path.Dir(path.Clean(strings.TrimPrefix(sourcePath, "/")))
	var out []Link
	var fence fenceState
	for i, line := range strings.Split(strings.ReplaceAll(string(body), "\r\n", "\n"), "\n") {
		if fence.skip(line) {
			continue
		}
		out = scanLine(out, maskCode(line), dir, i+1)
	}
	return out
}

// fenceState tracks an open fenced code block.
type fenceState struct {
	open  bool
	char  byte
	count int
}

// skip reports whether line is part of (or opens or closes) a fenced block.
func (f *fenceState) skip(line string) bool {
	trimmed := strings.TrimLeft(line, " ")
	indented := len(line)-len(trimmed) <= 3
	c, n := fenceRun(trimmed)
	if f.open {
		if indented && c == f.char && n >= f.count && strings.TrimSpace(trimmed[n:]) == "" {
			f.open = false
		}
		return true
	}
	if indented && n >= 3 {
		// A backtick fence's info string may not contain a backtick (else it is an inline span).
		if c == '`' && strings.Contains(trimmed[n:], "`") {
			return false
		}
		*f = fenceState{open: true, char: c, count: n}
		return true
	}
	return false
}

// fenceRun returns the leading run of ` or ~ in s: its character and length (0 if none).
func fenceRun(s string) (byte, int) {
	if s == "" || (s[0] != '`' && s[0] != '~') {
		return 0, 0
	}
	n := 0
	for n < len(s) && s[n] == s[0] {
		n++
	}
	return s[0], n
}

// maskCode blanks out inline code spans so scanLine never sees them. A run of n backticks opens a
// span that the next run of exactly n closes; an unmatched run is literal.
func maskCode(line string) string {
	if !strings.Contains(line, "`") {
		return line
	}
	b := []byte(line)
	for i := 0; i < len(b); {
		if b[i] != '`' {
			i++
			continue
		}
		n := 0
		for i+n < len(b) && b[i+n] == '`' {
			n++
		}
		end := -1
		for j := i + n; j < len(b); {
			if b[j] != '`' {
				j++
				continue
			}
			m := 0
			for j+m < len(b) && b[j+m] == '`' {
				m++
			}
			if m == n {
				end = j + m
				break
			}
			j += m
		}
		if end < 0 {
			i += n
			continue
		}
		for k := i; k < end; k++ {
			b[k] = ' '
		}
		i = end
	}
	return string(b)
}

// scanLine appends the links on one code-masked line.
func scanLine(out []Link, line, dir string, lineNo int) []Link {
	for i := 0; i < len(line); {
		if line[i] == '\\' {
			i += 2
			continue
		}
		if line[i] != '[' {
			i++
			continue
		}
		image := i > 0 && line[i-1] == '!' && !(i > 1 && line[i-2] == '\\')
		if strings.HasPrefix(line[i:], "[[") {
			if j := strings.Index(line[i+2:], "]]"); j >= 0 {
				inner := line[i+2 : i+2+j]
				if !strings.ContainsAny(inner, "[]") {
					if l, ok := wikilink(inner, lineNo); ok && !image {
						out = append(out, l)
					}
					i += 2 + j + 2
					continue
				}
			}
		}
		textEnd := matchBracket(line, i)
		if textEnd < 0 || textEnd+1 >= len(line) || line[textEnd+1] != '(' {
			i++
			continue
		}
		dest, next, ok := parseDest(line, textEnd+2)
		if !ok {
			i++
			continue
		}
		if !image {
			if t, a, ok := resolve(dir, dest); ok {
				out = append(out, Link{Target: t, Anchor: a, Rel: RelLinks, Kind: KindMarkdown, Line: lineNo})
			}
		}
		i = next
	}
	return out
}

// matchBracket returns the index of the ']' closing the '[' at i, honouring nesting and
// backslash escapes, or -1.
func matchBracket(s string, i int) int {
	depth := 0
	for j := i; j < len(s); j++ {
		switch s[j] {
		case '\\':
			j++
		case '[':
			depth++
		case ']':
			depth--
			if depth == 0 {
				return j
			}
		}
	}
	return -1
}

// parseDest reads a link destination starting at s[start] (just after "("). It returns the
// destination, the index after the closing ")", and whether the construct is well formed. A
// destination is <...> or a run without whitespace with balanced parentheses, optionally followed
// by a title.
func parseDest(s string, start int) (dest string, next int, ok bool) {
	i := start
	for i < len(s) && (s[i] == ' ' || s[i] == '\t') {
		i++
	}
	if i < len(s) && s[i] == '<' {
		end := strings.IndexByte(s[i+1:], '>')
		if end < 0 {
			return "", 0, false
		}
		dest = s[i+1 : i+1+end]
		i += end + 2
	} else {
		depth := 0
		j := i
	loop:
		for ; j < len(s); j++ {
			switch s[j] {
			case '\\':
				j++
			case '(':
				depth++
			case ')':
				if depth == 0 {
					break loop
				}
				depth--
			case ' ', '\t':
				break loop
			}
		}
		if j > len(s) {
			j = len(s)
		}
		dest = s[i:j]
		i = j
	}
	// Optional title, then the closing paren.
	rest := s[i:]
	if k := strings.IndexByte(rest, ')'); k >= 0 {
		if t := strings.TrimSpace(rest[:k]); t == "" || t[0] == '"' || t[0] == '\'' || t[0] == '(' {
			return dest, i + k + 1, true
		}
	}
	return "", 0, false
}

// wikilink parses the inside of [[...]]: target[#anchor][|label].
func wikilink(inner string, lineNo int) (Link, bool) {
	target, _, _ := strings.Cut(inner, "|")
	target, anchor, _ := strings.Cut(target, "#")
	target = strings.TrimSpace(target)
	if target == "" {
		return Link{}, false
	}
	return Link{Target: target, Anchor: strings.TrimSpace(anchor), Rel: RelLinks, Kind: KindWikilink, Line: lineNo}, true
}

// resolve turns a link destination into a root-relative target and anchor. ok is false for
// anything that is not a relative in-corpus path.
func resolve(dir, dest string) (target, anchor string, ok bool) {
	dest = strings.TrimSpace(dest)
	p, frag, _ := strings.Cut(dest, "#")
	p, _, _ = strings.Cut(p, "?")
	if p == "" || strings.HasPrefix(p, "/") || strings.HasPrefix(p, "\\") || hasScheme(p) {
		return "", "", false
	}
	if dec, err := url.PathUnescape(p); err == nil {
		p = dec
	}
	if dec, err := url.PathUnescape(frag); err == nil {
		frag = dec
	}
	if strings.ContainsAny(p, "\x00\n\r") || strings.HasPrefix(p, "/") {
		return "", "", false
	}
	t := path.Join(dir, p)
	if t == "." || t == ".." || strings.HasPrefix(t, "../") || strings.HasPrefix(t, "/") {
		return "", "", false
	}
	if assert.Enabled {
		assert.Post(t != "" && path.Clean(t) == t && !path.IsAbs(t), "links.resolve: target %q is not a clean relative path", t)
	}
	return t, frag, true
}

// hasScheme reports whether p starts with a URI scheme ("http:", "mailto:", "C:").
func hasScheme(p string) bool {
	for i := 0; i < len(p); i++ {
		c := p[i]
		switch {
		case c == ':':
			return i > 0
		case c >= 'a' && c <= 'z', c >= 'A' && c <= 'Z':
		case i > 0 && (c >= '0' && c <= '9' || c == '+' || c == '-' || c == '.'):
		default:
			return false
		}
	}
	return false
}

// FromFrontMatter turns the link lists of fm into Links with Rel = the key (related, supersedes,
// depends_on). An item written [[name]] is a wikilink; any other item is a path relative to
// sourcePath's directory, resolved like a body link (anchors, query and escapes handled the same;
// scheme, absolute and root-escaping items are dropped). Line is 0. Order: related, supersedes,
// depends_on, each in list order.
func FromFrontMatter(sourcePath string, fm entry.FrontMatter) []Link {
	dir := path.Dir(path.Clean(strings.TrimPrefix(sourcePath, "/")))
	var out []Link
	for _, g := range []struct {
		rel   string
		items []string
	}{{RelRelated, fm.Related}, {RelSupersedes, fm.Supersedes}, {RelDependsOn, fm.DependsOn}} {
		for _, it := range g.items {
			it = strings.TrimSpace(it)
			if strings.HasPrefix(it, "[[") && strings.HasSuffix(it, "]]") {
				inner := it[2 : len(it)-2]
				if strings.ContainsAny(inner, "[]") {
					continue
				}
				if l, ok := wikilink(inner, 0); ok {
					l.Rel = g.rel
					out = append(out, l)
				}
				continue
			}
			if t, a, ok := resolve(dir, it); ok {
				out = append(out, Link{Target: t, Anchor: a, Rel: g.rel, Kind: KindMarkdown})
			}
		}
	}
	return out
}

// Dedupe drops repeated links with the same (Rel, Kind, Target), keeping the first, so a caller
// writing edges keyed by (target, rel) never collides.
func Dedupe(ls []Link) []Link {
	type key struct {
		rel, target string
		kind        Kind
	}
	seen := make(map[key]bool, len(ls))
	var out []Link
	for _, l := range ls {
		k := key{l.Rel, l.Target, l.Kind}
		if !seen[k] {
			seen[k] = true
			out = append(out, l)
		}
	}
	return out
}
