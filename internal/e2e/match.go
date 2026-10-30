package e2e

import (
	"fmt"
	"regexp"
	"strings"
)

const stderrPrefix = "[stderr] "

// lineMatches reports whether one expected line that is not "..." matches the actual line.
// "~ RE" matches the whole line; "[stderr] ~ RE" matches a stderr line by its text.
func lineMatches(exp, act string) bool {
	if re, ok := patternOf(exp); ok {
		return re.MatchString(act)
	}
	return exp == act
}

// patternOf compiles an elision line. A bad regexp never matches anything.
func patternOf(exp string) (*regexp.Regexp, bool) {
	prefix, body := "", exp
	if rest, ok := strings.CutPrefix(exp, stderrPrefix); ok {
		prefix, body = stderrPrefix, rest
	}
	re, ok := strings.CutPrefix(body, "~ ")
	if !ok {
		return nil, false
	}
	c, err := regexp.Compile(`^` + regexp.QuoteMeta(prefix) + `(?:` + re + `)$`)
	if err != nil {
		return regexp.MustCompile(`\A\z.\A`), true
	}
	return c, true
}

// Match reports whether actual satisfies expected, where an expected "..." line stands for any
// number of lines (including none).
func Match(expected, actual []string) bool {
	// reach[j]: expected[:i] can match actual[:j]
	reach := make([]bool, len(actual)+1)
	reach[0] = true
	for _, e := range expected {
		next := make([]bool, len(actual)+1)
		if e == "..." {
			seen := false
			for j := range reach {
				seen = seen || reach[j]
				next[j] = seen
			}
		} else {
			for j := 0; j < len(actual); j++ {
				next[j+1] = reach[j] && lineMatches(e, actual[j])
			}
		}
		reach = next
	}
	return reach[len(actual)]
}

// Merge returns the new expected block for -update: the lines of actual, keeping every expected
// "..." and "~ RE" line that still matches in its place. When the kept form would not match
// actual, it returns actual unchanged.
func Merge(expected, actual []string) []string {
	if Match(expected, actual) {
		return expected
	}
	// LCS over (expected line, actual line) pairs that match.
	n, m := len(expected), len(actual)
	lcs := make([][]int, n+1)
	for i := range lcs {
		lcs[i] = make([]int, m+1)
	}
	for i := n - 1; i >= 0; i-- {
		for j := m - 1; j >= 0; j-- {
			switch {
			case expected[i] != "..." && lineMatches(expected[i], actual[j]):
				lcs[i][j] = lcs[i+1][j+1] + 1
			case lcs[i+1][j] >= lcs[i][j+1]:
				lcs[i][j] = lcs[i+1][j]
			default:
				lcs[i][j] = lcs[i][j+1]
			}
		}
	}
	var out []string
	i, j := 0, 0
	for i < n && j < m {
		switch {
		case expected[i] != "..." && lineMatches(expected[i], actual[j]) && lcs[i][j] == lcs[i+1][j+1]+1:
			out = append(out, expected[i])
			i++
			j++
		case lcs[i+1][j] >= lcs[i][j+1]:
			if expected[i] == "..." {
				out = append(out, "...")
			}
			i++
		default:
			out = append(out, actual[j])
			j++
		}
	}
	for ; i < n; i++ {
		if expected[i] == "..." {
			out = append(out, "...")
		}
	}
	out = append(out, actual[j:]...)
	if Match(out, actual) {
		return out
	}
	return actual
}

// Diff is a readable line diff of expected against actual ("-" expected only, "+" actual only).
// Elision lines show as written; an expected line that matches by pattern is shown as matched.
func Diff(expected, actual []string) string {
	n, m := len(expected), len(actual)
	lcs := make([][]int, n+1)
	for i := range lcs {
		lcs[i] = make([]int, m+1)
	}
	for i := n - 1; i >= 0; i-- {
		for j := m - 1; j >= 0; j-- {
			switch {
			case expected[i] != "..." && lineMatches(expected[i], actual[j]):
				lcs[i][j] = lcs[i+1][j+1] + 1
			case lcs[i+1][j] >= lcs[i][j+1]:
				lcs[i][j] = lcs[i+1][j]
			default:
				lcs[i][j] = lcs[i][j+1]
			}
		}
	}
	var b strings.Builder
	i, j := 0, 0
	for i < n || j < m {
		switch {
		case i < n && j < m && expected[i] != "..." && lineMatches(expected[i], actual[j]) && lcs[i][j] == lcs[i+1][j+1]+1:
			fmt.Fprintf(&b, "  %s\n", actual[j])
			i++
			j++
		case i < n && (j >= m || lcs[i+1][j] >= lcs[i][j+1]):
			fmt.Fprintf(&b, "- %s\n", expected[i])
			i++
		default:
			fmt.Fprintf(&b, "+ %s\n", actual[j])
			j++
		}
	}
	return b.String()
}
