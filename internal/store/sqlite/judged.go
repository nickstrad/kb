// The store.JudgedLog implementation: the search log read back for kb eval.

package sqlite

import (
	"context"
	"fmt"
	"time"

	"github.com/nickstrad/kb/internal/errs"
	"github.com/nickstrad/kb/internal/store"
)

var _ store.JudgedLog = (*Store)(nil)

// Judged reads the searches matching f with their verdicts, --none and opens. Verdicts take the
// path and heading from search_results at the judged rank, so a reindex that reuses chunk ids
// cannot change what was judged.
func (s *Store) Judged(ctx context.Context, f store.JudgedFilter) ([]store.JudgedSearch, error) {
	out, err := s.judged(ctx, f)
	if err != nil {
		return nil, wrap(ctx, err, s.path, "sqlite.Judged", "could not read the judged searches from the log")
	}
	return out, nil
}

// judgedWhere selects the searches in f; ?1 is the since bound ("" = none), ?2 the caller.
const judgedWhere = `(?1 = '' OR s.ts >= ?1) AND (?2 = '' OR s.caller = ?2)`

func (s *Store) judged(ctx context.Context, f store.JudgedFilter) ([]store.JudgedSearch, error) {
	since := ""
	if !f.Since.IsZero() {
		since = f.Since.UTC().Format(time.RFC3339)
	}
	rows, err := s.db.QueryContext(ctx, `
		SELECT s.id, s.ts, s.query, s.mode, s.k, s.caller FROM searches AS s
		WHERE `+judgedWhere+` ORDER BY s.id`, since, f.Caller)
	if err != nil {
		return nil, fmt.Errorf("read searches: %w", err)
	}
	defer rows.Close()
	var out []store.JudgedSearch
	byID := map[int64]int{}
	for rows.Next() {
		var j store.JudgedSearch
		if err := rows.Scan(&j.ID, &j.TS, &j.Query, &j.Mode, &j.K, &j.Caller); err != nil {
			return nil, fmt.Errorf("scan searches: %w", err)
		}
		byID[j.ID] = len(out)
		out = append(out, j)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("read searches: %w", err)
	}
	if err := s.judgedVerdicts(ctx, since, f.Caller, out, byID); err != nil {
		return nil, err
	}
	if err := s.judgedOpens(ctx, since, f.Caller, out, byID); err != nil {
		return nil, err
	}
	return out, nil
}

// judgedVerdicts fills Verdicts (joined to the result at the judged rank) and None.
func (s *Store) judgedVerdicts(ctx context.Context, since, caller string, out []store.JudgedSearch, byID map[int64]int) error {
	rows, err := s.db.QueryContext(ctx, `
		SELECT f.search_id, f.rank, f.useful, f.ts, coalesce(r.store, ''), coalesce(r.entry_path, ''), coalesce(r.heading, '')
		FROM search_feedback AS f
		JOIN searches AS s ON s.id = f.search_id
		LEFT JOIN search_results AS r ON r.search_id = f.search_id AND r.rank = f.rank
		WHERE `+judgedWhere+`
		ORDER BY f.search_id, f.rank`, since, caller)
	if err != nil {
		return fmt.Errorf("read feedback: %w", err)
	}
	defer rows.Close()
	for rows.Next() {
		var (
			id     int64
			v      store.Verdict
			useful int
			ts     string
		)
		if err := rows.Scan(&id, &v.Rank, &useful, &ts, &v.Store, &v.EntryPath, &v.Heading); err != nil {
			return fmt.Errorf("scan feedback: %w", err)
		}
		j := &out[byID[id]]
		if v.Rank == store.WholeSearchRank {
			j.None = true
			continue
		}
		if v.EntryPath == "" {
			continue // a rank with no logged result: Feedback refuses it, so only a damaged log has one
		}
		v.Useful = useful == 1
		if v.At, err = parseLogTime(ts); err != nil {
			return s.damagedLog(err, fmt.Sprintf("the verdict on search %d rank %d has timestamp %q", id, v.Rank, ts))
		}
		j.Verdicts = append(j.Verdicts, v)
	}
	if err := rows.Err(); err != nil {
		return fmt.Errorf("read feedback: %w", err)
	}
	return nil
}

// judgedOpens fills Opens and OtherOpens from search_opens.store (NULL = this store; schema v6
// filled it in for older opens from what the search returned at that path).
func (s *Store) judgedOpens(ctx context.Context, since, caller string, out []store.JudgedSearch, byID map[int64]int) error {
	rows, err := s.db.QueryContext(ctx, `
		SELECT o.search_id, o.entry_path, coalesce(o.store, ''), o.ts
		FROM search_opens AS o
		JOIN searches AS s ON s.id = o.search_id
		WHERE `+judgedWhere+`
		ORDER BY o.search_id, 3, o.entry_path`, since, caller)
	if err != nil {
		return fmt.Errorf("read opens: %w", err)
	}
	defer rows.Close()
	for rows.Next() {
		var (
			id               int64
			path, owner, ots string
		)
		if err := rows.Scan(&id, &path, &owner, &ots); err != nil {
			return fmt.Errorf("scan opens: %w", err)
		}
		j := &out[byID[id]]
		if owner == "" {
			at, err := parseLogTime(ots)
			if err != nil {
				return s.damagedLog(err, fmt.Sprintf("the open of %s after search %d has timestamp %q", path, id, ots))
			}
			j.Opens = append(j.Opens, path)
			if j.OpenedAt == nil {
				j.OpenedAt = map[string]time.Time{}
			}
			j.OpenedAt[path] = at
		} else {
			j.OtherOpens = append(j.OtherOpens, store.StorePath{Store: owner, Path: path})
		}
	}
	if err := rows.Err(); err != nil {
		return fmt.Errorf("read opens: %w", err)
	}
	return nil
}

// damagedLog is a log row kb never writes (a hand edit or a damaged file), such as a timestamp
// that does not parse: a Conflict naming the row, so kb search, eval and stats exit 1 with
// something to act on instead of an internal error.
func (s *Store) damagedLog(err error, row string) error {
	return errs.Wrap(err, errs.Conflict, "sqlite.Judged",
		"the search log in %s is damaged: %s; correct or delete that row", s.path, row).With("db", s.path)
}

// parseLogTime reads a ts column as stamp wrote it (RFC 3339, UTC).
func parseLogTime(ts string) (time.Time, error) {
	t, err := time.Parse(time.RFC3339, ts)
	if err != nil {
		return time.Time{}, fmt.Errorf("bad log timestamp %q: %w", ts, err)
	}
	return t, nil
}
