// Package search is the hybrid searcher: an FTS5/BM25 list and a sqlite-vec KNN list fused with
// reciprocal rank fusion, capped to at most three chunks per entry, and logged to the searches,
// search_results and search_candidates tables.
//
// P3.2 (this file) is the search core: the FTS5 query sanitiser, the two lists, the RRF fusion,
// the diversity cap, the tag filter and the per-stage timings. It computes everything the log
// tables need and hands it back in an Outcome; P3.3 writes those rows and P3.4 wires the command.
// Nothing here touches the database except to read.
package search

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"sort"
	"strings"
	"time"
	"unicode"

	sqlite_vec "github.com/asg017/sqlite-vec-go-bindings/cgo"
	// sqlite-vec's cgo package compiles sqlite-vec.c, which calls sqlite3_malloc,
	// sqlite3_free, sqlite3_auto_extension and friends but does not provide them. Those symbols
	// come from the amalgamation inside mattn/go-sqlite3, so every binary that links this
	// package must link that driver too — internal/store's blank import does not help a binary
	// (like cmd/kb before P3.4 wires it) that reaches search without reaching store.
	_ "github.com/mattn/go-sqlite3"

	"knowledge/kb/internal/embed"
)

// Modes a search can run in. ModeFTSFallback is recorded when hybrid was asked for but the
// embedder was unreachable.
const (
	ModeHybrid      = "hybrid"
	ModeFTS         = "fts"
	ModeVec         = "vec"
	ModeFTSFallback = "fts-fallback"
)

// Defaults and limits from the CLI contract and the search algorithm.
const (
	DefaultK       = 8
	MinK           = 1
	MaxK           = 20
	ListLimit      = 20 // candidates taken from each list
	CandidateLimit = 40 // rows written to search_candidates
	PerEntryCap    = 3  // diversity cap on the returned list
	RRFConstant    = 60
)

// vecOverFetch is the k asked of vec0 when a tag filter is in play. vec0 cannot filter on a
// column of another table, so the KNN has to be run wide and narrowed afterwards; without the
// over-fetch a tag that matches few entries would come back nearly empty.
const vecOverFetch = 60

// ErrEmbedderUnavailable marks "the embedding service is not reachable" as opposed to "the
// embedding service said no". Hybrid searches degrade to fts-fallback on it and vec searches
// report it (CLI exit code 2). The embedder packages have their own sentinels — the Ollama client
// has ErrUnavailable — so a caller either wraps that error in this one or sets Searcher.
// IsUnavailable to recognise it.
var ErrEmbedderUnavailable = errors.New("embedder unavailable")

// Hit is one returned chunk. The JSON tags are the --json contract: the hits array of
// {search_id, query, mode, hits:[...]}. FTSRank and VecRank are null when the chunk appeared in
// only one of the two lists.
type Hit struct {
	Rank     int     `json:"rank"`
	RRFScore float64 `json:"rrf_score"`
	FTSRank  *int    `json:"fts_rank"`
	VecRank  *int    `json:"vec_rank"`
	Path     string  `json:"path"`
	Heading  string  `json:"heading"`
	Text     string  `json:"text"`
}

// Result is one whole search, shaped exactly like the --json output. Hits is never nil — an empty
// search returns an empty slice — so --json emits [] rather than null. NewResult guarantees that.
type Result struct {
	SearchID int64  `json:"search_id"`
	Query    string `json:"query"`
	Mode     string `json:"mode"`
	Hits     []Hit  `json:"hits"`
}

// NewResult starts an empty result for one query, with Hits initialised to the empty slice.
func NewResult(query, mode string) *Result {
	return &Result{Query: query, Mode: mode, Hits: []Hit{}}
}

// Request is one search as the CLI asked for it. Mode is one of the Mode constants, Caller comes
// from --caller / KB_CALLER / "unknown", and Tag is an optional tag filter.
type Request struct {
	Query  string
	Mode   string
	K      int
	Tag    string
	Caller string
}

