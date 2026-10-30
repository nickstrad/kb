// Package render turns search hits into compact, grep-like text for people and agents, optionally
// shrunk to fit a token budget.
//
// Inputs: ranked Hits (path, heading, updated date, entry summary, chunk text) and a budget in
// estimated tokens (0 = unlimited). Output: the hit section of kb search's human output. The
// "search N · M hits" line and the search_id footer belong to the caller and are not counted.
//
// Token estimate: EstimateTokens is ceil(runes/4). It is deterministic and approximate (English
// prose averages 3.5 to 4.5 characters per token), so a budget is a size target, not a promise
// about any one model's tokenizer.
//
// Elastic rendering. Every hit has three levels: 1 = one header line, 2 = header plus the
// entry summary, 3 = header plus the chunk text. Level 3 is never cheaper than level 2 (when the
// chunk text is shorter than the summary, level 3 shows the summary), so cost grows with level.
// Under a budget the renderer starts all hits at level 1, then upgrades hits in rank order to
// level 2, then to level 3, stopping each pass at the first hit that does not fit. A tail line
// "(+N hits not shown, K condensed; raise --budget)" says what was cut. If even the headers do not
// fit, the largest prefix of headers that fits is shown. If not even one header plus the tail fits,
// the output is empty.
//
// Guarantees (asserted): the output is at most the budget in estimated tokens when the budget is
// positive, and a larger budget never lowers any hit's level or the number of hits shown.
package render

import (
	"fmt"
	"strings"
	"unicode/utf8"

	"github.com/nickstrad/kb/internal/assert"
)

// Hit is one search hit as the renderer needs it. Updated and Summary may be empty. Store is the
// store a cross-store hit came from, as the user named it; "" for the primary store's hits.
type Hit struct {
	Rank    int
	Store   string
	Path    string
	Heading string
	Updated string // the entry's front-matter date, shown verbatim
	Summary string // the entry's front-matter summary
	Text    string // the chunk text
}

const (
	// TextLimit is the rune cap on a hit's chunk text at level 3.
	TextLimit = 600
	// SummaryLimit is the rune cap on a summary at level 2.
	SummaryLimit = 300

	levelHeader  = 1
	levelSummary = 2
	levelFull    = 3
)

// EstimateTokens returns ceil(runes/4) for s. It is the one token estimate kb uses.
func EstimateTokens(s string) int {
	return (utf8.RuneCountInString(s) + 3) / 4
}

// Result is a rendered hit section. Text has no trailing newline.
type Result struct {
	Text   string
	Tokens int   // EstimateTokens(Text)
	Levels []int // per input hit: 0 = not shown, else 1..3
}

// Render renders hits (in the order given) within budget estimated tokens; budget 0 is
// unlimited and renders every hit at level 3. A negative budget is treated as 0.
func Render(hits []Hit, budget int) Result {
	if budget < 0 {
		budget = 0
	}
	n := len(hits)
	levels := make([]int, n)
	if n == 0 {
		return Result{Levels: levels}
	}
	if budget == 0 {
		for i := range levels {
			levels[i] = levelFull
		}
		text := compose(hits, levels)
		return Result{Text: text, Tokens: EstimateTokens(text), Levels: levels}
	}

	fits := func(lv []int) (string, bool) {
		s := compose(hits, lv)
		return s, EstimateTokens(s) <= budget
	}

	// Every hit at level 3 is the only state without a tail, so it can fit when no partial state
	// does (a tail costs tokens). Check it first.
	for i := range levels {
		levels[i] = levelFull
	}
	if _, ok := fits(levels); ok {
		return finish(hits, levels, budget)
	}

	// Phase 1: the largest prefix of headers that fits (with its tail).
	shown := 0
	for m := n; m >= 1; m-- {
		for i := range levels {
			levels[i] = 0
			if i < m {
				levels[i] = levelHeader
			}
		}
		if _, ok := fits(levels); ok {
			shown = m
			break
		}
	}
	if shown == 0 {
		for i := range levels {
			levels[i] = 0
		}
		return finish(hits, levels, budget)
	}

	// Phase 2: upgrade in rank order, level 2 for everyone, then level 3, each pass stopping at
	// the first hit that does not fit. Passes only run when every hit is shown.
	if shown == n {
		for i := range levels {
			levels[i] = levelHeader
		}
		for target := levelSummary; target <= levelFull; target++ {
			stopped := false
			for i := 0; i < n && !stopped; i++ {
				levels[i] = target
				if _, ok := fits(levels); !ok {
					levels[i] = target - 1
					stopped = true
				}
			}
			if stopped {
				break
			}
		}
	}
	return finish(hits, levels, budget)
}

