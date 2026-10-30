package sqlite

import (
	"context"
	"fmt"
	"path/filepath"
	"testing"
	"unicode"
	"unicode/utf8"

	"github.com/nickstrad/kb/internal/store"
)

// ftsFixture opens an FTS-only store holding a few chunks the sanitiser tests match against.
func ftsFixture(t testing.TB) *Store {
	t.Helper()
	ctx := context.Background()
	s, err := Open(ctx, filepath.Join(t.TempDir(), "kb.sqlite"), Options{})
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	t.Cleanup(func() { s.Close() })
	if err := s.EnsureEmbedMeta(ctx, store.EmbedMeta{Model: store.FTSOnlyModel}); err != nil {
		t.Fatalf("embed meta: %v", err)
	}
	for i, text := range []string{
		"the zebracorn widget is a fixture",
		"a quokka lives here; quokka facts",
		"zebracorn alone, widget elsewhere",
	} {
		path := fmt.Sprintf("fts%d.md", i)
		if _, err := s.ReplaceEntry(ctx, store.EntryInput{Path: path, Kind: "file", Title: "T", Summary: "S"},
			[]store.ChunkInput{{Ord: 0, SourceFile: path, Text: text, TextHash: path}}); err != nil {
			t.Fatalf("index: %v", err)
		}
	}
	return s
}

// runMatch runs a raw MATCH expression against chunks_fts and returns the rows in rowid order.
func runMatch(ctx context.Context, s *Store, match string) ([]store.FTSRow, error) {
	rows, err := s.DB().QueryContext(ctx,
		"SELECT rowid, bm25(chunks_fts, 1.0, 2.0, 3.0) FROM chunks_fts WHERE chunks_fts MATCH ? ORDER BY rowid", match)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []store.FTSRow
	for rows.Next() {
		var r store.FTSRow
		if err := rows.Scan(&r.ChunkID, &r.Score); err != nil {
			return nil, err
		}
		out = append(out, r)
	}
	return out, rows.Err()
}

// TestSanitizeFTS covers the queries from the plan's validation list plus the syntax characters
// that make raw FTS5 throw: quotes, colons, parentheses, a leading dash, and the operator words.
func TestSanitizeFTS(t *testing.T) {
	for _, tc := range []struct {
		name, in, wantAnd, wantOr string
	}{
		{
			name:    "plain sentence",
			in:      "daemon dies when I log out of ssh",
			wantAnd: `"daemon" AND "dies" AND "when" AND "I" AND "log" AND "out" AND "of" AND "ssh"`,
			wantOr:  `"daemon" OR "dies" OR "when" OR "I" OR "log" OR "out" OR "of" OR "ssh"`,
		},
		{
			name:    "leading dash flag",
			in:      "set -e loop counter exits",
			wantAnd: `"set" AND "-e" AND "loop" AND "counter" AND "exits"`,
			wantOr:  `"set" OR "-e" OR "loop" OR "counter" OR "exits"`,
		},
		{
			name:    "embedded quotes and a bare NOT",
			in:      `grpcurl says "server does not support reflection"`,
			wantAnd: `"grpcurl" AND "says" AND "server" AND "does" AND "support" AND "reflection"`,
			wantOr:  `"grpcurl" OR "says" OR "server" OR "does" OR "support" OR "reflection"`,
		},
		{
			name:    "colon and parentheses",
			in:      "foo:bar (baz)",
			wantAnd: `"foo:bar" AND "(baz)"`,
			wantOr:  `"foo:bar" OR "(baz)"`,
		},
		{
			name:    "hyphenated name stays one phrase",
			in:      "draw-visual go-build",
			wantAnd: `"draw-visual" AND "go-build"`,
			wantOr:  `"draw-visual" OR "go-build"`,
		},
		{
			name:    "punctuation-only fields are dropped, not left as empty phrases",
			in:      "quokka ... && — widget",
			wantAnd: `"quokka" AND "widget"`,
			wantOr:  `"quokka" OR "widget"`,
		},
		{
			name:    "letters SQLite does not tokenize are dropped like punctuation",
			in:      "quokka ᳳ ᦳ",
			wantAnd: `"quokka"`,
			wantOr:  `"quokka"`,
		},
		{
			name:    "NUL byte breaks the word",
			in:      "a\x00b",
			wantAnd: `"a b"`,
			wantOr:  `"a b"`,
		},
		{
			name:    "operator word inside a hyphenated name is kept",
			in:      "not-found",
			wantAnd: `"not-found"`,
			wantOr:  `"not-found"`,
		},
		{
			name:    "operator word alone",
			in:      "NOT a",
			wantAnd: `"a"`,
			wantOr:  `"a"`,
		},
		{
			name:    "every operator word",
			in:      "and OR Not near",
			wantAnd: "",
			wantOr:  "",
		},
		{
			name:    "empty",
			in:      "",
			wantAnd: "",
			wantOr:  "",
		},
		{
			name:    "whitespace only",
			in:      "   \t\n  ",
			wantAnd: "",
			wantOr:  "",
		},
		{
			name:    "only punctuation",
			in:      `"" ** (( )) :: ^^ -- ++ {}`,
			wantAnd: "",
			wantOr:  "",
		},
		{
			name:    "prefix star stripped",
			in:      "post*gres",
			wantAnd: `"postgres"`,
			wantOr:  `"postgres"`,
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			gotAnd, gotOr := SanitizeFTS(tc.in)
			if gotAnd != tc.wantAnd {
				t.Errorf("and = %s, want %s", gotAnd, tc.wantAnd)
			}
			if gotOr != tc.wantOr {
				t.Errorf("or = %s, want %s", gotOr, tc.wantOr)
			}
		})
	}
}

