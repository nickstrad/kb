package search

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

	"knowledge/kb/internal/embed"
	"knowledge/kb/internal/embed/fake"
	"knowledge/kb/internal/store"
)

// logFixtures are three entries of five chunks each, every chunk containing "widget". A query for
// "widget" therefore matches all fifteen, which is what makes the log assertions meaningful: the
// fused list is longer than k, so search_candidates is strictly larger than search_results and the
// returned=1 flag has something to distinguish.
func logFixtures() []struct {
	path   string
	title  string
	tags   []string
	chunks []string
} {
	return []struct {
		path   string
		title  string
		tags   []string
		chunks []string
	}{
		{
			path:  "logalpha.md",
			title: "Log Alpha",
			tags:  []string{"linux"},
			chunks: []string{
				"Log Alpha: a widget entry about the alpha subsystem",
				"widget alpha one: the first section of the alpha entry",
				"widget alpha two: the second section of the alpha entry",
				"widget alpha three: the third section of the alpha entry",
				"widget alpha four: the fourth section of the alpha entry",
			},
		},
		{
			path:  "logbeta.md",
			title: "Log Beta",
			tags:  []string{"postgres"},
			chunks: []string{
				"Log Beta: a widget entry about the beta subsystem",
				"widget beta one: the first section of the beta entry",
				"widget beta two: the second section of the beta entry",
				"widget beta three: the third section of the beta entry",
				"widget beta four: the fourth section of the beta entry",
			},
		},
		{
			path:  "loggamma/README.md",
			title: "Log Gamma",
			tags:  []string{"docker"},
			chunks: []string{
				"Log Gamma: a widget entry about the gamma subsystem",
				"widget gamma one: the first section of the gamma entry",
				"widget gamma two: the second section of the gamma entry",
				"widget gamma three: the third section of the gamma entry",
				"widget gamma four: the fourth section of the gamma entry",
			},
		},
	}
}

// logSearcher builds a temp database, indexes logFixtures with the deterministic fake embedder at
// 768 dims (the width chunks_vec is declared with), and returns a Searcher wired to it. The
// vectors carry no meaning, so nothing here asserts on relevance — only on what gets logged.
func logSearcher(t *testing.T) *Searcher {
	t.Helper()

	emb := fake.New("fake-embed", 768)
	st, err := store.Open(filepath.Join(t.TempDir(), "kb.sqlite"))
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	t.Cleanup(func() { st.Close() })
	if err := st.EnsureEmbedMeta(emb.Model(), emb.Dim()); err != nil {
		t.Fatalf("embed meta: %v", err)
	}

	ctx := context.Background()
	for _, e := range logFixtures() {
		texts := make([]string, len(e.chunks))
		for j, text := range e.chunks {
			texts[j] = embed.DocPrefix + text
		}
		vecs, err := emb.Embed(ctx, texts)
		if err != nil {
			t.Fatalf("embed %s: %v", e.path, err)
		}
		chunks := make([]store.ChunkInput, len(e.chunks))
		for j, text := range e.chunks {
			heading := ""
			if j > 0 {
				heading = fmt.Sprintf("section %d", j)
			}
			sum := sha256.Sum256([]byte(text))
			chunks[j] = store.ChunkInput{
				Ord:        j,
				SourceFile: e.path,
				Heading:    heading,
				Text:       text,
				TextHash:   hex.EncodeToString(sum[:]),
				Embedding:  vecs[j],
			}
		}
		kind := "file"
		if strings.Contains(e.path, "/") {
			kind = "dir"
		}
		if _, err := st.ReplaceEntryChunks(ctx, store.EntryInput{
			Path:     e.path,
			Kind:     kind,
			Title:    e.title,
			Summary:  "fixture",
			Tags:     e.tags,
			BodyHash: "hash-" + e.path,
		}, chunks); err != nil {
			t.Fatalf("index %s: %v", e.path, err)
		}
	}

	return &Searcher{DB: st.DB(), Embedder: emb, KBVersion: "deadbee"}
}

