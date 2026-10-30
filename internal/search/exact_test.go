package search

import (
	"context"
	"database/sql"
	"fmt"
	"math/rand"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strings"
	"testing"
	"unicode/utf8"

	"github.com/nickstrad/kb/internal/config"
	"github.com/nickstrad/kb/internal/store"
	"github.com/nickstrad/kb/internal/store/sqlite"
)

// TestExactTerms: which fields of a query are identifiers, and how they are cleaned.
func TestExactTerms(t *testing.T) {
	for _, tc := range []struct {
		query string
		want  []string
	}{
		{"rotate tmux logs", nil},
		{"foo_bar", []string{"foo_bar"}},
		{"how does reindex_test reuse vectors", []string{"reindex_test"}},
		{"--embed-model flag", []string{"--embed-model"}},
		{"set -e", []string{"-e"}},
		{"kb.toml", []string{"kb.toml"}},
		{"edit kb.toml.", []string{"kb.toml"}}, // sentence full stop
		{"(kb.toml),", []string{"kb.toml"}},    // surrounding punctuation
		{"a/b:c", []string{"a/b:c"}},           // path with a colon
		{"internal/store/sqlite/retrieve.go", []string{"internal/store/sqlite/retrieve.go"}},
		{"see: EnsureEmbedMeta", []string{"EnsureEmbedMeta"}}, // camelCase; "see:" trims to "see"
		{"HTTPServer", nil},                                   // no lower-then-upper
		{"v1beta 2nd", []string{"v1beta", "2nd"}},             // digit then lower case
		{"v2", nil},              // letter then digit only
		{"logs.", nil},           // a sentence end is not an identifier
		{"-- ... && :: //", nil}, // no letter or digit
		{"foo_bar FOO_BAR foo_bar", []string{"foo_bar", "FOO_BAR"}}, // duplicates dropped; case kept
		{"café_au_lait naïve", []string{"café_au_lait"}},
		{`"pkg.Func"`, []string{"pkg.Func"}},
		{".0!.", []string{".0"}}, // trimmed to a fixed point (FuzzExactTerms)
		// The ASCII edges of the camelCase and digit-then-lower tests.
		{"aA zZ", []string{"aA", "zZ"}},
		{"a@ z[ `A {A", nil}, // '@' '[' are beside A-Z; '`' '{' beside a-z
		{"0a 9z", []string{"0a", "9z"}},
		{"/a :a", []string{"/a", ":a"}}, // '/' and ':' are beside 0-9, and tokenchars
		{"x/ x:", []string{"x/"}},       // a trailing ':' is trimmed; '/' is kept
		{"", nil},
	} {
		if got := ExactTerms(tc.query); !slices.Equal(got, tc.want) {
			t.Errorf("ExactTerms(%q) = %q, want %q", tc.query, got, tc.want)
		}
	}
}

// identRe is the PLAN's identifier test.
var identRe = regexp.MustCompile(`[_./:-]|[a-z][A-Z]|\d[a-z]`)

// FuzzExactTerms: every term is a whitespace-free substring of the query that passes the PLAN's
// identifier regexp, holds a letter or digit, does not end in '.' or ':' and appears once; the
// terms of the terms are the terms (idempotent). The committed crasher `.0!.` is the one-pass trim
// that left `.0!` (a trailing `!` exposed by removing the `.`), fixed by trimIdentifier.
//
// Run: go test -tags fts5 ./internal/search -run '^$' -fuzz '^FuzzExactTerms$' -fuzztime 15s
func FuzzExactTerms(f *testing.F) {
	for _, seed := range []string{"foo_bar", "--embed-model kb.toml", "a/b:c. (x.y),", "EnsureEmbedMeta", "v1beta 2nd v2", "café_x", "", "-- ::", "\x00_a", ".0!."} {
		f.Add(seed)
	}
	f.Fuzz(func(t *testing.T, q string) {
		terms := ExactTerms(q)
		seen := map[string]bool{}
		for _, term := range terms {
			switch {
			case term == "" || strings.ContainsFunc(term, func(r rune) bool { return strings.ContainsRune(" \t\n\r\v\f\u0085\u00a0", r) }):
				t.Fatalf("ExactTerms(%q): term %q is empty or has whitespace", q, term)
			case !strings.Contains(q, term):
				t.Fatalf("ExactTerms(%q): term %q is not in the query", q, term)
			case !identRe.MatchString(term):
				t.Fatalf("ExactTerms(%q): term %q fails the identifier regexp", q, term)
			case strings.HasSuffix(term, ".") || strings.HasSuffix(term, ":"):
				t.Fatalf("ExactTerms(%q): term %q ends in sentence punctuation", q, term)
			case seen[term]:
				t.Fatalf("ExactTerms(%q): term %q twice", q, term)
			}
			seen[term] = true
		}
		if again := ExactTerms(strings.Join(terms, " ")); !slices.Equal(again, terms) {
			t.Fatalf("ExactTerms(%q) = %q, but ExactTerms of those = %q", q, terms, again)
		}
	})
}

