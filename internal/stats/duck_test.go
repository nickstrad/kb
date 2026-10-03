package stats

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/nickstrad/kb/internal/embed"
	"github.com/nickstrad/kb/internal/embed/fake"
	"github.com/nickstrad/kb/internal/search"
	"github.com/nickstrad/kb/internal/store"
)

// fixture is a real SQLite database (built with store.Open, the entry/chunk write API and
// search.Searcher.Log/Feedback) whose contents are known precisely enough to assert Load's output
// column by column. Search timestamps are stamped by Log() as time.Now(), which is useless for a
// Since-filter boundary test, so setSearchTS rewrites them afterwards with a direct UPDATE.
type fixture struct {
	st     *store.Store
	dbPath string

	// Search ids, named for what distinguishes them.
	old      int64 // caller alice, ts before cutoff: dropped by --since
	newAlice int64 // caller alice, ts at/after cutoff
	newBob   int64 // caller bob, ts after cutoff

	cutoff time.Time // equals newAlice's ts, so the Since test also proves the boundary is inclusive
}

// buildFixture indexes three entries (alpha.md, beta.md, gamma.md — gamma is never returned by
// any search, exercising "entries is copied whole regardless of the filter") and logs three
// searches by hand, exercising every column and every NULL case the schema allows:
//
//   - old: hybrid, kb_version set, tag_filter set, a "both" hit (rank 1) and an "fts-only" hit
//     (rank 2, NULL vec_rank/vec_distance) in search_results, plus a "vec-only" candidate that was
//     never returned (NULL fts_rank/fts_score, returned=false). Feedback: rank 1 useful with a
//     note, rank 2 not useful with an empty (NULL) note.
//   - newAlice: fts, so vec_ms/embed_ms are NULL (the mode never ran those stages) and embed_model
//     is NULL; kb_version NULL (Searcher.KBVersion unset); no feedback at all (an unjudged search).
//   - newBob: vec, so fts_ms is NULL; different caller, for the --caller filter.
func buildFixture(t *testing.T) *fixture {
	t.Helper()
	dbPath := filepath.Join(t.TempDir(), "kb.sqlite")
	st, err := store.Open(dbPath)
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	t.Cleanup(func() { st.Close() })

	emb := fake.New("fake-embed", 768)
	if err := st.EnsureEmbedMeta(emb.Model(), emb.Dim()); err != nil {
		t.Fatalf("embed meta: %v", err)
	}
	ctx := context.Background()
	indexEntry(t, st, emb, "alpha.md", "Alpha", []string{"linux"}, []string{"alpha chunk one", "alpha chunk two"})
	indexEntry(t, st, emb, "beta.md", "Beta", nil, []string{"beta chunk one"})
	indexEntry(t, st, emb, "gamma.md", "Gamma", []string{"docker", "postgres"}, []string{"gamma chunk one"})

	f := &fixture{st: st, dbPath: dbPath}

	// old: caller alice, hybrid, full column coverage, ts stamped before the cutoff.
	sAlice := &search.Searcher{DB: st.DB(), KBVersion: "cafefeed"}
	oldOut := &search.Outcome{
		Mode: search.ModeHybrid, K: 8, TagFilter: "linux", EmbedModel: "fake-embed",
		NFTS: 5, NVec: 6, FTSMs: 12, VecMs: 34, EmbedMs: 7, TotalMs: 60,
		Returned: []search.Candidate{
			{ChunkID: 101, FTSRank: pInt(1), FTSScore: pFloat(-1.5), VecRank: pInt(2), VecDistance: pFloat(0.31),
				RRFScore: 0.9, EntryPath: "alpha.md", Heading: "Section 1"},
			{ChunkID: 102, FTSRank: pInt(2), FTSScore: pFloat(-1.1), VecRank: nil, VecDistance: nil,
				RRFScore: 0.5, EntryPath: "alpha.md", Heading: ""},
		},
	}
	oldOut.Candidates = append(append([]search.Candidate{}, oldOut.Returned...), search.Candidate{
		ChunkID: 103, FTSRank: nil, FTSScore: nil, VecRank: pInt(1), VecDistance: pFloat(0.12),
		RRFScore: 0.8, EntryPath: "beta.md", Heading: "Section A",
	})
	f.old = logSearch(t, sAlice, search.Request{Query: "alpha widget", Caller: "alice"}, oldOut)
	if err := sAlice.Feedback(ctx, f.old, 1, true, "great match"); err != nil {
		t.Fatalf("feedback: %v", err)
	}
	if err := sAlice.Feedback(ctx, f.old, 2, false, ""); err != nil {
		t.Fatalf("feedback: %v", err)
	}
	oldTS := time.Date(2020, 1, 1, 0, 0, 0, 0, time.UTC)
	setSearchTS(t, st.DB(), f.old, oldTS)

	// newAlice: fts, no embedder stage, no feedback (unjudged), ts is the cutoff itself.
	sAlice2 := &search.Searcher{DB: st.DB()} // no KBVersion: kb_version must come back NULL
	newAliceOut := &search.Outcome{
		Mode: search.ModeFTS, K: 5, NFTS: 3, NVec: 0, FTSMs: 9, TotalMs: 15,
		Returned: []search.Candidate{
			{ChunkID: 104, FTSRank: pInt(1), FTSScore: pFloat(-0.7), RRFScore: 0.6, EntryPath: "beta.md", Heading: ""},
		},
	}
	newAliceOut.Candidates = append([]search.Candidate{}, newAliceOut.Returned...)
	f.newAlice = logSearch(t, sAlice2, search.Request{Query: "beta widget", Caller: "alice"}, newAliceOut)
	f.cutoff = time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	setSearchTS(t, st.DB(), f.newAlice, f.cutoff)

	// newBob: vec, different caller, ts after the cutoff.
	sBob := &search.Searcher{DB: st.DB()}
	newBobOut := &search.Outcome{
		Mode: search.ModeVec, K: 6, NFTS: 0, NVec: 4, EmbedModel: "fake-embed", VecMs: 22, EmbedMs: 9, TotalMs: 31,
		Returned: []search.Candidate{
			{ChunkID: 105, VecRank: pInt(1), VecDistance: pFloat(0.05), RRFScore: 0.7, EntryPath: "alpha.md", Heading: "Section 1"},
		},
	}
	newBobOut.Candidates = append([]search.Candidate{}, newBobOut.Returned...)
	f.newBob = logSearch(t, sBob, search.Request{Query: "alpha widget again", Caller: "bob"}, newBobOut)
	setSearchTS(t, st.DB(), f.newBob, f.cutoff.Add(5*time.Second))

	return f
}

