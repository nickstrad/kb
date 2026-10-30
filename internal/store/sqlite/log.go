// The store.SearchLog implementation. One search writes one searches row, one search_results row
// per returned hit and one search_candidates row per fused candidate, in a single transaction, so
// a search is either wholly on the record or not at all. entry_path and heading are copied into
// both log tables because chunks.id is a plain rowid: a reindex hands the same ids to unrelated
// chunks, so chunk_id is kept only for forensics.

package sqlite

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"time"

	"github.com/nickstrad/kb/internal/errs"
	"github.com/nickstrad/kb/internal/store"
)

// LogSearch writes r and returns the new searches.id. Missing values (no tag, nothing embedded, a
// stage that did not run, a chunk absent from one list, a hit of this store) are written as NULL,
// never "" or 0. r.TS is the search's time; zero means now.
func (s *Store) LogSearch(ctx context.Context, r store.SearchRecord) (int64, error) {
	id, err := s.logSearch(ctx, r)
	if err != nil {
		return 0, wrap(ctx, err, s.path, "sqlite.LogSearch", "could not write the search to the log")
	}
	return id, nil
}

func (s *Store) logSearch(ctx context.Context, r store.SearchRecord) (int64, error) {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return 0, fmt.Errorf("begin: %w", err)
	}
	defer tx.Rollback()

	res, err := tx.ExecContext(ctx, `
		INSERT INTO searches
			(ts, query, mode, k, tag_filter, embed_model,
			 n_fts, n_vec, n_returned, fts_ms, vec_ms, embed_ms, total_ms, caller, kb_version, corpus_rev,
			 settings, settings_hash, fts_query)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		stamp(r.TS), r.Query, r.Mode, r.K, nullString(r.Tag), nullString(r.EmbedModel),
		r.NFTS, r.NVec, len(r.Results), nullMs(r.FTSMs), nullMs(r.VecMs), nullMs(r.EmbedMs), r.TotalMs,
		r.Caller, nullString(r.KBVersion), nullString(r.CorpusRev),
		nullString(r.Settings), nullString(r.SettingsHash), nullString(r.FTSMatch))
	if err != nil {
		return 0, fmt.Errorf("insert searches: %w", err)
	}
	searchID, err := res.LastInsertId()
	if err != nil {
		return 0, fmt.Errorf("search id: %w", err)
	}
	if err := insertResults(ctx, tx, searchID, r.Results); err != nil {
		return 0, err
	}
	if err := insertCandidates(ctx, tx, searchID, r.Candidates, r.Results); err != nil {
		return 0, err
	}
	if err := tx.Commit(); err != nil {
		return 0, fmt.Errorf("commit: %w", err)
	}
	return searchID, nil
}

// insertResults writes the hits the caller was shown, ranked from 1.
func insertResults(ctx context.Context, tx *sql.Tx, searchID int64, results []store.LoggedHit) error {
	if len(results) == 0 {
		return nil
	}
	stmt, err := tx.PrepareContext(ctx, `
		INSERT INTO search_results
			(search_id, rank, store, chunk_id, entry_path, heading,
			 fts_rank, fts_score, vec_rank, vec_distance, exact_rank, exact_score, rrf_score, prior,
			 memory_rank, memory_score, fts_tier)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`)
	if err != nil {
		return fmt.Errorf("prepare search_results: %w", err)
	}
	defer stmt.Close()
	for i, c := range results {
		if _, err := stmt.ExecContext(ctx, searchID, i+1, nullString(c.Store), c.ChunkID, c.EntryPath, c.Heading,
			nullInt(c.FTSRank), nullFloat(c.FTSScore), nullInt(c.VecRank), nullFloat(c.VecDistance),
			nullInt(c.ExactRank), nullFloat(c.ExactScore), c.RRFScore, nullFloat(c.Prior),
			nullInt(c.MemoryRank), nullFloat(c.MemoryScore), nullString(c.FTSTier)); err != nil {
			return fmt.Errorf("insert search_results rank %d: %w", i+1, err)
		}
	}
	return nil
}

// hitKey identifies a logged hit: chunk ids are per store, so two stores can share one.
type hitKey struct {
	store string
	chunk int64
}

// insertCandidates writes the whole fused list, with returned=1 on the rows that are also in
// results, so recall analysis can see what the cap and k dropped.
func insertCandidates(ctx context.Context, tx *sql.Tx, searchID int64, candidates, results []store.LoggedHit) error {
	if len(candidates) == 0 {
		return nil
	}
	wasReturned := make(map[hitKey]bool, len(results))
	for _, c := range results {
		wasReturned[hitKey{c.Store, c.ChunkID}] = true
	}
	stmt, err := tx.PrepareContext(ctx, `
		INSERT INTO search_candidates
			(search_id, fused_rank, store, chunk_id, entry_path, heading,
			 fts_rank, fts_score, vec_rank, vec_distance, exact_rank, exact_score, rrf_score, prior,
			 memory_rank, memory_score, fts_tier, returned)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`)
	if err != nil {
		return fmt.Errorf("prepare search_candidates: %w", err)
	}
	defer stmt.Close()
	for i, c := range candidates {
		flag := 0
		if wasReturned[hitKey{c.Store, c.ChunkID}] {
			flag = 1
		}
		if _, err := stmt.ExecContext(ctx, searchID, i+1, nullString(c.Store), c.ChunkID, c.EntryPath, c.Heading,
			nullInt(c.FTSRank), nullFloat(c.FTSScore), nullInt(c.VecRank), nullFloat(c.VecDistance),
			nullInt(c.ExactRank), nullFloat(c.ExactScore), c.RRFScore, nullFloat(c.Prior),
			nullInt(c.MemoryRank), nullFloat(c.MemoryScore), nullString(c.FTSTier), flag); err != nil {
			return fmt.Errorf("insert search_candidates rank %d: %w", i+1, err)
		}
	}
	return nil
}

// Feedback records whether the hit at rank of searchID was useful. It is an upsert, so a caller
// may change its mind. rank is checked against that search's n_returned, because a rank nobody was
// shown would poison every hit-rate query; rank 0 is FeedbackNone's. A useful verdict also
// withdraws an earlier whole-search --none, in the same transaction.
func (s *Store) Feedback(ctx context.Context, searchID int64, rank int, useful bool, note string, at time.Time) error {
	if err := s.feedback(ctx, searchID, rank, useful, note, at); err != nil {
		return wrap(ctx, err, s.path, "sqlite.Feedback", "could not record feedback on search %d", searchID)
	}
	return nil
}

func (s *Store) feedback(ctx context.Context, searchID int64, rank int, useful bool, note string, at time.Time) error {
	const op = "sqlite.Feedback"
	nReturned, err := s.searchReturned(ctx, searchID)
	if err != nil {
		return err
	}
	if nReturned == 0 {
		return errs.New(errs.Usage, op, "search %d has 0 results, so there is no rank %d to mark; judge it with --none", searchID, rank)
	}
	if rank < 1 || rank > nReturned {
		return errs.New(errs.Usage, op, "rank %d is out of range: search %d has %d results", rank, searchID, nReturned)
	}
	usefulValue := 0
	if useful {
		usefulValue = 1
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("begin: %w", err)
	}
	defer tx.Rollback()
	if _, err := tx.ExecContext(ctx, `
		INSERT OR REPLACE INTO search_feedback (search_id, rank, useful, note, ts)
		VALUES (?, ?, ?, ?, ?)`,
		searchID, rank, usefulValue, nullString(note), stamp(at)); err != nil {
		return fmt.Errorf("write search %d rank %d: %w", searchID, rank, err)
	}
	if useful {
		if _, err := tx.ExecContext(ctx,
			"DELETE FROM search_feedback WHERE search_id = ? AND rank = ?", searchID, store.WholeSearchRank); err != nil {
			return fmt.Errorf("withdraw the --none verdict on search %d: %w", searchID, err)
		}
	}
	if err := tx.Commit(); err != nil {
		return fmt.Errorf("commit search %d rank %d: %w", searchID, rank, err)
	}
	return nil
}

// FeedbackNone records that nothing searchID returned was useful: one WholeSearchRank row with
// useful = 0, an upsert. A search that already has a useful verdict is a Conflict naming that
// rank; the check and the insert are one statement, so a concurrent --useful cannot slip between.
func (s *Store) FeedbackNone(ctx context.Context, searchID int64, note string, at time.Time) error {
	if err := s.feedbackNone(ctx, searchID, note, at); err != nil {
		return wrap(ctx, err, s.path, "sqlite.FeedbackNone", "could not record --none on search %d", searchID)
	}
	return nil
}

func (s *Store) feedbackNone(ctx context.Context, searchID int64, note string, at time.Time) error {
	const op = "sqlite.FeedbackNone"
	if _, err := s.searchReturned(ctx, searchID); err != nil {
		return err
	}
	res, err := s.db.ExecContext(ctx, `
		INSERT OR REPLACE INTO search_feedback (search_id, rank, useful, note, ts)
		SELECT ?, ?, 0, ?, ?
		WHERE NOT EXISTS (SELECT 1 FROM search_feedback WHERE search_id = ? AND useful = 1)`,
		searchID, store.WholeSearchRank, nullString(note), stamp(at), searchID)
	if err != nil {
		return fmt.Errorf("write search %d --none: %w", searchID, err)
	}
	n, err := res.RowsAffected()
	if err != nil {
		return fmt.Errorf("write search %d --none: %w", searchID, err)
	}
	if n > 0 {
		return nil
	}
	var usefulRank sql.NullInt64
	if err := s.db.QueryRowContext(ctx,
		"SELECT min(rank) FROM search_feedback WHERE search_id = ? AND useful = 1", searchID).Scan(&usefulRank); err != nil {
		return fmt.Errorf("read search %d verdicts: %w", searchID, err)
	}
	if !usefulRank.Valid {
		return errs.New(errs.Conflict, op, "search %d's verdicts changed while recording --none; run the command again", searchID)
	}
	return errs.New(errs.Conflict, op, "search %d already has rank %d marked useful, which contradicts --none; "+
		"if it did not help, run `kb feedback %d %d --not-useful` first", searchID, usefulRank.Int64, searchID, usefulRank.Int64)
}

// RecordOpen notes that o.EntryPath of o.Store was read at o.At (kb show, kb show REF:path). It
// attaches the open to the newest search by id at most o.Window old (inclusive, at the log's
// one-second resolution) that returned that entry of that store (store IS NULL for this store's
// own entries, so a same-named path of another store never matches), and writes at most one row
// per (search, store, path), with the store (NULL = this one). Only returned results count: the
// reader never saw the candidates.
func (s *Store) RecordOpen(ctx context.Context, o store.OpenRecord) (bool, error) {
	entryPath := o.EntryPath
	now := o.At.UTC()
	res, err := s.db.ExecContext(ctx, `
		INSERT OR IGNORE INTO search_opens (search_id, store, entry_path, ts)
		SELECT s.id, ?4, ?1, ?2 FROM searches AS s
		WHERE s.ts >= ?3
		  AND EXISTS (SELECT 1 FROM search_results AS r WHERE r.search_id = s.id AND r.entry_path = ?1 AND r.store IS ?4)
		ORDER BY s.id DESC
		LIMIT 1`,
		entryPath, now.Format(time.RFC3339), now.Add(-o.Window).Format(time.RFC3339), nullString(o.Store))
	if err != nil {
		return false, wrap(ctx, err, s.path, "sqlite.RecordOpen", "could not record the open of %s", entryPath)
	}
	n, err := res.RowsAffected()
	if err != nil {
		return false, wrap(ctx, err, s.path, "sqlite.RecordOpen", "could not record the open of %s", entryPath)
	}
	return n > 0, nil
}

// LastSearch returns the newest search logged by caller. "Newest" is the highest id, which follows
// commit order; ts has only one-second resolution. No such search is NotFound.
func (s *Store) LastSearch(ctx context.Context, caller string) (store.LastSearch, error) {
	var ls store.LastSearch
	err := s.db.QueryRowContext(ctx, `
		SELECT id, query, ts, n_returned FROM searches
		WHERE caller = ?
		ORDER BY id DESC
		LIMIT 1`, caller).Scan(&ls.ID, &ls.Query, &ls.TS, &ls.NReturned)
	if errors.Is(err, sql.ErrNoRows) {
		return store.LastSearch{}, errs.New(errs.NotFound, "sqlite.LastSearch", "no search logged for caller %q", caller).With("caller", caller)
	}
	if err != nil {
		return store.LastSearch{}, wrap(ctx, err, s.path, "sqlite.LastSearch", "could not read the newest search for caller %q", caller)
	}
	return ls, nil
}

// searchReturned reads n_returned for one search; a missing search is NotFound.
func (s *Store) searchReturned(ctx context.Context, searchID int64) (int, error) {
	var n int
	err := s.db.QueryRowContext(ctx, "SELECT n_returned FROM searches WHERE id = ?", searchID).Scan(&n)
	if errors.Is(err, sql.ErrNoRows) {
		return 0, errs.New(errs.NotFound, "sqlite.Feedback",
			"no search with id %d; `kb search` prints the id on its first line (search N · …)", searchID).With("search_id", searchID)
	}
	if err != nil {
		return 0, fmt.Errorf("read search %d: %w", searchID, err)
	}
	return n, nil
}

// stamp is the searches.ts text of t: UTC, one-second resolution; the zero time is now.
func stamp(t time.Time) string {
	if t.IsZero() {
		return nowUTC()
	}
	return t.UTC().Format(time.RFC3339)
}

// nullInt and nullFloat write a missing rank or score as NULL rather than 0, which for bm25()
// would read as a plausible score.
func nullInt(p *int) any {
	if p == nil {
		return nil
	}
	return *p
}

func nullFloat(p *float64) any {
	if p == nil {
		return nil
	}
	return *p
}

// nullMs writes a stage that did not run as NULL, so averages over a stage count only the
// searches that ran it.
func nullMs(p *int64) any {
	if p == nil {
		return nil
	}
	return *p
}