// identCorpus is a small corpus where each target identifier sits verbatim in one chunk while its
// words, split apart and repeated, fill another: the main FTS list ranks the split chunk at least
// as high, and only the exact list tells them apart.
func identCorpus() []testEntry {
	return append(fixtures(),
		testEntry{path: "reindex.md", title: "Reindex", chunks: []string{
			"Reindex: rebuild the index",
			"the reindex test suite runs a reindex test for every test helper",
			"reindex_test covers the vector reuse path",
		}},
		testEntry{path: "flags.md", title: "Flags", chunks: []string{
			"Flags: command line flags",
			"the embed model flag picks the embed model, model by model",
			"--embed-model picks the model",
		}},
		testEntry{path: "config.md", title: "Config", chunks: []string{
			"Config: kb settings",
			"the kb toml file: toml for kb, kb in toml",
			"kb.toml holds the search keys",
		}},
		testEntry{path: "paths.md", title: "Paths", chunks: []string{
			"Paths: module layout",
			"internal store sqlite retrieve: the store retrieves from sqlite internal",
			"internal/store/sqlite/retrieve.go builds the FTS statement",
		}},
	)
}

// identQueries pairs each identifier query with the chunk text that holds it verbatim.
var identQueries = []struct{ query, target string }{
	{"reindex_test", "reindex_test covers the vector reuse path"},
	{"--embed-model", "--embed-model picks the model"},
	{"kb.toml", "kb.toml holds the search keys"},
	{"internal/store/sqlite/retrieve.go", "internal/store/sqlite/retrieve.go builds the FTS statement"},
	{"how is reindex_test run", "reindex_test covers the vector reuse path"},
}