// searchRow is one searches row, read back for the assertions below.
type searchRow struct {
	Mode       string
	K          int
	Caller     string
	NReturned  int
	NFTS       int
	NVec       int
	TagFilter  sql.NullString
	EmbedModel sql.NullString
	KBVersion  sql.NullString
	FTSMs      sql.NullInt64
	VecMs      sql.NullInt64
	EmbedMs    sql.NullInt64
	TotalMs    sql.NullInt64
}

func readSearch(t *testing.T, db *sql.DB, id int64) searchRow {
	t.Helper()
	var r searchRow
	err := db.QueryRow(`SELECT mode, k, caller, n_returned, n_fts, n_vec,
		tag_filter, embed_model, kb_version, fts_ms, vec_ms, embed_ms, total_ms
		FROM searches WHERE id = ?`, id).
		Scan(&r.Mode, &r.K, &r.Caller, &r.NReturned, &r.NFTS, &r.NVec,
			&r.TagFilter, &r.EmbedModel, &r.KBVersion,
			&r.FTSMs, &r.VecMs, &r.EmbedMs, &r.TotalMs)
	if err != nil {
		t.Fatalf("read searches row %d: %v", id, err)
	}
	return r
}

func count(t *testing.T, db *sql.DB, query string, args ...any) int {
	t.Helper()
	var n int
	if err := db.QueryRow(query, args...).Scan(&n); err != nil {
		t.Fatalf("%s: %v", query, err)
	}
	return n
}

// logIDs reads a set of integer ids (chunk ids, ranks) out of one of the log tables.
func logIDs(t *testing.T, db *sql.DB, query string, args ...any) map[int64]bool {
	t.Helper()
	rows, err := db.Query(query, args...)
	if err != nil {
		t.Fatalf("%s: %v", query, err)
	}
	defer rows.Close()
	out := map[int64]bool{}
	for rows.Next() {
		var id int64
		if err := rows.Scan(&id); err != nil {
			t.Fatalf("scan: %v", err)
		}
		out[id] = true
	}
	if err := rows.Err(); err != nil {
		t.Fatalf("rows: %v", err)
	}
	return out
}

