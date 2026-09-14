// Package stats loads the search log (searches, search_results, search_candidates,
// search_feedback) and the entries table out of the SQLite database into an in-memory DuckDB, so
// `kb stats` can run analytic SQL — window functions, quantile_cont, correlated subqueries —
// that SQLite either cannot express or expresses awkwardly.
//
// D2 (plan.md): this package copies, it does not attach. Load opens the SQLite file read-only,
// creates the five tables below in a fresh in-memory DuckDB and bulk-loads them with the
// Appender. It never writes to the SQLite file and never creates one, so a missing database is a
// clear error rather than an empty one materialising on disk.
package stats

import (
	"context"
	"database/sql"
	"database/sql/driver"
	"fmt"
	"net/url"
	"os"
	"strings"
	"time"

	duckdb "github.com/marcboeker/go-duckdb/v2"
	_ "github.com/mattn/go-sqlite3"
)

// schemaDDL is the DuckDB schema the log is copied into — the contract shared with the query
// registry (S3) and the CLI (S4). Column order matches the Appender calls in copy*.go exactly:
// the Appender writes positionally, so a column added here without a matching AppendRow argument
// silently shifts every value after it.
const schemaDDL = `
CREATE TABLE searches (
  id          BIGINT PRIMARY KEY,
  ts          TIMESTAMP NOT NULL,
  query       VARCHAR NOT NULL,
  mode        VARCHAR NOT NULL,
  k           INTEGER NOT NULL,
  tag_filter  VARCHAR,
  embed_model VARCHAR,
  n_fts       INTEGER NOT NULL,
  n_vec       INTEGER NOT NULL,
  n_returned  INTEGER NOT NULL,
  fts_ms INTEGER, vec_ms INTEGER, embed_ms INTEGER, total_ms INTEGER,
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
  tags VARCHAR NOT NULL,
  updated VARCHAR, verified VARCHAR
);
`

// Filter narrows which searches are loaded (D3). The zero value loads everything.
type Filter struct {
	Since  time.Time // zero = no lower bound; keeps searches with ts >= Since
	Caller string    // "" = every caller
}

// DB is an in-memory DuckDB holding a typed copy of the search log.
type DB struct {
	db             *sql.DB
	searchesLoaded int64
}

// Table is one query's output. Values are the driver's Go types: int64, float64, string, bool,
// time.Time or nil.
type Table struct {
	Columns []string
	Rows    [][]any
}

// Load opens the SQLite file read-only, creates the schema above in a fresh in-memory DuckDB and
// copies searches (filtered by f), their search_results / search_candidates / search_feedback
// rows, and every entries row. It never creates or modifies the SQLite file.
func Load(ctx context.Context, sqlitePath string, f Filter) (*DB, error) {
	if _, err := os.Stat(sqlitePath); err != nil {
		return nil, fmt.Errorf("stats: no database at %s: %w", sqlitePath, err)
	}

	// mode=ro refuses to create the file if it somehow vanished between the Stat above and here,
	// and guarantees this package can never be the thing that writes to the search log.
	dsn := (&url.URL{Scheme: "file", Path: sqlitePath}).String() + "?mode=ro&_busy_timeout=5000"
	sqliteDB, err := sql.Open("sqlite3", dsn)
	if err != nil {
		return nil, fmt.Errorf("stats: open %s: %w", sqlitePath, err)
	}
	defer sqliteDB.Close()
	if err := sqliteDB.PingContext(ctx); err != nil {
		return nil, fmt.Errorf("stats: open %s: %w", sqlitePath, err)
	}

	connector, err := duckdb.NewConnector("", nil)
	if err != nil {
		return nil, fmt.Errorf("stats: open in-memory duckdb: %w", err)
	}
	// sql.OpenDB takes ownership of connector: duckDB.Close() closes it too.
	duckDB := sql.OpenDB(connector)

	if _, err := duckDB.ExecContext(ctx, schemaDDL); err != nil {
		duckDB.Close()
		return nil, fmt.Errorf("stats: create schema: %w", err)
	}

	// One dedicated raw connection for the Appenders: NewAppenderFromConn needs the driver.Conn
	// itself, not a pooled *sql.Conn. It shares the same in-memory database as duckDB (both come
	// from the same connector), so closing it once every table is copied does not lose the data.
	appConn, err := connector.Connect(ctx)
	if err != nil {
		duckDB.Close()
		return nil, fmt.Errorf("stats: open duckdb load connection: %w", err)
	}

	where, args := buildFilter(f)
	n, copyErr := copyAll(ctx, sqliteDB, appConn, where, args)
	if closeErr := appConn.Close(); closeErr != nil && copyErr == nil {
		copyErr = fmt.Errorf("stats: close duckdb load connection: %w", closeErr)
	}
	if copyErr != nil {
		duckDB.Close()
		return nil, copyErr
	}

	return &DB{db: duckDB, searchesLoaded: n}, nil
}

