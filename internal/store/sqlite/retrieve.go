// The store.Retriever implementation: the FTS5/BM25 list, the sqlite-vec KNN list, chunk display
// data, and the FTS5 query sanitiser, which encodes the unicode61 tokenizer's behaviour.

package sqlite

import (
	"context"
	"fmt"
	"math"
	"slices"
	"strconv"
	"strings"
	"unicode"

	sqlite_vec "github.com/asg017/sqlite-vec-go-bindings/cgo"

	"github.com/nickstrad/kb/internal/assert"
	"github.com/nickstrad/kb/internal/store"
)

// FTS runs one MATCH over chunks_fts (q.Exact: chunks_exact), scored with bm25 and q.Weights
// (text, heading, title; nil is store.DefaultBM25; chunks_exact has no title column) and returns
// at most q.Limit rows, best first (bm25 is negative, lower is better). Each term is quoted as one
// phrase (MatchExpr), which the table's own tokenizer splits; a query whose terms all sanitise
// away runs nothing. A tag filter joins out to entry_tags (the normalised tag, store.NormTag)
// inside the query, so the LIMIT stays full;
// q.ChunkIDs, when non-nil, restricts the rows to those chunk ids.
func (s *Store) FTS(ctx context.Context, q store.FTSQuery) (store.FTSResult, error) {
	assert.Pre(q.Limit > 0, "FTS: limit=%d must be positive", q.Limit)
	w := store.DefaultBM25()
	if q.Weights != nil {
		w = *q.Weights
	}
	assert.Pre(validWeight(w.Text) && validWeight(w.Heading) && validWeight(w.Title), "FTS: bm25 weights %+v must be finite and 0 or more", w)
	assert.Pre(q.Near >= 0, "FTS: near=%d must not be negative", q.Near)
	match := matchFor(q)
	if match == "" || (q.ChunkIDs != nil && len(q.ChunkIDs) == 0) {
		return store.FTSResult{Match: match}, nil
	}
	query, args := ftsSQL(match, w, q)
	rows, err := s.db.QueryContext(ctx, query, args...)
	if err != nil {
		return store.FTSResult{}, wrap(ctx, fmt.Errorf("match %q: %w", match, err), s.path, "sqlite.FTS", "full-text search failed")
	}
	defer rows.Close()
	res := store.FTSResult{Match: match}
	for rows.Next() {
		var r store.FTSRow
		if err := rows.Scan(&r.ChunkID, &r.Score); err != nil {
			return store.FTSResult{}, wrap(ctx, err, s.path, "sqlite.FTS", "full-text search failed")
		}
		res.Rows = append(res.Rows, r)
	}
	if err := rows.Err(); err != nil {
		return store.FTSResult{}, wrap(ctx, err, s.path, "sqlite.FTS", "full-text search failed")
	}
	return res, nil
}

// ftsSQL builds the FTS statement and its arguments. Only the optional clauses and the table (one
// of two fixed names) vary; every value is a bound parameter.
func ftsSQL(match string, w store.BM25Weights, q store.FTSQuery) (string, []any) {
	var b strings.Builder
	table := "chunks_fts"
	args := []any{w.Text, w.Heading, w.Title}
	if q.Exact {
		table = "chunks_exact" // columns text, heading
		args = args[:2]
	}
	b.WriteString(`SELECT f.rowid, bm25(` + table + `, ` + placeholders(len(args)) + `) AS s FROM ` + table + ` f`)
	if q.Tag != "" {
		b.WriteString(` JOIN chunks c ON c.id = f.rowid JOIN entry_tags t ON t.entry_id = c.entry_id AND t.tag = ?`)
		args = append(args, store.NormTag(q.Tag))
	}
	b.WriteString(` WHERE ` + table + ` MATCH ?`)
	args = append(args, match)
	if q.ChunkIDs != nil {
		// The unary + keeps the IN list away from FTS5's xBestIndex: without it SQLite runs the
		// MATCH once per id (a fresh cursor, bm25 statistics and all, each time), 10x slower for
		// the 20 ids of a tier rescoring (W13 pass, BenchmarkTiers). With it the MATCH runs once
		// and the ids filter its rows.
		b.WriteString(` AND +f.rowid IN (` + placeholders(len(q.ChunkIDs)) + `)`)
		for _, id := range q.ChunkIDs {
			args = append(args, id)
		}
	}
	b.WriteString(` ORDER BY s LIMIT ?`)
	return b.String(), append(args, q.Limit)
}

