package stats

import (
	"context"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/nickstrad/kb/internal/store/sqlite"
)

// The fixture. Nine searches, hand-built so every number the five queries produce can be worked
// out on paper; the workings live in the comments beside each expectation below.
//
//	id ts                 query                 mode          caller n_ret total embed
//	 1 2026-09-07 09:00   postgres tuning       hybrid        claude   3    100    40
//	 2 2026-09-08 10:00   "Postgres Tuning "    hybrid        codex    2    120    50   same query as 1
//	 3 2026-09-09 11:00   duckdb appender       fts           claude   2     30   NULL
//	 4 2026-09-10 12:00   ollama offline        fts-fallback  codex    2    200   150   never judged
//	 5 2026-09-11 13:00   zero hits topic       hybrid        claude   0     60    30   zero-result
//	 6 2026-09-14 09:00   vector search         vec           claude   2     90    45
//	 7 2026-09-15 10:00   ssh daemon dies       hybrid        codex    3    110    42   never judged
//	 8 2026-09-16 11:00   duckdb appender       hybrid        codex    1     95    38   same query as 3
//	 9 2026-09-17 12:00   nothing written yet   fts           claude   1     20   NULL
//
// Searches 1-5 fall in the ISO week beginning Monday 2026-09-07, searches 6-9 in the week
// beginning Monday 2026-09-14.
//
// Feedback (search, rank, useful, note): (1,1,false,-) (1,3,true,"answered on the third hit")
// (2,1,true,-) (3,1,false,"wrong entry") (3,2,false,"that is the disk section") (6,2,true,-)
// (8,1,false,"still not the appender docs").
// So searches 1 and 3 each carry two verdicts — each must still count as *one* judged search —
// search 1's first useful result is at rank 3 (MRR 1/3, not 1), searches 3 and 8 are judged only
// not-useful, and searches 4, 5, 7 and 9 are unjudged (unknown, not failed).
const fixtureSQL = `
INSERT INTO searches (id, ts, query, mode, k, tag_filter, embed_model, n_fts, n_vec, n_returned, fts_ms, vec_ms, embed_ms, total_ms, caller, corpus_rev) VALUES
 (1, '2026-09-07T09:00:00Z', 'postgres tuning',     'hybrid',       5, NULL,       'nomic', 5, 5, 3,    5,   20,   40, 100, 'claude', 'abc123'),
 (2, '2026-09-08T10:00:00Z', 'Postgres Tuning ',    'hybrid',       5, 'postgres', 'nomic', 4, 4, 2,    6,   25,   50, 120, 'codex',  'abc123'),
 (3, '2026-09-09T11:00:00Z', 'duckdb appender',     'fts',          5, NULL,       NULL,    6, 0, 2,    8, NULL, NULL,  30, 'claude', 'abc123'),
 (4, '2026-09-10T12:00:00Z', 'ollama offline',      'fts-fallback', 5, NULL,       'nomic', 3, 0, 2,    9, NULL,  150, 200, 'codex',  'abc123'),
 (5, '2026-09-11T13:00:00Z', 'zero hits topic',     'hybrid',       5, NULL,       'nomic', 0, 0, 0,    4,   10,   30,  60, 'claude', 'abc123'),
 (6, '2026-09-14T09:00:00Z', 'vector search',       'vec',          5, NULL,       'nomic', 0, 5, 2, NULL,   35,   45,  90, 'claude', 'def456'),
 (7, '2026-09-15T10:00:00Z', 'ssh daemon dies',     'hybrid',       5, NULL,       'nomic', 5, 5, 3,    7,   22,   42, 110, 'codex',  'def456'),
 (8, '2026-09-16T11:00:00Z', 'duckdb appender',     'hybrid',       5, NULL,       'nomic', 2, 3, 1,    5,   18,   38,  95, 'codex',  'def456'),
 (9, '2026-09-17T12:00:00Z', 'nothing written yet', 'fts',          5, NULL,       NULL,    1, 0, 1,    5, NULL, NULL,  20, 'claude', NULL);

-- Provenance: a row with both ranks is 'both', one rank is 'fts-only' / 'vec-only'. The
-- fts-fallback search (4) has no vector list at all, so both its rows are fts-only.
INSERT INTO search_results (search_id, rank, chunk_id, entry_path, heading, fts_rank, fts_score, vec_rank, vec_distance, rrf_score) VALUES
 (1, 1, 101, 'postgres.md',         'Tuning',     1, -5.0,    1, 0.10, 0.033),
 (1, 2, 102, 'droplet.md',          'Disks',      2, -4.0, NULL, NULL, 0.016),
 (1, 3, 103, 'postgres-tuning.md',  'WAL',     NULL, NULL,    1, 0.12, 0.016),
 (2, 1, 101, 'postgres.md',         'Tuning',     1, -5.5,    2, 0.15, 0.030),
 (2, 2, 103, 'postgres-tuning.md',  'WAL',     NULL, NULL,    1, 0.11, 0.016),
 (3, 1, 104, 'duckdb.md',           'Appender',   1, -3.0, NULL, NULL, 0.016),
 (3, 2, 102, 'droplet.md',          'Disks',      2, -2.0, NULL, NULL, 0.015),
 (4, 1, 105, 'ollama.md',           'Offline',    1, -6.0, NULL, NULL, 0.016),
 (4, 2, 102, 'droplet.md',          'Disks',      2, -1.0, NULL, NULL, 0.015),
 (6, 1, 106, 'vector.md',           'Embeddings', NULL, NULL, 1, 0.09, 0.016),
 (6, 2, 104, 'duckdb.md',           'Appender',   NULL, NULL, 2, 0.20, 0.015),
 (7, 1, 107, 'ssh.md',              'Daemons',    1, -7.0,    1, 0.08, 0.033),
 (7, 2, 102, 'droplet.md',          'Disks',      3, -1.5, NULL, NULL, 0.014),
 (7, 3, 108, 'systemd.md',          'Units',   NULL, NULL,    2, 0.19, 0.015),
 (8, 1, 104, 'duckdb.md',           'Appender',   1, -4.5,    1, 0.13, 0.033),
 (9, 1, 102, 'droplet.md',          'Disks',      1, -2.5, NULL, NULL, 0.016);

-- Candidates exist but no query may read them: orphan.md appears here only as a candidate that
-- lost the fusion, so the entries query must still report it as never returned.
INSERT INTO search_candidates (search_id, fused_rank, chunk_id, entry_path, heading, fts_rank, fts_score, vec_rank, vec_distance, rrf_score, returned) VALUES
 (1, 1, 101, 'postgres.md',        'Tuning',   1, -5.0,    1, 0.10, 0.033, 1),
 (1, 2, 102, 'droplet.md',         'Disks',    2, -4.0, NULL, NULL, 0.016, 1),
 (1, 3, 103, 'postgres-tuning.md', 'WAL',   NULL, NULL,    1, 0.12, 0.016, 1),
 (1, 4, 109, 'orphan.md',          'Nobody',   3, -0.5,    3, 0.40, 0.008, 0),
 (8, 1, 104, 'duckdb.md',          'Appender', 1, -4.5,    1, 0.13, 0.033, 1),
 (8, 2, 109, 'orphan.md',          'Nobody',   2, -0.2, NULL, NULL, 0.016, 0);

INSERT INTO search_feedback VALUES
 (1, 1, 0, NULL,                          '2026-09-07T09:05:00Z'),
 (1, 3, 1,  'answered on the third hit',   '2026-09-07T09:06:00Z'),
 (2, 1, 1,  NULL,                          '2026-09-08T10:05:00Z'),
 (3, 1, 0, 'wrong entry',                 '2026-09-09T11:05:00Z'),
 -- droplet.md's only verdict anywhere: it makes that entry judged-and-never-useful (rate 0.0),
 -- which must read differently from an entry nobody ever rated (rate NULL). Search 3 was already
 -- judged, so this row moves nothing in overview or trend, and search 3 is fts, so it is outside
 -- the sources scope too.
 (3, 2, 0, 'that is the disk section',     '2026-09-09T11:06:00Z'),
 (6, 2, 1,  NULL,                          '2026-09-14T09:05:00Z'),
 (8, 1, 0, 'still not the appender docs', '2026-09-16T11:05:00Z');

INSERT INTO entries (path, kind, title, summary, tags, updated, verified, body_hash, indexed_at)
SELECT column1, column2, column3, '', column4, column5, column6, '', '' FROM (VALUES
 ('droplet.md',         'file', 'Droplet',         '["droplet"]',  '2026-09-01', NULL),
 ('duckdb.md',          'file', 'DuckDB',          '["duckdb"]',   '2026-09-02', '2026-09-02'),
 ('ollama.md',          'file', 'Ollama',          '["ollama"]',   '2026-09-03', NULL),
 ('orphan.md',          'file', 'Orphan',          '["misc"]',     '2026-09-04', NULL),
 ('postgres-tuning.md', 'file', 'Postgres tuning', '["postgres"]', '2026-09-05', NULL),
 ('postgres.md',        'file', 'Postgres',        '["postgres"]', '2026-09-06', NULL),
 ('ssh.md',             'file', 'SSH',             '["ssh"]',      '2026-09-07', NULL),
 ('systemd.md',         'file', 'systemd',         '["systemd"]',  '2026-09-08', NULL),
 ('vector.md',          'file', 'Vectors',         '["vector"]',   '2026-09-09', NULL));
`