// buildFilter renders Filter as a SQL WHERE clause (D3) and its positional arguments. ts is
// compared as RFC3339 text: log.go always writes it with time.RFC3339 (fixed width, UTC "Z"
// suffix, no fractional seconds), which sorts identically to chronological order.
func buildFilter(f Filter) (where string, args []any) {
	var conds []string
	if !f.Since.IsZero() {
		conds = append(conds, "ts >= ?")
		args = append(args, f.Since.UTC().Format(time.RFC3339))
	}
	if f.Caller != "" {
		conds = append(conds, "caller = ?")
		args = append(args, f.Caller)
	}
	if len(conds) == 0 {
		return "", nil
	}
	return " WHERE " + strings.Join(conds, " AND "), args
}

// Query runs sqlText against the loaded DuckDB and returns every row as plain Go values.
func (d *DB) Query(ctx context.Context, sqlText string) (*Table, error) {
	rows, err := d.db.QueryContext(ctx, sqlText)
	if err != nil {
		return nil, fmt.Errorf("stats: query: %w", err)
	}
	defer rows.Close()

	cols, err := rows.Columns()
	if err != nil {
		return nil, fmt.Errorf("stats: query: read columns: %w", err)
	}
	t := &Table{Columns: cols}

	for rows.Next() {
		raw := make([]any, len(cols))
		ptrs := make([]any, len(cols))
		for i := range raw {
			ptrs[i] = &raw[i]
		}
		if err := rows.Scan(ptrs...); err != nil {
			return nil, fmt.Errorf("stats: query: scan row %d: %w", len(t.Rows)+1, err)
		}
		row := make([]any, len(cols))
		for i, v := range raw {
			row[i] = normalizeValue(v)
		}
		t.Rows = append(t.Rows, row)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("stats: query: %w", err)
	}
	return t, nil
}

// normalizeValue turns whatever concrete type the DuckDB driver scanned a column into as one of
// the plain values Table promises: int64, float64, string, bool, time.Time or nil. The driver
// hands back its narrowest native width (an INTEGER column scans as int32, not int64), so those
// widths are widened explicitly; a type this schema never produces (HUGEINT, DECIMAL) is rendered
// with fmt.Sprint rather than making Query fail on a query nobody anticipated.
func normalizeValue(v any) any {
	switch x := v.(type) {
	case nil, int64, float64, string, bool, time.Time:
		return x
	case int:
		return int64(x)
	case int8:
		return int64(x)
	case int16:
		return int64(x)
	case int32:
		return int64(x)
	case uint:
		return int64(x)
	case uint8:
		return int64(x)
	case uint16:
		return int64(x)
	case uint32:
		return int64(x)
	case uint64:
		return int64(x)
	case float32:
		return float64(x)
	case []byte:
		return string(x)
	default:
		return fmt.Sprint(x)
	}
}

