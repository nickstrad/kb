package search

import (
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"io/fs"
	"slices"
	"sort"
	"strings"

	"github.com/nickstrad/kb/internal/config"
	"github.com/nickstrad/kb/internal/errs"
)

// SynonymsFile is the file name of a store's synonyms (<store dir>/synonyms.txt).
const SynonymsFile = "synonyms.txt"

// MaxAlternatives caps the alternatives one query term expands to (the first ones, in file
// order), so a hostile file cannot turn a query into an enormous MATCH expression.
const MaxAlternatives = 16

// Synonyms maps a lowercase term to the other members of every group it belongs to. The zero
// value and nil are empty: no query changes.
//
// File format (PLAN 5.5): one group per line, members separated by commas. A line whose first
// non-space character is `#` is a comment (an inline `#` is part of a member, so `c#, csharp`
// works). Members are trimmed, inner whitespace is collapsed and the text is lowercased; an empty
// member is dropped and a group with fewer than two distinct members does nothing. A member of
// several words (`kube cluster`) is an alternative matched as a phrase, but only a single-word
// member can be looked up from a query term. Groups that share a member do not merge: a term
// expands to the members of every group it is in, and not to those groups' other members' groups.
type Synonyms struct {
	byTerm map[string][]string
	hash   string
}

// ParseSynonyms reads synonyms text. It never fails: a line it cannot use is skipped. A term
// keeps only its first MaxAlternatives alternatives (file order), the most Lookup returns, so the
// work and memory stay linear in the file size however large a group is.
func ParseSynonyms(data []byte) *Synonyms {
	byTerm := map[string][]string{}
	has := map[string]map[string]bool{} // byTerm as sets, for the "already added" check
	for _, line := range strings.Split(string(data), "\n") {
		line = strings.TrimSpace(line)
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		var members []string
		seen := map[string]bool{}
		for _, m := range strings.Split(line, ",") {
			m = strings.ToLower(strings.Join(strings.Fields(m), " "))
			if m != "" && !seen[m] {
				seen[m] = true
				members = append(members, m)
			}
		}
		if len(members) < 2 {
			continue
		}
		for _, m := range members {
			if has[m] == nil {
				has[m] = map[string]bool{}
			}
			for _, other := range members {
				if len(byTerm[m]) >= MaxAlternatives {
					break
				}
				if other != m && !has[m][other] {
					has[m][other] = true
					byTerm[m] = append(byTerm[m], other)
				}
			}
		}
	}
	return &Synonyms{byTerm: byTerm, hash: synonymsHash(byTerm)}
}

// synonymsHash hashes the canonical form (sorted terms, each with its sorted alternatives: the
// alternatives are ORed, so their order changes no result), so a comment, a blank line or the
// order of members changes nothing. "" for no groups.
func synonymsHash(byTerm map[string][]string) string {
	if len(byTerm) == 0 {
		return ""
	}
	terms := make([]string, 0, len(byTerm))
	for t := range byTerm {
		terms = append(terms, t)
	}
	sort.Strings(terms)
	h := sha256.New()
	for _, t := range terms {
		h.Write([]byte(t))
		for _, a := range slices.Sorted(slices.Values(byTerm[t])) {
			h.Write([]byte{0})
			h.Write([]byte(a))
		}
		h.Write([]byte{'\n'})
	}
	return hex.EncodeToString(h.Sum(nil))
}

// LoadSynonyms reads and parses the file at path: a regular file of at most config.MaxFileBytes.
// A missing file is a NotFound error; a directory, a pipe, a device or an oversized file is Usage.
func LoadSynonyms(path string) (*Synonyms, error) {
	const op = "search.LoadSynonyms"
	data, err := config.ReadCapped(op, "synonyms", path)
	if errors.Is(err, fs.ErrNotExist) {
		return nil, errs.Wrap(err, errs.NotFound, op, "no synonyms file at %s", path)
	}
	if err != nil {
		return nil, err
	}
	return ParseSynonyms(data), nil
}

// Len is the number of terms that expand.
func (s *Synonyms) Len() int {
	if s == nil {
		return 0
	}
	return len(s.byTerm)
}

// Hash identifies the synonyms' content ("" when there are none): the same groups hash the same
// whatever the comments, spacing, case or order of lines and members.
func (s *Synonyms) Hash() string {
	if s == nil {
		return ""
	}
	return s.hash
}

// has reports whether to is one of from's kept alternatives (exact keys, no trimming): the pair
// is in force. Unlike Lookup it allocates nothing and cannot confuse a punctuated field with a
// term. A pair that fell past MaxAlternatives is not in force, so it is not "had".
func (s *Synonyms) has(from, to string) bool {
	if s == nil {
		return false
	}
	return slices.Contains(s.byTerm[from], to)
}

// Lookup returns the alternatives of a query field: the other members of its groups, at most
// MaxAlternatives. The field is lowercased and, if that finds nothing, stripped of the
// punctuation around it (`Kubernetes,` finds `kubernetes`).
func (s *Synonyms) Lookup(field string) []string {
	if s.Len() == 0 {
		return nil
	}
	key := strings.ToLower(field)
	alts, ok := s.byTerm[key]
	if !ok {
		alts = s.byTerm[strings.Trim(key, synonymTrim)]
	}
	return slices.Clip(alts[:min(len(alts), MaxAlternatives)])
}

// synonymTrim is the punctuation removed from around a query field before its second lookup.
const synonymTrim = `.,;:!?"'()[]{}<>`

// Expand maps each field of a query that has synonyms (keyed by the field as typed) to its
// alternatives; nil when none has. This is store.FTSQuery.Alts.
func (s *Synonyms) Expand(fields []string) map[string][]string {
	var out map[string][]string
	for _, f := range fields {
		if alts := s.Lookup(f); len(alts) > 0 {
			if out == nil {
				out = map[string][]string{}
			}
			out[f] = alts
		}
	}
	return out
}