func indexEntry(t *testing.T, st *store.Store, emb *fake.Embedder, path, title string, tags []string, chunkTexts []string) {
	t.Helper()
	ctx := context.Background()
	chunks := make([]store.ChunkInput, len(chunkTexts))
	for i, text := range chunkTexts {
		vecs, err := emb.Embed(ctx, []string{embed.DocPrefix + text})
		if err != nil {
			t.Fatalf("embed %s chunk %d: %v", path, i, err)
		}
		sum := sha256.Sum256([]byte(text))
		chunks[i] = store.ChunkInput{
			Ord: i, SourceFile: path, Heading: "", Text: text,
			TextHash: hex.EncodeToString(sum[:]), Embedding: vecs[0],
		}
	}
	if _, err := st.ReplaceEntryChunks(ctx, store.EntryInput{
		Path: path, Kind: "file", Title: title, Summary: "fixture", Tags: tags, BodyHash: "hash-" + path,
	}, chunks); err != nil {
		t.Fatalf("index %s: %v", path, err)
	}
}

func logSearch(t *testing.T, s *search.Searcher, req search.Request, out *search.Outcome) int64 {
	t.Helper()
	id, err := s.Log(context.Background(), req, out)
	if err != nil {
		t.Fatalf("log search: %v", err)
	}
	return id
}

