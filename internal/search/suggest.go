// Synonym suggestions (PLAN 5.6, `kb synonyms suggest`): mined from the search log, printed for a
// human to approve, never written to synonyms.txt.
//
// A reworded pair is two consecutive searches of one caller, at most a window apart, where the
// first found nothing worth using (no useful verdict and no open) and the second did (a useful
// verdict or an open), and the queries differ in exactly one word: one word dropped, one added.
// Those two words are a candidate group. Groups are counted over the log, with the callers that
// produced them and each word's document count in the index (a word no chunk holds is the gap the
// group fills). Words of failed searches that no chunk holds are listed as gaps.
package search

import (
	"slices"
	"sort"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/nickstrad/kb/internal/store"
)

// DefaultSuggestWindow is how far apart a reworded pair's two searches may be.
const DefaultSuggestWindow = 5 * time.Minute

// SuggestOptions tune Suggest. Window is the most time between the two searches of a pair (0 means
// DefaultSuggestWindow); MinPairs drops groups seen fewer times (0 means 1); MaxGaps caps the gap
// list (0 means no cap). Synonyms already in force are not suggested again.
type SuggestOptions struct {
	Window   time.Duration
	MinPairs int
	MaxGaps  int
	Synonyms *Synonyms
}

// Suggestions are the mined groups and gaps. The JSON tags are the `kb synonyms suggest --json`
// contract.
type Suggestions struct {
	Window      string       `json:"window"`
	Searches    int          `json:"searches"` // searches read from the log
	Pairs       int          `json:"pairs"`    // reworded pairs found (before MinPairs)
	Suggestions []Suggestion `json:"suggestions"`
	Gaps        []Gap        `json:"gaps"`
}

// Suggestion is one candidate group of two words. Terms are sorted; Line is them as a
// synonyms.txt line. Pairs counts the reworded pairs behind it, From the pairs per dropped word
// (the word the first search had), Callers the distinct callers. Doc is each word's document
// count (0 = no chunk holds it). Typo is true when spell correction would already turn the dropped
// word into the added one (a letters-only word within MaxEdits).
type Suggestion struct {
	Terms   [2]string      `json:"terms"`
	Line    string         `json:"line"`
	Pairs   int            `json:"pairs"`
	From    map[string]int `json:"from"`
	Callers int            `json:"callers"`
	Doc     map[string]int `json:"doc"`
	Typo    bool           `json:"typo"`
}

// Gap is a word of searches that found nothing worth using that no chunk holds, with the number
// of such searches.
type Gap struct {
	Term     string `json:"term"`
	Searches int    `json:"searches"`
}

// SuggestTerms are the distinct words of the searches (QueryWords), sorted: what Suggest needs
// document counts for.
func SuggestTerms(searches []store.JudgedSearch) []string {
	seen := map[string]bool{}
	for _, s := range searches {
		for _, w := range QueryWords(s.Query) {
			seen[w] = true
		}
	}
	out := make([]string, 0, len(seen))
	for w := range seen {
		out = append(out, w)
	}
	sort.Strings(out)
	return out
}

// QueryWords are the distinct words of a query as a synonyms lookup sees them: fields lowercased
// and stripped of the punctuation around them, in order, empty ones dropped.
func QueryWords(query string) []string {
	var out []string
	for _, f := range strings.Fields(query) {
		w := strings.Trim(strings.ToLower(f), synonymTrim)
		if w != "" && !slices.Contains(out, w) {
			out = append(out, w)
		}
	}
	return out
}

// worked reports whether a search found something worth using: a useful verdict or an open.
func worked(s store.JudgedSearch) bool {
	if len(s.Opens) > 0 || len(s.OtherOpens) > 0 {
		return true
	}
	for _, v := range s.Verdicts {
		if v.Useful {
			return true
		}
	}
	return false
}

// reword is the one-word change from a to b: the word only a has and the word only b has. ok is
// false unless exactly one word differs each way.
func reword(a, b []string) (from, to string, ok bool) {
	var gone, added []string
	for _, w := range a {
		if !slices.Contains(b, w) {
			gone = append(gone, w)
		}
	}
	for _, w := range b {
		if !slices.Contains(a, w) {
			added = append(added, w)
		}
	}
	if len(gone) != 1 || len(added) != 1 {
		return "", "", false
	}
	return gone[0], added[0], true
}