// at builds a fixture timestamp as the log stores it; every fixture row is in September 2026 UTC.
func at(day, hour, min int) string {
	return time.Date(2026, time.September, day, hour, min, 0, 0, time.UTC).Format(time.RFC3339)
}

// openFixture writes the fixture, plus any extra statements, into a real kb database and opens
// it read-only.
func openFixture(t *testing.T, extra ...string) *DB {
	t.Helper()
	ctx := context.Background()
	path := filepath.Join(t.TempDir(), "kb.sqlite")
	st, err := sqlite.Open(ctx, path, sqlite.Options{})
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	for _, stmt := range append([]string{fixtureSQL}, extra...) {
		if _, err := st.DB().Exec(stmt); err != nil {
			st.Close()
			t.Fatalf("fixture setup: %v", err)
		}
	}
	if err := st.Close(); err != nil {
		t.Fatalf("close store: %v", err)
	}
	ro, err := sqlite.Open(ctx, path, sqlite.Options{ReadOnly: true})
	if err != nil {
		t.Fatalf("Open read-only: %v", err)
	}
	t.Cleanup(func() { ro.Close() })
	return New(ro)
}

// storeOf is the read-only store behind a fixture DB.
func storeOf(d *DB) *sqlite.Store { return d.q.(*sqlite.Store) }

