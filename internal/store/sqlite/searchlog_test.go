package sqlite

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"errors"
	"fmt"
	"path/filepath"
	"strings"
	"testing"

	"github.com/nickstrad/kb/internal/config"
	"github.com/nickstrad/kb/internal/embed/fake"
	"github.com/nickstrad/kb/internal/errs"
	"github.com/nickstrad/kb/internal/search"
	"github.com/nickstrad/kb/internal/store"
)

// logFixture indexes three entries of five chunks each, every chunk containing "widget", and
// returns the store and a Searcher logging to it, so the tests below have real searches to judge.
func logFixture(t *testing.T) (*Store, *search.Searcher) {
	t.Helper()
	ctx := context.Background()
	emb := fake.New("fake-embed", 16)
	st, err := Open(ctx, filepath.Join(t.TempDir(), "kb.sqlite"), Options{})
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	t.Cleanup(func() { st.Close() })
	if err := st.EnsureEmbedMeta(ctx, store.EmbedMeta{Model: emb.Model(), Dim: emb.Dim()}); err != nil {
		t.Fatalf("embed meta: %v", err)
	}
	for _, name := range []string{"alpha", "beta", "gamma"} {
		path := "log" + name + ".md"
		chunks := make([]store.ChunkInput, 5)
		for j := range chunks {
			text := fmt.Sprintf("widget %s %d: a section of the %s entry", name, j, name)
			vecs, err := emb.Embed(ctx, []string{text})
			if err != nil {
				t.Fatalf("embed: %v", err)
			}
			sum := sha256.Sum256([]byte(text))
			chunks[j] = store.ChunkInput{Ord: j, SourceFile: path, Heading: fmt.Sprintf("section %d", j),
				Text: text, TextHash: hex.EncodeToString(sum[:]), Embedding: vecs[0]}
		}
		chunks[0].Heading = ""
		if _, err := st.ReplaceEntry(ctx, store.EntryInput{Path: path, Kind: "file", Title: "Log " + name,
			Summary: "fixture", BodyHash: "hash-" + path}, chunks); err != nil {
			t.Fatalf("index %s: %v", path, err)
		}
	}
	return st, &search.Searcher{Store: st, Log: st, Embedder: emb, KBVersion: "deadbee"}
}

// count runs a count(*) query.
func count(t *testing.T, db *sql.DB, query string, args ...any) int {
	t.Helper()
	var n int
	if err := db.QueryRow(query, args...).Scan(&n); err != nil {
		t.Fatalf("%s: %v", query, err)
	}
	return n
}