// copyAll copies the five tables in dependency order (searches before its children; entries is
// independent) and returns the number of searches rows loaded.
func copyAll(ctx context.Context, sqliteDB *sql.DB, conn driver.Conn, where string, args []any) (int64, error) {
	n, err := copySearches(ctx, sqliteDB, conn, where, args)
	if err != nil {
		return 0, err
	}
	if err := copySearchResults(ctx, sqliteDB, conn, where, args); err != nil {
		return 0, err
	}
	if err := copySearchCandidates(ctx, sqliteDB, conn, where, args); err != nil {
		return 0, err
	}
	if err := copySearchFeedback(ctx, sqliteDB, conn, where, args); err != nil {
		return 0, err
	}
	if err := copyEntries(ctx, sqliteDB, conn); err != nil {
		return 0, err
	}
	return n, nil
}

// copySearches copies the searches rows matching where/args, parsing ts and widening SQLite's
// nullable INTEGER columns to the DuckDB INTEGER width (int32) the Appender expects.
func copySearches(ctx context.Context, sqliteDB *sql.DB, conn driver.Conn, where string, args []any) (n int64, err error) {
	query := fmt.Sprintf(`
		SELECT id, ts, query, mode, k, tag_filter, embed_model,
		       n_fts, n_vec, n_returned, fts_ms, vec_ms, embed_ms, total_ms, caller, kb_version
		FROM searches%s ORDER BY id`, where)
	rows, err := sqliteDB.QueryContext(ctx, query, args...)
	if err != nil {
		return 0, fmt.Errorf("stats: read searches: %w", err)
	}
	defer rows.Close()

	app, err := duckdb.NewAppenderFromConn(conn, "", "searches")
	if err != nil {
		return 0, fmt.Errorf("stats: create searches appender: %w", err)
	}
	defer func() {
		if cerr := app.Close(); cerr != nil && err == nil {
			err = fmt.Errorf("stats: close searches appender: %w", cerr)
		}
	}()

	for rows.Next() {
		var id int64
		var ts, query_, mode, caller string
		var k, nFTS, nVec, nReturned int
		var tagFilter, embedModel, kbVersion sql.NullString
		var ftsMs, vecMs, embedMs, totalMs sql.NullInt64
		if err = rows.Scan(&id, &ts, &query_, &mode, &k, &tagFilter, &embedModel,
			&nFTS, &nVec, &nReturned, &ftsMs, &vecMs, &embedMs, &totalMs, &caller, &kbVersion); err != nil {
			return 0, fmt.Errorf("stats: scan searches: %w", err)
		}
		tsVal, perr := time.Parse(time.RFC3339, ts)
		if perr != nil {
			err = fmt.Errorf("stats: parse searches.ts %q (search %d): %w", ts, id, perr)
			return 0, err
		}
		if err = app.AppendRow(
			id, tsVal, query_, mode, int32(k), nullString(tagFilter), nullString(embedModel),
			int32(nFTS), int32(nVec), int32(nReturned),
			nullInt32(ftsMs), nullInt32(vecMs), nullInt32(embedMs), nullInt32(totalMs),
			caller, nullString(kbVersion),
		); err != nil {
			err = fmt.Errorf("stats: append searches row %d: %w", id, err)
			return 0, err
		}
		n++
	}
	if err = rows.Err(); err != nil {
		return 0, fmt.Errorf("stats: read searches: %w", err)
	}
	return n, nil
}

