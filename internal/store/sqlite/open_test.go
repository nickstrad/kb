package sqlite

import (
	"context"
	"path/filepath"
	"testing"
	"time"

	"github.com/nickstrad/kb/internal/store"
)

func openTestDB(t *testing.T) *Store {
	t.Helper()
	st, err := Open(context.Background(), filepath.Join(t.TempDir(), "kb.sqlite"), Options{})
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	t.Cleanup(func() { st.Close() })
	return st
}

// addSearch inserts a search logged at ts that returned the given entries (one result each) and,
// when candidate is non-empty, also lost that entry in the fusion (candidates only).
func addSearch(t *testing.T, st *Store, ts time.Time, returned []string, candidate string) int64 {
	t.Helper()
	db := st.DB()
	res, err := db.Exec(`INSERT INTO searches (ts, query, mode, k, n_returned, total_ms, caller)
		VALUES (?, 'q', 'fts', 5, ?, 1, 't')`, ts.UTC().Format(time.RFC3339), len(returned))
	if err != nil {
		t.Fatalf("insert search: %v", err)
	}
	id, _ := res.LastInsertId()
	for i, p := range returned {
		if _, err := db.Exec(`INSERT INTO search_results (search_id, rank, chunk_id, entry_path, heading, rrf_score)
			VALUES (?, ?, ?, ?, '', 0.1)`, id, i+1, i+1, p); err != nil {
			t.Fatalf("insert result: %v", err)
		}
	}
	if candidate != "" {
		if _, err := db.Exec(`INSERT INTO search_candidates (search_id, fused_rank, chunk_id, entry_path, heading, rrf_score, returned)
			VALUES (?, 1, 99, ?, '', 0.1, 0)`, id, candidate); err != nil {
			t.Fatalf("insert candidate: %v", err)
		}
	}
	return id
}

func openedSearches(t *testing.T, st *Store, entryPath string) []int64 {
	t.Helper()
	db := st.DB()
	rows, err := db.Query("SELECT search_id FROM search_opens WHERE entry_path = ? ORDER BY search_id", entryPath)
	if err != nil {
		t.Fatalf("read opens: %v", err)
	}
	defer rows.Close()
	var ids []int64
	for rows.Next() {
		var id int64
		if err := rows.Scan(&id); err != nil {
			t.Fatal(err)
		}
		ids = append(ids, id)
	}
	return ids
}

// TestRecordOpenWindow pins the edges of store.OpenWindow: a search exactly window old still counts,
// one second older does not.
func TestRecordOpenWindow(t *testing.T) {
	now := time.Date(2026, time.October, 29, 12, 0, 0, 0, time.UTC)
	for _, tc := range []struct {
		name string
		age  time.Duration
		want bool
	}{
		{"just now", 0, true},
		{"just inside the window", store.OpenWindow - time.Second, true},
		{"exactly at the window", store.OpenWindow, true},
		{"just outside the window", store.OpenWindow + time.Second, false},
		{"long ago", 24 * time.Hour, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			db := openTestDB(t)
			id := addSearch(t, db, now.Add(-tc.age), []string{"data/a.md"}, "")
			got, err := db.RecordOpen(context.Background(), "data/a.md", now, store.OpenWindow)
			if err != nil {
				t.Fatalf("RecordOpen: %v", err)
			}
			if got != tc.want {
				t.Errorf("recorded = %t, want %t", got, tc.want)
			}
			ids := openedSearches(t, db, "data/a.md")
			if tc.want && (len(ids) != 1 || ids[0] != id) {
				t.Errorf("opens = %v, want [%d]", ids, id)
			}
			if !tc.want && len(ids) != 0 {
				t.Errorf("opens = %v, want none", ids)
			}
		})
	}
}

func TestRecordOpenSelection(t *testing.T) {
	now := time.Date(2026, time.October, 29, 12, 0, 0, 0, time.UTC)
	ctx := context.Background()

	t.Run("no searches at all", func(t *testing.T) {
		db := openTestDB(t)
		if got, err := db.RecordOpen(ctx, "data/a.md", now, store.OpenWindow); err != nil || got {
			t.Errorf("RecordOpen = %t, %v; want false, nil", got, err)
		}
	})

	t.Run("entry not returned by the recent search", func(t *testing.T) {
		db := openTestDB(t)
		addSearch(t, db, now, []string{"data/b.md"}, "")
		if got, err := db.RecordOpen(ctx, "data/a.md", now, store.OpenWindow); err != nil || got {
			t.Errorf("RecordOpen = %t, %v; want false, nil", got, err)
		}
	})

	t.Run("a candidate that lost the fusion does not count", func(t *testing.T) {
		db := openTestDB(t)
		addSearch(t, db, now, []string{"data/b.md"}, "data/a.md")
		if got, err := db.RecordOpen(ctx, "data/a.md", now, store.OpenWindow); err != nil || got {
			t.Errorf("RecordOpen = %t, %v; want false, nil", got, err)
		}
	})

	t.Run("two searches returning the entry: the newest gets it", func(t *testing.T) {
		db := openTestDB(t)
		addSearch(t, db, now.Add(-10*time.Minute), []string{"data/a.md"}, "")
		newest := addSearch(t, db, now.Add(-5*time.Minute), []string{"data/a.md"}, "")
		addSearch(t, db, now.Add(-time.Minute), []string{"data/b.md"}, "") // newer, but not a hit
		if got, err := db.RecordOpen(ctx, "data/a.md", now, store.OpenWindow); err != nil || !got {
			t.Fatalf("RecordOpen = %t, %v; want true, nil", got, err)
		}
		if ids := openedSearches(t, db, "data/a.md"); len(ids) != 1 || ids[0] != newest {
			t.Errorf("opens = %v, want [%d]", ids, newest)
		}
	})

	t.Run("repeated open is a no-op", func(t *testing.T) {
		db := openTestDB(t)
		addSearch(t, db, now, []string{"data/a.md"}, "")
		if got, _ := db.RecordOpen(ctx, "data/a.md", now, store.OpenWindow); !got {
			t.Fatal("first open not recorded")
		}
		if got, err := db.RecordOpen(ctx, "data/a.md", now.Add(time.Minute), store.OpenWindow); err != nil || got {
			t.Errorf("second RecordOpen = %t, %v; want false, nil", got, err)
		}
		if ids := openedSearches(t, db, "data/a.md"); len(ids) != 1 {
			t.Errorf("opens = %v, want one row", ids)
		}
	})
}

func TestRecordOpenErrors(t *testing.T) {
	db := openTestDB(t)
	if _, err := db.DB().Exec("DROP TABLE search_opens"); err != nil {
		t.Fatal(err)
	}
	if recorded, err := db.RecordOpen(context.Background(), "data/a.md", time.Now(), store.OpenWindow); err == nil || recorded {
		t.Errorf("missing table: recorded, err = %v, %v; want false and an error", recorded, err)
	}
}

// TestOpenWindowContract pins the window once: `kb show --help` and the README promise "the last
// 30 minutes", and every other test uses the symbol (ADVERSARIAL_TESTING §3.1, symbol-relative
// constant that is a contract).
func TestOpenWindowContract(t *testing.T) {
	if store.OpenWindow != 30*time.Minute {
		t.Errorf("store.OpenWindow = %s; the documented window is 30m", store.OpenWindow)
	}
}