// TestFeedback covers the happy path, the upsert, and every way a rank can be wrong.
func TestFeedback(t *testing.T) {
	st, s := logFixture(t)
	db := st.DB()
	ctx := context.Background()

	out, err := s.SearchAndLog(ctx, search.Request{Query: "widget", Mode: search.ModeHybrid, K: 8, Caller: "test"})
	if err != nil {
		t.Fatalf("SearchAndLog: %v", err)
	}
	id := out.Result.SearchID
	n := len(out.Returned)

	if err := st.Feedback(ctx, id, 1, true, "exactly what I needed"); err != nil {
		t.Fatalf("Feedback: %v", err)
	}
	var useful int
	var note sql.NullString
	if err := db.QueryRow("SELECT useful, note FROM search_feedback WHERE search_id = ? AND rank = 1", id).
		Scan(&useful, &note); err != nil {
		t.Fatalf("read feedback: %v", err)
	}
	if useful != 1 || note.String != "exactly what I needed" {
		t.Errorf("useful/note = %d / %q", useful, note.String)
	}

	// The last rank is in range; an empty note is stored as NULL.
	if err := st.Feedback(ctx, id, n, false, ""); err != nil {
		t.Fatalf("Feedback on the last rank: %v", err)
	}
	if err := db.QueryRow("SELECT useful, note FROM search_feedback WHERE search_id = ? AND rank = ?", id, n).
		Scan(&useful, &note); err != nil {
		t.Fatalf("read feedback: %v", err)
	}
	if useful != 0 || note.Valid {
		t.Errorf("useful/note = %d / %v, want 0 / NULL", useful, note)
	}

	// Re-marking the same rank replaces the verdict instead of failing on the primary key.
	if err := st.Feedback(ctx, id, 1, false, "changed my mind"); err != nil {
		t.Fatalf("Feedback upsert: %v", err)
	}
	if got := count(t, db, "SELECT count(*) FROM search_feedback WHERE search_id = ?", id); got != 2 {
		t.Errorf("feedback rows = %d, want 2", got)
	}
	if err := db.QueryRow("SELECT useful FROM search_feedback WHERE search_id = ? AND rank = 1", id).Scan(&useful); err != nil {
		t.Fatalf("read feedback: %v", err)
	}
	if useful != 0 {
		t.Errorf("useful = %d after the upsert, want 0", useful)
	}

	for _, tc := range []struct {
		name     string
		searchID int64
		rank     int
		wantMsg  string
	}{
		{name: "rank zero", searchID: id, rank: 0, wantMsg: fmt.Sprintf("search %d has %d results", id, n)},
		{name: "rank too high", searchID: id, rank: n + 1, wantMsg: fmt.Sprintf("search %d has %d results", id, n)},
		{name: "negative rank", searchID: id, rank: -3, wantMsg: "out of range"},
		{name: "unknown search", searchID: id + 999, rank: 1, wantMsg: fmt.Sprintf("no search with id %d", id+999)},
	} {
		t.Run(tc.name, func(t *testing.T) {
			err := st.Feedback(ctx, tc.searchID, tc.rank, true, "")
			if err == nil {
				t.Fatal("Feedback accepted an invalid argument")
			}
			if !strings.Contains(err.Error(), tc.wantMsg) {
				t.Errorf("error = %q, want it to contain %q", err, tc.wantMsg)
			}
		})
	}
	if got := count(t, db, "SELECT count(*) FROM search_feedback"); got != 2 {
		t.Errorf("feedback rows = %d after the rejections, want 2", got)
	}
}

// feedbackRow reads one search_feedback row back; ok is false when the row does not exist.
func feedbackRow(t *testing.T, db *sql.DB, searchID int64, rank int) (useful int, note sql.NullString, ok bool) {
	t.Helper()
	err := db.QueryRow("SELECT useful, note FROM search_feedback WHERE search_id = ? AND rank = ?", searchID, rank).
		Scan(&useful, &note)
	if errors.Is(err, sql.ErrNoRows) {
		return 0, note, false
	}
	if err != nil {
		t.Fatalf("read feedback (%d, %d): %v", searchID, rank, err)
	}
	return useful, note, true
}