// checkQuery runs the named query under f and asserts its column names, in order, and every cell.
func checkQuery(t *testing.T, db *DB, name string, f Filter, wantCols []string, wantRows [][]any) {
	t.Helper()

	q, ok := Lookup(name)
	if !ok {
		t.Fatalf("Lookup(%q): not found", name)
	}
	tbl, err := db.Run(context.Background(), q, f)
	if err != nil {
		t.Fatalf("Run(%s): %v", name, err)
	}
	cols, rows := tbl.Columns, tbl.Rows

	if !reflect.DeepEqual(cols, wantCols) {
		t.Fatalf("%s columns = %v, want %v", name, cols, wantCols)
	}
	if len(rows) != len(wantRows) {
		t.Fatalf("%s returned %d rows, want %d: %v", name, len(rows), len(wantRows), rows)
	}
	for i, want := range wantRows {
		for j := range want {
			if !reflect.DeepEqual(rows[i][j], want[j]) {
				t.Errorf("%s row %d column %q = %#v (%T), want %#v (%T)",
					name, i, cols[j], rows[i][j], rows[i][j], want[j], want[j])
			}
		}
	}
}

// TestOverview.
//
// hybrid = searches 1, 2, 5, 7, 8 (5). Judged: 1, 2, 8 (search 1 has two verdicts and must count
// once) = 3, so coverage = 100*3/5 = 60.0. Hits among the judged: 1 (useful at rank 3) and 2
// (useful at rank 1); 8 was judged not-useful only. hit_rate = 100*2/3 = 66.666… -> 66.7.
// MRR = (1/3 + 1/1 + 0)/3 = 1.33333…/3 = 0.44444… -> 0.444. Search 5 returned nothing, so
// zero_result = 100*1/5 = 20.0. total_ms sorted = 60, 95, 100, 110, 120: the 0.5 quantile sits
// at index 0.5*4 = 2 -> 100.0, the 0.95 quantile at 0.95*4 = 3.8 -> 110 + 0.8*(120-110) = 118.0.
// embed_ms = (40+50+30+42+38)/5 = 40.0.
//
// fts = searches 3, 9 (2). Judged: 3 only -> coverage 50.0. Search 3 was judged not-useful, so
// hit_rate = 0.0 and MRR = 0/1 = 0.0 (a judged search with no useful hit contributes 0, it is not
// dropped). total_ms sorted = 20, 30: p50 at 0.5 -> 25.0, p95 at 0.95 -> 20 + 0.95*10 = 29.5.
// avg_embed_ms is NULL because the embed stage never runs for fts — NULL, not 0.
//
// fts-fallback = search 4 alone, never judged: coverage 0.0 and both hit_rate and mrr NULL
// (nullif keeps an empty denominator from reading as a 0% hit rate). embed_ms 150 was still
// recorded: the embedder was called and failed, which is what makes this mode worth watching.
//
// vec = search 6 alone, judged with a useful hit at rank 2: coverage 100.0, hit_rate 100.0,
// MRR 0.5.
//
// Order is searches DESC then mode, so the two single-search modes break their tie
// alphabetically: fts-fallback before vec.
func TestOverview(t *testing.T) {
	db := openFixture(t)
	checkQuery(t, db, "overview", Filter{},
		[]string{"mode", "searches", "judged", "coverage_pct", "hit_rate_pct", "mrr",
			"zero_result_pct", "opened_pct", "p50_ms", "p95_ms", "avg_embed_ms"},
		[][]any{
			{"hybrid", int64(5), int64(3), 60.0, 66.7, 0.444, 20.0, 0.0, 100.0, 118.0, 40.0},
			{"fts", int64(2), int64(1), 50.0, 0.0, 0.0, 0.0, 0.0, 25.0, 29.5, nil},
			{"fts-fallback", int64(1), int64(0), 0.0, nil, nil, 0.0, 0.0, 200.0, 200.0, 150.0},
			{"vec", int64(1), int64(1), 100.0, 100.0, 0.5, 0.0, 0.0, 90.0, 90.0, 45.0},
		})
}

// TestSources.
//
// Scope is hybrid + fts-fallback: searches 1, 2, 4, 5, 7, 8. Search 5 returned nothing, so the
// scope holds 3+2+2+0+3+1 = 11 result rows. Searches 3, 6 and 9 (fts and vec) are excluded, which
// is what keeps vec-only from being polluted by a pure vector search.
//
//	both     s1r1, s2r1, s7r1, s8r1            = 4 rows, ranks 1,1,1,1     -> avg 1.0
//	fts-only s1r2, s4r1, s4r2, s7r2            = 4 rows, ranks 2,1,2,2     -> avg 7/4 = 1.75
//	vec-only s1r3, s2r2, s7r3                  = 3 rows, ranks 3,2,3       -> avg 8/3 = 2.666… -> 2.67
//
// share_pct: 4/11 = 36.3636… -> 36.4 (twice) and 3/11 = 27.2727… -> 27.3.
//
// In-scope verdicts are (1,1) false -> both, (1,3) true -> vec-only, (2,1) true -> both,
// (8,1) false -> both. So both: judged 3, useful 1, rate 100*1/3 = 33.3; vec-only: judged 1,
// useful 1, rate 100.0; fts-only: judged 0, useful 0, rate NULL — nobody rated those rows, which
// is unknown rather than 0%. (6,2) and (3,1) are out of scope and must not appear anywhere.
func TestSources(t *testing.T) {
	db := openFixture(t)
	checkQuery(t, db, "sources", Filter{},
		[]string{"source", "results", "share_pct", "judged", "useful", "useful_rate_pct", "avg_rank", "with_exact"},
		[][]any{
			{"both", int64(4), 36.4, int64(3), int64(1), 33.3, 1.0, int64(0)},
			{"fts-only", int64(4), 36.4, int64(0), int64(0), nil, 1.75, int64(0)},
			{"vec-only", int64(3), 27.3, int64(1), int64(1), 100.0, 2.67, int64(0)},
		})
}

