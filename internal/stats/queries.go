// The `kb stats` query registry: the five named analytic queries that read the search log back
// out of the in-memory DuckDB copy built by Load. Adding a metric means appending a Query here,
// not widening the command.
//
// Every query follows the definitions in data/kb.md (plan decision D4), because a number that
// means one thing in `kb stats` and another in a hand-written sqlite3 query is worse than no
// number at all:
//
//   - a search is a *hit* when at least one of its rated results was marked useful;
//   - rates are taken over *judged* searches only (searches with at least one feedback row);
//     an unjudged search is unknown, not a failure, so every rate sits next to its denominator
//     and a coverage column;
//   - a whole-search verdict (`kb feedback <id> --none`) is a feedback row at rank 0 with
//     useful = false (search.WholeSearchRank). The definitions above already treat it right with
//     no special case: it makes its search judged and never a hit, it is the only way a
//     zero-result search can be judged, and since no result has rank 0 it joins no search_results
//     row, so the per-result queries (sources, entries) never see it;
//   - a missing measurement is NULL, never 0 — an unrated result, a stage that never ran, and an
//     entry nobody ever saw all have to stay distinguishable from a real zero.
//
// Three shapes recur in the SQL below and are deliberate:
//
//   - per-search CTEs that fold search_feedback down with *correlated scalar subqueries* rather
//     than joining it. A search can carry several feedback rows; joining would count that search
//     once per verdict in `judged` and `searches`.
//   - nullif(denominator, 0) everywhere a rate is divided, so "nothing was judged" reads as NULL
//     instead of collapsing to 0 or raising an error.
//   - count(...) FILTER (...) rather than sum(CASE ...) for whole-number columns: DuckDB widens
//     sum() over an integer to HUGEINT, which the driver cannot hand back as an int64.
//
// Percentages are rounded to 1 decimal, mrr to 3, and every query ends in an explicit ORDER BY
// with a tiebreaker so two runs over the same log print the same table.

package stats

// Query is one named analytic query. SQL takes no parameters: the plan's D3 says `--since` and
// `--caller` are applied when the log is loaded, so every query here can stay filter-free.
type Query struct {
	Name    string // CLI name, kebab-case
	Summary string // one line, shown by `kb stats`
	Help    string // what each column means and which decision it informs; shown by --help
	SQL     string // DuckDB SQL over the loaded schema; no parameters
}