// TestFeedbackNone covers the whole-search verdict: it is the one verdict a zero-result search can
// take, it is stored as a single rank-0 not-useful row, it upserts, it refuses to contradict a
// useful verdict, and a later useful verdict withdraws it.
func TestFeedbackNone(t *testing.T) {
	st, s := logFixture(t)
	db := st.DB()
	ctx := context.Background()

	// A zero-result search: no fixture chunk contains this token, and fts never embeds.
	zero, err := s.SearchAndLog(ctx, search.Request{Query: "nonexistentzebra", Mode: search.ModeFTS, K: 8, Caller: "test"})
	if err != nil {
		t.Fatalf("SearchAndLog zero: %v", err)
	}
	if len(zero.Returned) != 0 {
		t.Fatalf("zero-result fixture returned %d hits", len(zero.Returned))
	}
	zeroID := zero.Result.SearchID

	// Its ranks cannot be marked, and the error points at --none.
	if err := st.Feedback(ctx, zeroID, 1, false, ""); err == nil || !strings.Contains(err.Error(), "--none") {
		t.Errorf("Feedback on a zero-result search = %v, want an error suggesting --none", err)
	}
	if err := st.FeedbackNone(ctx, zeroID, "nothing about zebras"); err != nil {
		t.Fatalf("FeedbackNone on a zero-result search: %v", err)
	}
	useful, note, ok := feedbackRow(t, db, zeroID, store.WholeSearchRank)
	if !ok || useful != 0 || note.String != "nothing about zebras" {
		t.Errorf("rank-0 row = useful %d note %q present %t, want 0 / \"nothing about zebras\" / true", useful, note.String, ok)
	}

	// A second --none replaces the row (one row, new note; an empty note is NULL), not a PK error.
	if err := st.FeedbackNone(ctx, zeroID, ""); err != nil {
		t.Fatalf("FeedbackNone upsert: %v", err)
	}
	if got := count(t, db, "SELECT count(*) FROM search_feedback WHERE search_id = ?", zeroID); got != 1 {
		t.Errorf("feedback rows for the zero-result search = %d, want 1", got)
	}
	if _, note, _ := feedbackRow(t, db, zeroID, store.WholeSearchRank); note.Valid {
		t.Errorf("note after an empty-note upsert = %q, want NULL", note.String)
	}

	// A search with hits: --none coexists with not-useful verdicts on individual hits.
	hits, err := s.SearchAndLog(ctx, search.Request{Query: "widget", Mode: search.ModeFTS, K: 8, Caller: "test"})
	if err != nil {
		t.Fatalf("SearchAndLog hits: %v", err)
	}
	id := hits.Result.SearchID
	if len(hits.Returned) < 2 {
		t.Fatalf("hits fixture returned %d hits, want at least 2", len(hits.Returned))
	}
	if err := st.Feedback(ctx, id, 1, false, "wrong entry"); err != nil {
		t.Fatalf("Feedback not-useful: %v", err)
	}
	if err := st.FeedbackNone(ctx, id, "none of these"); err != nil {
		t.Fatalf("FeedbackNone after a not-useful verdict: %v", err)
	}
	if _, _, ok := feedbackRow(t, db, id, store.WholeSearchRank); !ok {
		t.Error("rank-0 row missing after FeedbackNone")
	}
	// A later not-useful verdict leaves the --none row in place: the two agree.
	if err := st.Feedback(ctx, id, 2, false, ""); err != nil {
		t.Fatalf("Feedback not-useful rank 2: %v", err)
	}
	if _, _, ok := feedbackRow(t, db, id, store.WholeSearchRank); !ok {
		t.Error("a not-useful verdict withdrew the --none row")
	}

	// A later useful verdict withdraws --none: rows (0), (1,false), (2,false) become (1,false),
	// (2,true).
	if err := st.Feedback(ctx, id, 2, true, "rank 2 did help"); err != nil {
		t.Fatalf("Feedback useful: %v", err)
	}
	if _, _, ok := feedbackRow(t, db, id, store.WholeSearchRank); ok {
		t.Error("the rank-0 row survived a useful verdict")
	}
	if got := count(t, db, "SELECT count(*) FROM search_feedback WHERE search_id = ?", id); got != 2 {
		t.Errorf("feedback rows = %d after the useful verdict, want 2", got)
	}

	// Now --none contradicts rank 2 and is refused, naming the command that resolves it, and
	// writes nothing.
	err = st.FeedbackNone(ctx, id, "")
	wantFix := fmt.Sprintf("kb feedback %d 2 --not-useful", id)
	if err == nil || !strings.Contains(err.Error(), wantFix) {
		t.Errorf("FeedbackNone with a useful verdict = %v, want it to name %q", err, wantFix)
	}
	if _, _, ok := feedbackRow(t, db, id, store.WholeSearchRank); ok {
		t.Error("a refused FeedbackNone still wrote the rank-0 row")
	}

	if err := st.FeedbackNone(ctx, id+999, ""); err == nil || !strings.Contains(err.Error(), fmt.Sprintf("no search with id %d", id+999)) {
		t.Errorf("FeedbackNone on an unknown search = %v, want a no-search error", err)
	}
	// The zero-result search's row is untouched by everything above.
	if got := count(t, db, "SELECT count(*) FROM search_feedback"); got != 3 {
		t.Errorf("total feedback rows = %d, want 3 (zero:0, hits:1, hits:2)", got)
	}
}