// validWeight reports whether a bm25 column weight is usable: finite and not negative.
func validWeight(v float64) bool { return !math.IsNaN(v) && !math.IsInf(v, 0) && v >= 0 }

// KNN runs the sqlite-vec KNN inside SQLite (no vectors leave the database). vec0 cannot filter
// on another table, so with a tag it fetches max(K, OverFetch) and keeps the first K that carry
// the tag, in distance order. Call it only when HasVectors is true.
func (s *Store) KNN(ctx context.Context, q store.KNNQuery) ([]store.VecRow, error) {
	assert.Pre(q.K > 0, "KNN: k=%d must be positive", q.K)
	if len(q.Vec) == 0 {
		return nil, nil
	}
	out, err := s.knn(ctx, q)
	if err != nil {
		return nil, wrap(ctx, err, s.path, "sqlite.KNN", "vector search failed")
	}
	if assert.Enabled {
		assert.Post(len(out) <= q.K, "KNN: %d rows > k=%d", len(out), q.K)
	}
	return out, nil
}

func (s *Store) knn(ctx context.Context, q store.KNNQuery) ([]store.VecRow, error) {
	blob, err := sqlite_vec.SerializeFloat32(q.Vec)
	if err != nil {
		return nil, fmt.Errorf("serialize query vector: %w", err)
	}
	k := q.K
	if q.Tag != "" && q.OverFetch > k {
		k = q.OverFetch
	}
	// vec0 wants k as a literal in the KNN constraint; it is an int, never user text.
	rows, err := s.db.QueryContext(ctx, fmt.Sprintf(`
		SELECT chunk_id, distance
		FROM chunks_vec
		WHERE embedding MATCH ? AND k = %d`, k), blob)
	if err != nil {
		return nil, err
	}
	var out []store.VecRow
	for rows.Next() {
		var r store.VecRow
		if err := rows.Scan(&r.ChunkID, &r.Distance); err != nil {
			rows.Close()
			return nil, err
		}
		out = append(out, r)
	}
	err = rows.Err()
	rows.Close()
	if err != nil {
		return nil, err
	}
	if q.Tag != "" {
		if out, err = s.filterByTag(ctx, out, q.Tag); err != nil {
			return nil, err
		}
	}
	if len(out) > q.K {
		out = out[:q.K]
	}
	return out, nil
}

// filterByTag keeps the rows whose entry carries the tag (normalised, as FTS matches it), in the
// order they came back.
func (s *Store) filterByTag(ctx context.Context, rows []store.VecRow, tag string) ([]store.VecRow, error) {
	if len(rows) == 0 {
		return rows, nil
	}
	args := make([]any, 0, len(rows)+1)
	args = append(args, store.NormTag(tag))
	for _, r := range rows {
		args = append(args, r.ChunkID)
	}
	res, err := s.db.QueryContext(ctx, `SELECT c.id FROM chunks c JOIN entry_tags t ON t.entry_id = c.entry_id
		WHERE t.tag = ? AND c.id IN (`+placeholders(len(rows))+`)`, args...)
	if err != nil {
		return nil, fmt.Errorf("vector search tag filter: %w", err)
	}
	defer res.Close()
	keep := make(map[int64]bool, len(rows))
	for res.Next() {
		var id int64
		if err := res.Scan(&id); err != nil {
			return nil, fmt.Errorf("vector search tag filter: %w", err)
		}
		keep[id] = true
	}
	if err := res.Err(); err != nil {
		return nil, fmt.Errorf("vector search tag filter: %w", err)
	}
	out := rows[:0:0]
	for _, r := range rows {
		if keep[r.ChunkID] {
			out = append(out, r)
		}
	}
	return out, nil
}