func finish(hits []Hit, levels []int, budget int) Result {
	text := compose(hits, levels)
	tokens := EstimateTokens(text)
	if tokens > budget && allZero(levels) {
		// Not even the tail fits: print nothing.
		text, tokens = "", 0
	}
	assert.Post(tokens <= budget, "render: %d tokens exceed budget %d", tokens, budget)
	return Result{Text: text, Tokens: tokens, Levels: levels}
}

func allZero(lv []int) bool {
	for _, l := range lv {
		if l != 0 {
			return false
		}
	}
	return true
}

// compose joins the blocks of the shown hits with blank lines and appends the tail when anything
// was hidden or condensed. When no hit is shown and the tail alone would be all there is, the
// tail still prints (it tells the reader to raise the budget); Render's fit test decides whether
// that is allowed.
func compose(hits []Hit, levels []int) string {
	var blocks []string
	hidden, condensed := 0, 0
	for i, h := range hits {
		switch {
		case levels[i] == 0:
			hidden++
		default:
			blocks = append(blocks, block(h, levels[i]))
			if levels[i] < levelFull {
				condensed++
			}
		}
	}
	if hidden == 0 && condensed == 0 {
		return strings.Join(blocks, "\n\n")
	}
	var parts []string
	if hidden > 0 {
		parts = append(parts, fmt.Sprintf("+%d hits not shown", hidden))
	}
	if condensed > 0 {
		parts = append(parts, fmt.Sprintf("%s%d condensed", plus(len(parts) == 0), condensed))
	}
	blocks = append(blocks, "("+strings.Join(parts, ", ")+"; raise --budget)")
	return strings.Join(blocks, "\n\n")
}

func plus(first bool) string {
	if first {
		return "+"
	}
	return ""
}

// header is "[r] path › heading · updated DATE", with the path written "store:path" for another
// store's hit: the form kb show takes.
func header(h Hit) string {
	path := h.Path
	if h.Store != "" {
		path = h.Store + ":" + path
	}
	s := fmt.Sprintf("[%d] %s", h.Rank, path)
	if h.Heading != "" {
		s += " › " + h.Heading
	}
	if h.Updated != "" {
		s += " · updated " + h.Updated
	}
	return s
}

// block renders one hit at a level. Level 3 falls back to level 2 when that is longer, so cost
// never decreases with level.
func block(h Hit, level int) string {
	head := header(h)
	l2 := head
	if h.Summary != "" {
		l2 += "\n" + Trim(h.Summary, SummaryLimit)
	}
	switch level {
	case levelHeader:
		return head
	case levelSummary:
		return l2
	}
	l3 := head
	if h.Text != "" {
		l3 += "\n" + Trim(h.Text, TextLimit)
	}
	if utf8.RuneCountInString(l3) < utf8.RuneCountInString(l2) {
		return l2
	}
	return l3
}

// Trim caps text at limit runes, cutting at a rune boundary. When it has to cut, it prefers the
// last newline or space at or before the limit (so a word is not sliced in half) and marks the cut
// with a trailing " …"; text at or under the limit is returned unchanged.
func Trim(text string, limit int) string {
	runes := []rune(text)
	if len(runes) <= limit {
		return text
	}
	cut := limit
	firstBoundary := limit - 80
	if firstBoundary < 0 {
		firstBoundary = 0
	}
	for i := limit; i > firstBoundary; i-- {
		if runes[i-1] == '\n' || runes[i-1] == ' ' {
			cut = i - 1
			break
		}
	}
	return strings.TrimRight(string(runes[:cut]), " \n") + " …"
}
