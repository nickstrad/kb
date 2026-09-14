package stats

import (
	"database/sql"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/marcboeker/go-duckdb/v2"
)

// testSchemaDDL is the plan's "DuckDB schema" section verbatim. It is duplicated here rather than
// imported from the loader on purpose: these tests pin the *query semantics* against a hand-built
// fixture, so they must keep passing (or fail loudly) independently of how Load fills the tables.
const testSchemaDDL = `
CREATE TABLE searches (
  id          BIGINT PRIMARY KEY,
  ts          TIMESTAMP NOT NULL,      -- parsed from the RFC3339 text in SQLite (UTC)
  query       VARCHAR NOT NULL,
  mode        VARCHAR NOT NULL,        -- hybrid | fts | vec | fts-fallback
  k           INTEGER NOT NULL,
  tag_filter  VARCHAR,
  embed_model VARCHAR,
  n_fts       INTEGER NOT NULL,
  n_vec       INTEGER NOT NULL,
  n_returned  INTEGER NOT NULL,
  fts_ms      INTEGER, vec_ms INTEGER, embed_ms INTEGER, total_ms INTEGER,
  caller      VARCHAR NOT NULL,
  kb_version  VARCHAR
);
CREATE TABLE search_results (
  search_id BIGINT NOT NULL, rank INTEGER NOT NULL, chunk_id BIGINT,
  entry_path VARCHAR NOT NULL, heading VARCHAR NOT NULL,
  fts_rank INTEGER, fts_score DOUBLE, vec_rank INTEGER, vec_distance DOUBLE,
  rrf_score DOUBLE NOT NULL
);
CREATE TABLE search_candidates (
  search_id BIGINT NOT NULL, fused_rank INTEGER NOT NULL, chunk_id BIGINT,
  entry_path VARCHAR NOT NULL, heading VARCHAR NOT NULL,
  fts_rank INTEGER, fts_score DOUBLE, vec_rank INTEGER, vec_distance DOUBLE,
  rrf_score DOUBLE NOT NULL, returned BOOLEAN NOT NULL
);
CREATE TABLE search_feedback (
  search_id BIGINT NOT NULL, rank INTEGER NOT NULL, useful BOOLEAN NOT NULL,
  note VARCHAR, ts TIMESTAMP NOT NULL
);
CREATE TABLE entries (
  path VARCHAR PRIMARY KEY, kind VARCHAR NOT NULL, title VARCHAR NOT NULL,
  tags VARCHAR NOT NULL,                -- JSON array text, verbatim from SQLite
  updated VARCHAR, verified VARCHAR
);
`

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
INSERT INTO searches VALUES
 (1, TIMESTAMP '2026-09-07 09:00:00', 'postgres tuning',     'hybrid',       5, NULL,       'nomic', 5, 5, 3,    5,   20,   40, 100, 'claude', 'abc123'),
 (2, TIMESTAMP '2026-09-08 10:00:00', 'Postgres Tuning ',    'hybrid',       5, 'postgres', 'nomic', 4, 4, 2,    6,   25,   50, 120, 'codex',  'abc123'),
 (3, TIMESTAMP '2026-09-09 11:00:00', 'duckdb appender',     'fts',          5, NULL,       NULL,    6, 0, 2,    8, NULL, NULL,  30, 'claude', 'abc123'),
 (4, TIMESTAMP '2026-09-10 12:00:00', 'ollama offline',      'fts-fallback', 5, NULL,       'nomic', 3, 0, 2,    9, NULL,  150, 200, 'codex',  'abc123'),
 (5, TIMESTAMP '2026-09-11 13:00:00', 'zero hits topic',     'hybrid',       5, NULL,       'nomic', 0, 0, 0,    4,   10,   30,  60, 'claude', 'abc123'),
 (6, TIMESTAMP '2026-09-14 09:00:00', 'vector search',       'vec',          5, NULL,       'nomic', 0, 5, 2, NULL,   35,   45,  90, 'claude', 'def456'),
 (7, TIMESTAMP '2026-09-15 10:00:00', 'ssh daemon dies',     'hybrid',       5, NULL,       'nomic', 5, 5, 3,    7,   22,   42, 110, 'codex',  'def456'),
 (8, TIMESTAMP '2026-09-16 11:00:00', 'duckdb appender',     'hybrid',       5, NULL,       'nomic', 2, 3, 1,    5,   18,   38,  95, 'codex',  'def456'),
 (9, TIMESTAMP '2026-09-17 12:00:00', 'nothing written yet', 'fts',          5, NULL,       NULL,    1, 0, 1,    5, NULL, NULL,  20, 'claude', NULL);