// Suggest mines searches (in log order, oldest first) for reworded pairs and gaps. docFreq holds
// the document count of the words (SuggestTerms); a word absent from it counts 0. Groups are
// sorted by pairs, then callers (both descending), then line; gaps by searches descending, then
// term. A timestamp that does not parse ends no pair.
func Suggest(searches []store.JudgedSearch, docFreq map[string]int, o SuggestOptions) Suggestions {
	window := o.Window
	if window <= 0 {
		window = DefaultSuggestWindow
	}
	out := Suggestions{Window: window.String(), Searches: len(searches), Suggestions: []Suggestion{}, Gaps: []Gap{}}

	type group struct {
		s       Suggestion
		callers map[string]bool
	}
	groups := map[[2]string]*group{}
	last := map[string]store.JudgedSearch{} // each caller's previous search
	gapCount := map[string]int{}
	for _, cur := range searches {
		words := QueryWords(cur.Query)
		if !worked(cur) {
			for _, w := range words {
				if docFreq[w] == 0 {
					gapCount[w]++
				}
			}
		}
		prev, hasPrev := last[cur.Caller]
		last[cur.Caller] = cur
		if !hasPrev || worked(prev) || !worked(cur) || !withinWindow(prev.TS, cur.TS, window) {
			continue
		}
		from, to, ok := reword(QueryWords(prev.Query), words)
		if !ok || slices.Contains(o.Synonyms.Lookup(from), to) {
			continue
		}
		out.Pairs++
		key := [2]string{min(from, to), max(from, to)}
		g := groups[key]
		if g == nil {
			g = &group{s: Suggestion{
				Terms: key, Line: key[0] + ", " + key[1], From: map[string]int{},
				Doc:  map[string]int{key[0]: docFreq[key[0]], key[1]: docFreq[key[1]]},
				Typo: isTypo(from, to),
			}, callers: map[string]bool{}}
			groups[key] = g
		}
		g.s.Pairs++
		g.s.From[from]++
		g.s.Typo = g.s.Typo || isTypo(from, to)
		g.callers[cur.Caller] = true
	}

	minPairs := max(o.MinPairs, 1)
	for _, g := range groups {
		if g.s.Pairs < minPairs {
			continue
		}
		g.s.Callers = len(g.callers)
		out.Suggestions = append(out.Suggestions, g.s)
	}
	sort.Slice(out.Suggestions, func(i, j int) bool {
		a, b := out.Suggestions[i], out.Suggestions[j]
		if a.Pairs != b.Pairs {
			return a.Pairs > b.Pairs
		}
		if a.Callers != b.Callers {
			return a.Callers > b.Callers
		}
		return a.Line < b.Line
	})
	for w, n := range gapCount {
		out.Gaps = append(out.Gaps, Gap{Term: w, Searches: n})
	}
	sort.Slice(out.Gaps, func(i, j int) bool {
		if out.Gaps[i].Searches != out.Gaps[j].Searches {
			return out.Gaps[i].Searches > out.Gaps[j].Searches
		}
		return out.Gaps[i].Term < out.Gaps[j].Term
	})
	if o.MaxGaps > 0 && len(out.Gaps) > o.MaxGaps {
		out.Gaps = out.Gaps[:o.MaxGaps]
	}
	return out
}

// isTypo reports whether spell correction's distance rule would take from to to: both words of
// letters only and from within MaxEdits of to (at least one edit).
func isTypo(from, to string) bool {
	if !onlyLetters(from) || !onlyLetters(to) {
		return false
	}
	maxd := MaxEdits(utf8.RuneCountInString(from))
	d, ok := editDistanceWithin([]rune(from), []rune(to), maxd)
	return ok && d >= 1
}

// withinWindow reports whether b was logged at most window after a (and not before it). Either
// timestamp failing to parse is false.
func withinWindow(a, b string, window time.Duration) bool {
	ta, err := time.Parse(time.RFC3339Nano, a)
	if err != nil {
		return false
	}
	tb, err := time.Parse(time.RFC3339Nano, b)
	if err != nil {
		return false
	}
	gap := tb.Sub(ta)
	return gap >= 0 && gap <= window
}
