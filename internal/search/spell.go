// Spell correction (PLAN 5.6, DEFAULTS #7): when the main full-text list comes back with fewer
// than SpellBelow rows and search.spell is on, each query word that no chunk holds is matched
// against the identifier index's vocabulary (store.Vocab) and the closest common term joins it as
// an alternative, ("kuberntes" OR "kubernetes"), through FTSQuery.Alts like a synonym. The list
// runs once more; the CLI says so on stderr and the log's fts_query shows the rewrite.
//
// Rules, in order:
//   - A word is a field of the query stripped like an identifier (trimIdentifier), made only of
//     letters, at least SpellMinLen long and not an identifier (ExactTerms' test): identifiers,
//     numbers and short words are never corrected.
//   - Only a word whose document count is 0 is corrected (a rare word is still a word).
//   - Candidates are vocabulary terms of letters only with at least SpellMinDoc documents, within
//     MaxEdits(len) optimal-string-alignment edits (a transposition is one edit) and at least one.
//   - The best candidate has the fewest edits, then the most documents, then the smallest term.
//   - The correction's own synonyms come with it, so a corrected word expands like a typed one.
package search

import (
	"context"
	"slices"
	"strings"
	"unicode"
	"unicode/utf8"

	"github.com/nickstrad/kb/internal/assert"
	"github.com/nickstrad/kb/internal/store"
)

// Spell thresholds.
const (
	SpellBelow  = 3 // spell runs only when the main FTS list has fewer rows than this
	SpellMinLen = 4 // shorter words are never corrected: too many terms are one edit away
	SpellMinDoc = 2 // a correction must be in at least this many chunks (bounds the vocabulary read)
	spellLongAt = 8 // words this long or longer may be two edits away
)

// SpellFix is one corrected query word: Term is the query field as typed, Word its lowercase
// core, Correction the vocabulary term searched as well, Distance their edit distance and Doc the
// correction's document count. The JSON tags are part of the `kb explain --json` contract.
type SpellFix struct {
	Term       string `json:"term"`
	Word       string `json:"word"`
	Correction string `json:"correction"`
	Distance   int    `json:"distance"`
	Doc        int    `json:"doc"`
}

// MaxEdits is how many edits a word of n characters may be from its correction: 0 below
// SpellMinLen (never corrected), 1 up to spellLongAt-1, else 2.
func MaxEdits(n int) int {
	switch {
	case n < SpellMinLen:
		return 0
	case n < spellLongAt:
		return 1
	default:
		return 2
	}
}

// spellWanted is the trigger: the main FTS list ran (it has an expression), the spell switch is
// on, the store can read its vocabulary, and the list came back short.
func spellWanted(on, hasVocab bool, match string, rows int) bool {
	return on && hasVocab && match != "" && rows < SpellBelow
}

// spellWord is the lowercase word spell may correct in a query field, ok false when the field is
// not one: an identifier, a number or anything with a non-letter in it, or shorter than
// SpellMinLen.
func spellWord(field string) (string, bool) {
	core := trimIdentifier(field)
	if isIdentifier(core) || utf8.RuneCountInString(core) < SpellMinLen || !onlyLetters(core) {
		return "", false
	}
	return strings.ToLower(core), true
}

// onlyLetters reports whether s is non-empty and every rune is a letter or a combining mark.
func onlyLetters(s string) bool {
	if s == "" {
		return false
	}
	for _, r := range s {
		if !unicode.IsLetter(r) && !unicode.IsMark(r) {
			return false
		}
	}
	return true
}

// Correct returns the correction of word from vocab by the package rules (vocab need not be
// filtered or sorted; terms with fewer than SpellMinDoc documents or a non-letter are skipped). ok
// is false when no term qualifies.
func Correct(word string, vocab []store.VocabTerm) (fix SpellFix, ok bool) {
	w := []rune(word)
	maxd := MaxEdits(len(w))
	if maxd == 0 {
		return SpellFix{}, false
	}
	for _, v := range vocab {
		if v.Doc < SpellMinDoc || v.Term == word || !onlyLetters(v.Term) {
			continue
		}
		d, within := editDistanceWithin(w, []rune(v.Term), maxd)
		if !within || d == 0 {
			continue
		}
		if !ok || d < fix.Distance || (d == fix.Distance && (v.Doc > fix.Doc || (v.Doc == fix.Doc && v.Term < fix.Correction))) {
			fix, ok = SpellFix{Word: word, Correction: v.Term, Distance: d, Doc: v.Doc}, true
		}
	}
	if ok && assert.Enabled {
		assert.Post(fix.Distance >= 1 && fix.Distance <= maxd, "Correct: %q -> %q at distance %d, max %d", word, fix.Correction, fix.Distance, maxd)
	}
	return fix, ok
}

// EditDistance is the optimal-string-alignment distance between a and b over runes: insertions,
// deletions, substitutions and transpositions of two adjacent runes each cost one, and no
// substring is edited twice.
func EditDistance(a, b string) int {
	ra, rb := []rune(a), []rune(b)
	d, _ := editDistanceWithin(ra, rb, max(len(ra), len(rb)))
	return d
}