// TestSourcesExact: an exact-list rank (PLAN 4.5) marks a row with_exact, and a row only the
// exact list returned is 'exact-only'. Search 7 (hybrid) gains an exact rank on its 'both' row
// (rank 1) and a fourth, exact-only row at rank 4: both keeps 4 rows with 1 with_exact, and
// exact-only is 1 row of 12 (8.3%), never judged, at rank 4. The other shares become
// 4/12 = 33.3 and 3/12 = 25.0.
func TestSourcesExact(t *testing.T) {
	db := openFixture(t, `
		UPDATE search_results SET exact_rank = 1, exact_score = -3.0 WHERE search_id = 7 AND rank = 1;
		INSERT INTO search_results (search_id, rank, chunk_id, entry_path, heading, exact_rank, exact_score, rrf_score)
		VALUES (7, 4, 110, 'kb-toml.md', 'Keys', 1, -2.0, 0.016);`)
	checkQuery(t, db, "sources", Filter{},
		[]string{"source", "results", "share_pct", "judged", "useful", "useful_rate_pct", "avg_rank", "with_exact"},
		[][]any{
			{"both", int64(4), 33.3, int64(3), int64(1), 33.3, 1.0, int64(1)},
			{"fts-only", int64(4), 33.3, int64(0), int64(0), nil, 1.75, int64(0)},
			{"vec-only", int64(3), 25.0, int64(1), int64(1), 100.0, 2.67, int64(0)},
			{"exact-only", int64(1), 8.3, int64(0), int64(0), nil, 4.0, int64(1)},
		})
}

// TestTrend.
//
// Week of Monday 2026-09-07 = searches 1-5. distinct lower(trim(query)) = "postgres tuning"
// (searches 1 and 2 — search 2 is "Postgres Tuning " and must normalise onto search 1),
// "duckdb appender", "ollama offline", "zero hits topic" = 4. hybrid = 1, 2, 5 -> 100*3/5 = 60.0;
// fts-fallback = 4 -> 20.0. Judged = 1, 2, 3 -> coverage 60.0; hits = 1, 2 -> 100*2/3 = 66.7;
// MRR = (1/3 + 1 + 0)/3 = 0.444. total_ms sorted = 30, 60, 100, 120, 200 -> p50 at index 2 = 100.0.
//
// Week of Monday 2026-09-14 = searches 6-9, four distinct queries. hybrid = 7, 8 -> 50.0; no
// fallback -> 0.0. Judged = 6, 8 -> coverage 50.0; hits = 6 only -> 50.0; MRR = (1/2 + 0)/2 = 0.25.
// total_ms sorted = 20, 90, 95, 110 -> p50 at 0.5*3 = 1.5 -> 90 + 0.5*(95-90) = 92.5.
func TestTrend(t *testing.T) {
	db := openFixture(t)
	checkQuery(t, db, "trend", Filter{},
		[]string{"week", "searches", "distinct_queries", "hybrid_pct", "fallback_pct",
			"coverage_pct", "hit_rate_pct", "mrr", "p50_ms"},
		[][]any{
			{at(7, 0, 0), int64(5), int64(4), 60.0, 20.0, 60.0, 66.7, 0.444, 100.0},
			{at(14, 0, 0), int64(4), int64(4), 50.0, 0.0, 50.0, 50.0, 0.25, 92.5},
		})
}

// TestEntries.
//
// The 16 result rows group by entry_path as:
//
//	droplet.md         s1r2, s3r2, s4r2, s7r2, s9r1  -> 5 rows / 5 searches, ranks 2,2,2,2,1 -> 1.8
//	duckdb.md          s3r1, s6r2, s8r1              -> 3 rows / 3 searches, ranks 1,2,1 -> 4/3 = 1.33
//	postgres-tuning.md s1r3, s2r2                    -> 2 rows / 2 searches, ranks 3,2 -> 2.5
//	postgres.md        s1r1, s2r1                    -> 2 rows / 2 searches, ranks 1,1 -> 1.0
//	ollama.md          s4r1                          -> 1 row,  rank 1
//	ssh.md             s7r1                          -> 1 row,  rank 1
//	systemd.md         s7r3                          -> 1 row,  rank 3
//	vector.md          s6r1                          -> 1 row,  rank 1
//	orphan.md          none                          -> never returned (it only ever lost a fusion,
//	                                                    so search_candidates must not resurrect it)
//
// Verdicts land on result rows, so they never multiply the counts: duckdb.md collects (3,1) false,
// (6,2) true and (8,1) false -> useful 1, not_useful 2, rate 100*1/3 = 33.3; postgres.md collects
// (1,1) false and (2,1) true -> 50.0; postgres-tuning.md collects (1,3) true -> 100.0.
//
// droplet.md collects exactly one verdict, (3,2) false -> useful 0, not_useful 1, rate
// 100*0/1 = 0.0. That 0.0 is the point: it must be a float 0.0, not NULL, because the entry *was*
// judged and never helped. ollama.md, ssh.md, systemd.md, vector.md and orphan.md have no verdict
// at all, so their rate stays NULL — unknown, not 0% — and orphan.md keeps NULL avg_rank and NULL
// last_returned rather than 0 and the epoch.
//
// last_returned is the ts of the newest search that showed the entry: droplet.md 2026-09-17 12:00
// (search 9), duckdb.md 2026-09-16 11:00 (search 8), both postgres entries 2026-09-08 10:00
// (search 2).
//
// Order is returned DESC then path, binary collation: "postgres-tuning.md" sorts before
// "postgres.md" because '-' (0x2D) < '.' (0x2E), and "ssh.md" before "systemd.md".
func TestEntries(t *testing.T) {
	db := openFixture(t)
	checkQuery(t, db, "entries", Filter{},
		[]string{"path", "returned", "searches", "useful", "not_useful", "useful_rate_pct",
			"avg_rank", "last_returned"},
		[][]any{
			{"droplet.md", int64(5), int64(5), int64(0), int64(1), 0.0, 1.8, at(17, 12, 0)},
			{"duckdb.md", int64(3), int64(3), int64(1), int64(2), 33.3, 1.33, at(16, 11, 0)},
			{"postgres-tuning.md", int64(2), int64(2), int64(1), int64(0), 100.0, 2.5, at(8, 10, 0)},
			{"postgres.md", int64(2), int64(2), int64(1), int64(1), 50.0, 1.0, at(8, 10, 0)},
			{"ollama.md", int64(1), int64(1), int64(0), int64(0), nil, 1.0, at(10, 12, 0)},
			{"ssh.md", int64(1), int64(1), int64(0), int64(0), nil, 1.0, at(15, 10, 0)},
			{"systemd.md", int64(1), int64(1), int64(0), int64(0), nil, 3.0, at(15, 10, 0)},
			{"vector.md", int64(1), int64(1), int64(0), int64(0), nil, 1.0, at(14, 9, 0)},
			{"orphan.md", int64(0), int64(0), int64(0), int64(0), nil, nil, nil},
		})
}

