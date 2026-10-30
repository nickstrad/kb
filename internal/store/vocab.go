package store

import "context"

// Vocab reads the vocabulary of the identifier index (chunks_exact: unstemmed, case-folded,
// diacritics kept; PLAN 5.6 spell correction and kb synonyms suggest). It is separate from
// Retriever so a backend can add it without widening the interface every command takes; search
// uses it when the store has it. A term that ends a sentence is indexed with its trailing `.` or
// `:` (`kb.toml.`), so a backend trims those and adds the variants' document counts together: a
// chunk holding both `foo` and `foo.` counts twice, so Doc is an upper bound.
type Vocab interface {
	// VocabTerms returns the terms with at least q.MinDoc documents (chunks) whose length in
	// characters is within [q.MinLen, q.MaxLen] (a bound of 0 is none), sorted by term.
	VocabTerms(ctx context.Context, q VocabQuery) ([]VocabTerm, error)
	// DocFreq returns the document count of each of terms (lowercase), with its `.` and `:`
	// variants added; a term no chunk holds is absent.
	DocFreq(ctx context.Context, terms []string) (map[string]int, error)
}

// VocabQuery narrows Vocab.VocabTerms.
type VocabQuery struct {
	MinDoc int
	MinLen int
	MaxLen int
}

// VocabTerm is one term of the vocabulary and the number of chunks that hold it.
type VocabTerm struct {
	Term string
	Doc  int
}