// TestSearchAndLogPerMode is the core of P3.3: every mode, including the degraded one, writes a
// complete and self-consistent set of log rows.
func TestSearchAndLogPerMode(t *testing.T) {
	for _, tc := range []struct {
		name        string
		mode        string
		nilEmbedder bool
		wantMode    string
		wantFTS     bool // the FTS list ran
		wantVec     bool // the vector list ran
	}{
		{name: "hybrid", mode: ModeHybrid, wantMode: ModeHybrid, wantFTS: true, wantVec: true},
		{name: "fts", mode: ModeFTS, wantMode: ModeFTS, wantFTS: true},
		{name: "vec", mode: ModeVec, wantMode: ModeVec, wantVec: true},
		{name: "hybrid without embedder", mode: ModeHybrid, nilEmbedder: true, wantMode: ModeFTSFallback, wantFTS: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			s := logSearcher(t)
			if tc.nilEmbedder {
				s.Embedder = nil
			}
			db := s.DB

			req := Request{Query: "widget", Mode: tc.mode, K: 8, Caller: "test-caller"}
			out, err := s.SearchAndLog(context.Background(), req)
			if err != nil {
				t.Fatalf("SearchAndLog: %v", err)
			}
			id := out.Result.SearchID
			if id == 0 {
				t.Fatal("Result.SearchID was not set")
			}

			if n := count(t, db, "SELECT count(*) FROM searches"); n != 1 {
				t.Fatalf("searches rows = %d, want 1", n)
			}
			row := readSearch(t, db, id)
			if row.Mode != tc.wantMode {
				t.Errorf("mode = %q, want %q", row.Mode, tc.wantMode)
			}
			if row.K != 8 {
				t.Errorf("k = %d, want 8", row.K)
			}
			if row.Caller != "test-caller" {
				t.Errorf("caller = %q, want %q", row.Caller, "test-caller")
			}
			if row.NReturned != len(out.Returned) {
				t.Errorf("n_returned = %d, want %d", row.NReturned, len(out.Returned))
			}
			if len(out.Returned) == 0 {
				t.Fatal("search returned nothing; the fixtures cannot exercise the log")
			}
			if tc.wantFTS && row.NFTS == 0 {
				t.Error("n_fts = 0, want the FTS list to have run")
			}
			if !tc.wantFTS && row.NFTS != 0 {
				t.Errorf("n_fts = %d, want 0", row.NFTS)
			}
			if tc.wantVec && row.NVec == 0 {
				t.Error("n_vec = 0, want the vector list to have run")
			}
			if !tc.wantVec && row.NVec != 0 {
				t.Errorf("n_vec = %d, want 0", row.NVec)
			}
			if row.TagFilter.Valid {
				t.Errorf("tag_filter = %q, want NULL", row.TagFilter.String)
			}
			if tc.wantVec != row.EmbedModel.Valid {
				t.Errorf("embed_model valid = %v, want %v", row.EmbedModel.Valid, tc.wantVec)
			}
			if row.KBVersion.String != "deadbee" {
				t.Errorf("kb_version = %q, want %q", row.KBVersion.String, "deadbee")
			}
			if !row.TotalMs.Valid {
				t.Error("total_ms is NULL")
			}

			// Row counts mirror the outcome exactly.
			if n := count(t, db, "SELECT count(*) FROM search_results WHERE search_id = ?", id); n != len(out.Returned) {
				t.Errorf("search_results rows = %d, want %d", n, len(out.Returned))
			}
			if n := count(t, db, "SELECT count(*) FROM search_candidates WHERE search_id = ?", id); n != len(out.Candidates) {
				t.Errorf("search_candidates rows = %d, want %d", n, len(out.Candidates))
			}
			if len(out.Candidates) <= len(out.Returned) {
				t.Errorf("fixtures produced %d candidates for %d returned; the cap/k truncation is not being exercised",
					len(out.Candidates), len(out.Returned))
			}

			// returned=1 is exactly the returned set.
			nFlagged := count(t, db, "SELECT count(*) FROM search_candidates WHERE search_id = ? AND returned = 1", id)
			if nFlagged != len(out.Returned) {
				t.Errorf("candidates with returned=1 = %d, want %d", nFlagged, len(out.Returned))
			}
			resultIDs := logIDs(t, db, "SELECT chunk_id FROM search_results WHERE search_id = ?", id)
			flaggedIDs := logIDs(t, db, "SELECT chunk_id FROM search_candidates WHERE search_id = ? AND returned = 1", id)
			if len(resultIDs) != len(flaggedIDs) {
				t.Fatalf("distinct chunk ids: results %d, flagged candidates %d", len(resultIDs), len(flaggedIDs))
			}
			for cid := range resultIDs {
				if !flaggedIDs[cid] {
					t.Errorf("chunk %d is in search_results but not flagged returned=1", cid)
				}
			}

			// Ranks are 1..n with no gaps, and paths/headings are copied, not left empty.
			ranks := logIDs(t, db, "SELECT rank FROM search_results WHERE search_id = ?", id)
			for want := 1; want <= len(out.Returned); want++ {
				if !ranks[int64(want)] {
					t.Errorf("search_results is missing rank %d", want)
				}
			}
			if n := count(t, db, "SELECT count(*) FROM search_results WHERE search_id = ? AND entry_path = ''", id); n != 0 {
				t.Errorf("%d search_results rows have an empty entry_path", n)
			}
			if n := count(t, db, "SELECT count(*) FROM search_candidates WHERE search_id = ? AND fused_rank BETWEEN 1 AND ?", id, len(out.Candidates)); n != len(out.Candidates) {
				t.Errorf("fused_rank is not 1..%d", len(out.Candidates))
			}
		})
	}
}