-- Provenance: a row with both ranks is 'both', one rank is 'fts-only' / 'vec-only'. The
-- fts-fallback search (4) has no vector list at all, so both its rows are fts-only.
INSERT INTO search_results VALUES
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
INSERT INTO search_candidates VALUES
 (1, 1, 101, 'postgres.md',        'Tuning',   1, -5.0,    1, 0.10, 0.033, TRUE),
 (1, 2, 102, 'droplet.md',         'Disks',    2, -4.0, NULL, NULL, 0.016, TRUE),
 (1, 3, 103, 'postgres-tuning.md', 'WAL',   NULL, NULL,    1, 0.12, 0.016, TRUE),
 (1, 4, 109, 'orphan.md',          'Nobody',   3, -0.5,    3, 0.40, 0.008, FALSE),
 (8, 1, 104, 'duckdb.md',          'Appender', 1, -4.5,    1, 0.13, 0.033, TRUE),
 (8, 2, 109, 'orphan.md',          'Nobody',   2, -0.2, NULL, NULL, 0.016, FALSE);

INSERT INTO search_feedback VALUES
 (1, 1, FALSE, NULL,                          TIMESTAMP '2026-09-07 09:05:00'),
 (1, 3, TRUE,  'answered on the third hit',   TIMESTAMP '2026-09-07 09:06:00'),
 (2, 1, TRUE,  NULL,                          TIMESTAMP '2026-09-08 10:05:00'),
 (3, 1, FALSE, 'wrong entry',                 TIMESTAMP '2026-09-09 11:05:00'),
 -- droplet.md's only verdict anywhere: it makes that entry judged-and-never-useful (rate 0.0),
 -- which must read differently from an entry nobody ever rated (rate NULL). Search 3 was already
 -- judged, so this row moves nothing in overview or trend, and search 3 is fts, so it is outside
 -- the sources scope too.
 (3, 2, FALSE, 'that is the disk section',     TIMESTAMP '2026-09-09 11:06:00'),
 (6, 2, TRUE,  NULL,                          TIMESTAMP '2026-09-14 09:05:00'),
 (8, 1, FALSE, 'still not the appender docs', TIMESTAMP '2026-09-16 11:05:00');

INSERT INTO entries VALUES
 ('droplet.md',         'file', 'Droplet',         '["droplet"]',  '2026-09-01', NULL),
 ('duckdb.md',          'file', 'DuckDB',          '["duckdb"]',   '2026-09-02', '2026-09-02'),
 ('ollama.md',          'file', 'Ollama',          '["ollama"]',   '2026-09-03', NULL),
 ('orphan.md',          'file', 'Orphan',          '["misc"]',     '2026-09-04', NULL),
 ('postgres-tuning.md', 'file', 'Postgres tuning', '["postgres"]', '2026-09-05', NULL),
 ('postgres.md',        'file', 'Postgres',        '["postgres"]', '2026-09-06', NULL),
 ('ssh.md',             'file', 'SSH',             '["ssh"]',      '2026-09-07', NULL),
 ('systemd.md',         'file', 'systemd',         '["systemd"]',  '2026-09-08', NULL),
 ('vector.md',          'file', 'Vectors',         '["vector"]',   '2026-09-09', NULL);
