// The store.Vocab implementation: the identifier index's vocabulary, read through
// chunks_exact_vocab (fts5vocab 'row' over chunks_exact, PLAN 5.6).

package sqlite

import (
	"context"
	"fmt"

	"github.com/nickstrad/kb/internal/store"
)

var _ store.Vocab = (*Store)(nil)

// vocabTrim is what a term loses before it is counted: the sentence punctuation a document leaves
// on an identifier token (`kb.toml.`), as search.ExactTerms trims it on the query side.
const vocabTrim = ".:"

// VocabTerms reads the vocabulary in one scan of chunks_exact_vocab, trimmed and merged
// (store.Vocab), keeping the terms with at least q.MinDoc chunks and a length in [MinLen, MaxLen].
func (s *Store) VocabTerms(ctx context.Context, q store.VocabQuery) ([]store.VocabTerm, error) {
	maxLen := q.MaxLen
	if maxLen <= 0 {
		maxLen = 1 << 30
	}
	rows, err := s.db.QueryContext(ctx, `
		SELECT t, sum(doc) AS n FROM (SELECT rtrim(term, ?1) AS t, doc FROM chunks_exact_vocab)
		WHERE t <> '' AND length(t) BETWEEN ?2 AND ?3
		GROUP BY t HAVING n >= ?4 ORDER BY t`, vocabTrim, q.MinLen, maxLen, q.MinDoc)
	if err != nil {
		return nil, wrap(ctx, err, s.path, "sqlite.VocabTerms", "could not read the index vocabulary")
	}
	defer rows.Close()
	var out []store.VocabTerm
	for rows.Next() {
		var v store.VocabTerm
		if err := rows.Scan(&v.Term, &v.Doc); err != nil {
			return nil, wrap(ctx, err, s.path, "sqlite.VocabTerms", "could not read the index vocabulary")
		}
		out = append(out, v)
	}
	if err := rows.Err(); err != nil {
		return nil, wrap(ctx, err, s.path, "sqlite.VocabTerms", "could not read the index vocabulary")
	}
	return out, nil
}

// DocFreq looks each term up with its `.` and `:` variants (fts5vocab answers term = ? without a
// scan), docFreqBatch terms per statement. The terms must already be trimmed (no trailing `.` or
// `:`), as search's words are; deeper variants (`foo.:`) are not looked up.
func (s *Store) DocFreq(ctx context.Context, terms []string) (map[string]int, error) {
	out := map[string]int{}
	// Dedupe once, so a term repeated across two batches is not counted twice.
	seen := make(map[string]bool, len(terms))
	distinct := make([]string, 0, len(terms))
	for _, t := range terms {
		if !seen[t] {
			seen[t] = true
			distinct = append(distinct, t)
		}
	}
	for start := 0; start < len(distinct); start += docFreqBatch {
		if err := s.docFreq(ctx, distinct[start:min(start+docFreqBatch, len(distinct))], out); err != nil {
			return nil, wrap(ctx, err, s.path, "sqlite.DocFreq", "could not read the index vocabulary")
		}
	}
	return out, nil
}

// docFreqBatch bounds the variables of one DocFreq statement (3 per term) well under SQLite's limit.
const docFreqBatch = 256

// docFreq adds the counts of one batch of distinct terms to out.
func (s *Store) docFreq(ctx context.Context, terms []string, out map[string]int) error {
	args := make([]any, 0, 3*len(terms)+1)
	args = append(args, vocabTrim)
	for _, t := range terms {
		args = append(args, t, t+".", t+":")
	}
	rows, err := s.db.QueryContext(ctx, fmt.Sprintf(
		`SELECT rtrim(term, ?1), doc FROM chunks_exact_vocab WHERE term IN (%s)`, placeholdersFrom(2, len(args)-1)), args...)
	if err != nil {
		return err
	}
	defer rows.Close()
	for rows.Next() {
		var t string
		var n int
		if err := rows.Scan(&t, &n); err != nil {
			return err
		}
		out[t] += n
	}
	return rows.Err()
}

// placeholdersFrom is n numbered placeholders ?first, ?first+1, ... joined by commas (a statement
// that numbers one placeholder must number them all).
func placeholdersFrom(first, n int) string {
	b := make([]byte, 0, n*4)
	for i := 0; i < n; i++ {
		if i > 0 {
			b = append(b, ',')
		}
		b = fmt.Appendf(b, "?%d", first+i)
	}
	return string(b)
}