// TestLogStageTimingsNullPerMode: a stage that never ran is logged as NULL, not as 0 ms, so that
// avg(embed_ms) is the average over the searches that actually embedded something. fts-fallback is
// the interesting case — the embedder was called and timed, and only then failed, so embed_ms is
// real while vec_ms is NULL.
func TestLogStageTimingsNullPerMode(t *testing.T) {
	for _, tc := range []struct {
		name                          string
		mode                          string
		nilEmbedder                   bool
		wantFTS, wantVec, wantEmbedMs bool
	}{
		{name: "hybrid", mode: ModeHybrid, wantFTS: true, wantVec: true, wantEmbedMs: true},
		{name: "fts", mode: ModeFTS, wantFTS: true},
		{name: "vec", mode: ModeVec, wantVec: true, wantEmbedMs: true},
		{name: "fts-fallback", mode: ModeHybrid, nilEmbedder: true, wantFTS: true, wantEmbedMs: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			s := logSearcher(t)
			if tc.nilEmbedder {
				s.Embedder = nil
			}
			out, err := s.SearchAndLog(context.Background(), Request{Query: "widget", Mode: tc.mode, K: 8})
			if err != nil {
				t.Fatalf("SearchAndLog: %v", err)
			}
			row := readSearch(t, s.DB, out.Result.SearchID)
			for _, col := range []struct {
				name string
				got  sql.NullInt64
				want bool
			}{
				{"fts_ms", row.FTSMs, tc.wantFTS},
				{"vec_ms", row.VecMs, tc.wantVec},
				{"embed_ms", row.EmbedMs, tc.wantEmbedMs},
			} {
				if col.got.Valid != col.want {
					state := "NULL"
					if col.got.Valid {
						state = fmt.Sprintf("%d", col.got.Int64)
					}
					wanted := "NULL"
					if col.want {
						wanted = "a value"
					}
					t.Errorf("%s = %s, want %s", col.name, state, wanted)
				}
			}
			if !row.TotalMs.Valid {
				t.Error("total_ms is NULL; every search has a total")
			}
		})
	}
}

// TestLogNullableColumns pins what has to be NULL rather than zero: no tag filter, no embedder,
// and the per-list rank/score columns of a chunk that appeared in only one list.
func TestLogNullableColumns(t *testing.T) {
	s := logSearcher(t)
	db := s.DB

	// A vec-only search: every logged row must have NULL fts_rank and fts_score.
	out, err := s.SearchAndLog(context.Background(), Request{Query: "widget", Mode: ModeVec, K: 5, Tag: "linux"})
	if err != nil {
		t.Fatalf("SearchAndLog: %v", err)
	}
	id := out.Result.SearchID

	row := readSearch(t, db, id)
	if !row.TagFilter.Valid || row.TagFilter.String != "linux" {
		t.Errorf("tag_filter = %v, want linux", row.TagFilter)
	}
	if !row.EmbedModel.Valid || row.EmbedModel.String != "fake-embed" {
		t.Errorf("embed_model = %v, want fake-embed", row.EmbedModel)
	}
	if row.Caller != "unknown" {
		t.Errorf("caller = %q, want unknown (the empty-Caller default)", row.Caller)
	}
	if n := count(t, db, "SELECT count(*) FROM search_results WHERE search_id = ? AND (fts_rank IS NOT NULL OR fts_score IS NOT NULL)", id); n != 0 {
		t.Errorf("%d vec-only results carry an FTS rank or score", n)
	}
	if n := count(t, db, "SELECT count(*) FROM search_results WHERE search_id = ? AND vec_rank IS NULL", id); n != 0 {
		t.Errorf("%d vec-only results are missing their vec_rank", n)
	}

	// An fts-only search is the mirror image.
	out2, err := s.SearchAndLog(context.Background(), Request{Query: "widget", Mode: ModeFTS, K: 5})
	if err != nil {
		t.Fatalf("SearchAndLog: %v", err)
	}
	id2 := out2.Result.SearchID
	row2 := readSearch(t, db, id2)
	if row2.EmbedModel.Valid {
		t.Errorf("embed_model = %q on an fts search, want NULL", row2.EmbedModel.String)
	}
	if n := count(t, db, "SELECT count(*) FROM search_results WHERE search_id = ? AND (vec_rank IS NOT NULL OR vec_distance IS NOT NULL)", id2); n != 0 {
		t.Errorf("%d fts-only results carry a vector rank or distance", n)
	}
}