// setSearchTS overwrites a logged search's ts, since Log() always stamps time.Now() and the
// Since-filter tests need exact, comparable timestamps.
func setSearchTS(t *testing.T, db *sql.DB, id int64, ts time.Time) {
	t.Helper()
	if _, err := db.Exec(`UPDATE searches SET ts = ? WHERE id = ?`, ts.UTC().Format(time.RFC3339), id); err != nil {
		t.Fatalf("set ts for search %d: %v", id, err)
	}
}

func pInt(v int) *int           { return &v }
func pFloat(v float64) *float64 { return &v }

// col looks up a column by name in a Table and returns its index, failing the test if absent.
func colIndex(t *testing.T, tbl *Table, name string) int {
	t.Helper()
	for i, c := range tbl.Columns {
		if c == name {
			return i
		}
	}
	t.Fatalf("column %q not found in %v", name, tbl.Columns)
	return -1
}

// rowByID returns the row of tbl whose "id"-named column (idCol) equals id, as a map from column
// name to normalised value.
func rowByID(t *testing.T, tbl *Table, idCol string, id int64) map[string]any {
	t.Helper()
	idx := colIndex(t, tbl, idCol)
	for _, row := range tbl.Rows {
		if got, ok := row[idx].(int64); ok && got == id {
			m := make(map[string]any, len(tbl.Columns))
			for i, c := range tbl.Columns {
				m[c] = row[i]
			}
			return m
		}
	}
	t.Fatalf("no row with %s = %d in columns %v", idCol, id, tbl.Columns)
	return nil
}

func mustQuery(t *testing.T, db *DB, sqlText string) *Table {
	t.Helper()
	tbl, err := db.Query(context.Background(), sqlText)
	if err != nil {
		t.Fatalf("query %q: %v", sqlText, err)
	}
	return tbl
}

// TestLoadSearchesRoundTrip checks every column of the searches table, including the columns that
// must come back NULL for a given mode (embed_model/vec_ms/embed_ms for fts, kb_version when the
// Searcher never set one, tag_filter when the request had none).
func TestLoadSearchesRoundTrip(t *testing.T) {
	f := buildFixture(t)
	db, err := Load(context.Background(), f.dbPath, Filter{})
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	defer db.Close()

	tbl := mustQuery(t, db, `SELECT id, ts, query, mode, k, tag_filter, embed_model,
		n_fts, n_vec, n_returned, fts_ms, vec_ms, embed_ms, total_ms, caller, kb_version
		FROM searches ORDER BY id`)

	old := rowByID(t, tbl, "id", f.old)
	wantOldTS := time.Date(2020, 1, 1, 0, 0, 0, 0, time.UTC)
	if ts, ok := old["ts"].(time.Time); !ok || !ts.Equal(wantOldTS) {
		t.Errorf("old.ts = %v, want %v", old["ts"], wantOldTS)
	}
	if old["query"] != "alpha widget" {
		t.Errorf("old.query = %v", old["query"])
	}
	if old["mode"] != "hybrid" {
		t.Errorf("old.mode = %v", old["mode"])
	}
	if old["k"] != int64(8) {
		t.Errorf("old.k = %v (%T), want int64(8)", old["k"], old["k"])
	}
	if old["tag_filter"] != "linux" {
		t.Errorf("old.tag_filter = %v", old["tag_filter"])
	}
	if old["embed_model"] != "fake-embed" {
		t.Errorf("old.embed_model = %v", old["embed_model"])
	}
	if old["n_fts"] != int64(5) || old["n_vec"] != int64(6) || old["n_returned"] != int64(2) {
		t.Errorf("old n_fts/n_vec/n_returned = %v/%v/%v", old["n_fts"], old["n_vec"], old["n_returned"])
	}
	if old["fts_ms"] != int64(12) || old["vec_ms"] != int64(34) || old["embed_ms"] != int64(7) || old["total_ms"] != int64(60) {
		t.Errorf("old timings = %v/%v/%v/%v", old["fts_ms"], old["vec_ms"], old["embed_ms"], old["total_ms"])
	}
	if old["caller"] != "alice" {
		t.Errorf("old.caller = %v", old["caller"])
	}
	if old["kb_version"] != "cafefeed" {
		t.Errorf("old.kb_version = %v", old["kb_version"])
	}

	newAlice := rowByID(t, tbl, "id", f.newAlice)
	if newAlice["mode"] != "fts" {
		t.Errorf("newAlice.mode = %v", newAlice["mode"])
	}
	if newAlice["tag_filter"] != nil {
		t.Errorf("newAlice.tag_filter = %v, want nil", newAlice["tag_filter"])
	}
	if newAlice["embed_model"] != nil {
		t.Errorf("newAlice.embed_model = %v, want nil (fts never embeds)", newAlice["embed_model"])
	}
	if newAlice["vec_ms"] != nil {
		t.Errorf("newAlice.vec_ms = %v, want nil (fts never runs vec)", newAlice["vec_ms"])
	}
	if newAlice["embed_ms"] != nil {
		t.Errorf("newAlice.embed_ms = %v, want nil (fts never embeds)", newAlice["embed_ms"])
	}
	if newAlice["fts_ms"] != int64(9) {
		t.Errorf("newAlice.fts_ms = %v, want 9", newAlice["fts_ms"])
	}
	if newAlice["kb_version"] != nil {
		t.Errorf("newAlice.kb_version = %v, want nil (Searcher.KBVersion unset)", newAlice["kb_version"])
	}

	newBob := rowByID(t, tbl, "id", f.newBob)
	if newBob["mode"] != "vec" {
		t.Errorf("newBob.mode = %v", newBob["mode"])
	}
	if newBob["fts_ms"] != nil {
		t.Errorf("newBob.fts_ms = %v, want nil (vec never runs fts)", newBob["fts_ms"])
	}
	if newBob["vec_ms"] != int64(22) || newBob["embed_ms"] != int64(9) {
		t.Errorf("newBob vec_ms/embed_ms = %v/%v", newBob["vec_ms"], newBob["embed_ms"])
	}
	if newBob["caller"] != "bob" {
		t.Errorf("newBob.caller = %v", newBob["caller"])
	}
}

