package sqlite

import (
	"context"
	"errors"
	"path/filepath"
	"reflect"
	"testing"
	"time"

	"github.com/nickstrad/kb/internal/errs"
	"github.com/nickstrad/kb/internal/store"
)

// judgedLog opens an empty store and logs three searches with hand-made results, then judges
// them: search 1 gets a useful verdict re-marked not useful plus a useful one, search 2 --none
// and an open, search 3 nothing. driver "" is the real one.
func judgedLog(t *testing.T, driver string) *Store {
	t.Helper()
	ctx := context.Background()
	st, err := Open(ctx, filepath.Join(t.TempDir(), "kb.sqlite"), Options{driver: driver})
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	t.Cleanup(func() { st.Close() })
	hits := func(paths ...string) []store.LoggedHit {
		out := make([]store.LoggedHit, len(paths))
		for i, p := range paths {
			// The same chunk id on every search: ids are reused after a reindex and must not matter.
			out[i] = store.LoggedHit{ChunkID: 7, EntryPath: p, Heading: "H " + p, RRFScore: 1}
		}
		return out
	}
	for _, r := range []store.SearchRecord{
		{Query: "Tmux  Logout", Mode: "hybrid", K: 8, Caller: "claude", Results: hits("a.md", "b.md", "c.md")},
		{Query: "ssh", Mode: "fts", K: 3, Caller: "codex", Results: hits("d.md", "a.md")},
		{Query: "git", Mode: "fts", K: 8, Caller: "claude", Results: hits("e.md")},
	} {
		if _, err := st.LogSearch(ctx, r); err != nil {
			t.Fatal(err)
		}
	}
	steps := []func() error{
		func() error { return st.Feedback(ctx, 1, 1, true, "", time.Time{}) },
		func() error { return st.Feedback(ctx, 1, 1, false, "changed my mind", time.Time{}) }, // the latest verdict wins
		func() error { return st.Feedback(ctx, 1, 3, true, "", time.Time{}) },
		func() error { return st.FeedbackNone(ctx, 2, "", time.Time{}) },
	}
	for i, step := range steps {
		if err := step(); err != nil {
			t.Fatalf("step %d: %v", i, err)
		}
	}
	if ok, err := st.RecordOpen(ctx, store.OpenRecord{EntryPath: "d.md", At: time.Now(), Window: time.Hour}); err != nil || !ok {
		t.Fatalf("RecordOpen: %v %v", ok, err)
	}
	return st
}

// TestJudgedReadsVerdictsNoneAndOpens: the read side of the log returns every search, the latest
// verdict per rank joined to the path and heading logged at that rank, --none and opens.
func TestJudgedReadsVerdictsNoneAndOpens(t *testing.T) {
	st := judgedLog(t, "")
	got, err := st.Judged(context.Background(), store.JudgedFilter{})
	if err != nil {
		t.Fatal(err)
	}
	for i := range got {
		if got[i].TS == "" {
			t.Errorf("search %d: no ts", got[i].ID)
		}
		got[i].TS = ""
	}
	want := []store.JudgedSearch{
		{ID: 1, Query: "Tmux  Logout", Mode: "hybrid", K: 8, Caller: "claude", Verdicts: []store.Verdict{
			{Rank: 1, Useful: false, EntryPath: "a.md", Heading: "H a.md"},
			{Rank: 3, Useful: true, EntryPath: "c.md", Heading: "H c.md"},
		}},
		{ID: 2, Query: "ssh", Mode: "fts", K: 3, Caller: "codex", None: true, Opens: []string{"d.md"}},
		{ID: 3, Query: "git", Mode: "fts", K: 8, Caller: "claude"},
	}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("Judged:\n got %+v\nwant %+v", got, want)
	}
}

// TestJudgedFilter: caller and since narrow the searches, and the verdicts and opens with them.
func TestJudgedFilter(t *testing.T) {
	st := judgedLog(t, "")
	for _, tc := range []struct {
		name string
		f    store.JudgedFilter
		ids  []int64
	}{
		{"no filter", store.JudgedFilter{}, []int64{1, 2, 3}},
		{"caller", store.JudgedFilter{Caller: "claude"}, []int64{1, 3}},
		{"caller without searches", store.JudgedFilter{Caller: "nobody"}, nil},
		{"since an hour ago", store.JudgedFilter{Since: time.Now().Add(-time.Hour)}, []int64{1, 2, 3}},
		{"since tomorrow", store.JudgedFilter{Since: time.Now().Add(24 * time.Hour)}, nil},
		{"caller and since", store.JudgedFilter{Caller: "codex", Since: time.Now().Add(-time.Hour)}, []int64{2}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got, err := st.Judged(context.Background(), tc.f)
			if err != nil {
				t.Fatal(err)
			}
			var ids []int64
			for _, j := range got {
				ids = append(ids, j.ID)
				if j.ID == 2 && len(j.Opens) != 1 {
					t.Errorf("search 2 lost its open: %+v", j)
				}
			}
			if !reflect.DeepEqual(ids, tc.ids) {
				t.Errorf("ids %v, want %v", ids, tc.ids)
			}
		})
	}
}

// TestJudgedDriverFailures: a failure in each of the three reads is the injected fault, wrapped
// once as Internal.
func TestJudgedDriverFailures(t *testing.T) {
	for _, match := range []string{"SELECT s.id, s.ts", "FROM search_feedback AS f", "FROM search_opens AS o"} {
		t.Run(match, func(t *testing.T) {
			st := judgedLog(t, faultDriverName)
			fail(t, match, 1)
			_, err := st.Judged(context.Background(), store.JudgedFilter{})
			if !errors.Is(err, errFault) || errs.KindOf(err) != errs.Internal {
				t.Fatalf("err = %v (kind %v), want the injected fault as Internal", err, errs.KindOf(err))
			}
		})
	}
}

// TestJudgedSkipsOrphanVerdicts: a verdict on a rank the search never returned (only a damaged
// or hand-edited log has one; Feedback refuses it) is skipped, not read as gold with no path.
func TestJudgedSkipsOrphanVerdicts(t *testing.T) {
	st := judgedLog(t, "")
	if _, err := st.db.Exec("INSERT INTO search_feedback (search_id, rank, useful, ts) VALUES (3, 9, 1, 'x')"); err != nil {
		t.Fatal(err)
	}
	got, err := st.Judged(context.Background(), store.JudgedFilter{Caller: "claude"})
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 2 || got[1].ID != 3 || len(got[1].Verdicts) != 0 {
		t.Errorf("Judged = %+v, want search 3 without verdicts", got)
	}
}

// emptyLog is a fresh store with nothing indexed or logged.
func emptyLog(t *testing.T) *Store {
	t.Helper()
	st, err := Open(context.Background(), filepath.Join(t.TempDir(), "kb.sqlite"), Options{})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { st.Close() })
	return st
}