// TestBaseRuneMatchesSQLite checks ftsBaseRune against the linked SQLite for every assigned code
// point it accepts: each one, alone in a document, must give the unicode61 tokenizer at least one
// token. A failure lists code points to add to ftsSkewNotToken, typically after a SQLite upgrade.
func TestBaseRuneMatchesSQLite(t *testing.T) {
	s := ftsFixture(t)
	ctx := context.Background()
	conn, err := s.DB().Conn(ctx)
	if err != nil {
		t.Fatalf("dedicated connection: %v", err)
	}
	defer conn.Close()
	for _, stmt := range []string{
		`CREATE VIRTUAL TABLE temp.base_doc USING fts5(x, tokenize='porter unicode61')`,
		`CREATE VIRTUAL TABLE temp.base_vocab USING fts5vocab(temp, base_doc, instance)`,
		`BEGIN`,
	} {
		if _, err := conn.ExecContext(ctx, stmt); err != nil {
			t.Fatalf("%s: %v", stmt, err)
		}
	}
	checked := 0
	for r := rune(0); r <= unicode.MaxRune; r++ {
		if !utf8.ValidRune(r) || unicode.Is(unicode.Cn, r) || !ftsBaseRune(r) {
			continue
		}
		if _, err := conn.ExecContext(ctx, `INSERT INTO temp.base_doc(rowid, x) VALUES (?, ?)`, int64(r), string(r)); err != nil {
			t.Fatalf("insert U+%04X: %v", r, err)
		}
		checked++
	}
	if _, err := conn.ExecContext(ctx, `COMMIT`); err != nil {
		t.Fatalf("commit: %v", err)
	}
	rows, err := conn.QueryContext(ctx,
		`SELECT rowid FROM temp.base_doc WHERE rowid NOT IN (SELECT doc FROM temp.base_vocab) ORDER BY rowid`)
	if err != nil {
		t.Fatalf("find untokenized: %v", err)
	}
	defer rows.Close()
	var bad []string
	for rows.Next() {
		var r int64
		if err := rows.Scan(&r); err != nil {
			t.Fatalf("scan: %v", err)
		}
		bad = append(bad, fmt.Sprintf("U+%04X", r))
	}
	if checked < 100000 {
		t.Fatalf("checked only %d code points; the enumeration is broken", checked)
	}
	if len(bad) > 0 {
		t.Errorf("ftsBaseRune accepts %d code points SQLite does not tokenize: %v", len(bad), bad)
	}
}

// TestQuotedPunctuationMatchesLikeSplitWords backs the TestSanitizeFTS expectations that keep
// punctuation inside a phrase ("foo:bar", "(baz)", "-e"): FTS5 must tokenize such a phrase into
// the same adjacent words, so each pair below has to match exactly the same rows.
func TestQuotedPunctuationMatchesLikeSplitWords(t *testing.T) {
	s := ftsFixture(t)
	ctx := context.Background()
	match := func(q string) []int64 {
		rows, err := runMatch(ctx, s, q)
		if err != nil {
			t.Fatalf("match %s: %v", q, err)
		}
		ids := make([]int64, len(rows))
		for i, r := range rows {
			ids[i] = r.ChunkID
		}
		return ids
	}
	for _, pair := range [][2]string{
		{`"zebracorn-widget"`, `"zebracorn widget"`},
		{`"zebracorn:widget"`, `"zebracorn widget"`},
		{`"(quokka)"`, `"quokka"`},
		{`"-quokka"`, `"quokka"`},
		{`"Zebracorn-WIDGET"`, `"zebracorn widget"`},
	} {
		got, want := match(pair[0]), match(pair[1])
		if len(want) == 0 {
			t.Fatalf("%s matched nothing; the pair proves nothing", pair[1])
		}
		if fmt.Sprint(got) != fmt.Sprint(want) {
			t.Errorf("%s matched %v, %s matched %v; want the same rows", pair[0], got, pair[1], want)
		}
	}
}