// TestGaps.
//
// Groups by lower(trim(query)). "postgres tuning" (searches 1, 2) and "vector search" (search 6)
// each collected a useful verdict, so they are excluded. What is left:
//
//	duckdb appender      searches 3 (claude) and 8 (codex): asked twice, last 2026-09-16 11:00.
//	                     Feedback rows 3 — (3,1), (3,2) and (8,1) — all not-useful, so judged 3 and
//	                     not_useful 3 while times_asked is only 2. zero_results 0. The group's most
//	                     recent search is 8, whose rank 1 is duckdb.md. Of the three notes the
//	                     newest by ts is search 8's, at 2026-09-16 11:05.
//	nothing written yet  search 9, claude, unjudged, rank 1 is droplet.md.
//	ssh daemon dies      search 7, codex, unjudged, rank 1 is ssh.md.
//	zero hits topic      search 5, claude, unjudged, returned nothing: zero_results 1 and
//	                     top_entry NULL (there is no rank 1 to name).
//	ollama offline       search 4, codex, unjudged, rank 1 is ollama.md.
//
// judged counts feedback *rows*, not searches, so a group can show judged > times_asked. Order is
// times_asked DESC, then last_asked DESC: 09-17, 09-15, 09-11, 09-10.
func TestGaps(t *testing.T) {
	db := openFixture(t)
	checkQuery(t, db, "gaps", Filter{},
		[]string{"query", "times_asked", "last_asked", "callers", "judged", "not_useful",
			"zero_results", "top_entry", "last_note"},
		[][]any{
			{"duckdb appender", int64(2), at(16, 11, 0), "claude,codex", int64(3), int64(3),
				int64(0), "duckdb.md", "still not the appender docs"},
			{"nothing written yet", int64(1), at(17, 12, 0), "claude", int64(0), int64(0),
				int64(0), "droplet.md", nil},
			{"ssh daemon dies", int64(1), at(15, 10, 0), "codex", int64(0), int64(0),
				int64(0), "ssh.md", nil},
			{"zero hits topic", int64(1), at(11, 13, 0), "claude", int64(0), int64(0),
				int64(1), nil, nil},
			{"ollama offline", int64(1), at(10, 12, 0), "codex", int64(0), int64(0),
				int64(0), "ollama.md", nil},
		})
}

// wholeSearchSQL adds two whole-search verdicts (`kb feedback <id> --none`, stored at rank 0 with
// useful = false) to the fixture: one on zero-result search 5 and one on search 7, which returned
// three hits nobody had rated. Both searches were unjudged before.
const wholeSearchSQL = `
INSERT INTO search_feedback VALUES
 (5, 0, 0, 'nothing on this topic yet',        '2026-09-11T13:05:00Z'),
 (7, 0, 0, 'all about sshd config, not tmux', '2026-09-15T10:05:00Z');
`