// Searcher runs searches against one open database, using Embedder for the vector list. A nil
// Embedder means vector search is unavailable and hybrid degrades to fts-fallback.
type Searcher struct {
	DB       *sql.DB
	Embedder embed.Embedder
	// KBVersion is the repo's git short hash at search time, stored in searches.kb_version.
	KBVersion string
	// IsUnavailable optionally recognises an embedder error as "service not reachable" when it
	// does not already wrap ErrEmbedderUnavailable. The CLI points it at ollama.ErrUnavailable.
	IsUnavailable func(error) bool
}

// Candidate is one chunk in the fused list. FTSRank/FTSScore are nil when the chunk was not in
// the FTS list and VecRank/VecDistance are nil when it was not in the vector list; at least one
// pair is always set. RRFScore is the reciprocal-rank-fusion score the list is sorted by. The
// path/heading/text fields are filled in once the candidates are looked up in the database, so
// P3.3 can write search_candidates without re-reading anything.
type Candidate struct {
	ChunkID     int64
	FTSRank     *int
	FTSScore    *float64
	VecRank     *int
	VecDistance *float64
	RRFScore    float64

	EntryPath string
	Heading   string
	Text      string
}

// Outcome is everything one search produced: the caller-facing Result plus the full candidate
// list, the per-stage timings and the counts that searches/search_results/search_candidates need.
// Mode is the mode that actually ran, which is ModeFTSFallback when a hybrid search lost its
// embedder; in that case FallbackErr says why and Search still returns a nil error.
type Outcome struct {
	Result     *Result
	Candidates []Candidate // the fused list, top CandidateLimit, in fused order
	Returned   []Candidate // the subset that survived the diversity cap and k (returned=1)
	Mode       string

	NFTS int
	NVec int

	FTSMs   int64
	VecMs   int64
	EmbedMs int64
	TotalMs int64

	EmbedModel  string
	TagFilter   string
	K           int
	FallbackErr error
}

// FTSRow is one row of the BM25 list, in rank order. Score is SQLite's bm25(), which is negative
// and lower-is-better; it is stored verbatim in search_results.fts_score. It is exported only so
// that Fuse, which is a pure function worth testing and reusing on its own, is callable.
type FTSRow struct {
	ChunkID int64
	Score   float64
}

// VecRow is one row of the vec0 KNN list, in rank (ascending distance) order.
type VecRow struct {
	ChunkID  int64
	Distance float64
}

// chunkMeta is the display data for one candidate chunk.
type chunkMeta struct {
	EntryPath string
	Heading   string
	Text      string
}

// ftsBaseRune reports whether r can carry a token on its own: a letter, number, private-use or
// unassigned code point (unicode61 keeps unassigned ones, such as U+0378, inside tokens). A field
// with none of these, like `&&`, `...` or a lone combining mark, tokenizes to nothing; quoted, it
// becomes a phrase with no tokens, which matches nothing and so empties the whole AND query.
func ftsBaseRune(r rune) bool {
	if unicode.Is(ftsSkewNotToken, r) {
		return false
	}
	return unicode.IsLetter(r) || unicode.IsNumber(r) || unicode.Is(unicode.Co, r) || unicode.Is(unicode.Cn, r)
}

// ftsSkewNotToken lists the code points Go's Unicode 15 tables call letters but the unicode61
// tokenizer of the bundled SQLite (3.53.4) does not: New Tai Lue vowel signs and two Vedic signs
// that were combining marks in older Unicode. A field made only of them tokenizes to nothing.
// TestBaseRuneMatchesSQLite recomputes the list against the linked SQLite, so an upgrade that
// changes it fails there.
var ftsSkewNotToken = &unicode.RangeTable{
	R16: []unicode.Range16{
		{Lo: 0x19B0, Hi: 0x19C0, Stride: 1},
		{Lo: 0x19C8, Hi: 0x19C9, Stride: 1},
		{Lo: 0x1CF2, Hi: 0x1CF3, Stride: 1},
	},
}