// TestSearchLogModeBreakdown is the P3 acceptance query: `select mode, count(*) from searches
// group by 1` shows every mode that ran.
func TestSearchLogModeBreakdown(t *testing.T) {
	s := logSearcher(t)
	db := s.DB
	ctx := context.Background()

	for _, mode := range []string{ModeHybrid, ModeFTS, ModeVec} {
		if _, err := s.SearchAndLog(ctx, Request{Query: "widget", Mode: mode, K: 8, Caller: "test"}); err != nil {
			t.Fatalf("search %s: %v", mode, err)
		}
	}
	s.Embedder = nil
	if _, err := s.SearchAndLog(ctx, Request{Query: "widget", Mode: ModeHybrid, K: 8, Caller: "test"}); err != nil {
		t.Fatalf("fallback search: %v", err)
	}

	rows, err := db.Query("SELECT mode, count(*) FROM searches GROUP BY 1 ORDER BY 1")
	if err != nil {
		t.Fatalf("group by mode: %v", err)
	}
	defer rows.Close()
	got := map[string]int{}
	for rows.Next() {
		var mode string
		var n int
		if err := rows.Scan(&mode, &n); err != nil {
			t.Fatalf("scan: %v", err)
		}
		got[mode] = n
	}
	if err := rows.Err(); err != nil {
		t.Fatalf("rows: %v", err)
	}
	want := map[string]int{ModeHybrid: 1, ModeFTS: 1, ModeVec: 1, ModeFTSFallback: 1}
	if len(got) != len(want) {
		t.Fatalf("modes = %v, want %v", got, want)
	}
	for mode, n := range want {
		if got[mode] != n {
			t.Errorf("mode %s: %d searches, want %d", mode, got[mode], n)
		}
	}
}

// TestSearchNotLoggedOnError: a search that fails writes nothing, so the log never contains a
// searches row for an answer nobody received.
func TestSearchNotLoggedOnError(t *testing.T) {
	s := logSearcher(t)
	if _, err := s.SearchAndLog(context.Background(), Request{Query: "widget", Mode: "sideways", K: 8}); err == nil {
		t.Fatal("SearchAndLog with an unknown mode returned no error")
	}
	if n := count(t, s.DB, "SELECT count(*) FROM searches"); n != 0 {
		t.Errorf("searches rows = %d after a failed search, want 0", n)
	}
}