// TestWholeSearchVerdict pins what a rank-0 --none row does to every query, on top of the base
// fixture whose numbers are worked out in the tests above.
//
// overview, hybrid (searches 1, 2, 5, 7, 8): judged was 1, 2, 8 and is now 1, 2, 5, 7, 8 = 5, so
// coverage = 100*5/5 = 100.0. The rank-0 rows are not useful, so the hits are still 1 and 2 only:
// hit_rate = 100*2/5 = 40.0. MRR = (1/3 + 1 + 0 + 0 + 0)/5 = 1.33333…/5 = 0.26666… -> 0.267.
// zero_result_pct, latency and embed columns do not read feedback and stay 20.0, 100.0, 118.0,
// 40.0. The other modes have no new rows and are unchanged.
//
// sources and entries: no search_results row has rank 0, so both are exactly as in TestSources
// and TestEntries.
//
// trend, week of 09-07 (searches 1-5): judged 1, 2, 3, 5 = 4 -> coverage 100*4/5 = 80.0; hits 1, 2
// -> 100*2/4 = 50.0; MRR = (1/3 + 1 + 0 + 0)/4 = 0.33333… -> 0.333. Week of 09-14 (searches 6-9):
// judged 6, 7, 8 = 3 -> coverage 100*3/4 = 75.0; hits 6 -> 100*1/3 = 33.333… -> 33.3; MRR =
// (1/2 + 0 + 0)/3 = 0.16666… -> 0.167.
//
// gaps: "zero hits topic" and "ssh daemon dies" each gain one feedback row, not useful, so judged
// = not_useful = 1 (judged and failed, no longer unknown) and last_note is the --none note.
// zero_results and top_entry are unchanged (search 5 still returned nothing; search 7's rank 1 is
// still ssh.md), and the ordering columns did not move.
func TestWholeSearchVerdict(t *testing.T) {
	db := openFixture(t, wholeSearchSQL)

	checkQuery(t, db, "overview", Filter{},
		[]string{"mode", "searches", "judged", "coverage_pct", "hit_rate_pct", "mrr",
			"zero_result_pct", "opened_pct", "p50_ms", "p95_ms", "avg_embed_ms"},
		[][]any{
			{"hybrid", int64(5), int64(5), 100.0, 40.0, 0.267, 20.0, 0.0, 100.0, 118.0, 40.0},
			{"fts", int64(2), int64(1), 50.0, 0.0, 0.0, 0.0, 0.0, 25.0, 29.5, nil},
			{"fts-fallback", int64(1), int64(0), 0.0, nil, nil, 0.0, 0.0, 200.0, 200.0, 150.0},
			{"vec", int64(1), int64(1), 100.0, 100.0, 0.5, 0.0, 0.0, 90.0, 90.0, 45.0},
		})

	checkQuery(t, db, "sources", Filter{},
		[]string{"source", "results", "share_pct", "judged", "useful", "useful_rate_pct", "avg_rank", "with_exact"},
		[][]any{
			{"both", int64(4), 36.4, int64(3), int64(1), 33.3, 1.0, int64(0)},
			{"fts-only", int64(4), 36.4, int64(0), int64(0), nil, 1.75, int64(0)},
			{"vec-only", int64(3), 27.3, int64(1), int64(1), 100.0, 2.67, int64(0)},
		})

	checkQuery(t, db, "trend", Filter{},
		[]string{"week", "searches", "distinct_queries", "hybrid_pct", "fallback_pct",
			"coverage_pct", "hit_rate_pct", "mrr", "p50_ms"},
		[][]any{
			{at(7, 0, 0), int64(5), int64(4), 60.0, 20.0, 80.0, 50.0, 0.333, 100.0},
			{at(14, 0, 0), int64(4), int64(4), 50.0, 0.0, 75.0, 33.3, 0.167, 92.5},
		})

	checkQuery(t, db, "entries", Filter{},
		[]string{"path", "returned", "searches", "useful", "not_useful", "useful_rate_pct",
			"avg_rank", "last_returned"},
		[][]any{
			{"droplet.md", int64(5), int64(5), int64(0), int64(1), 0.0, 1.8, at(17, 12, 0)},
			{"duckdb.md", int64(3), int64(3), int64(1), int64(2), 33.3, 1.33, at(16, 11, 0)},
			{"postgres-tuning.md", int64(2), int64(2), int64(1), int64(0), 100.0, 2.5, at(8, 10, 0)},
			{"postgres.md", int64(2), int64(2), int64(1), int64(1), 50.0, 1.0, at(8, 10, 0)},
			{"ollama.md", int64(1), int64(1), int64(0), int64(0), nil, 1.0, at(10, 12, 0)},
			{"ssh.md", int64(1), int64(1), int64(0), int64(0), nil, 1.0, at(15, 10, 0)},
			{"systemd.md", int64(1), int64(1), int64(0), int64(0), nil, 3.0, at(15, 10, 0)},
			{"vector.md", int64(1), int64(1), int64(0), int64(0), nil, 1.0, at(14, 9, 0)},
			{"orphan.md", int64(0), int64(0), int64(0), int64(0), nil, nil, nil},
		})

	checkQuery(t, db, "gaps", Filter{},
		[]string{"query", "times_asked", "last_asked", "callers", "judged", "not_useful",
			"zero_results", "top_entry", "last_note"},
		[][]any{
			{"duckdb appender", int64(2), at(16, 11, 0), "claude,codex", int64(3), int64(3),
				int64(0), "duckdb.md", "still not the appender docs"},
			{"nothing written yet", int64(1), at(17, 12, 0), "claude", int64(0), int64(0),
				int64(0), "droplet.md", nil},
			{"ssh daemon dies", int64(1), at(15, 10, 0), "codex", int64(1), int64(1),
				int64(0), "ssh.md", "all about sshd config, not tmux"},
			{"zero hits topic", int64(1), at(11, 13, 0), "claude", int64(1), int64(1),
				int64(1), nil, "nothing on this topic yet"},
			{"ollama offline", int64(1), at(10, 12, 0), "codex", int64(0), int64(0),
				int64(0), "ollama.md", nil},
		})
}