// TestExactListImproves is the evidence for exact_list = true by default: on a corpus where the
// identifier's split words compete, every identifier query ranks the chunk holding it verbatim
// first with the exact list and never lower than without it, in hybrid and fts mode. The ranked
// output of both settings is pinned in testdata/golden/search_exact.golden (-update).
func TestExactListImproves(t *testing.T) {
	s := indexedSearcher(t, identCorpus())
	on, err := NewSettings(config.Default())
	if err != nil {
		t.Fatal(err)
	}
	offCfg := config.Default()
	offCfg.Search.ExactList = false
	off, err := NewSettings(offCfg)
	if err != nil {
		t.Fatal(err)
	}
	var golden strings.Builder
	improved := 0
	for _, mode := range []string{ModeHybrid, ModeFTS} {
		for _, q := range identQueries {
			req := Request{Query: q.query, Mode: mode, K: DefaultK}
			rank := map[string]int{}
			for _, setting := range []struct {
				name string
				st   Settings
			}{{"on", on}, {"off", off}} {
				s.Settings = setting.st
				out, err := s.Search(context.Background(), req)
				if err != nil {
					t.Fatal(err)
				}
				fmt.Fprintf(&golden, "-- exact_list=%s\n%s", setting.name, renderOutcome(req, out))
				rank[setting.name] = len(out.Result.Hits) + 1 // absent ranks below every hit
				for _, h := range out.Result.Hits {
					if h.Text == q.target {
						rank[setting.name] = h.Rank
						break
					}
				}
			}
			if rank["on"] != 1 {
				t.Errorf("%s %q: the verbatim chunk is hit %d with the exact list, want 1", mode, q.query, rank["on"])
			}
			if rank["on"] > rank["off"] {
				t.Errorf("%s %q: the exact list moved the verbatim chunk down, %d -> %d", mode, q.query, rank["off"], rank["on"])
			}
			if rank["on"] < rank["off"] {
				improved++
			}
			t.Logf("%s %q: verbatim chunk at hit %d without the exact list, %d with it", mode, q.query, rank["off"], rank["on"])
		}
	}
	if improved == 0 {
		t.Error("the exact list improved no query: the corpus no longer tests it")
	}
	path := filepath.Join("testdata", "golden", "search_exact.golden")
	if *update {
		if err := os.WriteFile(path, []byte(golden.String()), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	want, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read golden (run with -update to create it): %v", err)
	}
	if golden.String() != string(want) {
		t.Errorf("ranked output differs from %s:\n%s", path, firstDiff(string(want), golden.String()))
	}
}

// TestExactListVerbatimRanksFirst is the property: an identifier present verbatim in exactly one
// chunk ranks that chunk #1 in the exact list, whatever other chunks hold its parts. Identifiers
// are generated from a fixed seed (printed on failure); distractor chunks hold the identifier's
// words split apart, repeated, and as a prefix of a longer identifier.
func TestExactListVerbatimRanksFirst(t *testing.T) {
	const seed = 4501
	rng := rand.New(rand.NewSource(seed))
	words := []string{"embed", "meta", "reindex", "test", "kb", "toml", "store", "sqlite", "flag", "model", "path", "func"}
	seps := []string{"_", ".", "/", ":", "-"}
	ctx := context.Background()
	s := indexedSearcher(t, fixtures())
	idx := s.Store.(store.Index)
	for i := 0; i < 40; i++ {
		n := 2 + rng.Intn(3)
		parts := make([]string, n)
		for j := range parts {
			parts[j] = words[rng.Intn(len(words))]
		}
		var b strings.Builder
		if rng.Intn(4) == 0 {
			b.WriteString("--")
		}
		for j, p := range parts {
			if j > 0 {
				b.WriteString(seps[rng.Intn(len(seps))])
			}
			b.WriteString(p)
		}
		ident := b.String()
		split := strings.Join(parts, " ")
		texts := []string{
			"split: " + split + " " + split + " " + split,
			"longer: " + ident + "_extra and " + ident + "x",
			"target: here is " + ident + " once",
			"words: " + strings.Join(parts, " and "),
		}
		chunks := make([]store.ChunkInput, len(texts))
		for j, text := range texts {
			chunks[j] = store.ChunkInput{Ord: j, SourceFile: "prop.md", Text: text, TextHash: fmt.Sprint(i, j), Embedding: make([]float32, 768)}
			chunks[j].Embedding[j] = 1
		}
		if _, err := idx.ReplaceEntry(ctx, store.EntryInput{Path: "prop.md", Kind: "file", Title: "Prop", Summary: "s", BodyHash: fmt.Sprint(i)}, chunks); err != nil {
			t.Fatal(err)
		}
		out, err := s.Search(ctx, Request{Query: "where is " + ident, Mode: ModeFTS, K: DefaultK})
		if err != nil {
			t.Fatal(err)
		}
		var first *Candidate
		for j := range out.Candidates {
			if c := &out.Candidates[j]; c.ExactRank != nil && *c.ExactRank == 1 {
				first = c
			}
		}
		if first == nil || first.Text != texts[2] {
			t.Fatalf("seed %d, case %d, ident %q: exact rank 1 is %+v, want the target chunk %q", seed, i, ident, first, texts[2])
		}
		if out.NExact != 1 {
			t.Errorf("seed %d, ident %q: %d exact rows, want only the target", seed, ident, out.NExact)
		}
	}
}

// TestCommonTermIsNeverDropped (coordinator report, W9): a term in all but one of N chunks has an
// idf near zero, so bm25 scores it about -1e-6, but every matching chunk still reaches the fused
// list and, within k and the per-entry cap, the hits. Spread over N entries and in one entry.
func TestCommonTermIsNeverDropped(t *testing.T) {
	for n := 2; n <= 4; n++ {
		for _, spread := range []bool{false, true} {
			var entries []testEntry
			texts := make([]string, n)
			for i := range texts {
				texts[i] = fmt.Sprintf("commonterm chunk %d", i)
			}
			texts[n-1] = "nothing here"
			if spread {
				for i, text := range texts {
					entries = append(entries, testEntry{path: fmt.Sprintf("e%d.md", i), title: fmt.Sprintf("E%d", i), chunks: []string{text}})
				}
			} else {
				entries = []testEntry{{path: "a.md", title: "A", chunks: texts}}
			}
			s := indexedSearcher(t, entries)
			for _, mode := range []string{ModeFTS, ModeHybrid} {
				out, err := s.Search(context.Background(), Request{Query: "commonterm", Mode: mode, K: MaxK})
				if err != nil {
					t.Fatal(err)
				}
				got := 0
				for _, h := range out.Result.Hits {
					if strings.Contains(h.Text, "commonterm") {
						got++
					}
				}
				if want := n - 1; got != want || out.NFTS != want {
					t.Errorf("n=%d spread=%v %s: %d hits and %d FTS rows hold the term, want %d", n, spread, mode, got, out.NFTS, want)
				}
			}
		}
	}
}

// FuzzExactMatch checks the exact list's query path against SQLite, with a table built with the
// identifier index's own tokenizer (schema v6 chunks_exact): the AND expression of the query's
// identifiers (ExactTerms, then the store's MatchExpr) always parses, every phrase in it is at
// least one token, and a document holding the identifiers as words is found by it. This is the
// sanitiser contract (sqlite FuzzSanitizeFTS) for the second tokenizer.
//
// Run: go test -tags fts5 ./internal/search -run '^$' -fuzz '^FuzzExactMatch$' -fuzztime 15s
func FuzzExactMatch(f *testing.F) {
	for _, seed := range []string{"foo_bar", "--embed-model kb.toml", "a/b:c. (x.y),", "EnsureEmbedMeta", "café_x", "-e", `a"b_c`, "x*_y", "ᳳ_a", "a_\x00b", "AND_or NOT-x"} {
		f.Add(seed)
	}
	// SQLite is the oracle for the tokenizer: a scratch in-memory database, not a kb store, so the
	// test touches no kb schema.
	db, err := sql.Open("sqlite3", ":memory:")
	if err != nil {
		f.Fatal(err)
	}
	f.Cleanup(func() { db.Close() })
	ctx := context.Background()
	conn, err := db.Conn(ctx) // temp tables are per connection
	if err != nil {
		f.Fatal(err)
	}
	f.Cleanup(func() { conn.Close() })
	for _, q := range []string{
		`CREATE VIRTUAL TABLE temp.exact_doc USING fts5(x, tokenize="unicode61 remove_diacritics 0 tokenchars '_-.:/'")`,
		`CREATE VIRTUAL TABLE temp.exact_vocab USING fts5vocab(temp, exact_doc, instance)`,
	} {
		if _, err := conn.ExecContext(ctx, q); err != nil {
			f.Fatal(err)
		}
	}
	tokens := func(t *testing.T, text string) int {
		t.Helper()
		if _, err := conn.ExecContext(ctx, `DELETE FROM temp.exact_doc`); err != nil {
			t.Fatal(err)
		}
		if _, err := conn.ExecContext(ctx, `INSERT INTO temp.exact_doc(x) VALUES (?)`, text); err != nil {
			t.Fatal(err)
		}
		var n int
		if err := conn.QueryRowContext(ctx, `SELECT count(*) FROM temp.exact_vocab`).Scan(&n); err != nil {
			t.Fatal(err)
		}
		return n
	}
	f.Fuzz(func(t *testing.T, q string) {
		terms := ExactTerms(q)
		expr := sqlite.MatchExpr(terms, store.OpAnd)
		if expr == "" {
			return
		}
		pieces := strings.Split(expr, `"`)
		for i := 1; i < len(pieces); i += 2 {
			if tokens(t, pieces[i]) == 0 {
				t.Fatalf("ExactTerms(%q) = %q: phrase %q has no tokens", q, terms, pieces[i])
			}
		}
		if !utf8.ValidString(q) {
			return
		}
		// The sanitiser deletes '*' and turns '"' and NUL into spaces inside a term, as for the
		// main index; the document holds each term so cleaned, between spaces.
		clean := strings.NewReplacer("*", "", `"`, " ", "\x00", " ")
		doc := "start " + clean.Replace(strings.Join(terms, " ")) + " end"
		tokens(t, doc)
		var n int
		if err := conn.QueryRowContext(ctx, `SELECT count(*) FROM temp.exact_doc WHERE exact_doc MATCH ?`, expr).Scan(&n); err != nil {
			t.Fatalf("ExactTerms(%q): %s does not parse: %v", q, expr, err)
		}
		if n != 1 {
			t.Fatalf("document %q is not found by the exact expression %s of %q", doc, expr, q)
		}
	})
}

// TestExactListLogged: SearchAndLog writes each hit's exact rank and score (NULL off the list),
// and a cross-store merge keeps a store's exact ranks on its candidates.
func TestExactListLogged(t *testing.T) {
	s := indexedSearcher(t, identCorpus())
	out, err := s.SearchAndLog(context.Background(), Request{Query: "kb.toml", Mode: ModeFTS, K: DefaultK, Caller: "t"})
	if err != nil {
		t.Fatal(err)
	}
	if out.NExact != 1 || out.ExactMatch == "" || !slices.Equal(out.ExactTerms, []string{"kb.toml"}) {
		t.Fatalf("outcome exact trace = %d %q %q", out.NExact, out.ExactMatch, out.ExactTerms)
	}
	tab, err := s.Store.(store.SearchLog).QueryLog(context.Background(),
		"SELECT exact_rank, exact_score IS NOT NULL FROM search_candidates WHERE search_id = ? ORDER BY fused_rank", out.Result.SearchID)
	if err != nil {
		t.Fatal(err)
	}
	if len(tab.Rows) != len(out.Candidates) || fmt.Sprint(tab.Rows[0]) != "[1 1]" {
		t.Fatalf("logged exact columns = %v, want [1 1] first", tab.Rows)
	}
	for i, row := range tab.Rows[1:] {
		if row[0] != nil {
			t.Errorf("candidate %d has exact rank %v, want NULL (one exact row)", i+2, row[0])
		}
	}
	if h := out.Result.Hits[0]; h.ExactRank == nil || *h.ExactRank != 1 {
		t.Errorf("hit 1 ExactRank = %v, want 1 (--json exact_rank)", h.ExactRank)
	}
	merged := Merge([]Part{{}}, []*Outcome{out})
	if c := merged.Candidates[0]; c.ExactRank == nil || merged.NExact != 1 {
		t.Errorf("Merge dropped the exact rank or count: %+v, n_exact %d", c, merged.NExact)
	}
}

// TestExplainExactOrRetry: two identifiers that no chunk holds together make the exact list's AND
// match nothing; its OR retry fires and explain reports it with each candidate's exact term.
func TestExplainExactOrRetry(t *testing.T) {
	s := indexedSearcher(t, identCorpus())
	ex, err := s.Explain(context.Background(), Request{Query: "kb.toml reindex_test", Mode: ModeFTS, K: DefaultK})
	if err != nil {
		t.Fatal(err)
	}
	if ex.Exact == nil || !ex.Exact.OrFallback || ex.Exact.Op != "OR" || ex.NExact != 2 {
		t.Fatalf("exact = %+v, n_exact %d; want the OR retry with 2 rows", ex.Exact, ex.NExact)
	}
	if !ex.Settings.ExactList || ex.Settings.ExactWeight != 1 {
		t.Errorf("settings = %+v, want exact_list on at weight 1", ex.Settings)
	}
	n := 0
	for _, h := range ex.Candidates {
		if h.Exact == nil {
			continue
		}
		n++
		last := h.RRF[len(h.RRF)-1]
		if last.List != "exact" || last.Rank != h.Exact.Rank || last.Term != 1/float64(RRFConstant+h.Exact.Rank) {
			t.Errorf("%s: last RRF term %+v, want the exact term for rank %d", h.Path, last, h.Exact.Rank)
		}
	}
	if n != 2 {
		t.Errorf("%d candidates carry an exact rank, want 2", n)
	}
}

// TestNoIdentifierRunsOneLexicalList: a query with no identifier makes exactly one FTS call (the
// main AND list); the exact list costs nothing then.
func TestNoIdentifierRunsOneLexicalList(t *testing.T) {
	s, _ := testSearcher(t)
	f := &faultyRetriever{Retriever: s.Store}
	s.Store = f
	out, err := s.Search(context.Background(), Request{Query: "quokka", Mode: ModeFTS, K: DefaultK})
	if err != nil {
		t.Fatal(err)
	}
	if len(f.exact) != 1 || f.exact[0] || out.ExactTerms != nil || out.ExactMatch != "" {
		t.Errorf("FTS calls (Exact flags) = %v, exact trace %q %q; want one main call and no exact list", f.exact, out.ExactTerms, out.ExactMatch)
	}
}