// TestLast: the newest search is per caller and by id, and a caller with no search is ErrNoSearch.
func TestLast(t *testing.T) {
	st, s := logFixture(t)
	ctx := context.Background()

	var ids []int64
	for _, req := range []search.Request{
		{Query: "widget alpha", Caller: "claude"},
		{Query: "widget beta", Caller: "codex"},
		{Query: "widget gamma", Caller: "claude"},
		{Query: "widget delta", Caller: "codex"},
	} {
		req.Mode, req.K = search.ModeFTS, 8
		out, err := s.SearchAndLog(ctx, req)
		if err != nil {
			t.Fatalf("SearchAndLog %q: %v", req.Query, err)
		}
		ids = append(ids, out.Result.SearchID)
	}

	// claude searched 1st and 3rd, codex 2nd and 4th: each caller's newest is its later search,
	// even though all four may share one ts second.
	for _, tc := range []struct {
		caller string
		wantID int64
		wantQ  string
	}{
		{"claude", ids[2], "widget gamma"},
		{"codex", ids[3], "widget delta"},
	} {
		got, err := st.LastSearch(ctx, tc.caller)
		if err != nil {
			t.Fatalf("Last(%q): %v", tc.caller, err)
		}
		if got.ID != tc.wantID || got.Query != tc.wantQ || got.NReturned == 0 || got.TS == "" {
			t.Errorf("Last(%q) = %+v, want id %d query %q with hits and a ts", tc.caller, got, tc.wantID, tc.wantQ)
		}
	}

	_, err := st.LastSearch(ctx, "nobody")
	if errs.KindOf(err) != errs.NotFound || !strings.Contains(err.Error(), `"nobody"`) {
		t.Errorf("Last(nobody) = %v, want ErrNoSearch naming the caller", err)
	}
}

// TestLogCascadeDelete: deleting a searches row takes its results, candidates and feedback with
// it, so the log can be pruned with one DELETE. (foreign_keys=ON is a store.Open pragma; this is
// also what proves it reaches the pooled connections.)
func TestLogCascadeDelete(t *testing.T) {
	st, s := logFixture(t)
	db := st.DB()
	ctx := context.Background()

	out, err := s.SearchAndLog(ctx, search.Request{Query: "widget", Mode: search.ModeHybrid, K: 8, Caller: "test"})
	if err != nil {
		t.Fatalf("SearchAndLog: %v", err)
	}
	id := out.Result.SearchID
	if err := st.Feedback(ctx, id, 1, true, "keep"); err != nil {
		t.Fatalf("Feedback: %v", err)
	}
	for _, table := range []string{"search_results", "search_candidates", "search_feedback"} {
		if n := count(t, db, "SELECT count(*) FROM "+table); n == 0 {
			t.Fatalf("%s is empty before the delete", table)
		}
	}

	if _, err := db.ExecContext(ctx, "DELETE FROM searches WHERE id = ?", id); err != nil {
		t.Fatalf("delete search: %v", err)
	}
	for _, table := range []string{"searches", "search_results", "search_candidates", "search_feedback"} {
		if n := count(t, db, "SELECT count(*) FROM "+table); n != 0 {
			t.Errorf("%s has %d rows after the parent search was deleted, want 0", table, n)
		}
	}
}