// TestTiers sets fts_tier and memory_rank on the fixture's results: search 1 ranks 1-2 are
// 'phrase' and 'and' (rank 1 also memory-placed, judged not useful; rank 3 judged useful is
// 'none'), search 2 rank 1 is 'phrase' and memory-placed (judged useful), search 3 rank 1 is
// 'and', search 7 rank 1 is 'near', search 8 rank 1 (judged not useful) is 'or'. The other 10 of
// the 16 rows are NULL -> 'none'. One verdict per (search, rank) (the primary key), so max() in
// the query only turns "no verdict" into NULL.
func TestTiers(t *testing.T) {
	db := openFixture(t, `
UPDATE search_results SET fts_tier = 'phrase', memory_rank = 1 WHERE search_id = 1 AND rank = 1;
UPDATE search_results SET fts_tier = 'and' WHERE search_id = 1 AND rank = 2;
UPDATE search_results SET fts_tier = 'phrase', memory_rank = 2 WHERE search_id = 2 AND rank = 1;
UPDATE search_results SET fts_tier = 'and' WHERE search_id = 3 AND rank = 1;
UPDATE search_results SET fts_tier = 'near' WHERE search_id = 7 AND rank = 1;
UPDATE search_results SET fts_tier = 'or' WHERE search_id = 8 AND rank = 1;`)
	// 16 rows. phrase: (1,1) not useful, (2,1) useful -> 2 results, judged 2, useful 1, 50.0,
	// avg rank 1.0, with_memory 2, memory_useful 1. near: 1 row, unjudged. and: (1,2) unjudged,
	// (3,1) not useful -> 2 rows, judged 1, useful 0 -> 0.0, avg rank 1.5. or: (8,1) not useful ->
	// 1 row, 6.3, judged 1, useful 0, 0.0, avg 1.0. none: 10 rows, judged (1,3) useful, (3,2) not
	// useful, (6,2) useful -> 3, useful 2, 66.7; ranks sum 3+2+2+3+3+5+1 = 19, avg 1.9.
	checkQuery(t, db, "tiers", Filter{},
		[]string{"tier", "results", "share_pct", "judged", "useful", "useful_rate_pct", "avg_rank", "with_memory", "memory_useful"},
		[][]any{
			{"phrase", int64(2), 12.5, int64(2), int64(1), 50.0, 1.0, int64(2), int64(1)},
			{"near", int64(1), 6.3, int64(0), int64(0), nil, 1.0, int64(0), int64(0)},
			{"and", int64(2), 12.5, int64(1), int64(0), 0.0, 1.5, int64(0), int64(0)},
			{"or", int64(1), 6.3, int64(1), int64(0), 0.0, 1.0, int64(0), int64(0)},
			{"none", int64(10), 62.5, int64(3), int64(2), 66.7, 1.9, int64(0), int64(0)},
		})
}

// TestLookup checks the registry itself: the names in display order, a populated Summary and
// Help on each, a Help paragraph that ends in a "Decision:" sentence, and a
// miss for a name nobody defined (which is what the CLI turns into a usage error).
func TestLookup(t *testing.T) {
	want := []string{"overview", "sources", "tiers", "trend", "entries", "gaps", "settings"}

	if len(Queries) != len(want) {
		t.Fatalf("len(Queries) = %d, want %d", len(Queries), len(want))
	}
	for i, name := range want {
		if Queries[i].Name != name {
			t.Errorf("Queries[%d].Name = %q, want %q", i, Queries[i].Name, name)
		}
		q, ok := Lookup(name)
		if !ok {
			t.Errorf("Lookup(%q) = not found", name)
			continue
		}
		if q.Name != name {
			t.Errorf("Lookup(%q).Name = %q", name, q.Name)
		}
		if q.Summary == "" {
			t.Errorf("Lookup(%q): empty Summary", name)
		}
		if q.Help == "" {
			t.Errorf("Lookup(%q): empty Help", name)
		}
		if q.SQL == "" {
			t.Errorf("Lookup(%q): empty SQL", name)
		}
		if !strings.Contains(q.Help, "\nDecision: ") {
			t.Errorf("Lookup(%q): Help does not end with a Decision sentence:\n%s", name, q.Help)
		}
		if !strings.HasSuffix(strings.TrimSpace(q.Help), ".") {
			t.Errorf("Lookup(%q): Help does not end in a full stop", name)
		}
	}
	if _, ok := Lookup("no-such-query"); ok {
		t.Error(`Lookup("no-such-query") = found, want not found`)
	}
	if _, ok := Lookup(""); ok {
		t.Error(`Lookup("") = found, want not found`)
	}
}

// TestSettings (PLAN 2.5): one row per settings_hash, over the base fixture with settings recorded
// on some searches. Configuration A (hash a1b2c3d4e5f6...) ran searches 1, 2, 3 and 6, B
// (0f9e8d7c6b5a...) ran 7 and 8, and searches 4, 5 and 9 predate the columns (NULL hash).
//
// A: all four judged -> coverage 100.0. Hits: 1 (useful at rank 3), 2 (rank 1) and 6 (rank 2);
// search 3 was judged not-useful only -> hit_rate 75.0, MRR = (1/3 + 1 + 0 + 1/2)/4 = 0.4583 ->
// 0.458. first/last seen are search 1's and search 6's timestamps.
// B: 2 searches, only 8 judged (not useful) -> coverage 50.0, hit_rate 0.0, MRR 0.0.
// NULL: 3 searches, none judged -> coverage 0.0, hit_rate and mrr NULL, and no settings JSON.
// Rows are ordered by last_seen DESC: NULL (09-17), B (09-16), A (09-14).
func TestSettings(t *testing.T) {
	db := openFixture(t, `
		UPDATE searches SET settings_hash = 'a1b2c3d4e5f6' || printf('%052d', 0), settings = '{"search":{"rrf_k":60}}' WHERE id IN (1, 2, 3, 6);
		UPDATE searches SET settings_hash = '0f9e8d7c6b5a' || printf('%052d', 0), settings = '{"search":{"rrf_k":20}}' WHERE id IN (7, 8);`)
	cols := []string{"settings_hash", "searches", "judged", "coverage_pct", "hit_rate_pct", "mrr", "first_seen", "last_seen", "settings"}
	checkQuery(t, db, "settings", Filter{}, cols, [][]any{
		{nil, int64(3), int64(0), 0.0, nil, nil, at(10, 12, 0), at(17, 12, 0), nil},
		{"0f9e8d7c6b5a", int64(2), int64(1), 50.0, 0.0, 0.0, at(15, 10, 0), at(16, 11, 0), `{"search":{"rrf_k":20}}`},
		{"a1b2c3d4e5f6", int64(4), int64(4), 100.0, 75.0, 0.458, at(7, 9, 0), at(14, 9, 0), `{"search":{"rrf_k":60}}`},
	})
	// The caller filter reaches the grouping: codex ran searches 2, 4, 7, 8 (B last seen 09-16, NULL 09-10, A 09-08).
	checkQuery(t, db, "settings", Filter{Caller: "codex"}, cols, [][]any{
		{"0f9e8d7c6b5a", int64(2), int64(1), 50.0, 0.0, 0.0, at(15, 10, 0), at(16, 11, 0), `{"search":{"rrf_k":20}}`},
		{nil, int64(1), int64(0), 0.0, nil, nil, at(10, 12, 0), at(10, 12, 0), nil},
		{"a1b2c3d4e5f6", int64(1), int64(1), 100.0, 100.0, 1.0, at(8, 10, 0), at(8, 10, 0), `{"search":{"rrf_k":60}}`},
	})
}