// TestLoadSearchResultsAndCandidatesRoundTrip checks the per-list NULL rules (a chunk seen by only
// one of FTS/vec has NULL rank/score for the other) and the candidates table's returned flag.
func TestLoadSearchResultsAndCandidatesRoundTrip(t *testing.T) {
	f := buildFixture(t)
	db, err := Load(context.Background(), f.dbPath, Filter{})
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	defer db.Close()

	all := mustQuery(t, db, `SELECT search_id, rank, chunk_id, entry_path, heading,
		fts_rank, fts_score, vec_rank, vec_distance, rrf_score
		FROM search_results ORDER BY search_id, rank`)

	var rank1, rank2 map[string]any
	for _, row := range all.Rows {
		m := make(map[string]any, len(all.Columns))
		for i, c := range all.Columns {
			m[c] = row[i]
		}
		if m["search_id"] == f.old && m["rank"] == int64(1) {
			rank1 = m
		}
		if m["search_id"] == f.old && m["rank"] == int64(2) {
			rank2 = m
		}
	}
	if rank1 == nil || rank2 == nil {
		t.Fatalf("missing expected search_results rows for search %d: %+v", f.old, all.Rows)
	}
	if rank1["chunk_id"] != int64(101) || rank1["fts_rank"] != int64(1) || rank1["vec_rank"] != int64(2) {
		t.Errorf("rank1 = %+v", rank1)
	}
	if rank1["fts_score"] != -1.5 || rank1["vec_distance"] != 0.31 {
		t.Errorf("rank1 scores = %+v", rank1)
	}
	if rank1["entry_path"] != "alpha.md" || rank1["heading"] != "Section 1" || rank1["rrf_score"] != 0.9 {
		t.Errorf("rank1 entry_path/heading/rrf_score = %+v", rank1)
	}
	if rank2["chunk_id"] != int64(102) || rank2["fts_rank"] != int64(2) {
		t.Errorf("rank2 = %+v", rank2)
	}
	if rank2["vec_rank"] != nil || rank2["vec_distance"] != nil {
		t.Errorf("rank2 vec_rank/vec_distance = %v/%v, want nil (fts-only hit)", rank2["vec_rank"], rank2["vec_distance"])
	}
	if rank2["entry_path"] != "alpha.md" || rank2["heading"] != "" || rank2["rrf_score"] != 0.5 {
		t.Errorf("rank2 entry_path/heading/rrf_score = %+v", rank2)
	}

	cands := mustQuery(t, db, `SELECT search_id, fused_rank, chunk_id, entry_path, heading,
		fts_rank, fts_score, vec_rank, vec_distance, rrf_score, returned
		FROM search_candidates ORDER BY search_id, fused_rank`)
	var vecOnly map[string]any
	for _, row := range cands.Rows {
		m := make(map[string]any, len(cands.Columns))
		for i, c := range cands.Columns {
			m[c] = row[i]
		}
		if m["search_id"] == f.old && m["chunk_id"] == int64(103) {
			vecOnly = m
		}
	}
	if vecOnly == nil {
		t.Fatalf("missing candidate chunk 103 for search %d", f.old)
	}
	if vecOnly["fts_rank"] != nil || vecOnly["fts_score"] != nil {
		t.Errorf("vec-only candidate fts_rank/fts_score = %v/%v, want nil", vecOnly["fts_rank"], vecOnly["fts_score"])
	}
	if vecOnly["returned"] != false {
		t.Errorf("vec-only candidate returned = %v, want false (it was never shown)", vecOnly["returned"])
	}
	if vecOnly["entry_path"] != "beta.md" || vecOnly["heading"] != "Section A" || vecOnly["rrf_score"] != 0.8 {
		t.Errorf("vec-only candidate entry_path/heading/rrf_score = %+v", vecOnly)
	}
}