// editDistanceWithin is the OSA distance of a and b when it is at most maxd (within true);
// otherwise within is false and d is only a lower bound above maxd. It stops as soon as a whole
// row exceeds maxd.
func editDistanceWithin(a, b []rune, maxd int) (d int, within bool) {
	if diff := len(a) - len(b); diff > maxd || -diff > maxd {
		return maxd + 1, false
	}
	// Three rows: prev2 (i-2), prev (i-1), cur (i).
	prev2 := make([]int, len(b)+1)
	prev := make([]int, len(b)+1)
	cur := make([]int, len(b)+1)
	for j := range prev {
		prev[j] = j
	}
	for i := 1; i <= len(a); i++ {
		cur[0] = i
		rowMin := cur[0]
		for j := 1; j <= len(b); j++ {
			cost := 1
			if a[i-1] == b[j-1] {
				cost = 0
			}
			v := min(prev[j]+1, cur[j-1]+1, prev[j-1]+cost)
			if i > 1 && j > 1 && a[i-1] == b[j-2] && a[i-2] == b[j-1] {
				v = min(v, prev2[j-2]+1)
			}
			cur[j] = v
			rowMin = min(rowMin, v)
		}
		if rowMin > maxd {
			return maxd + 1, false
		}
		prev2, prev, cur = prev, cur, prev2
	}
	d = prev[len(b)]
	return d, d <= maxd
}

// spellPlan is the corrections of one query: the fixes, in query order, and the FTS alternatives
// with them (alts plus each corrected field's correction and the correction's synonyms).
type spellPlan struct {
	fixes []SpellFix
	alts  map[string][]string
}

// planSpell finds the corrections of fields: the words (spellWord) with no document get the best
// vocabulary term (Correct). syn expands each correction; alts are the query's synonym
// alternatives, which the result extends (never modified in place). An empty plan means nothing
// to correct, and then the vocabulary is not read.
func (s *Searcher) planSpell(ctx context.Context, v store.Vocab, fields []string, alts map[string][]string) (spellPlan, error) {
	type cand struct{ field, word string }
	var cands []cand
	var words []string
	seen := map[string]bool{}
	for _, f := range fields {
		w, ok := spellWord(f)
		if !ok || seen[f] {
			continue
		}
		seen[f] = true
		cands = append(cands, cand{f, w})
		words = append(words, w)
	}
	if len(cands) == 0 {
		return spellPlan{}, nil
	}
	df, err := v.DocFreq(ctx, words)
	if err != nil {
		return spellPlan{}, err
	}
	minLen, maxLen := -1, 0
	kept := cands[:0]
	for _, c := range cands {
		if df[c.word] > 0 {
			continue
		}
		kept = append(kept, c)
		n := utf8.RuneCountInString(c.word)
		lo, hi := n-MaxEdits(n), n+MaxEdits(n)
		if minLen < 0 || lo < minLen {
			minLen = lo
		}
		maxLen = max(maxLen, hi)
	}
	if len(kept) == 0 {
		return spellPlan{}, nil
	}
	vocab, err := v.VocabTerms(ctx, store.VocabQuery{MinDoc: SpellMinDoc, MinLen: minLen, MaxLen: maxLen})
	if err != nil {
		return spellPlan{}, err
	}
	var plan spellPlan
	for _, c := range kept {
		fix, ok := Correct(c.word, vocab)
		if !ok {
			continue
		}
		fix.Term = c.field
		plan.fixes = append(plan.fixes, fix)
		if plan.alts == nil {
			plan.alts = make(map[string][]string, len(alts)+1)
			for k, a := range alts {
				plan.alts[k] = a
			}
		}
		plan.alts[c.field] = withAlternatives(plan.alts[c.field], c.word, append([]string{fix.Correction}, s.Synonyms.Lookup(fix.Correction)...))
	}
	return plan, nil
}

// withAlternatives is have plus each of more that is new and not word itself, at most
// MaxAlternatives in all; have is copied, never extended in place.
func withAlternatives(have []string, word string, more []string) []string {
	out := slices.Clone(have)
	for _, m := range more {
		if len(out) >= MaxAlternatives {
			break
		}
		if m != word && !slices.Contains(out, m) {
			out = append(out, m)
		}
	}
	return out
}

// keepOriginal is the spell rerun's rows with every row of the first run kept: rows of the first
// run the rerun pushed out (it returns at most limit rows, and new matches can outrank them) go
// back at the end in their first-run order, in place of the rerun's last new rows. So spell never
// loses a row the typed query found.
func keepOriginal(first, rerun []FTSRow, limit int) []FTSRow {
	inFirst := make(map[int64]bool, len(first))
	for _, r := range first {
		inFirst[r.ChunkID] = true
	}
	inRerun := make(map[int64]bool, len(rerun))
	for _, r := range rerun {
		inRerun[r.ChunkID] = true
	}
	var missing []FTSRow
	for _, r := range first {
		if !inRerun[r.ChunkID] {
			missing = append(missing, r)
		}
	}
	if len(missing) == 0 {
		return rerun
	}
	budget := limit - len(first) // room for rows the first run did not have
	out := make([]FTSRow, 0, min(limit, len(rerun)+len(missing)))
	for _, r := range rerun {
		if inFirst[r.ChunkID] {
			out = append(out, r)
		} else if budget > 0 {
			out = append(out, r)
			budget--
		}
	}
	out = append(out, missing...)
	if assert.Enabled {
		ids := make(map[int64]bool, len(out))
		for _, r := range out {
			assert.Post(!ids[r.ChunkID], "keepOriginal: chunk %d twice", r.ChunkID)
			ids[r.ChunkID] = true
		}
		for _, r := range first {
			assert.Post(ids[r.ChunkID], "keepOriginal: first-run chunk %d lost", r.ChunkID)
		}
		assert.Post(len(out) <= max(limit, len(first)), "keepOriginal: %d rows > limit %d", len(out), limit)
	}
	return out
}