// ChunkMeta reads the display data and position of the given chunks in one query. Ids can dangle
// if the index was rebuilt mid-search; those are absent from the map.
func (s *Store) ChunkMeta(ctx context.Context, ids []int64) (map[int64]store.ChunkMeta, error) {
	out := make(map[int64]store.ChunkMeta, len(ids))
	if len(ids) == 0 {
		return out, nil
	}
	args := make([]any, len(ids))
	for i, id := range ids {
		args[i] = id
	}
	rows, err := s.db.QueryContext(ctx, `SELECT c.id, e.path, c.heading, c.text, e.id, COALESCE(e.updated, ''), c.ord, c.source_file
		FROM chunks c JOIN entries e ON e.id = c.entry_id
		WHERE c.id IN (`+placeholders(len(args))+`)`, args...)
	if err != nil {
		return nil, wrap(ctx, err, s.path, "sqlite.ChunkMeta", "could not read the matching chunks")
	}
	defer rows.Close()
	for rows.Next() {
		var id int64
		var m store.ChunkMeta
		if err := rows.Scan(&id, &m.EntryPath, &m.Heading, &m.Text, &m.EntryID, &m.EntryUpdated, &m.Ord, &m.SourceFile); err != nil {
			return nil, wrap(ctx, err, s.path, "sqlite.ChunkMeta", "could not read the matching chunks")
		}
		out[id] = m
	}
	if err := rows.Err(); err != nil {
		return nil, wrap(ctx, err, s.path, "sqlite.ChunkMeta", "could not read the matching chunks")
	}
	return out, nil
}

// placeholders renders "?,?,?" for an IN clause.
func placeholders(n int) string {
	return strings.TrimSuffix(strings.Repeat("?,", n), ",")
}

// ftsBaseRune reports whether r can carry a token on its own: a letter, number, private-use or
// unassigned code point (unicode61 keeps unassigned ones inside tokens). A field with none of
// these tokenizes to nothing; quoted it would be an empty phrase that empties the AND query.
func ftsBaseRune(r rune) bool {
	if unicode.Is(ftsSkewNotToken, r) {
		return false
	}
	return unicode.IsLetter(r) || unicode.IsNumber(r) || unicode.Is(unicode.Co, r) || unicode.Is(unicode.Cn, r)
}

// ftsSkewNotToken lists code points Go's Unicode tables call letters but the bundled SQLite's
// unicode61 tokenizer does not. TestBaseRuneMatchesSQLite recomputes it against the linked SQLite.
var ftsSkewNotToken = &unicode.RangeTable{
	R16: []unicode.Range16{
		{Lo: 0x19B0, Hi: 0x19C0, Stride: 1},
		{Lo: 0x19C8, Hi: 0x19C9, Stride: 1},
		{Lo: 0x1CF2, Hi: 0x1CF3, Stride: 1},
	},
}

// ftsOperatorWords are bare FTS5 operators; a field that is only one of them is dropped like a
// stopword, so `NOT a` searches for "a".
var ftsOperatorWords = map[string]bool{"and": true, "or": true, "not": true, "near": true}

// quoteTerm turns one whitespace-free user field into a quoted FTS5 phrase, or "" when it would
// match nothing. Inside quotes every character but the quote is plain text that FTS5 splits with
// the same unicode61 tokenizer that indexed the documents, so `draw-visual` matches the adjacent
// tokens draw and visual and `set -e` cannot reach the query parser. A quote or NUL becomes a
// space, the prefix star is deleted, and a bare operator word or a field with no base rune is
// dropped.
func quoteTerm(field string) string {
	cleaned := strings.TrimSpace(strings.Map(func(r rune) rune {
		switch r {
		case '*':
			return -1
		case '"', 0:
			return ' '
		}
		return r
	}, field))
	if !strings.ContainsFunc(cleaned, ftsBaseRune) {
		return ""
	}
	words := strings.FieldsFunc(cleaned, func(r rune) bool { return !ftsBaseRune(r) && !unicode.IsMark(r) })
	if len(words) == 1 && ftsOperatorWords[strings.ToLower(words[0])] {
		return ""
	}
	return `"` + cleaned + `"`
}

// matchFor is the MATCH expression of q: the phrase or NEAR form for OpPhrase and OpNear (which
// ignore q.Alts: a tier of the literal words), else the AND/OR form.
func matchFor(q store.FTSQuery) string {
	switch q.Op {
	case store.OpPhrase:
		return MatchPhrase(q.Terms)
	case store.OpNear:
		return MatchNear(q.Terms, orDefault(q.Near, store.NearDistance))
	}
	return MatchExprAlts(q.Terms, q.Op, q.Alts)
}