// TestLogSearchRanksAndReturnedFlag: results are stored at rank 1..n in the order given;
// candidates at fused_rank 1..n, with returned=1 exactly on the chunks that are also results.
func TestLogSearchRanksAndReturnedFlag(t *testing.T) {
	st, _ := logFixture(t)
	ctx := context.Background()
	ids := make([]int64, 0, 3)
	rows, err := st.DB().Query(`SELECT id FROM chunks ORDER BY id LIMIT 3`)
	if err != nil {
		t.Fatal(err)
	}
	for rows.Next() {
		var id int64
		if err := rows.Scan(&id); err != nil {
			t.Fatal(err)
		}
		ids = append(ids, id)
	}
	rows.Close()
	hit := func(id int64) store.LoggedHit { return store.LoggedHit{ChunkID: id, EntryPath: "logalpha.md"} }
	// shown: chunk 3 then chunk 1; fused: 1, 2, 3 (2 was dropped by the cap)
	searchID, err := st.LogSearch(ctx, store.SearchRecord{Query: "q", Mode: "fts", K: 2, Caller: "t",
		Results:    []store.LoggedHit{hit(ids[2]), hit(ids[0])},
		Candidates: []store.LoggedHit{hit(ids[0]), hit(ids[1]), hit(ids[2])}})
	if err != nil {
		t.Fatal(err)
	}
	read := func(q string) [][2]int64 {
		rs, err := st.DB().Query(q, searchID)
		if err != nil {
			t.Fatal(err)
		}
		defer rs.Close()
		var out [][2]int64
		for rs.Next() {
			var a, b int64
			if err := rs.Scan(&a, &b); err != nil {
				t.Fatal(err)
			}
			out = append(out, [2]int64{a, b})
		}
		return out
	}
	gotResults := read(`SELECT rank, chunk_id FROM search_results WHERE search_id = ? ORDER BY rank`)
	wantResults := [][2]int64{{1, ids[2]}, {2, ids[0]}}
	if fmt.Sprint(gotResults) != fmt.Sprint(wantResults) {
		t.Errorf("results (rank, chunk) = %v, want %v", gotResults, wantResults)
	}
	gotCands := read(`SELECT fused_rank, chunk_id FROM search_candidates WHERE search_id = ? ORDER BY fused_rank`)
	wantCands := [][2]int64{{1, ids[0]}, {2, ids[1]}, {3, ids[2]}}
	if fmt.Sprint(gotCands) != fmt.Sprint(wantCands) {
		t.Errorf("candidates (fused_rank, chunk) = %v, want %v", gotCands, wantCands)
	}
	gotFlags := read(`SELECT fused_rank, returned FROM search_candidates WHERE search_id = ? ORDER BY fused_rank`)
	wantFlags := [][2]int64{{1, 1}, {2, 0}, {3, 1}}
	if fmt.Sprint(gotFlags) != fmt.Sprint(wantFlags) {
		t.Errorf("candidates (fused_rank, returned) = %v, want %v", gotFlags, wantFlags)
	}
}

// settingsRow reads the three v4 columns of a search; each is nil when NULL.
func settingsRow(t *testing.T, st *Store, id int64) (settings, hash, ftsQuery *string) {
	t.Helper()
	var a, b, c sql.NullString
	if err := st.DB().QueryRow("SELECT settings, settings_hash, fts_query FROM searches WHERE id = ?", id).Scan(&a, &b, &c); err != nil {
		t.Fatal(err)
	}
	ptr := func(n sql.NullString) *string {
		if !n.Valid {
			return nil
		}
		return &n.String
	}
	return ptr(a), ptr(b), ptr(c)
}

