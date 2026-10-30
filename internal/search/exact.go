package search

import (
	"strings"
	"unicode"
)

// ExactTerms returns the identifier-like fields of a query, in order and without duplicates: the
// terms of the exact-identifier list (PLAN 4.5). A whitespace field is first stripped of the
// punctuation around it (anything but a letter, a digit, a mark or one of `_ - . : /`, so
// `(kb.toml),` gives `kb.toml`) and of a trailing `.` or `:` (sentence punctuation:
// `EnsureEmbedMeta.` gives `EnsureEmbedMeta`), until neither is left. What is left is an identifier when it contains one
// of `_ . / : -`, a lower-case letter followed by an upper-case one (camelCase) or a digit followed
// by a lower-case letter (`v1beta`, `0x1f`). nil when the query has none, and then no exact
// list runs.
//
// The terms go to the identifier index, whose tokenizer keeps `_ - . : /` inside a token, so each
// matches only itself: `foo_bar` not `foo bar`, `--embed-model` not `embed-model`. A document whose
// identifier ends a sentence (`see kb.toml.`) holds the token `kb.toml.`, which the term `kb.toml`
// does not match; the main FTS list still finds it.
func ExactTerms(query string) []string {
	var out []string
	seen := map[string]bool{}
	for _, field := range strings.Fields(query) {
		core := trimIdentifier(field)
		if !isIdentifier(core) || seen[core] {
			continue
		}
		seen[core] = true
		out = append(out, core)
	}
	return out
}

// trimIdentifier strips a field to its identifier: the punctuation around it, then a trailing `.`
// or `:`, repeated until neither applies (`.0!.` gives `.0`, not `.0!`), so ExactTerms of its own
// terms gives them back (FuzzExactTerms found the one-pass version wanting).
func trimIdentifier(field string) string {
	for {
		core := strings.TrimRight(strings.TrimFunc(field, func(r rune) bool { return !exactTokenRune(r) }), ".:")
		if core == field {
			return core
		}
		field = core
	}
}

// exactTokenRune reports whether r can be part of a token of the identifier index: the unicode61
// token runes plus its tokenchars.
func exactTokenRune(r rune) bool {
	return unicode.IsLetter(r) || unicode.IsNumber(r) || unicode.IsMark(r) || strings.ContainsRune(exactTokenChars, r)
}

// exactTokenChars are the identifier index's tokenchars (schema v6, chunks_exact).
const exactTokenChars = "_-.:/"

// isIdentifier is the PLAN's identifier test, `[_./:-]|[a-z][A-Z]|\d[a-z]`, on one field. A field
// with no letter or digit (`--`, `...`) is not one: it would match nothing useful.
func isIdentifier(s string) bool {
	if !strings.ContainsFunc(s, func(r rune) bool { return unicode.IsLetter(r) || unicode.IsNumber(r) }) {
		return false
	}
	if strings.ContainsAny(s, exactTokenChars) {
		return true
	}
	prev := rune(0)
	for _, r := range s {
		if (isLowerASCII(prev) && r >= 'A' && r <= 'Z') || (prev >= '0' && prev <= '9' && isLowerASCII(r)) {
			return true
		}
		prev = r
	}
	return false
}

func isLowerASCII(r rune) bool { return r >= 'a' && r <= 'z' }