`

// at builds a fixture timestamp; every fixture row is in September 2026 UTC.
func at(day, hour, min int) time.Time {
	return time.Date(2026, time.September, day, hour, min, 0, 0, time.UTC)
}

// openFixture returns an in-memory DuckDB holding the schema and the fixture above.
func openFixture(t *testing.T) *sql.DB {
	t.Helper()

	connector, err := duckdb.NewConnector("", nil)
	if err != nil {
		t.Fatalf("duckdb connector: %v", err)
	}
	db := sql.OpenDB(connector)
	t.Cleanup(func() {
		db.Close()
		connector.Close()
	})

	for _, stmt := range []string{testSchemaDDL, fixtureSQL} {
		if _, err := db.Exec(stmt); err != nil {
			t.Fatalf("fixture setup: %v", err)
		}
	}
	return db
}

// runQuery executes one registry query and returns its columns and rows as the driver hands them
// back: int64, float64, string, bool, time.Time or nil, which is exactly what stats.Table holds.
func runQuery(t *testing.T, db *sql.DB, sqlText string) ([]string, [][]any) {
	t.Helper()

	rows, err := db.Query(sqlText)
	if err != nil {
		t.Fatalf("query: %v", err)
	}
	defer rows.Close()

	cols, err := rows.Columns()
	if err != nil {
		t.Fatalf("columns: %v", err)
	}

	var out [][]any
	for rows.Next() {
		vals := make([]any, len(cols))
		ptrs := make([]any, len(cols))
		for i := range vals {
			ptrs[i] = &vals[i]
		}
		if err := rows.Scan(ptrs...); err != nil {
			t.Fatalf("scan: %v", err)
		}
		out = append(out, vals)
	}
	if err := rows.Err(); err != nil {
		t.Fatalf("rows: %v", err)
	}
	return cols, out
}

// checkQuery runs the named query and asserts its column names, in order, and every cell.
func checkQuery(t *testing.T, db *sql.DB, name string, wantCols []string, wantRows [][]any) {
	t.Helper()

	q, ok := Lookup(name)
	if !ok {
		t.Fatalf("Lookup(%q): not found", name)
	}
	cols, rows := runQuery(t, db, q.SQL)

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
// zero_result = 100*1/5 = 20.0. total_ms sorted = 60, 95, 100, 110, 120: quantile_cont(0.5) sits
// at index 0.5*4 = 2 -> 100.0, quantile_cont(0.95) at 0.95*4 = 3.8 -> 110 + 0.8*(120-110) = 118.0.
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
	checkQuery(t, db, "overview",
		[]string{"mode", "searches", "judged", "coverage_pct", "hit_rate_pct", "mrr",
			"zero_result_pct", "p50_ms", "p95_ms", "avg_embed_ms"},
		[][]any{
			{"hybrid", int64(5), int64(3), 60.0, 66.7, 0.444, 20.0, 100.0, 118.0, 40.0},
			{"fts", int64(2), int64(1), 50.0, 0.0, 0.0, 0.0, 25.0, 29.5, nil},
			{"fts-fallback", int64(1), int64(0), 0.0, nil, nil, 0.0, 200.0, 200.0, 150.0},
			{"vec", int64(1), int64(1), 100.0, 100.0, 0.5, 0.0, 90.0, 90.0, 45.0},
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
	checkQuery(t, db, "sources",
		[]string{"source", "results", "share_pct", "judged", "useful", "useful_rate_pct", "avg_rank"},
		[][]any{
			{"both", int64(4), 36.4, int64(3), int64(1), 33.3, 1.0},
			{"fts-only", int64(4), 36.4, int64(0), int64(0), nil, 1.75},
			{"vec-only", int64(3), 27.3, int64(1), int64(1), 100.0, 2.67},
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
	checkQuery(t, db, "trend",
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
	checkQuery(t, db, "entries",
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
	checkQuery(t, db, "gaps",
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

// TestLookup checks the registry itself: the five names in display order, a populated Summary and
// Help on each, a Help paragraph that ends in the "Decision:" sentence the plan requires, and a
// miss for a name nobody defined (which is what the CLI turns into a usage error).
func TestLookup(t *testing.T) {
	want := []string{"overview", "sources", "trend", "entries", "gaps"}

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