// TestSettingsEmpty: a log with no searches is an empty table, not an error.
func TestSettingsEmpty(t *testing.T) {
	db := openFixture(t, `DELETE FROM searches`)
	checkQuery(t, db, "settings", Filter{},
		[]string{"settings_hash", "searches", "judged", "coverage_pct", "hit_rate_pct", "mrr", "first_seen", "last_seen", "settings"}, nil)
}

// TestOverviewOpenedPct: opened_pct is the share of all searches (judged or not) with at least
// one search_opens row, per mode, and a search with several opens still counts once.
//
// None opened (the base fixture): 0.0 in every mode. Some: opens on searches 1 (two entries) and 2
// (one path from this store and from another) make hybrid 2/5 = 40.0, fts 0/2 = 0.0. All: every
// search opened (another store's entry), 100.0 everywhere.
func TestOverviewOpenedPct(t *testing.T) {
	pick := func(t *testing.T, db *DB) map[string]any {
		t.Helper()
		q, _ := Lookup("overview")
		tbl, err := db.Run(context.Background(), q, Filter{})
		if err != nil {
			t.Fatal(err)
		}
		got := map[string]any{}
		for _, r := range tbl.Rows {
			got[r[0].(string)] = r[7]
		}
		return got
	}
	t.Run("none", func(t *testing.T) {
		got := pick(t, openFixture(t))
		for mode, v := range got {
			if v != 0.0 {
				t.Errorf("%s opened_pct = %v, want 0.0", mode, v)
			}
		}
	})
	t.Run("some", func(t *testing.T) {
		got := pick(t, openFixture(t, `INSERT INTO search_opens (search_id, store, entry_path, ts) VALUES
			(1, NULL, 'postgres.md', '2026-09-07T09:10:00Z'), (1, NULL, 'droplet.md', '2026-09-07T09:11:00Z'),
			(2, NULL, 'postgres.md', '2026-09-08T10:10:00Z'),
			(2, 'notes@/elsewhere', 'postgres.md', '2026-09-08T10:11:00Z');`))
		if got["hybrid"] != 40.0 || got["fts"] != 0.0 {
			t.Errorf("opened_pct = %v, want hybrid 40.0 and fts 0.0", got)
		}
	})
	t.Run("all", func(t *testing.T) {
		got := pick(t, openFixture(t, `INSERT INTO search_opens
			SELECT id, 'notes@/elsewhere', 'x.md', ts FROM searches;`))
		for mode, v := range got {
			if v != 100.0 {
				t.Errorf("%s opened_pct = %v, want 100.0", mode, v)
			}
		}
	})
}

// TestPercentileRounding pins Pct.Digits = 1 for overview and trend: two October hybrid searches
// with fractional latencies 12.31 and 12.33 give p50 = 12.32 -> 12.3 (not 12 or 12.32) and
// p95 = 12.31 + 0.95*0.02 = 12.329 -> 12.3 (not 12.33).
func TestPercentileRounding(t *testing.T) {
	db := openFixture(t, `
INSERT INTO searches (id, ts, query, mode, k, n_fts, n_vec, n_returned, total_ms, caller) VALUES
 (20, '2026-10-06T09:00:00Z', 'rounding a', 'hybrid', 5, 1, 1, 1, 12.31, 'claude'),
 (21, '2026-10-07T09:00:00Z', 'rounding b', 'hybrid', 5, 1, 1, 1, 12.33, 'claude');`)
	f := Filter{Since: mustParse(t, "2026-10-01T00:00:00Z")}
	q, _ := Lookup("overview")
	tbl, err := db.Run(context.Background(), q, f)
	if err != nil {
		t.Fatal(err)
	}
	if len(tbl.Rows) != 1 {
		t.Fatalf("overview rows = %v, want one", tbl.Rows)
	}
	col := func(name string) int {
		for i, c := range tbl.Columns {
			if c == name {
				return i
			}
		}
		t.Fatalf("no column %q", name)
		return -1
	}
	if got := tbl.Rows[0][col("p50_ms")]; got != 12.3 {
		t.Errorf("overview p50_ms = %#v, want 12.3", got)
	}
	if got := tbl.Rows[0][col("p95_ms")]; got != 12.3 {
		t.Errorf("overview p95_ms = %#v, want 12.3", got)
	}
	q, _ = Lookup("trend")
	tbl, err = db.Run(context.Background(), q, f)
	if err != nil {
		t.Fatal(err)
	}
	if len(tbl.Rows) != 1 {
		t.Fatalf("trend rows = %v, want one", tbl.Rows)
	}
	if got := tbl.Rows[0][col("p50_ms")]; got != 12.3 {
		t.Errorf("trend p50_ms = %#v, want 12.3", got)
	}
}