// TestLoadFeedbackRoundTrip checks search_feedback's useful/bool conversion and the NULL note.
func TestLoadFeedbackRoundTrip(t *testing.T) {
	f := buildFixture(t)
	db, err := Load(context.Background(), f.dbPath, Filter{})
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	defer db.Close()

	tbl := mustQuery(t, db, `SELECT search_id, rank, useful, note, ts FROM search_feedback ORDER BY search_id, rank`)
	if len(tbl.Rows) != 2 {
		t.Fatalf("search_feedback rows = %d, want 2 (only %d has feedback)", len(tbl.Rows), f.old)
	}
	var rank1, rank2 map[string]any
	for _, row := range tbl.Rows {
		m := make(map[string]any, len(tbl.Columns))
		for i, c := range tbl.Columns {
			m[c] = row[i]
		}
		if m["rank"] == int64(1) {
			rank1 = m
		} else if m["rank"] == int64(2) {
			rank2 = m
		}
	}
	if rank1["useful"] != true || rank1["note"] != "great match" {
		t.Errorf("rank1 feedback = %+v", rank1)
	}
	if rank2["useful"] != false {
		t.Errorf("rank2 feedback useful = %v, want false", rank2["useful"])
	}
	if rank2["note"] != nil {
		t.Errorf("rank2 feedback note = %v, want nil (empty note)", rank2["note"])
	}
	if _, ok := rank1["ts"].(time.Time); !ok {
		t.Errorf("rank1 feedback ts = %v (%T), want time.Time", rank1["ts"], rank1["ts"])
	}
}