// orDefault is v, or def when v is 0.
func orDefault(v, def int) int {
	if v == 0 {
		return def
	}
	return v
}

// MatchPhrase is the MATCH expression that finds the terms as one phrase, in order and adjacent:
// every surviving field's words inside a single pair of quotes, `"a b c"`. "" unless two or more
// fields survive sanitising (one field is the AND query's own phrase, so there is no tier to
// add). A field such as `draw-visual` contributes its words as it does in the AND query.
func MatchPhrase(terms []string) string {
	inner := survivingPhrases(terms)
	if len(inner) < 2 {
		return ""
	}
	return `"` + strings.Join(inner, " ") + `"`
}

// MatchNear is the MATCH expression that finds the terms within distance tokens of each other, in
// any order: `NEAR("a" "b", 8)`, each surviving field one phrase. "" unless two or more fields
// survive sanitising or distance is below 1.
func MatchNear(terms []string, distance int) string {
	inner := survivingPhrases(terms)
	if len(inner) < 2 || distance < 1 {
		return ""
	}
	return "NEAR(" + `"` + strings.Join(inner, `" "`) + `"` + ", " + strconv.Itoa(distance) + ")"
}

// survivingPhrases is the sanitised text (without quotes) of every field of terms that survives
// quoteTerm, in order.
func survivingPhrases(terms []string) []string {
	var out []string
	for _, t := range terms {
		for _, field := range strings.Fields(t) {
			if q := quoteTerm(field); q != "" {
				out = append(out, q[1:len(q)-1])
			}
		}
	}
	return out
}

// MatchExpr is the FTS5 MATCH expression for terms joined by op: every surviving term quoted as one
// phrase. "" when no term survives, which means "run no FTS list".
func MatchExpr(terms []string, op store.FTSOp) string { return MatchExprAlts(terms, op, nil) }

// MatchExprAlts is MatchExpr where a field that is a key of alts also matches each alternative
// (itself first, alternatives that sanitise away or repeat a phrase dropped): ("a" OR "b"). A field
// that sanitises away stays dropped whatever its alternatives, so an expansion can only widen a
// query. A field with no surviving alternative renders as before; a nil alts gives MatchExpr.
func MatchExprAlts(terms []string, op store.FTSOp, alts map[string][]string) string {
	var phrases []string
	for _, t := range terms {
		for _, field := range strings.Fields(t) {
			q := quoteTerm(field)
			if q != "" {
				q = orGroup(q, alts[field])
			}
			if q != "" {
				phrases = append(phrases, q)
			}
		}
	}
	sep := " AND "
	if op == store.OpOr {
		sep = " OR "
	}
	return strings.Join(phrases, sep)
}

// orGroup joins the quoted phrase first and the quoted alternatives into one parenthesised OR, or
// first alone when no alternative survives.
func orGroup(first string, alts []string) string {
	parts := []string{first}
	for _, a := range alts {
		if q := quoteTerm(a); q != "" && !slices.Contains(parts, q) {
			parts = append(parts, q)
		}
	}
	if len(parts) == 1 {
		return first
	}
	return "(" + strings.Join(parts, " OR ") + ")"
}

// SanitizeFTS turns a raw user query into the AND and OR MATCH expressions search tries in turn.
// Both are "" when no field survives.
func SanitizeFTS(q string) (andQuery, orQuery string) {
	fields := strings.Fields(q)
	andQuery, orQuery = MatchExpr(fields, store.OpAnd), MatchExpr(fields, store.OpOr)
	if assert.Enabled {
		assert.Post((andQuery == "") == (orQuery == ""), "SanitizeFTS: and=%q or=%q: one empty, one not", andQuery, orQuery)
		assert.Post(strings.Count(andQuery, `"`)%2 == 0, "SanitizeFTS: odd number of quotes in %q", andQuery)
		assert.Post(!strings.Contains(andQuery, `""`), "SanitizeFTS: empty phrase in %q", andQuery)
	}
	return andQuery, orQuery
}