// TestLogSearchSettingsColumns (PLAN 2.5): the settings JSON, its hash and the FTS expression are
// stored as given, and an empty value is NULL (not recorded), never "".
func TestLogSearchSettingsColumns(t *testing.T) {
	st, _ := logFixture(t)
	ctx := context.Background()
	for _, tc := range []struct {
		name             string
		rec              store.SearchRecord
		wantSettings     *string
		wantHash, wantFQ *string
	}{
		{"all three", store.SearchRecord{Settings: `{"a":1}`, SettingsHash: "abc", FTSMatch: `"x" AND "y"`}, ptrTo(`{"a":1}`), ptrTo("abc"), ptrTo(`"x" AND "y"`)},
		{"none given: NULL not empty", store.SearchRecord{}, nil, nil, nil},
		{"vec search has no FTS expression", store.SearchRecord{Settings: `{}`, SettingsHash: "h"}, ptrTo(`{}`), ptrTo("h"), nil},
	} {
		t.Run(tc.name, func(t *testing.T) {
			tc.rec.Query, tc.rec.Mode, tc.rec.K, tc.rec.Caller = "q", "fts", 5, "t"
			id, err := st.LogSearch(ctx, tc.rec)
			if err != nil {
				t.Fatal(err)
			}
			s, h, f := settingsRow(t, st, id)
			for _, c := range []struct {
				col       string
				got, want *string
			}{{"settings", s, tc.wantSettings}, {"settings_hash", h, tc.wantHash}, {"fts_query", f, tc.wantFQ}} {
				if (c.got == nil) != (c.want == nil) || (c.got != nil && *c.got != *c.want) {
					t.Errorf("%s = %v, want %v", c.col, deref(c.got), deref(c.want))
				}
			}
		})
	}
}

func ptrTo(s string) *string { return &s }

func deref(p *string) string {
	if p == nil {
		return "NULL"
	}
	return fmt.Sprintf("%q", *p)
}

// TestSearcherLogsSettingsAndMatch: a logged search carries the settings it ran with (the hash
// equals config.EffectiveJSON's) and the FTS expression; an fts search of a no-embedder searcher
// differs only in the mode, and a custom [search] changes the hash.
func TestSearcherLogsSettingsAndMatch(t *testing.T) {
	st, sr := logFixture(t)
	ctx := context.Background()

	def, defHash, err := config.Default().EffectiveJSON()
	if err != nil {
		t.Fatal(err)
	}
	out, err := sr.SearchAndLog(ctx, search.Request{Query: "widget alpha", Mode: search.ModeFTS, K: 3, Caller: "t"})
	if err != nil {
		t.Fatal(err)
	}
	s, h, f := settingsRow(t, st, out.Result.SearchID)
	if s == nil || *s != string(def) || h == nil || *h != defHash {
		t.Errorf("default searcher logged settings %v hash %v; want the default config's %s", deref(s), deref(h), defHash)
	}
	if f == nil || *f != out.FTSMatch || !strings.Contains(*f, "widget") {
		t.Errorf("fts_query = %v, want the expression the search ran (%q)", deref(f), out.FTSMatch)
	}

	cfg := config.Default()
	cfg.Search.RRFK = 20
	custom, err := search.NewSettings(cfg)
	if err != nil {
		t.Fatal(err)
	}
	sr.Settings = custom
	out, err = sr.SearchAndLog(ctx, search.Request{Query: "widget", Mode: search.ModeFTS, K: 3, Caller: "t"})
	if err != nil {
		t.Fatal(err)
	}
	_, h2, _ := settingsRow(t, st, out.Result.SearchID)
	wantJSON, wantHash, _ := cfg.EffectiveJSON()
	if h2 == nil || *h2 != wantHash || *h2 == defHash {
		t.Errorf("custom searcher logged hash %v, want %s (not the default %s)", deref(h2), wantHash, defHash)
	}
	if s2, _, _ := settingsRow(t, st, out.Result.SearchID); s2 == nil || *s2 != string(wantJSON) {
		t.Errorf("custom settings JSON = %v", deref(s2))
	}

	// A vec-only search runs no FTS list: no expression.
	out, err = sr.SearchAndLog(ctx, search.Request{Query: "widget", Mode: search.ModeVec, K: 3, Caller: "t"})
	if err != nil {
		t.Fatal(err)
	}
	if _, _, f := settingsRow(t, st, out.Result.SearchID); f != nil {
		t.Errorf("a vec search logged fts_query %v, want NULL", deref(f))
	}
}