// ftsOperatorWords are the bare words FTS5 treats as operators. Quoted they would parse as plain
// words, but a field that is only one of them is dropped anyway, like a stopword, so that `NOT a`
// searches for "a" rather than also requiring the literal word "not".
var ftsOperatorWords = map[string]bool{"and": true, "or": true, "not": true, "near": true}

// SanitizeFTS turns a raw user query into two FTS5 MATCH expressions: every surviving
// whitespace-separated field quoted as one phrase, joined with AND for the precise attempt and
// with OR for the retry. Both are "" when no field survives, which the caller reads as "run no
// FTS list at all".
//
// Inside a quoted FTS5 string every character but the quote itself is plain text, and FTS5 splits
// it with the same unicode61 tokenizer that indexed the documents. So the sanitiser leaves the
// tokenizing to FTS5 rather than copying its rules: `draw-visual` becomes the phrase
// "draw-visual", which matches the adjacent tokens draw and visual exactly as the document was
// split, case is folded the same way on both sides, and syntax such as `set -e` or `foo:bar`
// cannot reach the query parser. Copies of those rules in Go went wrong three ways: deleting the
// hyphen searched for the nonexistent token "drawvisual", Go's ToLower folds letters SQLite's
// older tables do not (WYNN, DCHE), and Go's newer tables call U+061D punctuation where SQLite
// keeps it inside a token.
//
// What the sanitiser still does: a double quote or NUL becomes a space (one would end the string,
// the other truncates it); the prefix star is deleted, so `post*gres` reads as postgres; and a
// field is dropped when it is a bare operator word or holds no base rune.
func SanitizeFTS(q string) (andQuery, orQuery string) {
	var tokens []string
	for _, field := range strings.Fields(q) {
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
			continue
		}
		words := strings.FieldsFunc(cleaned, func(r rune) bool { return !ftsBaseRune(r) && !unicode.IsMark(r) })
		if len(words) == 1 && ftsOperatorWords[strings.ToLower(words[0])] {
			continue
		}
		tokens = append(tokens, `"`+cleaned+`"`)
	}
	if len(tokens) == 0 {
		return "", ""
	}
	return strings.Join(tokens, " AND "), strings.Join(tokens, " OR ")
}