// copySearchResults copies the search_results rows belonging to the searches selected by
// where/args.
func copySearchResults(ctx context.Context, sqliteDB *sql.DB, conn driver.Conn, where string, args []any) (err error) {
	query := fmt.Sprintf(`
		SELECT search_id, rank, chunk_id, entry_path, heading,
		       fts_rank, fts_score, vec_rank, vec_distance, rrf_score
		FROM search_results
		WHERE search_id IN (SELECT id FROM searches%s)
		ORDER BY search_id, rank`, where)
	rows, err := sqliteDB.QueryContext(ctx, query, args...)
	if err != nil {
		return fmt.Errorf("stats: read search_results: %w", err)
	}
	defer rows.Close()

	app, err := duckdb.NewAppenderFromConn(conn, "", "search_results")
	if err != nil {
		return fmt.Errorf("stats: create search_results appender: %w", err)
	}
	defer func() {
		if cerr := app.Close(); cerr != nil && err == nil {
			err = fmt.Errorf("stats: close search_results appender: %w", cerr)
		}
	}()

	for rows.Next() {
		var searchID int64
		var rank int
		var chunkID sql.NullInt64
		var entryPath, heading string
		var ftsRank, vecRank sql.NullInt64
		var ftsScore, vecDistance sql.NullFloat64
		var rrfScore float64
		if err = rows.Scan(&searchID, &rank, &chunkID, &entryPath, &heading,
			&ftsRank, &ftsScore, &vecRank, &vecDistance, &rrfScore); err != nil {
			return fmt.Errorf("stats: scan search_results: %w", err)
		}
		if err = app.AppendRow(
			searchID, int32(rank), nullInt64(chunkID), entryPath, heading,
			nullInt32(ftsRank), nullFloat64(ftsScore), nullInt32(vecRank), nullFloat64(vecDistance),
			rrfScore,
		); err != nil {
			return fmt.Errorf("stats: append search_results row (search %d, rank %d): %w", searchID, rank, err)
		}
	}
	if err = rows.Err(); err != nil {
		return fmt.Errorf("stats: read search_results: %w", err)
	}
	return nil
}

// copySearchCandidates copies the search_candidates rows belonging to the searches selected by
// where/args.
func copySearchCandidates(ctx context.Context, sqliteDB *sql.DB, conn driver.Conn, where string, args []any) (err error) {
	query := fmt.Sprintf(`
		SELECT search_id, fused_rank, chunk_id, entry_path, heading,
		       fts_rank, fts_score, vec_rank, vec_distance, rrf_score, returned
		FROM search_candidates
		WHERE search_id IN (SELECT id FROM searches%s)
		ORDER BY search_id, fused_rank`, where)
	rows, err := sqliteDB.QueryContext(ctx, query, args...)
	if err != nil {
		return fmt.Errorf("stats: read search_candidates: %w", err)
	}
	defer rows.Close()

	app, err := duckdb.NewAppenderFromConn(conn, "", "search_candidates")
	if err != nil {
		return fmt.Errorf("stats: create search_candidates appender: %w", err)
	}
	defer func() {
		if cerr := app.Close(); cerr != nil && err == nil {
			err = fmt.Errorf("stats: close search_candidates appender: %w", cerr)
		}
	}()

	for rows.Next() {
		var searchID int64
		var fusedRank int
		var chunkID sql.NullInt64
		var entryPath, heading string
		var ftsRank, vecRank sql.NullInt64
		var ftsScore, vecDistance sql.NullFloat64
		var rrfScore float64
		var returned int
		if err = rows.Scan(&searchID, &fusedRank, &chunkID, &entryPath, &heading,
			&ftsRank, &ftsScore, &vecRank, &vecDistance, &rrfScore, &returned); err != nil {
			return fmt.Errorf("stats: scan search_candidates: %w", err)
		}
		if err = app.AppendRow(
			searchID, int32(fusedRank), nullInt64(chunkID), entryPath, heading,
			nullInt32(ftsRank), nullFloat64(ftsScore), nullInt32(vecRank), nullFloat64(vecDistance),
			rrfScore, returned != 0,
		); err != nil {
			return fmt.Errorf("stats: append search_candidates row (search %d, fused_rank %d): %w", searchID, fusedRank, err)
		}
	}
	if err = rows.Err(); err != nil {
		return fmt.Errorf("stats: read search_candidates: %w", err)
	}
	return nil
}

