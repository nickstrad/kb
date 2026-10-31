package sqlite

import (
	"context"
	"database/sql"
	"errors"
	"flag"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strings"
	"testing"
)

// writeSchemaGolden writes the baseline schema golden when it is missing:
//
//	go test -tags fts5 ./internal/store/sqlite -run TestBaselineSchemaGolden -write-schema-golden
//
// It never overwrites it: the golden freezes the released baseline (index v1), so an edit to
// schema.sql fails TestBaselineSchemaGolden instead of reaching new stores only.
var writeSchemaGolden = flag.Bool("write-schema-golden", false, "write a missing testdata/schema-baseline.golden")

// schemaGoldenPath is the semantic schema of a fresh store at the baseline (index v1). It was
// first generated from the pre-release chain index/0001..0009 (a fresh v1 store migrated to v9),
// so the squashed baseline is proven equal to what that chain built.
var schemaGoldenPath = filepath.Join("testdata", "schema-baseline.golden")

// TestBaselineSchemaGolden: a fresh store built by the baseline step has exactly the frozen
// semantic schema (columns with types, NOT NULL, defaults and keys; CHECKs; foreign keys;
// indexes; virtual tables; triggers and views). The golden is semantic, not sqlite_schema text,
// because ALTER TABLE rewrites stored SQL: a squashed CREATE and the ALTERs it replaces differ as
// text but must not differ here.
func TestBaselineSchemaGolden(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	steps := indexSteps[:1] // the baseline alone: later steps are checked against their own fixtures
	path := filepath.Join(t.TempDir(), "kb.sqlite")
	st, err := open(ctx, path, Options{Steps: steps})
	if err != nil {
		t.Fatal(err)
	}
	st.Close()
	got := semanticSchema(t, rawDB(t, path))

	want, err := os.ReadFile(schemaGoldenPath)
	switch {
	case errors.Is(err, fs.ErrNotExist) && *writeSchemaGolden:
		if err := os.WriteFile(schemaGoldenPath, []byte(got), 0o644); err != nil {
			t.Fatal(err)
		}
		t.Logf("wrote %s", schemaGoldenPath)
		return
	case err != nil:
		t.Fatalf("read %s (generate it with -write-schema-golden): %v", schemaGoldenPath, err)
	}
	if got != string(want) {
		t.Errorf("the baseline schema differs from %s (schema.sql is frozen: a change is a new step):\n%s",
			schemaGoldenPath, diffSchema(strings.Split(string(want), "\n"), strings.Split(got, "\n")))
	}
}