// tagEscaper neutralises the characters LIKE treats as syntax. Without it a tag of "%" matches
// every entry (the filter silently does nothing) and "docke_" matches "docker", because _ is
// LIKE's single-character wildcard. The chosen escape character is backslash, which every LIKE
// using this pattern must declare with ESCAPE '\'.
var tagEscaper = strings.NewReplacer(`\`, `\\`, `%`, `\%`, `_`, `\_`)

// tagPattern is the LIKE pattern that finds a tag inside the JSON array stored in entries.tags.
// The quotes around the tag are what keep "docker" from matching "docker-compose"; the escaping
// is what keeps a wildcard in the tag itself from matching anything at all. Pair it with
// ESCAPE '\' in the query.
func tagPattern(tag string) string { return `%"` + tagEscaper.Replace(tag) + `"%` }

// ftsList runs the BM25 list. The AND query is tried first; if it matches nothing — one unknown
// word in an otherwise good query is enough — the OR query is tried, which is the "retry joined
// with OR" step of the search algorithm. Returns at most ListLimit rows in rank order.
//
// bm25 weights are text 1.0, heading 2.0, title 3.0, matching the three columns of chunks_fts.
// The value is negative and lower is better, so ORDER BY ascending is correct, and it is carried
// into fts_score verbatim.
//
// chunks_fts is contentless (an empty content= option plus contentless_delete=1), so it is
// write-and-MATCH only: nothing but rowid and bm25() can be selected from it, and it has no
// path or tags column to filter on. The tag filter therefore joins out to entries and tests the
// JSON array there. Doing it in the query rather than as a post-filter is what keeps the LIMIT
// full.
func (s *Searcher) ftsList(ctx context.Context, andQuery, orQuery, tag string) ([]FTSRow, int64, error) {
	start := time.Now()
	if andQuery == "" {
		return nil, time.Since(start).Milliseconds(), nil
	}

	query := fmt.Sprintf(`
		SELECT rowid, bm25(chunks_fts, 1.0, 2.0, 3.0) AS s
		FROM chunks_fts
		WHERE chunks_fts MATCH ?
		ORDER BY s
		LIMIT %d`, ListLimit)
	args := []any{andQuery}
	if tag != "" {
		query = fmt.Sprintf(`
			SELECT f.rowid, bm25(chunks_fts, 1.0, 2.0, 3.0) AS s
			FROM chunks_fts f
			JOIN chunks c ON c.id = f.rowid
			JOIN entries e ON e.id = c.entry_id
			WHERE chunks_fts MATCH ? AND e.tags LIKE ? ESCAPE '\'
			ORDER BY s
			LIMIT %d`, ListLimit)
		args = []any{andQuery, tagPattern(tag)}
	}

	rows, err := s.runFTS(ctx, query, args)
	if err != nil {
		return nil, time.Since(start).Milliseconds(), err
	}
	if len(rows) == 0 && orQuery != "" && orQuery != andQuery {
		args[0] = orQuery
		rows, err = s.runFTS(ctx, query, args)
		if err != nil {
			return nil, time.Since(start).Milliseconds(), err
		}
	}
	return rows, time.Since(start).Milliseconds(), nil
}

// runFTS executes one MATCH and collects the rows.
func (s *Searcher) runFTS(ctx context.Context, query string, args []any) ([]FTSRow, error) {
	rows, err := s.DB.QueryContext(ctx, query, args...)
	if err != nil {
		return nil, fmt.Errorf("fts search: %w", err)
	}
	defer rows.Close()

	var out []FTSRow
	for rows.Next() {
		var r FTSRow
		if err := rows.Scan(&r.ChunkID, &r.Score); err != nil {
			return nil, fmt.Errorf("fts search: %w", err)
		}
		out = append(out, r)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("fts search: %w", err)
	}
	return out, nil
}

// vecList runs the sqlite-vec KNN list. The KNN itself happens inside SQLite (no vectors are
// pulled into the process); a tag filter cannot be pushed into vec0, so the query over-fetches to
// vecOverFetch and the tag is applied afterwards by joining chunks to entries, keeping the first
// ListLimit survivors in distance order.
func (s *Searcher) vecList(ctx context.Context, qvec []float32, tag string) ([]VecRow, int64, error) {
	start := time.Now()
	if len(qvec) == 0 {
		return nil, time.Since(start).Milliseconds(), nil
	}
	blob, err := sqlite_vec.SerializeFloat32(qvec)
	if err != nil {
		return nil, time.Since(start).Milliseconds(), fmt.Errorf("vector search: serialize query vector: %w", err)
	}

	k := ListLimit
	if tag != "" {
		k = vecOverFetch
	}
	// k must be a literal in the vec0 KNN constraint, so it is formatted in from a constant.
	query := fmt.Sprintf(`
		SELECT chunk_id, distance
		FROM chunks_vec
		WHERE embedding MATCH ? AND k = %d`, k)

	rows, err := s.DB.QueryContext(ctx, query, blob)
	if err != nil {
		return nil, time.Since(start).Milliseconds(), fmt.Errorf("vector search: %w", err)
	}
	var out []VecRow
	for rows.Next() {
		var r VecRow
		if err := rows.Scan(&r.ChunkID, &r.Distance); err != nil {
			rows.Close()
			return nil, time.Since(start).Milliseconds(), fmt.Errorf("vector search: %w", err)
		}
		out = append(out, r)
	}
	if err := rows.Err(); err != nil {
		rows.Close()
		return nil, time.Since(start).Milliseconds(), fmt.Errorf("vector search: %w", err)
	}
	rows.Close()

	if tag != "" {
		out, err = s.filterByTag(ctx, out, tag)
		if err != nil {
			return nil, time.Since(start).Milliseconds(), err
		}
	}
	if len(out) > ListLimit {
		out = out[:ListLimit]
	}
	return out, time.Since(start).Milliseconds(), nil
}

// filterByTag keeps the rows whose entry carries the tag, in the order they came back.
func (s *Searcher) filterByTag(ctx context.Context, rows []VecRow, tag string) ([]VecRow, error) {
	if len(rows) == 0 {
		return rows, nil
	}
	ids := make([]int64, len(rows))
	for i, r := range rows {
		ids[i] = r.ChunkID
	}
	query := `SELECT c.id FROM chunks c JOIN entries e ON e.id = c.entry_id
		WHERE e.tags LIKE ? ESCAPE '\' AND c.id IN (` + placeholders(len(ids)) + `)`
	args := make([]any, 0, len(ids)+1)
	args = append(args, tagPattern(tag))
	for _, id := range ids {
		args = append(args, id)
	}

	res, err := s.DB.QueryContext(ctx, query, args...)
	if err != nil {
		return nil, fmt.Errorf("vector search tag filter: %w", err)
	}
	defer res.Close()
	keep := make(map[int64]bool, len(ids))
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

// Fuse merges the two lists with reciprocal rank fusion: a chunk scores 1/(RRFConstant+rank) for
// every list it appears in, rank starting at 1. The result is sorted by score descending and
// truncated to CandidateLimit.
//
// Ties are broken by FTS rank, then vector rank, then chunk id, all ascending, so the same two
// lists always fuse to the same order — the log tables are only comparable across runs if the
// ordering is total. A chunk missing from a list sorts after one that has a rank there.
func Fuse(fts []FTSRow, vec []VecRow) []Candidate {
	byChunk := make(map[int64]*Candidate, len(fts)+len(vec))
	order := make([]int64, 0, len(fts)+len(vec))

	get := func(id int64) *Candidate {
		c, ok := byChunk[id]
		if !ok {
			c = &Candidate{ChunkID: id}
			byChunk[id] = c
			order = append(order, id)
		}
		return c
	}

	for i, r := range fts {
		rank := i + 1
		c := get(r.ChunkID)
		score := r.Score
		c.FTSRank = &rank
		c.FTSScore = &score
		c.RRFScore += 1 / float64(RRFConstant+rank)
	}
	for i, r := range vec {
		rank := i + 1
		c := get(r.ChunkID)
		distance := r.Distance
		c.VecRank = &rank
		c.VecDistance = &distance
		c.RRFScore += 1 / float64(RRFConstant+rank)
	}

	out := make([]Candidate, 0, len(order))
	for _, id := range order {
		out = append(out, *byChunk[id])
	}
	sort.Slice(out, func(i, j int) bool {
		a, b := out[i], out[j]
		if a.RRFScore != b.RRFScore {
			return a.RRFScore > b.RRFScore
		}
		if ra, rb := rankOrMax(a.FTSRank), rankOrMax(b.FTSRank); ra != rb {
			return ra < rb
		}
		if ra, rb := rankOrMax(a.VecRank), rankOrMax(b.VecRank); ra != rb {
			return ra < rb
		}
		return a.ChunkID < b.ChunkID
	})
	if len(out) > CandidateLimit {
		out = out[:CandidateLimit]
	}
	return out
}

// rankOrMax sorts a missing rank after every present one.
func rankOrMax(p *int) int {
	if p == nil {
		return int(^uint(0) >> 1)
	}
	return *p
}

// applyCap is the diversity cap: walking the fused list in order, a chunk is skipped once its
// entry already holds PerEntryCap places in the returned list, and the walk stops at k. The
// candidate list itself is unaffected — that is the point, so recall analysis can see what the
// cap dropped. entryOf maps chunk id to entry path; a chunk missing from it is skipped, since
// without an entry it can neither be capped nor displayed.
func applyCap(cands []Candidate, entryOf map[int64]string, k int) []Candidate {
	out := make([]Candidate, 0, k)
	perEntry := make(map[string]int)
	for _, c := range cands {
		if len(out) >= k {
			break
		}
		path, ok := entryOf[c.ChunkID]
		if !ok {
			continue
		}
		if perEntry[path] >= PerEntryCap {
			continue
		}
		perEntry[path]++
		out = append(out, c)
	}
	return out
}

// loadChunkMeta reads the entry path, heading and text of every candidate chunk in one query.
// Chunk ids can dangle if the index was rebuilt mid-search; those are simply absent from the map.
func (s *Searcher) loadChunkMeta(ctx context.Context, cands []Candidate) (map[int64]chunkMeta, error) {
	out := make(map[int64]chunkMeta, len(cands))
	if len(cands) == 0 {
		return out, nil
	}
	args := make([]any, len(cands))
	for i, c := range cands {
		args[i] = c.ChunkID
	}
	query := `SELECT c.id, e.path, c.heading, c.text
		FROM chunks c JOIN entries e ON e.id = c.entry_id
		WHERE c.id IN (` + placeholders(len(args)) + `)`

	rows, err := s.DB.QueryContext(ctx, query, args...)
	if err != nil {
		return nil, fmt.Errorf("read candidate chunks: %w", err)
	}
	defer rows.Close()
	for rows.Next() {
		var id int64
		var m chunkMeta
		if err := rows.Scan(&id, &m.EntryPath, &m.Heading, &m.Text); err != nil {
			return nil, fmt.Errorf("read candidate chunks: %w", err)
		}
		out[id] = m
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("read candidate chunks: %w", err)
	}
	return out, nil
}

// placeholders renders "?, ?, ?" for an IN clause.
func placeholders(n int) string {
	return strings.TrimSuffix(strings.Repeat("?,", n), ",")
}

// Search runs one search and reports everything it produced, including the numbers P3.3 writes to
// the log tables. It does not write anything itself.
//
// Mode handling: hybrid runs both lists; fts and vec run one. A hybrid search whose embedder is
// missing or unreachable degrades to FTS only, reports Mode ModeFTSFallback and puts the reason
// in Outcome.FallbackErr — it is not an error, because a usable answer was produced. A vec search
// in the same situation returns the error, since there is nothing to fall back to.
//
// Empty queries: a blank query (empty or whitespace only) is an error in every mode, checked
// before any list runs. It is not a cheap "no results" — the vector list would happily embed the
// bare "search_query: " prefix and return k arbitrary chunks, which then get written to the
// search log as if someone had asked for them. A query that is not blank but sanitises to no FTS
// tokens, such as "(((  )))", is a different case and is allowed: the FTS list is skipped and the
// vector list still runs, because the user did type something the embedder can read.
func (s *Searcher) Search(ctx context.Context, req Request) (*Outcome, error) {
	start := time.Now()

	if strings.TrimSpace(req.Query) == "" {
		return nil, fmt.Errorf("search: empty query")
	}

	mode := req.Mode
	if mode == "" {
		mode = ModeHybrid
	}
	switch mode {
	case ModeHybrid, ModeFTS, ModeVec:
	default:
		return nil, fmt.Errorf("search: unknown mode %q (want hybrid, fts or vec)", req.Mode)
	}
	k := ClampK(req.K)

	out := &Outcome{
		Mode:       mode,
		TagFilter:  req.Tag,
		K:          k,
		Candidates: []Candidate{},
		Returned:   []Candidate{},
	}

	// Vector side first: whether it works decides the mode that is finally recorded.
	var vec []VecRow
	if mode == ModeHybrid || mode == ModeVec {
		qvec, embedMs, err := s.embedQuery(ctx, req.Query)
		out.EmbedMs = embedMs
		if err != nil {
			if !s.unavailable(err) || mode == ModeVec {
				return nil, err
			}
			out.Mode = ModeFTSFallback
			out.FallbackErr = err
		} else {
			out.EmbedModel = s.Embedder.Model()
			vecRows, vecMs, err := s.vecList(ctx, qvec, req.Tag)
			out.VecMs = vecMs
			if err != nil {
				return nil, err
			}
			vec = vecRows
		}
	}

	var fts []FTSRow
	if out.Mode != ModeVec {
		andQuery, orQuery := SanitizeFTS(req.Query)
		ftsRows, ftsMs, err := s.ftsList(ctx, andQuery, orQuery, req.Tag)
		out.FTSMs = ftsMs
		if err != nil {
			return nil, err
		}
		fts = ftsRows
	}

	out.NFTS = len(fts)
	out.NVec = len(vec)

	cands := Fuse(fts, vec)
	meta, err := s.loadChunkMeta(ctx, cands)
	if err != nil {
		return nil, err
	}
	entryOf := make(map[int64]string, len(meta))
	for id, m := range meta {
		entryOf[id] = m.EntryPath
	}
	for i := range cands {
		if m, ok := meta[cands[i].ChunkID]; ok {
			cands[i].EntryPath = m.EntryPath
			cands[i].Heading = m.Heading
			cands[i].Text = m.Text
		}
	}
	if len(cands) > 0 {
		out.Candidates = cands
	}

	returned := applyCap(cands, entryOf, k)
	if len(returned) > 0 {
		out.Returned = returned
	}

	result := NewResult(req.Query, out.Mode)
	for i, c := range returned {
		result.Hits = append(result.Hits, Hit{
			Rank:     i + 1,
			RRFScore: c.RRFScore,
			FTSRank:  c.FTSRank,
			VecRank:  c.VecRank,
			Path:     c.EntryPath,
			Heading:  c.Heading,
			Text:     c.Text,
		})
	}
	out.Result = result
	out.TotalMs = time.Since(start).Milliseconds()
	return out, nil
}

// embedQuery embeds the query with the search_query prefix nomic-embed-text expects, timing the
// call on its own because it is by far the slowest stage (~0.4 s warm) and the log separates it
// from the two SQLite stages. A nil Embedder is reported as ErrEmbedderUnavailable so callers
// need only one check.
func (s *Searcher) embedQuery(ctx context.Context, query string) ([]float32, int64, error) {
	if s.Embedder == nil {
		return nil, 0, fmt.Errorf("no embedder is configured: %w", ErrEmbedderUnavailable)
	}
	start := time.Now()
	vecs, err := s.Embedder.Embed(ctx, []string{embed.QueryPrefix + query})
	ms := time.Since(start).Milliseconds()
	if err != nil {
		return nil, ms, fmt.Errorf("embed query: %w", err)
	}
	if len(vecs) != 1 || len(vecs[0]) == 0 {
		return nil, ms, fmt.Errorf("embed query: embedder returned %d vectors, want 1", len(vecs))
	}
	return vecs[0], ms, nil
}

// unavailable reports whether an embedder error means "service not reachable", either because it
// wraps ErrEmbedderUnavailable or because the caller's IsUnavailable hook recognises it.
func (s *Searcher) unavailable(err error) bool {
	if errors.Is(err, ErrEmbedderUnavailable) {
		return true
	}
	return s.IsUnavailable != nil && s.IsUnavailable(err)
}

// ClampK forces k into the documented 1..20 range. DefaultK is the flag's default value, not a
// fallback applied here: an explicit -k 0 clamps up to MinK like any other out-of-range number.
func ClampK(k int) int {
	if k < MinK {
		return MinK
	}
	if k > MaxK {
		return MaxK
	}
	return k
}
