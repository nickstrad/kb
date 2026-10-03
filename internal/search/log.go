// P3.3: the search log. Every search — hybrid, fts, vec, or a hybrid that degraded to
// fts-fallback — writes one searches row, one search_results row per returned hit, and one
// search_candidates row per fused candidate, all in a single transaction, so search quality can be
// analysed later with plain SQL. `kb feedback` then marks one of those results useful or not, or
// the whole search as having nothing useful (WholeSearchRank).
//
// Why one transaction: a searches row without its results, or results without their parent, would
// silently skew every recall query written against these tables afterwards. Either the whole
// search is on the record or none of it is.
//
// Why entry_path and heading are copied into both log tables: chunks.id is a plain rowid, so a
// reindex does not merely orphan the ids recorded here — it hands the same ids to unrelated
// chunks. chunk_id is kept for forensics; the copied path and heading are the columns to trust.
package search

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"os/exec"
	"strings"
	"time"
)

// gitHashTimeout bounds the `git rev-parse` call GitShortHash makes, so a wedged git (an NFS
// repo, an index.lock held by another process) cannot hang a search.
const gitHashTimeout = 2 * time.Second

// ErrLogFailed marks "the search ran, but writing it to the log did not". SearchAndLog wraps it
// around the underlying failure so the CLI can tell the two apart with errors.Is: a search whose
// log write failed still has hits to print, so the command shows them, warns on stderr, and exits
// 0. A failed search has nothing to show and exits non-zero.
var ErrLogFailed = errors.New("search log")