// Queries is the registry, in display order.
var Queries = []Query{
	{
		Name:    "overview",
		Summary: "One row per search mode: coverage, hit rate, MRR, zero-result rate and latency.",
		Help: "One row per mode that actually ran (hybrid, fts, vec, or fts-fallback for a hybrid\n" +
			"search that lost its embedder). searches is every logged search in that mode; judged is\n" +
			"how many of them carry at least one feedback row — a verdict on a hit, or a whole-search\n" +
			"`kb feedback <id> --none`, which also covers zero-result searches — and coverage_pct is\n" +
			"judged/searches.\n" +
			"hit_rate_pct is the share of judged searches with at least one useful result, and mrr is\n" +
			"the mean over judged searches of 1/rank of their first useful result (0 for a judged\n" +
			"search with no useful result, which includes every --none search); both are NULL when\n" +
			"nothing in the mode was judged.\n" +
			"zero_result_pct is the share of all searches that returned nothing — that one needs no\n" +
			"feedback, so it is the most trustworthy column when coverage is low. p50_ms and p95_ms\n" +
			"are continuous quantiles of total_ms, and avg_embed_ms is the mean embedding time, NULL\n" +
			"for fts because that stage never runs there.\n" +
			"Decision: which mode to recommend in AGENTS.md; whether embedding latency buys enough\n" +
			"quality.",
		SQL: `WITH per_search AS (
    SELECT
        s.mode,
        s.n_returned,
        CAST(s.total_ms AS DOUBLE) AS total_ms,
        CAST(s.embed_ms AS DOUBLE) AS embed_ms,
        -- Correlated, not joined: a search with two verdicts must still be one judged search.
        (SELECT count(*) FROM search_feedback AS f WHERE f.search_id = s.id) > 0 AS is_judged,
        -- NULL when the search was never judged or was judged and never found useful; those two
        -- cases are separated by is_judged, not by this column.
        (SELECT min(f.rank) FROM search_feedback AS f
          WHERE f.search_id = s.id AND f.useful) AS first_useful_rank
    FROM searches AS s
)
SELECT
    mode,
    count(*) AS searches,
    count(*) FILTER (WHERE is_judged) AS judged,
    round(100.0 * count(*) FILTER (WHERE is_judged) / nullif(count(*), 0), 1) AS coverage_pct,
    round(100.0 * count(*) FILTER (WHERE first_useful_rank IS NOT NULL)
          / nullif(count(*) FILTER (WHERE is_judged), 0), 1) AS hit_rate_pct,
    -- Unjudged searches contribute 0 to the sum and 0 to the denominator, so they drop out.
    round(sum(CASE WHEN first_useful_rank IS NULL THEN CAST(0 AS DOUBLE)
                   ELSE 1.0 / CAST(first_useful_rank AS DOUBLE) END)
          / nullif(count(*) FILTER (WHERE is_judged), 0), 3) AS mrr,
    round(100.0 * count(*) FILTER (WHERE n_returned = 0) / nullif(count(*), 0), 1) AS zero_result_pct,
    round(quantile_cont(total_ms, 0.5), 1) AS p50_ms,
    round(quantile_cont(total_ms, 0.95), 1) AS p95_ms,
    round(avg(embed_ms), 1) AS avg_embed_ms
FROM per_search
GROUP BY mode
ORDER BY searches DESC, mode`,
	},
	{
		Name:    "sources",
		Summary: "Where returned hits came from (both lists, FTS only, vector only) and how they rated.",
		Help: "One row per provenance of a result that was actually shown, over hybrid and fts-fallback\n" +
			"searches only — the two modes where both lists could have contributed. source is 'both'\n" +
			"when the chunk carried an fts_rank and a vec_rank, 'fts-only' or 'vec-only' when it\n" +
			"carried just one. results counts result rows (not searches) and share_pct is that share\n" +
			"of every in-scope result row. judged is how many of those rows have a verdict, useful how\n" +
			"many were marked useful, and useful_rate_pct is useful/judged — NULL when that source was\n" +
			"never judged, which is the usual reading for fts-only rows nobody bothered to rate. A\n" +
			"whole-search --none verdict names no result, so it is not counted here.\n" +
			"avg_rank is the mean display rank of the source's rows, so a high useful_rate_pct at a\n" +
			"deep avg_rank means the fusion is burying good hits.\n" +
			"Decision: is the vector list earning its cost; should RRF weighting or vecOverFetch change.",
		SQL: `WITH in_scope AS (
    -- fts-fallback is included on purpose: it is a hybrid search whose vector list died, so its
    -- results are the control group for what hybrid looks like without embeddings.
    SELECT id FROM searches WHERE mode IN ('hybrid', 'fts-fallback')
),
res AS (
    SELECT
        CASE
            WHEN r.fts_rank IS NOT NULL AND r.vec_rank IS NOT NULL THEN 'both'
            WHEN r.fts_rank IS NOT NULL THEN 'fts-only'
            WHEN r.vec_rank IS NOT NULL THEN 'vec-only'
            ELSE 'unknown'
        END AS source,
        r.rank AS result_rank,
        -- One scalar per result row: feedback is keyed (search_id, rank), so this cannot multiply
        -- the row even if the log ever held a duplicate verdict.
        (SELECT bool_or(f.useful) FROM search_feedback AS f
          WHERE f.search_id = r.search_id AND f.rank = r.rank) AS useful_verdict
    FROM search_results AS r
    JOIN in_scope ON in_scope.id = r.search_id
)
SELECT
    source,
    count(*) AS results,
    round(100.0 * count(*) / nullif((SELECT count(*) FROM res), 0), 1) AS share_pct,
    count(useful_verdict) AS judged,
    count(*) FILTER (WHERE useful_verdict) AS useful,
    round(100.0 * count(*) FILTER (WHERE useful_verdict)
          / nullif(count(useful_verdict), 0), 1) AS useful_rate_pct,
    round(avg(CAST(result_rank AS DOUBLE)), 2) AS avg_rank
FROM res
GROUP BY source
ORDER BY CASE source WHEN 'both' THEN 1 WHEN 'fts-only' THEN 2 WHEN 'vec-only' THEN 3 ELSE 4 END,
         source`,
	},
	{
		Name:    "trend",
		Summary: "One row per ISO week: volume, mode mix, feedback coverage, hit rate, MRR and latency.",
		Help: "One row per ISO week (weeks start on Monday, from date_trunc('week', ts)). searches is\n" +
			"the week's volume and distinct_queries counts distinct lower(trim(query)), so a week with\n" +
			"many searches and few distinct queries is an agent retrying rather than exploring.\n" +
			"hybrid_pct and fallback_pct are the shares of the week's searches that ran as hybrid and\n" +
			"as fts-fallback — a rising fallback_pct means the embedder was down, and every quality\n" +
			"number that week should be read in that light. coverage_pct is judged/searches,\n" +
			"hit_rate_pct and mrr are the same measures as in overview restricted to the week (NULL\n" +
			"when the week judged nothing), and p50_ms is the median total_ms.\n" +
			"Decision: is quality improving as the corpus and the agents' habits change; did a change\n" +
			"(new entries, reindex, model) help.",
		SQL: `WITH per_search AS (
    SELECT
        date_trunc('week', s.ts) AS week,
        s.mode,
        lower(trim(s.query)) AS norm_query,
        CAST(s.total_ms AS DOUBLE) AS total_ms,
        (SELECT count(*) FROM search_feedback AS f WHERE f.search_id = s.id) > 0 AS is_judged,
        (SELECT min(f.rank) FROM search_feedback AS f
          WHERE f.search_id = s.id AND f.useful) AS first_useful_rank
    FROM searches AS s
)
SELECT
    week,
    count(*) AS searches,
    count(DISTINCT norm_query) AS distinct_queries,
    round(100.0 * count(*) FILTER (WHERE mode = 'hybrid') / nullif(count(*), 0), 1) AS hybrid_pct,
    round(100.0 * count(*) FILTER (WHERE mode = 'fts-fallback') / nullif(count(*), 0), 1) AS fallback_pct,
    round(100.0 * count(*) FILTER (WHERE is_judged) / nullif(count(*), 0), 1) AS coverage_pct,
    round(100.0 * count(*) FILTER (WHERE first_useful_rank IS NOT NULL)
          / nullif(count(*) FILTER (WHERE is_judged), 0), 1) AS hit_rate_pct,
    round(sum(CASE WHEN first_useful_rank IS NULL THEN CAST(0 AS DOUBLE)
                   ELSE 1.0 / CAST(first_useful_rank AS DOUBLE) END)
          / nullif(count(*) FILTER (WHERE is_judged), 0), 3) AS mrr,
    round(quantile_cont(total_ms, 0.5), 1) AS p50_ms
FROM per_search
GROUP BY week
ORDER BY week`,
	},
	{
		Name:    "entries",
		Summary: "One row per indexed entry: how often it was shown, how it rated, and when it last surfaced.",
		Help: "One row per entry currently in the index, including entries that have never been\n" +
			"returned (the join starts at entries, so those appear with zeros and NULL avg_rank /\n" +
			"last_returned). path is the entry identifier every other kb command takes, so it can be\n" +
			"passed straight to `kb show` or `kb edit`. returned counts result rows the entry\n" +
			"produced and searches counts the distinct searches it appeared in, so returned >\n" +
			"searches means several of its chunks came back at once. useful and not_useful count\n" +
			"verdicts on those rows and useful_rate_pct is useful/(useful + not_useful): 0.0 means the\n" +
			"entry was judged and never helped, while NULL means it has never been judged at all.\n" +
			"avg_rank is its mean display rank and last_returned the timestamp of the most recent\n" +
			"search that showed it. Only returned results count here, not candidates that lost the\n" +
			"fusion: a caller can only judge what it was shown. A whole-search --none verdict names no\n" +
			"result, so it is attributed to no entry.\n" +
			"Decision: which entries to rewrite (returned but voted down), which are dead weight or\n" +
			"invisible (never returned), which carry the store.",
		SQL: `WITH result_rows AS (
    SELECT
        r.entry_path,
        r.search_id,
        CAST(r.rank AS DOUBLE) AS result_rank,
        s.ts,
        (SELECT bool_or(f.useful) FROM search_feedback AS f
          WHERE f.search_id = r.search_id AND f.rank = r.rank) AS useful_verdict
    FROM search_results AS r
    JOIN searches AS s ON s.id = r.search_id
),
per_entry AS (
    SELECT
        entry_path,
        count(*) AS returned,
        count(DISTINCT search_id) AS searches,
        count(*) FILTER (WHERE useful_verdict) AS useful,
        count(*) FILTER (WHERE NOT useful_verdict) AS not_useful,
        avg(result_rank) AS avg_rank,
        max(ts) AS last_returned
    FROM result_rows
    GROUP BY entry_path
)
SELECT
    e.path,
    coalesce(p.returned, 0) AS returned,
    coalesce(p.searches, 0) AS searches,
    coalesce(p.useful, 0) AS useful,
    coalesce(p.not_useful, 0) AS not_useful,
    -- p.useful is NULL for an entry with no result rows at all, so this stays NULL rather than
    -- claiming a 0% success rate for an entry nobody ever saw.
    round(100.0 * p.useful / nullif(p.useful + p.not_useful, 0), 1) AS useful_rate_pct,
    round(p.avg_rank, 2) AS avg_rank,
    p.last_returned AS last_returned
FROM entries AS e
LEFT JOIN per_entry AS p ON p.entry_path = e.path
ORDER BY returned DESC, e.path`,
	},
	{
		Name:    "gaps",
		Summary: "Normalised queries that never produced a useful hit: what to write about next.",
		Help: "One row per normalised query text (lower(trim(query))) for which no feedback row anywhere\n" +
			"in the group says useful. times_asked counts the searches in the group, last_asked is the\n" +
			"most recent of them, and callers lists the distinct callers that asked. judged and\n" +
			"not_useful count feedback rows (not searches; a whole-search --none verdict is one row of\n" +
			"each): judged = 0 means nobody ever rated these searches, so the row is unknown rather\n" +
			"than failed, while judged = not_useful > 0 means the store really was asked and really\n" +
			"did not answer. zero_results counts the searches in the group that returned nothing at\n" +
			"all; they can only be judged with --none. top_entry is the rank-1 entry_path of the group's\n" +
			"most recent search, and is NULL when that search returned nothing; last_note is the most\n" +
			"recent non-empty feedback note in the group, which is usually the clearest statement of\n" +
			"what was missing.\n" +
			"Decision: what knowledge to write next; which searches to go back and judge (judged = 0\n" +
			"means unknown, not failed).",
		SQL: `WITH per_search AS (
    SELECT
        lower(trim(s.query)) AS norm_query,
        s.caller,
        s.ts,
        s.n_returned,
        -- Counted per search, then summed per group: joining searches to search_feedback would
        -- turn one search with three verdicts into three searches in times_asked.
        (SELECT count(*) FROM search_feedback AS f WHERE f.search_id = s.id) AS feedback_rows,
        (SELECT count(*) FROM search_feedback AS f
          WHERE f.search_id = s.id AND NOT f.useful) AS not_useful_rows,
        (SELECT count(*) FROM search_feedback AS f
          WHERE f.search_id = s.id AND f.useful) AS useful_rows
    FROM searches AS s
),
grouped AS (
    SELECT
        norm_query,
        count(*) AS times_asked,
        max(ts) AS last_asked,
        string_agg(DISTINCT caller, ',' ORDER BY caller) AS callers,
        -- CAST because DuckDB's sum() over an integer is HUGEINT, which does not scan to int64.
        CAST(sum(feedback_rows) AS BIGINT) AS judged,
        CAST(sum(not_useful_rows) AS BIGINT) AS not_useful,
        CAST(sum(useful_rows) AS BIGINT) AS useful,
        count(*) FILTER (WHERE n_returned = 0) AS zero_results
    FROM per_search
    GROUP BY norm_query
)
SELECT
    g.norm_query AS query,
    g.times_asked AS times_asked,
    g.last_asked AS last_asked,
    g.callers AS callers,
    g.judged AS judged,
    g.not_useful AS not_useful,
    g.zero_results AS zero_results,
    (SELECT r.entry_path
       FROM search_results AS r
      WHERE r.search_id = (SELECT s2.id FROM searches AS s2
                            WHERE lower(trim(s2.query)) = g.norm_query
                            ORDER BY s2.ts DESC, s2.id DESC
                            LIMIT 1)
        AND r.rank = 1
      -- LIMIT 1 is defensive: the loaded copy carries no (search_id, rank) key, and DuckDB
      -- fails a scalar subquery that yields two rows instead of picking one.
      LIMIT 1) AS top_entry,
    (SELECT f.note
       FROM search_feedback AS f
       JOIN searches AS s3 ON s3.id = f.search_id
      WHERE lower(trim(s3.query)) = g.norm_query AND f.note IS NOT NULL
      ORDER BY f.ts DESC, f.search_id DESC, f.rank DESC
      LIMIT 1) AS last_note
FROM grouped AS g
WHERE g.useful = 0
ORDER BY times_asked DESC, last_asked DESC, query`,
	},
}

// Lookup returns the named query. The registry is five entries long, so a linear scan is both
// cheaper than a map and keeps Queries the single source of truth for the display order.
func Lookup(name string) (Query, bool) {
	for _, q := range Queries {
		if q.Name == name {
			return q, true
		}
	}
	return Query{}, false
}
