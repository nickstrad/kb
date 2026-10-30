package search

import (
	"context"

	"github.com/nickstrad/kb/internal/assert"
	"github.com/nickstrad/kb/internal/store"
)

// The phrase/NEAR tiers of the main full-text list (PLAN 4.6). With two or more query groups and
// search.phrase_boost, the list is four queries concatenated in tier order and cut to list_limit:
// the terms as a phrase ("a b c"), the terms near each other (NEAR("a" "b", 8)), all the terms
// (AND, with synonym and spell alternatives), and, only when AND found nothing, any term (OR).
// The tiers are not separate RRF lists: they are one list whose order prefers exact phrasing, so
// a chunk keeps one FTS rank (and one RRF term), and its tier is logged beside it. The phrase and
// NEAR queries use the literal words only: an alternative (synonym, spell correction) says "this
// word or that one", which has no meaning inside a phrase. The exact-identifier list is never
// tiered.

// TierRun is one tier's query as it ran: which tier, its MATCH expression and the rows it
// returned (before the tiers were merged and deduplicated).
type TierRun struct {
	Tier  string
	Match string
	Rows  int
}

// tierOps are the queries that come before AND in a tiered main list, best first.
var tierOps = []struct {
	tier string
	op   store.FTSOp
}{{store.TierPhrase, store.OpPhrase}, {store.TierNear, store.OpNear}}

// tiered reports whether the main list runs the phrase and NEAR tiers: phrase_boost is on and
// the query has at least two groups (words or hyphenated fields that survive sanitising; one
// group is already a phrase in the AND query).
func tiered(groups int, boost bool) bool { return boost && groups >= 2 }

// countGroups is the number of fields of terms that reach the MATCH expression.
func countGroups(terms []string) int {
	n := 0
	for _, t := range terms {
		if fieldRenders(t) {
			n++
		}
	}
	return n
}

// tierSource is one tier's rows, best first.
type tierSource struct {
	tier string
	rows []store.FTSRow
}

// assembleTiers concatenates the tiers in the order given, drops a chunk already taken from an
// earlier tier, stamps each row with its tier and stops at limit. The scores are the tiers' own;
// rescore replaces them.
func assembleTiers(tiers []tierSource, limit int) []store.FTSRow {
	var out []store.FTSRow
	seen := make(map[int64]bool)
	for _, t := range tiers {
		for _, r := range t.rows {
			if len(out) >= limit {
				break
			}
			if seen[r.ChunkID] {
				continue
			}
			seen[r.ChunkID] = true
			r.Tier = t.tier
			out = append(out, r)
		}
	}
	if assert.Enabled {
		assert.Post(len(out) <= limit, "assembleTiers: %d rows > limit %d", len(out), limit)
	}
	return out
}

// rescore sets the score of every row that has one in scores. The phrase and NEAR queries score
// a chunk differently from the AND query (they match inside one column and count only the phrase),
// so the merged list is scored by the AND query alone: fts_score then means the same for every
// row, and it is the figure the per-column scores of kb explain are compared with.
func rescore(rows []store.FTSRow, scores map[int64]float64) {
	for i := range rows {
		if sc, ok := scores[rows[i].ChunkID]; ok {
			rows[i].Score = sc
		}
	}
}

// mainList is the main full-text list of a search: its rows (each carrying its tier when the list
// ran tiered), the AND (or OR retry) expression, whether the OR retry produced it, and each tier's
// query.
type mainList struct {
	res     store.FTSResult
	retried bool
	runs    []TierRun // nil when the list ran untiered
}

// tagRows stamps tier on every row of rows, in place.
func tagRows(rows []store.FTSRow, tier string) {
	for i := range rows {
		rows[i].Tier = tier
	}
}

// baseTier is the tier of the AND query, or of its OR retry.
func baseTier(retried bool) string {
	if retried {
		return store.TierOr
	}
	return store.TierAnd
}

// mainList runs the main full-text list: lexicalList (AND, then the OR retry), and, when
// tiered(...), the phrase and NEAR queries in front of it. The phrase and NEAR queries run only
// when AND matched, because every phrase or NEAR match is also an AND match (the same words, the
// literal ones, a subset of the alternatives AND accepts), so an empty AND leaves them empty.
func (s *Searcher) mainList(ctx context.Context, terms []string, alts map[string][]string, tag string, p Params) (mainList, error) {
	res, retried, err := s.lexicalList(ctx, terms, alts, tag, p, false)
	if err != nil {
		return mainList{}, err
	}
	ml := mainList{res: res, retried: retried}
	if !tiered(countGroups(terms), p.PhraseBoost) {
		return ml, nil
	}
	base := baseTier(retried)
	ml.runs = []TierRun{{Tier: base, Match: res.Match, Rows: len(res.Rows)}}
	if retried || len(res.Rows) == 0 {
		tagRows(ml.res.Rows, base)
		return ml, nil
	}
	weights := p.BM25
	var sources []tierSource
	var runs []TierRun
	for _, t := range tierOps {
		if t.tier == store.TierNear && len(sources) > 0 && len(sources[0].rows) >= p.ListLimit {
			break // the phrase rows already fill the list: assembleTiers would cut every NEAR row
		}
		r, err := s.Store.FTS(ctx, store.FTSQuery{Terms: terms, Op: t.op, Near: store.NearDistance, Tag: tag,
			Limit: p.ListLimit, Weights: &weights})
		if err != nil {
			return mainList{}, err
		}
		if r.Match == "" {
			continue
		}
		sources = append(sources, tierSource{t.tier, r.Rows})
		runs = append(runs, TierRun{Tier: t.tier, Match: r.Match, Rows: len(r.Rows)})
	}
	sources = append(sources, tierSource{base, res.Rows})
	ml.res.Rows = assembleTiers(sources, p.ListLimit)
	scores, err := s.andScores(ctx, terms, alts, tag, p, res.Rows, ml.res.Rows)
	if err != nil {
		return mainList{}, err
	}
	rescore(ml.res.Rows, scores)
	ml.runs = append(runs, ml.runs...)
	return ml, nil
}

// andScores is the AND query's bm25 of every row of merged: those the AND query itself returned
// (andRows) and, for the rest (a phrase or NEAR match below the AND top list_limit), one more AND
// query restricted to their chunk ids.
func (s *Searcher) andScores(ctx context.Context, terms []string, alts map[string][]string, tag string, p Params,
	andRows, merged []store.FTSRow) (map[int64]float64, error) {
	scores := make(map[int64]float64, len(merged))
	for _, r := range andRows {
		scores[r.ChunkID] = r.Score
	}
	var missing []int64
	for _, r := range merged {
		if _, ok := scores[r.ChunkID]; !ok {
			missing = append(missing, r.ChunkID)
		}
	}
	if len(missing) == 0 {
		return scores, nil
	}
	weights := p.BM25
	res, err := s.Store.FTS(ctx, store.FTSQuery{Terms: terms, Op: store.OpAnd, Tag: tag, Limit: len(missing), Weights: &weights,
		ChunkIDs: missing, Alts: alts})
	if err != nil {
		return nil, err
	}
	for _, r := range res.Rows {
		scores[r.ChunkID] = r.Score
	}
	return scores, nil
}
