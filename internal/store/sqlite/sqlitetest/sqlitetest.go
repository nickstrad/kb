// Package sqlitetest is the one door through which tests outside internal/store/sqlite reach the
// SQLite schema or handle. It exists so that schema SQL (table and column names, PRAGMAs) lives
// next to the schema: a migration that renames a column edits this file, not tests in cli, search
// and reindex. Prefer the store interface (Judged, Edges, QueryLog via storetest); add a verb here
// when a test must reach the database underneath it, and use RawDB only for a one-off the verbs do
// not cover.
//
// Test-support code: production binaries never import it.
package sqlitetest

import (
	"context"
	"database/sql"
	"fmt"
	"testing"

	"github.com/nickstrad/kb/internal/store"
	"github.com/nickstrad/kb/internal/store/sqlite"
)

// RawDB is the *sql.DB behind a sqlite store; it panics when st is not one (a test bug). It is
// the escape hatch under every verb here.
func RawDB(st any) *sql.DB {
	s, ok := st.(*sqlite.Store)
	if !ok {
		panic(fmt.Sprintf("sqlitetest: %T is not a *sqlite.Store", st))
	}
	return s.DB()
}

// CountRows runs a "SELECT count(*) ..." query.
func CountRows(t testing.TB, st store.Store, query string) int {
	t.Helper()
	var n int
	if err := RawDB(st).QueryRow(query).Scan(&n); err != nil {
		t.Fatalf("sqlitetest: %q: %v", query, err)
	}
	return n
}

// ForceEmbedMeta overwrites the recorded embedding model and dimension, to stage an index that
// disagrees with the configured embedder.
func ForceEmbedMeta(t testing.TB, st store.Store, model string, dim int) {
	t.Helper()
	if _, err := RawDB(st).Exec("UPDATE embed_meta SET model = ?, dim = ?", model, dim); err != nil {
		t.Fatalf("sqlitetest: force embed_meta: %v", err)
	}
}

// ForceEmbedModel overwrites only the recorded model, keeping the dimension.
func ForceEmbedModel(t testing.TB, st store.Store, model string) {
	t.Helper()
	if _, err := RawDB(st).Exec("UPDATE embed_meta SET model = ?", model); err != nil {
		t.Fatalf("sqlitetest: force embed_meta model: %v", err)
	}
}

// ForceEmbedDim overwrites only the recorded dimension, keeping the model.
func ForceEmbedDim(t testing.TB, st store.Store, dim int) {
	t.Helper()
	if _, err := RawDB(st).Exec("UPDATE embed_meta SET dim = ?", dim); err != nil {
		t.Fatalf("sqlitetest: force embed_meta dim: %v", err)
	}
}

// LoggedSearch is the columns of one search log row a test may assert on. Null columns are "".
type LoggedSearch struct {
	Caller       string
	Settings     string
	SettingsHash string
	FTSQuery     string
	HasSettings  bool // settings and settings_hash are both set
}

// LastSearch reads the newest row of the search log at dbPath (opened read-only).
func LastSearch(t testing.TB, dbPath string) LoggedSearch {
	t.Helper()
	st, err := sqlite.Open(context.Background(), dbPath, sqlite.Options{ReadOnly: true})
	if err != nil {
		t.Fatalf("sqlitetest: open %s: %v", dbPath, err)
	}
	defer st.Close()
	var caller, settings, hash, fts sql.NullString
	err = st.DB().QueryRow("SELECT caller, settings, settings_hash, fts_query FROM searches ORDER BY id DESC LIMIT 1").
		Scan(&caller, &settings, &hash, &fts)
	if err != nil {
		t.Fatalf("sqlitetest: read the last search: %v", err)
	}
	return LoggedSearch{Caller: caller.String, Settings: settings.String, SettingsHash: hash.String,
		FTSQuery: fts.String, HasSettings: settings.Valid && hash.Valid}
}

// Strings runs a one-column query against the database at dbPath (opened read-only) and returns
// each row as text.
func Strings(t testing.TB, dbPath, query string) []string {
	t.Helper()
	return StringsAt(t, dbPath, nil, query)
}

// StringsAt is Strings as a build with the given index steps sees the file (nil = this build's;
// sqlite.ProbeSteps for a database a probe build wrote).
func StringsAt(t testing.TB, dbPath string, steps sqlite.Steps, query string) []string {
	t.Helper()
	st, err := sqlite.Open(context.Background(), dbPath, sqlite.Options{ReadOnly: true, Steps: steps})
	if err != nil {
		t.Fatalf("sqlitetest: open %s: %v", dbPath, err)
	}
	defer st.Close()
	rows, err := st.DB().Query(query)
	if err != nil {
		t.Fatalf("sqlitetest: %q: %v", query, err)
	}
	defer rows.Close()
	var out []string
	for rows.Next() {
		var s sql.NullString
		if err := rows.Scan(&s); err != nil {
			t.Fatalf("sqlitetest: %q: %v", query, err)
		}
		out = append(out, s.String)
	}
	if err := rows.Err(); err != nil {
		t.Fatalf("sqlitetest: %q: %v", query, err)
	}
	return out
}

// LimitPages pins the database at its current size: the next write that needs a new page fails
// with SQLITE_FULL, as a full disk does. max_page_count is per connection, so the store is
// limited to one connection.
func LimitPages(t testing.TB, st store.Store) {
	t.Helper()
	db := RawDB(st)
	db.SetMaxOpenConns(1)
	var pages int
	if err := db.QueryRow("PRAGMA page_count").Scan(&pages); err != nil {
		t.Fatalf("sqlitetest: page_count: %v", err)
	}
	if _, err := db.Exec(fmt.Sprintf("PRAGMA max_page_count = %d", pages)); err != nil {
		t.Fatalf("sqlitetest: max_page_count: %v", err)
	}
}