// TestLoadEntriesRoundTrip checks the entries table, including a JSON-array tags column and a
// multi-tag entry.
func TestLoadEntriesRoundTrip(t *testing.T) {
	f := buildFixture(t)
	db, err := Load(context.Background(), f.dbPath, Filter{})
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	defer db.Close()

	tbl := mustQuery(t, db, `SELECT path, kind, title, tags, updated, verified FROM entries ORDER BY path`)
	if len(tbl.Rows) != 3 {
		t.Fatalf("entries rows = %d, want 3", len(tbl.Rows))
	}
	byPath := map[string]map[string]any{}
	for _, row := range tbl.Rows {
		m := make(map[string]any, len(tbl.Columns))
		for i, c := range tbl.Columns {
			m[c] = row[i]
		}
		byPath[m["path"].(string)] = m
	}
	gamma, ok := byPath["gamma.md"]
	if !ok {
		t.Fatal("gamma.md missing from entries (it should always be copied, even though no search returned it)")
	}
	if gamma["kind"] != "file" || gamma["title"] != "Gamma" {
		t.Errorf("gamma.md kind/title = %v/%v, want file/Gamma", gamma["kind"], gamma["title"])
	}
	if gamma["tags"] != `["docker","postgres"]` {
		t.Errorf("gamma.md tags = %v", gamma["tags"])
	}
	if gamma["updated"] != nil || gamma["verified"] != nil {
		t.Errorf("gamma.md updated/verified = %v/%v, want nil (never set)", gamma["updated"], gamma["verified"])
	}
	beta, ok := byPath["beta.md"]
	if !ok {
		t.Fatal("beta.md missing from entries")
	}
	if beta["kind"] != "file" || beta["title"] != "Beta" {
		t.Errorf("beta.md kind/title = %v/%v, want file/Beta", beta["kind"], beta["title"])
	}
	if beta["tags"] != "[]" {
		t.Errorf("beta.md tags = %v, want [] (no tags)", beta["tags"])
	}
}

// TestLoadSinceFilter proves D3: --since drops older searches and their children but always
// copies every entries row. The cutoff equals newAlice's own ts, so this also proves the bound is
// inclusive (ts >= Since, not ts > Since).
func TestLoadSinceFilter(t *testing.T) {
	f := buildFixture(t)
	db, err := Load(context.Background(), f.dbPath, Filter{Since: f.cutoff})
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	defer db.Close()

	if got := db.SearchesLoaded(); got != 2 {
		t.Errorf("SearchesLoaded = %d, want 2 (old search must be dropped)", got)
	}

	searches := mustQuery(t, db, `SELECT id FROM searches ORDER BY id`)
	ids := map[int64]bool{}
	for _, row := range searches.Rows {
		ids[row[0].(int64)] = true
	}
	if ids[f.old] {
		t.Error("old search was loaded despite being before --since")
	}
	if !ids[f.newAlice] {
		t.Error("newAlice search (ts == cutoff) was not loaded; the bound should be inclusive")
	}
	if !ids[f.newBob] {
		t.Error("newBob search was not loaded")
	}

	// The old search's children must be gone too.
	allResults := mustQuery(t, db, `SELECT search_id FROM search_results`)
	for _, row := range allResults.Rows {
		if row[0].(int64) == f.old {
			t.Errorf("search_results still has a row for the dropped search %d", f.old)
		}
	}
	allCandidates := mustQuery(t, db, `SELECT search_id FROM search_candidates`)
	for _, row := range allCandidates.Rows {
		if row[0].(int64) == f.old {
			t.Errorf("search_candidates still has a row for the dropped search %d", f.old)
		}
	}
	allFeedback := mustQuery(t, db, `SELECT search_id FROM search_feedback`)
	for _, row := range allFeedback.Rows {
		if row[0].(int64) == f.old {
			t.Errorf("search_feedback still has a row for the dropped search %d", f.old)
		}
	}

	// Entries are never filtered.
	entries := mustQuery(t, db, `SELECT count(*) FROM entries`)
	if entries.Rows[0][0].(int64) != 3 {
		t.Errorf("entries count = %v, want 3 (entries ignore --since)", entries.Rows[0][0])
	}
}

// TestLoadCallerFilter proves --caller keeps only that caller's searches (and their children)
// while, again, copying every entries row.
func TestLoadCallerFilter(t *testing.T) {
	f := buildFixture(t)
	db, err := Load(context.Background(), f.dbPath, Filter{Caller: "alice"})
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	defer db.Close()

	if got := db.SearchesLoaded(); got != 2 {
		t.Errorf("SearchesLoaded = %d, want 2 (alice's old + newAlice)", got)
	}
	searches := mustQuery(t, db, `SELECT id, caller FROM searches`)
	for _, row := range searches.Rows {
		if row[1].(string) != "alice" {
			t.Errorf("loaded a non-alice search: %+v", row)
		}
	}
	ids := map[int64]bool{}
	for _, row := range searches.Rows {
		ids[row[0].(int64)] = true
	}
	if !ids[f.old] || !ids[f.newAlice] {
		t.Errorf("expected both of alice's searches (%d, %d), got ids %v", f.old, f.newAlice, ids)
	}
	if ids[f.newBob] {
		t.Error("bob's search was loaded under --caller alice")
	}

	entries := mustQuery(t, db, `SELECT count(*) FROM entries`)
	if entries.Rows[0][0].(int64) != 3 {
		t.Errorf("entries count = %v, want 3 (entries ignore --caller)", entries.Rows[0][0])
	}
}

