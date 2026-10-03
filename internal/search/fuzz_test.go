package search

import (
	"context"
	"strings"
	"testing"
	"unicode/utf8"
)

// FuzzSanitizeFTS checks the sanitiser's contract against a real FTS5 table built with the same
// tokenizer as chunks_fts, using SQLite rather than Go to decide what a token is:
//
//  1. the AND and OR queries are both empty or both non-empty;
//  2. SQLite splits every quoted phrase into at least one token, so no phrase is empty;
//  3. both queries parse as FTS5 MATCH expressions;
//  4. a document whose text is the raw query is found by the AND query. This is the check the
//     hyphen bug failed: `draw-visual` was sanitised to a token the document did not contain.
//
// For rule 4 the document is the query with every '*' removed, matching the sanitiser's deliberate
// `post*gres` → postgres rule. It is skipped for input that is not valid UTF-8, which Go and SQLite
// decode differently, and for a document SQLite splits into no tokens, which no MATCH can find.
//
// Rule 2 is what found the Unicode skew behind ftsSkewNotToken (`ᳳ`, `ᦳ`): a word Go called a
// letter became a phrase with no tokens, so the AND query for `quokka ᳳ` missed.
//
// Run: go test -tags fts5 ./internal/search/ -run '^$' -fuzz FuzzSanitizeFTS -fuzztime 60s
func FuzzSanitizeFTS(f *testing.F) {
	for _, seed := range []string{
		"daemon dies when I log out of ssh",
		"set -e loop counter exits",
		`grpcurl says "server does not support reflection"`,
		"foo:bar (baz)",
		"draw-visual go-build app-server-daemon",
		"not-found NOT a AND b NEAR c",
		`a"OR".`,
		"quokka ... && — widget",
		"quokka ᳳ ᦳ",
		"post*gres **bold** *.go",
		"^x {y} +z col:val a--b -foo-",
		"café naïve 日本語 straße İstanbul",
		`"" ** (( )) :: ^^ -- ++ {}`,
		"",
	} {
		f.Add(seed)
	}

	s, _ := testSearcher(f)
	ctx := context.Background()
	// Temp tables belong to one connection, and database/sql may hand out another from its pool,
	// so all temp-table work goes through this one.
	conn, err := s.DB.Conn(ctx)
	if err != nil {
		f.Fatalf("dedicated connection: %v", err)
	}
	f.Cleanup(func() { conn.Close() })
	if _, err := conn.ExecContext(ctx,
		`CREATE VIRTUAL TABLE temp.fuzz_doc USING fts5(x, tokenize='porter unicode61')`); err != nil {
		f.Fatalf("create fuzz table: %v", err)
	}
	if _, err := conn.ExecContext(ctx,
		`CREATE VIRTUAL TABLE temp.fuzz_vocab USING fts5vocab(temp, fuzz_doc, instance)`); err != nil {
		f.Fatalf("create fuzz vocab table: %v", err)
	}
	// tokens stores text as the only document in fuzz_doc and returns how many tokens SQLite
	// split it into. The document stays in place for a following MATCH.
	tokens := func(t *testing.T, text string) int {
		t.Helper()
		if _, err := conn.ExecContext(ctx, `DELETE FROM temp.fuzz_doc`); err != nil {
			t.Fatalf("reset fuzz table: %v", err)
		}
		if _, err := conn.ExecContext(ctx, `INSERT INTO temp.fuzz_doc(x) VALUES (?)`, text); err != nil {
			t.Fatalf("insert %q: %v", text, err)
		}
		var n int
		if err := conn.QueryRowContext(ctx, `SELECT count(*) FROM temp.fuzz_vocab`).Scan(&n); err != nil {
			t.Fatalf("count tokens of %q: %v", text, err)
		}
		return n
	}

	f.Fuzz(func(t *testing.T, q string) {
		andQuery, orQuery := SanitizeFTS(q)
		if (andQuery == "") != (orQuery == "") {
			t.Fatalf("SanitizeFTS(%q): and=%q or=%q, want both empty or both set", q, andQuery, orQuery)
		}
		if andQuery == "" {
			return
		}
		// A phrase never contains '"', so the phrases are the odd pieces between quotes. Splitting
		// on " OR " instead would cut a phrase such as "a OR ." in two.
		pieces := strings.Split(orQuery, `"`)
		for i := 1; i < len(pieces); i += 2 {
			if tokens(t, pieces[i]) == 0 {
				t.Fatalf("SanitizeFTS(%q): phrase %q has no tokens", q, pieces[i])
			}
		}
		for _, mq := range []string{andQuery, orQuery} {
			if _, err := s.runFTS(ctx,
				"SELECT rowid, bm25(chunks_fts, 1.0, 2.0, 3.0) FROM chunks_fts WHERE chunks_fts MATCH ? LIMIT 1",
				[]any{mq}); err != nil {
				t.Fatalf("SanitizeFTS(%q) = %s: does not parse: %v", q, mq, err)
			}
		}

		if !utf8.ValidString(q) {
			return
		}
		doc := strings.ReplaceAll(q, "*", "")
		if tokens(t, doc) == 0 {
			return
		}
		var n int
		if err := conn.QueryRowContext(ctx, `SELECT count(*) FROM temp.fuzz_doc WHERE fuzz_doc MATCH ?`, andQuery).Scan(&n); err != nil {
			t.Fatalf("match %s: %v", andQuery, err)
		}
		if n != 1 {
			t.Fatalf("document %q is not found by its own sanitised query %s", doc, andQuery)
		}
	})
}