// Log writes one completed search to searches, search_results and search_candidates in a single
// transaction and returns the new searches.id, which it also stores in o.Result.SearchID (the
// number `kb search` prints on its first line and as search_id=N, and `kb feedback` takes).
//
// It records the mode that actually ran — o.Mode, which is "fts-fallback" when a hybrid search
// lost its embedder — not the mode the caller asked for. Columns that have no value are written
// as NULL rather than as "" or 0: an empty tag filter, a search that never reached the embedder,
// and a chunk that appeared in only one of the two lists are all different from a real zero.
//
// That applies to the per-stage timings too, which is why they go through nullMs: a stage that
// never ran is NULL, not 0, so that avg(embed_ms) over the log is the average of the searches
// that actually embedded something instead of being dragged toward zero by every fts search.
func (s *Searcher) Log(ctx context.Context, req Request, o *Outcome) (searchID int64, err error) {
	if s == nil || s.DB == nil {
		return 0, errors.New("log search: no database")
	}
	if o == nil {
		return 0, errors.New("log search: no outcome")
	}

	mode := o.Mode
	if mode == "" {
		mode = req.Mode
	}
	if mode == "" {
		mode = ModeHybrid
	}
	caller := req.Caller
	if caller == "" {
		caller = "unknown"
	}

	// Which stages ran, per mode. fts-fallback is the awkward one: the embedder was called and
	// the attempt was timed (that is what embed_ms measures), but it failed, so the vector list
	// never ran at all.
	ftsRan := mode != ModeVec
	vecRan := mode == ModeHybrid || mode == ModeVec
	embedRan := mode != ModeFTS

	tx, err := s.DB.BeginTx(ctx, nil)
	if err != nil {
		return 0, fmt.Errorf("log search: begin: %w", err)
	}
	defer func() {
		if err != nil {
			tx.Rollback()
		}
	}()

	res, err := tx.ExecContext(ctx, `
		INSERT INTO searches
			(ts, query, mode, k, tag_filter, embed_model,
			 n_fts, n_vec, n_returned, fts_ms, vec_ms, embed_ms, total_ms, caller, kb_version)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		time.Now().UTC().Format(time.RFC3339), req.Query, mode, o.K,
		nullString(o.TagFilter), nullString(o.EmbedModel),
		o.NFTS, o.NVec, len(o.Returned),
		nullMs(ftsRan, o.FTSMs), nullMs(vecRan, o.VecMs), nullMs(embedRan, o.EmbedMs), o.TotalMs,
		caller, nullString(s.KBVersion))
	if err != nil {
		return 0, fmt.Errorf("log search: insert searches: %w", err)
	}
	searchID, err = res.LastInsertId()
	if err != nil {
		return 0, fmt.Errorf("log search: search id: %w", err)
	}

	if err = insertResults(ctx, tx, searchID, o.Returned); err != nil {
		return 0, err
	}
	if err = insertCandidates(ctx, tx, searchID, o.Candidates, o.Returned); err != nil {
		return 0, err
	}

	if err = tx.Commit(); err != nil {
		return 0, fmt.Errorf("log search: commit: %w", err)
	}
	if o.Result != nil {
		o.Result.SearchID = searchID
	}
	return searchID, nil
}

// insertResults writes the k rows that were shown to the caller, ranked from 1.
func insertResults(ctx context.Context, tx *sql.Tx, searchID int64, returned []Candidate) error {
	if len(returned) == 0 {
		return nil
	}
	stmt, err := tx.PrepareContext(ctx, `
		INSERT INTO search_results
			(search_id, rank, chunk_id, entry_path, heading,
			 fts_rank, fts_score, vec_rank, vec_distance, rrf_score)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`)
	if err != nil {
		return fmt.Errorf("log search: prepare search_results: %w", err)
	}
	defer stmt.Close()

	for i, c := range returned {
		if _, err := stmt.ExecContext(ctx, searchID, i+1, c.ChunkID, c.EntryPath, c.Heading,
			nullInt(c.FTSRank), nullFloat(c.FTSScore),
			nullInt(c.VecRank), nullFloat(c.VecDistance), c.RRFScore); err != nil {
			return fmt.Errorf("log search: insert search_results rank %d: %w", i+1, err)
		}
	}
	return nil
}

// insertCandidates writes the whole fused list (D5), marking the rows that survived the diversity
// cap and k with returned=1 so recall analysis can see exactly what the cap and the truncation
// dropped.
func insertCandidates(ctx context.Context, tx *sql.Tx, searchID int64, candidates, returned []Candidate) error {
	if len(candidates) == 0 {
		return nil
	}
	wasReturned := make(map[int64]bool, len(returned))
	for _, c := range returned {
		wasReturned[c.ChunkID] = true
	}

	stmt, err := tx.PrepareContext(ctx, `
		INSERT INTO search_candidates
			(search_id, fused_rank, chunk_id, entry_path, heading,
			 fts_rank, fts_score, vec_rank, vec_distance, rrf_score, returned)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`)
	if err != nil {
		return fmt.Errorf("log search: prepare search_candidates: %w", err)
	}
	defer stmt.Close()

	for i, c := range candidates {
		flag := 0
		if wasReturned[c.ChunkID] {
			flag = 1
		}
		if _, err := stmt.ExecContext(ctx, searchID, i+1, c.ChunkID, c.EntryPath, c.Heading,
			nullInt(c.FTSRank), nullFloat(c.FTSScore),
			nullInt(c.VecRank), nullFloat(c.VecDistance), c.RRFScore, flag); err != nil {
			return fmt.Errorf("log search: insert search_candidates rank %d: %w", i+1, err)
		}
	}
	return nil
}

// SearchAndLog is what the CLI calls: run the search, then record it. A search that fell back to
// FTS is still logged — with mode "fts-fallback" — because a degraded answer is exactly the kind
// of thing the log exists to make visible. A search that failed outright is not logged: there is
// no outcome to record.
//
// When the search succeeded but the log write failed, the outcome is returned alongside an error
// wrapping ErrLogFailed, so the caller can still show the user their results and report the
// logging failure separately:
//
//	out, err := s.SearchAndLog(ctx, req)
//	if err != nil && !errors.Is(err, search.ErrLogFailed) {
//		return err // the search itself failed; out is nil
//	}
//
// In that case the whole log transaction rolled back, so out.Result.SearchID stays 0: there is no
// search id to print in the footer and nothing for `kb feedback` to attach to.
func (s *Searcher) SearchAndLog(ctx context.Context, req Request) (*Outcome, error) {
	out, err := s.Search(ctx, req)
	if err != nil {
		return nil, err
	}
	if _, err := s.Log(ctx, req, out); err != nil {
		return out, fmt.Errorf("%w: %w", ErrLogFailed, err)
	}
	return out, nil
}

// WholeSearchRank is the search_feedback.rank of a whole-search verdict: `kb feedback <id> --none`
// stores "nothing in this search helped" as one row at rank 0, a rank no result can have
// (search_results ranks start at 1). Reusing the existing table, rather than adding a column,
// needed no migration of the irreplaceable log, and it makes the stats definitions come out right
// without special cases: the row counts as a feedback row, so the search is judged, and it carries
// useful = 0, so it is never a hit. It joins no search_results row, so per-hit and per-entry
// numbers ignore it.
const WholeSearchRank = 0

// Feedback records whether the hit at rank rank of search searchID answered the question. It is an
// upsert: re-running `kb feedback` for the same (search, rank) replaces the earlier verdict rather
// than failing on the primary key, so an agent may change its mind.
//
// rank is validated against that search's own n_returned instead of being taken on trust, because
// a rank nobody was ever shown would quietly poison every hit-rate query written against this
// table later. rank 0 is therefore rejected here; the whole-search verdict goes through
// FeedbackNone.
//
// A useful verdict also withdraws an earlier whole-search "nothing useful" row for the same
// search, in the same transaction: the two contradict each other, and the later verdict wins, as
// it does for a re-marked rank.
func (s *Searcher) Feedback(ctx context.Context, searchID int64, rank int, useful bool, note string) (err error) {
	if s == nil || s.DB == nil {
		return errors.New("feedback: no database")
	}

	nReturned, err := s.searchReturned(ctx, searchID)
	if err != nil {
		return err
	}
	if nReturned == 0 {
		return fmt.Errorf("search %d has 0 results, so there is no rank %d to mark; judge it with --none", searchID, rank)
	}
	if rank < 1 || rank > nReturned {
		return fmt.Errorf("rank %d is out of range: search %d has %d results", rank, searchID, nReturned)
	}

	usefulValue := 0
	if useful {
		usefulValue = 1
	}
	tx, err := s.DB.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("feedback: begin: %w", err)
	}
	defer func() {
		if err != nil {
			tx.Rollback()
		}
	}()
	if _, err = tx.ExecContext(ctx, `
		INSERT OR REPLACE INTO search_feedback (search_id, rank, useful, note, ts)
		VALUES (?, ?, ?, ?, ?)`,
		searchID, rank, usefulValue, nullString(note), time.Now().UTC().Format(time.RFC3339)); err != nil {
		return fmt.Errorf("feedback: write search %d rank %d: %w", searchID, rank, err)
	}
	if useful {
		if _, err = tx.ExecContext(ctx,
			"DELETE FROM search_feedback WHERE search_id = ? AND rank = ?", searchID, WholeSearchRank); err != nil {
			return fmt.Errorf("feedback: withdraw the --none verdict on search %d: %w", searchID, err)
		}
	}
	if err = tx.Commit(); err != nil {
		return fmt.Errorf("feedback: commit search %d rank %d: %w", searchID, rank, err)
	}
	return nil
}

// FeedbackNone records that nothing search searchID returned answered the question — including a
// search that returned nothing at all, which has no rank to mark otherwise. It writes one
// WholeSearchRank row with useful = 0, and is an upsert like Feedback, so a second --none just
// replaces the note.
//
// It refuses a search that already carries a useful verdict instead of silently deleting it: the
// fix is to re-mark that hit --not-useful first, which leaves an explicit record of the change of
// mind. The check and the insert are one statement, so a concurrent --useful cannot slip between
// them.
func (s *Searcher) FeedbackNone(ctx context.Context, searchID int64, note string) error {
	if s == nil || s.DB == nil {
		return errors.New("feedback: no database")
	}
	if _, err := s.searchReturned(ctx, searchID); err != nil {
		return err
	}

	res, err := s.DB.ExecContext(ctx, `
		INSERT OR REPLACE INTO search_feedback (search_id, rank, useful, note, ts)
		SELECT ?, ?, 0, ?, ?
		WHERE NOT EXISTS (SELECT 1 FROM search_feedback WHERE search_id = ? AND useful = 1)`,
		searchID, WholeSearchRank, nullString(note), time.Now().UTC().Format(time.RFC3339), searchID)
	if err != nil {
		return fmt.Errorf("feedback: write search %d --none: %w", searchID, err)
	}
	n, err := res.RowsAffected()
	if err != nil {
		return fmt.Errorf("feedback: write search %d --none: %w", searchID, err)
	}
	if n > 0 {
		return nil
	}

	// Nothing was written, so a useful verdict exists; name it so the fix is one command away.
	var usefulRank sql.NullInt64
	if err := s.DB.QueryRowContext(ctx,
		"SELECT min(rank) FROM search_feedback WHERE search_id = ? AND useful = 1", searchID).Scan(&usefulRank); err != nil {
		return fmt.Errorf("feedback: read search %d verdicts: %w", searchID, err)
	}
	if !usefulRank.Valid {
		// The useful verdict was re-marked between the two statements; nothing was written.
		return fmt.Errorf("search %d's verdicts changed while recording --none; run the command again", searchID)
	}
	return fmt.Errorf("search %d already has rank %d marked useful, which contradicts --none; "+
		"if it did not help, run `kb feedback %d %d --not-useful` first", searchID, usefulRank.Int64, searchID, usefulRank.Int64)
}

// LastSearch is the newest logged search for one caller: what `kb feedback --last` targets.
type LastSearch struct {
	ID        int64
	Query     string
	TS        string // ISO-8601 UTC, verbatim from searches.ts
	NReturned int
}

// ErrNoSearch marks "this caller has no logged search", so the CLI can word it as a usage error.
var ErrNoSearch = errors.New("no logged search")

// Last returns the newest search logged by caller. "Newest" is the highest searches.id rather
// than the latest ts: ids are assigned in commit order, while ts only has one-second resolution.
//
// It cannot tell two concurrent sessions that share a caller name apart — the log records no
// session — so the CLI documents that limitation and echoes the chosen search's query back.
func (s *Searcher) Last(ctx context.Context, caller string) (LastSearch, error) {
	if s == nil || s.DB == nil {
		return LastSearch{}, errors.New("feedback: no database")
	}
	var ls LastSearch
	err := s.DB.QueryRowContext(ctx, `
		SELECT id, query, ts, n_returned FROM searches
		WHERE caller = ?
		ORDER BY id DESC
		LIMIT 1`, caller).Scan(&ls.ID, &ls.Query, &ls.TS, &ls.NReturned)
	if errors.Is(err, sql.ErrNoRows) {
		return LastSearch{}, fmt.Errorf("%w for caller %q", ErrNoSearch, caller)
	}
	if err != nil {
		return LastSearch{}, fmt.Errorf("feedback: read newest search for caller %q: %w", caller, err)
	}
	return ls, nil
}

// searchReturned reads n_returned for one search, turning a missing search into the error every
// feedback path reports.
func (s *Searcher) searchReturned(ctx context.Context, searchID int64) (int, error) {
	var nReturned int
	err := s.DB.QueryRowContext(ctx, "SELECT n_returned FROM searches WHERE id = ?", searchID).Scan(&nReturned)
	if errors.Is(err, sql.ErrNoRows) {
		return 0, fmt.Errorf("no search with id %d; `kb search` prints the id on its first line (search N · …)", searchID)
	}
	if err != nil {
		return 0, fmt.Errorf("feedback: read search %d: %w", searchID, err)
	}
	return nReturned, nil
}

// GitShortHash is the repo's current commit, short form, for searches.kb_version — the column that
// says which build of the knowledge base produced a logged result. It returns "" for every failure
// (no git, not a repository, an empty repository, a timeout): the hash is diagnostic metadata, and
// nothing about a search should fail because git did.
func GitShortHash(root string) string {
	ctx, cancel := context.WithTimeout(context.Background(), gitHashTimeout)
	defer cancel()

	out, err := exec.CommandContext(ctx, "git", "-C", root, "rev-parse", "--short", "HEAD").Output()
	if err != nil {
		return ""
	}
	return strings.TrimSpace(string(out))
}

// nullString writes "" as SQL NULL, keeping "no tag filter" and "no embedder" distinct from a tag
// or a model that is literally the empty string.
func nullString(s string) any {
	if s == "" {
		return nil
	}
	return s
}

// nullInt and nullFloat write a missing rank or score as SQL NULL rather than as 0, which for
// bm25() (negative, lower is better) would otherwise read as a plausible score.
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

// nullMs writes a stage's duration only when that stage ran. A stage that never ran is NULL, which
// is the difference between "this search did not embed anything" and "embedding took under a
// millisecond" — a distinction every average over fts_ms, vec_ms or embed_ms depends on.
// total_ms never goes through here: every search has a total.
func nullMs(ran bool, ms int64) any {
	if !ran {
		return nil
	}
	return ms
}