// TestLoadSinceAndCallerCombined exercises both filters at once (D3's WHERE clauses AND together).
func TestLoadSinceAndCallerCombined(t *testing.T) {
	f := buildFixture(t)
	db, err := Load(context.Background(), f.dbPath, Filter{Since: f.cutoff, Caller: "alice"})
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	defer db.Close()

	if got := db.SearchesLoaded(); got != 1 {
		t.Fatalf("SearchesLoaded = %d, want 1 (only newAlice matches both filters)", got)
	}
	searches := mustQuery(t, db, `SELECT id FROM searches`)
	if searches.Rows[0][0].(int64) != f.newAlice {
		t.Errorf("loaded search id = %v, want %d", searches.Rows[0][0], f.newAlice)
	}
}

// TestLoadMissingFile: no SQLite file at the path is a clear error, not an empty database.
func TestLoadMissingFile(t *testing.T) {
	path := filepath.Join(t.TempDir(), "does-not-exist.sqlite")
	db, err := Load(context.Background(), path, Filter{})
	if err == nil {
		db.Close()
		t.Fatal("Load succeeded against a missing file")
	}
}

// TestQueryBadSQL: a bad statement surfaces DuckDB's own error rather than panicking or hanging.
func TestQueryBadSQL(t *testing.T) {
	f := buildFixture(t)
	db, err := Load(context.Background(), f.dbPath, Filter{})
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	defer db.Close()

	_, err = db.Query(context.Background(), "SELEC * FORM nonsense_table")
	if err == nil {
		t.Fatal("Query with invalid SQL returned no error")
	}
	if !strings.Contains(err.Error(), "syntax error") {
		t.Errorf("error = %q, want it to contain DuckDB's own message (\"syntax error\")", err)
	}
}

// TestSearchesLoaded checks the unfiltered count matches the number of searches logged.
func TestSearchesLoaded(t *testing.T) {
	f := buildFixture(t)
	db, err := Load(context.Background(), f.dbPath, Filter{})
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	defer db.Close()

	if got := db.SearchesLoaded(); got != 3 {
		t.Errorf("SearchesLoaded = %d, want 3", got)
	}
}

// TestLoadDoesNotModifySQLite proves Load never writes to the SQLite file: its sha256 before and
// after Load (and after querying and closing the resulting DB) is unchanged. Opening the file in
// WAL mode (store.Open, used to build the fixture) can leave -wal/-shm sidecar files next to it,
// so only the main file is hashed, per the acceptance criterion.
func TestLoadDoesNotModifySQLite(t *testing.T) {
	f := buildFixture(t)
	before := hashFile(t, f.dbPath)

	db, err := Load(context.Background(), f.dbPath, Filter{})
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if _, err := db.Query(context.Background(), "SELECT count(*) FROM searches"); err != nil {
		t.Fatalf("Query: %v", err)
	}
	if err := db.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}

	after := hashFile(t, f.dbPath)
	if before != after {
		t.Errorf("sqlite file hash changed: before %s, after %s", before, after)
	}
}

func hashFile(t *testing.T, path string) string {
	t.Helper()
	f, err := os.Open(path)
	if err != nil {
		t.Fatalf("open %s: %v", path, err)
	}
	defer f.Close()
	h := sha256.New()
	if _, err := io.Copy(h, f); err != nil {
		t.Fatalf("hash %s: %v", path, err)
	}
	return hex.EncodeToString(h.Sum(nil))
}