// semanticSchema renders db's schema as sorted, comparable lines built from SQLite's own
// introspection (pragma_table_list, table_xinfo, foreign_key_list, index_list, index_xinfo), so
// two schemas that mean the same thing render the same whatever DDL built them. CHECK constraints
// have no pragma: they are read from the table's SQL with comments and whitespace removed. Virtual
// tables, expression indexes, triggers and views are compared by their normalised SQL. Tables a
// virtual table owns, SQLite's own objects and the vector table EnsureEmbedMeta creates at the
// model's width are left out. The user_version is not part of it.
func semanticSchema(t *testing.T, db *sql.DB) string {
	t.Helper()
	ctx := context.Background()
	owned, err := ownedTables(ctx, db)
	if err != nil {
		t.Fatal(err)
	}
	type obj struct{ typ, name, tbl, sql string }
	var objs []obj
	rows, err := db.Query("SELECT type, name, tbl_name, coalesce(sql, '') FROM sqlite_schema WHERE name NOT LIKE 'sqlite_%'")
	if err != nil {
		t.Fatal(err)
	}
	for rows.Next() {
		var o obj
		if err := rows.Scan(&o.typ, &o.name, &o.tbl, &o.sql); err != nil {
			t.Fatal(err)
		}
		if owned[o.tbl] || o.tbl == "chunks_vec" {
			continue
		}
		objs = append(objs, o)
	}
	if err := rows.Err(); err != nil {
		t.Fatal(err)
	}
	rows.Close()

	q := func(query string, args ...any) []string {
		t.Helper()
		rows, err := db.Query(query, args...)
		if err != nil {
			t.Fatalf("%s: %v", query, err)
		}
		defer rows.Close()
		cols, _ := rows.Columns()
		var out []string
		for rows.Next() {
			vals := make([]any, len(cols))
			ptrs := make([]any, len(cols))
			for i := range vals {
				ptrs[i] = &vals[i]
			}
			if err := rows.Scan(ptrs...); err != nil {
				t.Fatal(err)
			}
			parts := make([]string, len(cols))
			for i, v := range vals {
				if b, ok := v.([]byte); ok {
					v = string(b)
				}
				parts[i] = fmt.Sprintf("%s=%v", cols[i], v)
			}
			out = append(out, strings.Join(parts, " "))
		}
		if err := rows.Err(); err != nil {
			t.Fatal(err)
		}
		return out
	}

	var lines []string
	add := func(prefix string, items ...string) {
		for _, it := range items {
			lines = append(lines, prefix+it)
		}
	}
	for _, o := range objs {
		p := fmt.Sprintf("%s %s: ", o.typ, o.name)
		switch {
		case o.typ == "table" && strings.HasPrefix(strings.ToUpper(o.sql), "CREATE VIRTUAL"):
			add(p, "sql "+normSQL(o.sql))
			add(p+"column ", q("SELECT cid, name, type, hidden FROM pragma_table_xinfo(?)", o.name)...)
		case o.typ == "table":
			add(p, q("SELECT type, ncol, wr, strict FROM pragma_table_list(?) WHERE schema = 'main'", o.name)...)
			add(p+"column ", q(`SELECT cid, name, type, "notnull", dflt_value, pk, hidden FROM pragma_table_xinfo(?)`, o.name)...)
			add(p+"fk ", q(`SELECT id, seq, "table", "from", "to", on_update, on_delete, "match" FROM pragma_foreign_key_list(?)`, o.name)...)
			for _, idx := range q("SELECT name, \"unique\", origin, partial FROM pragma_index_list(?) WHERE origin != 'c'", o.name) {
				// Automatic indexes (UNIQUE, PRIMARY KEY) are named by position; their columns say what they are.
				name := strings.TrimPrefix(strings.Fields(idx)[0], "name=")
				cols := q("SELECT seqno, cid, name, \"desc\", coll, key FROM pragma_index_xinfo(?)", name)
				add(p+"autoindex ", strings.Join(append(strings.Fields(idx)[1:], cols...), " | "))
			}
			add(p+"check ", checks(o.sql)...)
		case o.typ == "index":
			add(p, "on "+o.tbl+" "+normSQL(o.sql))
			add(p+"key ", q("SELECT seqno, cid, name, \"desc\", coll, key FROM pragma_index_xinfo(?)", o.name)...)
		default: // trigger, view
			add(p, "on "+o.tbl+" "+normSQL(o.sql))
		}
	}
	slices.Sort(lines)
	return strings.Join(lines, "\n") + "\n"
}

var (
	sqlComment = regexp.MustCompile(`--[^\n]*`)
	checkKw    = regexp.MustCompile(`(?i)\bCHECK\s*\(`)
)

// normSQL is s without comments, with whitespace collapsed.
func normSQL(s string) string {
	return strings.Join(strings.Fields(sqlComment.ReplaceAllString(s, "")), " ")
}

// checks are the CHECK expressions in a CREATE TABLE, comments and all whitespace removed (the
// schemas here put no spaces inside string literals in a CHECK), sorted.
func checks(createSQL string) []string {
	s := sqlComment.ReplaceAllString(createSQL, "")
	var out []string
	for _, loc := range checkKw.FindAllStringIndex(s, -1) {
		depth, i := 1, loc[1]
		for ; i < len(s) && depth > 0; i++ {
			switch s[i] {
			case '(':
				depth++
			case ')':
				depth--
			}
		}
		out = append(out, strings.Join(strings.Fields(s[loc[1]:i-1]), ""))
	}
	slices.Sort(out)
	return out
}