// TestFeedback covers the happy path, the upsert, and every way a rank can be wrong.
func TestFeedback(t *testing.T) {
	s := logSearcher(t)
	db := s.DB
	ctx := context.Background()

	out, err := s.SearchAndLog(ctx, Request{Query: "widget", Mode: ModeHybrid, K: 8, Caller: "test"})
	if err != nil {
		t.Fatalf("SearchAndLog: %v", err)
	}
	id := out.Result.SearchID
	n := len(out.Returned)

	if err := s.Feedback(ctx, id, 1, true, "exactly what I needed"); err != nil {
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
	if err := s.Feedback(ctx, id, n, false, ""); err != nil {
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
	if err := s.Feedback(ctx, id, 1, false, "changed my mind"); err != nil {
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
			err := s.Feedback(ctx, tc.searchID, tc.rank, true, "")
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
	s := logSearcher(t)
	db := s.DB
	ctx := context.Background()

	// A zero-result search: no fixture chunk contains this token, and fts never embeds.
	zero, err := s.SearchAndLog(ctx, Request{Query: "nonexistentzebra", Mode: ModeFTS, K: 8, Caller: "test"})
	if err != nil {
		t.Fatalf("SearchAndLog zero: %v", err)
	}
	if len(zero.Returned) != 0 {
		t.Fatalf("zero-result fixture returned %d hits", len(zero.Returned))
	}
	zeroID := zero.Result.SearchID

	// Its ranks cannot be marked, and the error points at --none.
	if err := s.Feedback(ctx, zeroID, 1, false, ""); err == nil || !strings.Contains(err.Error(), "--none") {
		t.Errorf("Feedback on a zero-result search = %v, want an error suggesting --none", err)
	}
	if err := s.FeedbackNone(ctx, zeroID, "nothing about zebras"); err != nil {
		t.Fatalf("FeedbackNone on a zero-result search: %v", err)
	}
	useful, note, ok := feedbackRow(t, db, zeroID, WholeSearchRank)
	if !ok || useful != 0 || note.String != "nothing about zebras" {
		t.Errorf("rank-0 row = useful %d note %q present %t, want 0 / \"nothing about zebras\" / true", useful, note.String, ok)
	}

	// A second --none replaces the row (one row, new note; an empty note is NULL), not a PK error.
	if err := s.FeedbackNone(ctx, zeroID, ""); err != nil {
		t.Fatalf("FeedbackNone upsert: %v", err)
	}
	if got := count(t, db, "SELECT count(*) FROM search_feedback WHERE search_id = ?", zeroID); got != 1 {
		t.Errorf("feedback rows for the zero-result search = %d, want 1", got)
	}
	if _, note, _ := feedbackRow(t, db, zeroID, WholeSearchRank); note.Valid {
		t.Errorf("note after an empty-note upsert = %q, want NULL", note.String)
	}

	// A search with hits: --none coexists with not-useful verdicts on individual hits.
	hits, err := s.SearchAndLog(ctx, Request{Query: "widget", Mode: ModeFTS, K: 8, Caller: "test"})
	if err != nil {
		t.Fatalf("SearchAndLog hits: %v", err)
	}
	id := hits.Result.SearchID
	if len(hits.Returned) < 2 {
		t.Fatalf("hits fixture returned %d hits, want at least 2", len(hits.Returned))
	}
	if err := s.Feedback(ctx, id, 1, false, "wrong entry"); err != nil {
		t.Fatalf("Feedback not-useful: %v", err)
	}
	if err := s.FeedbackNone(ctx, id, "none of these"); err != nil {
		t.Fatalf("FeedbackNone after a not-useful verdict: %v", err)
	}
	if _, _, ok := feedbackRow(t, db, id, WholeSearchRank); !ok {
		t.Error("rank-0 row missing after FeedbackNone")
	}
	// A later not-useful verdict leaves the --none row in place: the two agree.
	if err := s.Feedback(ctx, id, 2, false, ""); err != nil {
		t.Fatalf("Feedback not-useful rank 2: %v", err)
	}
	if _, _, ok := feedbackRow(t, db, id, WholeSearchRank); !ok {
		t.Error("a not-useful verdict withdrew the --none row")
	}

	// A later useful verdict withdraws --none: rows (0), (1,false), (2,false) become (1,false),
	// (2,true).
	if err := s.Feedback(ctx, id, 2, true, "rank 2 did help"); err != nil {
		t.Fatalf("Feedback useful: %v", err)
	}
	if _, _, ok := feedbackRow(t, db, id, WholeSearchRank); ok {
		t.Error("the rank-0 row survived a useful verdict")
	}
	if got := count(t, db, "SELECT count(*) FROM search_feedback WHERE search_id = ?", id); got != 2 {
		t.Errorf("feedback rows = %d after the useful verdict, want 2", got)
	}

	// Now --none contradicts rank 2 and is refused, naming the command that resolves it, and
	// writes nothing.
	err = s.FeedbackNone(ctx, id, "")
	wantFix := fmt.Sprintf("kb feedback %d 2 --not-useful", id)
	if err == nil || !strings.Contains(err.Error(), wantFix) {
		t.Errorf("FeedbackNone with a useful verdict = %v, want it to name %q", err, wantFix)
	}
	if _, _, ok := feedbackRow(t, db, id, WholeSearchRank); ok {
		t.Error("a refused FeedbackNone still wrote the rank-0 row")
	}

	if err := s.FeedbackNone(ctx, id+999, ""); err == nil || !strings.Contains(err.Error(), fmt.Sprintf("no search with id %d", id+999)) {
		t.Errorf("FeedbackNone on an unknown search = %v, want a no-search error", err)
	}
	// The zero-result search's row is untouched by everything above.
	if got := count(t, db, "SELECT count(*) FROM search_feedback"); got != 3 {
		t.Errorf("total feedback rows = %d, want 3 (zero:0, hits:1, hits:2)", got)
	}
}

// TestLast: the newest search is per caller and by id, and a caller with no search is ErrNoSearch.
func TestLast(t *testing.T) {
	s := logSearcher(t)
	ctx := context.Background()

	var ids []int64
	for _, req := range []Request{
		{Query: "widget alpha", Caller: "claude"},
		{Query: "widget beta", Caller: "codex"},
		{Query: "widget gamma", Caller: "claude"},
		{Query: "widget delta", Caller: "codex"},
	} {
		req.Mode, req.K = ModeFTS, 8
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
		got, err := s.Last(ctx, tc.caller)
		if err != nil {
			t.Fatalf("Last(%q): %v", tc.caller, err)
		}
		if got.ID != tc.wantID || got.Query != tc.wantQ || got.NReturned == 0 || got.TS == "" {
			t.Errorf("Last(%q) = %+v, want id %d query %q with hits and a ts", tc.caller, got, tc.wantID, tc.wantQ)
		}
	}

	_, err := s.Last(ctx, "nobody")
	if !errors.Is(err, ErrNoSearch) || !strings.Contains(err.Error(), `"nobody"`) {
		t.Errorf("Last(nobody) = %v, want ErrNoSearch naming the caller", err)
	}
}

// TestLogCascadeDelete: deleting a searches row takes its results, candidates and feedback with
// it, so the log can be pruned with one DELETE. (foreign_keys=ON is a store.Open pragma; this is
// also what proves it reaches the pooled connections.)
func TestLogCascadeDelete(t *testing.T) {
	s := logSearcher(t)
	db := s.DB
	ctx := context.Background()

	out, err := s.SearchAndLog(ctx, Request{Query: "widget", Mode: ModeHybrid, K: 8, Caller: "test"})
	if err != nil {
		t.Fatalf("SearchAndLog: %v", err)
	}
	id := out.Result.SearchID
	if err := s.Feedback(ctx, id, 1, true, "keep"); err != nil {
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

// TestLogRollbackOnFailure: when a write partway through the log transaction fails, nothing is
// left behind — no searches row, no results — and the caller still gets its hits back with an
// error that identifies itself as a logging failure. A trigger that aborts every
// search_candidates insert stands in for the real causes (a locked or full database), which are
// hard to provoke on demand.
func TestLogRollbackOnFailure(t *testing.T) {
	s := logSearcher(t)
	db := s.DB

	if _, err := db.Exec(`CREATE TRIGGER boom BEFORE INSERT ON search_candidates
		BEGIN SELECT RAISE(ABORT, 'boom'); END`); err != nil {
		t.Fatalf("create trigger: %v", err)
	}

	out, err := s.SearchAndLog(context.Background(), Request{Query: "widget", Mode: ModeHybrid, K: 8, Caller: "test"})
	if err == nil {
		t.Fatal("SearchAndLog succeeded with a failing log write")
	}
	if !errors.Is(err, ErrLogFailed) {
		t.Errorf("error %q does not match ErrLogFailed", err)
	}
	if out == nil {
		t.Fatal("outcome is nil; the search itself succeeded and its hits must still be usable")
	}
	if len(out.Returned) == 0 {
		t.Error("outcome has no hits")
	}
	if out.Result.SearchID != 0 {
		t.Errorf("Result.SearchID = %d, want 0 after a rolled-back log write", out.Result.SearchID)
	}
	for _, table := range []string{"searches", "search_results", "search_candidates"} {
		if n := count(t, db, "SELECT count(*) FROM "+table); n != 0 {
			t.Errorf("%s has %d rows after the failed log write, want 0", table, n)
		}
	}
}

// TestGitShortHash: a real repository yields a hash, anything else yields "" rather than an error.
func TestGitShortHash(t *testing.T) {
	if got := GitShortHash(t.TempDir()); got != "" {
		t.Errorf("GitShortHash(non-repo) = %q, want \"\"", got)
	}
	if got := GitShortHash("/nonexistent-path-for-kb-test"); got != "" {
		t.Errorf("GitShortHash(missing dir) = %q, want \"\"", got)
	}
	// The module lives inside the knowledge repo, so the working directory is a git checkout
	// unless the tree was exported without .git; skip rather than fail in that case.
	if got := GitShortHash("."); got == "" {
		t.Skip("not running inside a git checkout")
	} else if len(got) < 4 || strings.ContainsAny(got, " \n") {
		t.Errorf("GitShortHash(.) = %q, want a bare short hash", got)
	}
}