// copySearchFeedback copies the search_feedback rows belonging to the searches selected by
// where/args.
func copySearchFeedback(ctx context.Context, sqliteDB *sql.DB, conn driver.Conn, where string, args []any) (err error) {
	query := fmt.Sprintf(`
		SELECT search_id, rank, useful, note, ts
		FROM search_feedback
		WHERE search_id IN (SELECT id FROM searches%s)
		ORDER BY search_id, rank`, where)
	rows, err := sqliteDB.QueryContext(ctx, query, args...)
	if err != nil {
		return fmt.Errorf("stats: read search_feedback: %w", err)
	}
	defer rows.Close()

	app, err := duckdb.NewAppenderFromConn(conn, "", "search_feedback")
	if err != nil {
		return fmt.Errorf("stats: create search_feedback appender: %w", err)
	}
	defer func() {
		if cerr := app.Close(); cerr != nil && err == nil {
			err = fmt.Errorf("stats: close search_feedback appender: %w", cerr)
		}
	}()

	for rows.Next() {
		var searchID int64
		var rank, useful int
		var note sql.NullString
		var ts string
		if err = rows.Scan(&searchID, &rank, &useful, &note, &ts); err != nil {
			return fmt.Errorf("stats: scan search_feedback: %w", err)
		}
		tsVal, perr := time.Parse(time.RFC3339, ts)
		if perr != nil {
			err = fmt.Errorf("stats: parse search_feedback.ts %q (search %d rank %d): %w", ts, searchID, rank, perr)
			return err
		}
		if err = app.AppendRow(searchID, int32(rank), useful != 0, nullString(note), tsVal); err != nil {
			return fmt.Errorf("stats: append search_feedback row (search %d, rank %d): %w", searchID, rank, err)
		}
	}
	if err = rows.Err(); err != nil {
		return fmt.Errorf("stats: read search_feedback: %w", err)
	}
	return nil
}

// copyEntries copies every entries row, unfiltered (D3: entries is always copied whole).
func copyEntries(ctx context.Context, sqliteDB *sql.DB, conn driver.Conn) (err error) {
	rows, err := sqliteDB.QueryContext(ctx, `
		SELECT path, kind, title, tags, updated, verified FROM entries ORDER BY path`)
	if err != nil {
		return fmt.Errorf("stats: read entries: %w", err)
	}
	defer rows.Close()

	app, err := duckdb.NewAppenderFromConn(conn, "", "entries")
	if err != nil {
		return fmt.Errorf("stats: create entries appender: %w", err)
	}
	defer func() {
		if cerr := app.Close(); cerr != nil && err == nil {
			err = fmt.Errorf("stats: close entries appender: %w", cerr)
		}
	}()

	for rows.Next() {
		var path, kind, title, tags string
		var updated, verified sql.NullString
		if err = rows.Scan(&path, &kind, &title, &tags, &updated, &verified); err != nil {
			return fmt.Errorf("stats: scan entries: %w", err)
		}
		if err = app.AppendRow(path, kind, title, tags, nullString(updated), nullString(verified)); err != nil {
			return fmt.Errorf("stats: append entries row %s: %w", path, err)
		}
	}
	if err = rows.Err(); err != nil {
		return fmt.Errorf("stats: read entries: %w", err)
	}
	return nil
}

// nullString, nullInt32, nullInt64 and nullFloat64 turn a SQL NULL into a Go nil so the Appender
// writes a genuine NULL rather than a zero value that would read as a real measurement.
func nullString(s sql.NullString) any {
	if !s.Valid {
		return nil
	}
	return s.String
}

func nullInt32(n sql.NullInt64) any {
	if !n.Valid {
		return nil
	}
	return int32(n.Int64)
}

func nullInt64(n sql.NullInt64) any {
	if !n.Valid {
		return nil
	}
	return n.Int64
}

func nullFloat64(f sql.NullFloat64) any {
	if !f.Valid {
		return nil
	}
	return f.Float64
}

// SearchesLoaded returns the number of rows loaded into the searches table (after Filter).
func (d *DB) SearchesLoaded() int64 { return d.searchesLoaded }

// Close closes the in-memory DuckDB.
func (d *DB) Close() error {
	if d == nil || d.db == nil {
		return nil
	}
	return d.db.Close()
}
