// The `kb stats` query registry: named analytic queries over the search log. Adding a metric
// means appending a Query here, not widening the command.
//
// Every query shares these definitions:
//
//   - a search is a *hit* when at least one of its rated results was marked useful;
//   - rates are taken over *judged* searches only (searches with at least one feedback row), so
//     every rate sits next to its denominator and a coverage column;
//   - a whole-search verdict (`kb feedback <id> --none`) is a feedback row at rank 0 with
//     useful = 0. It makes its search judged and never a hit, and since no result has rank 0 the
//     per-result queries (sources, entries) never see it;
//   - a missing measurement is NULL, never 0.
//
// Rounding is written round(x * 10^d) / 10^d, not round(x, d): SQLite's round(x, d) rounds the
// stored double exactly, so 137/40 (3.42499…) shows 3.42, where scaling first gives 3.43, which
// is what the reader expects and what percentiles do in Go.
//
// Feedback is folded into each search with correlated scalar subqueries, not joins: a search with
// several verdicts must still count once. Every division goes through nullif(denominator, 0) so
// "nothing was judged" reads as NULL. Every query ends in an ORDER BY with a tiebreaker so two
// runs print the same table.

package stats

// Pct marks a column whose SQL yields a JSON array of samples; Run replaces each cell with the
// continuous quantile P of those samples, rounded to Digits decimals.
type Pct struct {
	Column string
	P      float64
	Digits int
}

// Query is one named analytic query. SQL reads the filtered searches through the fs CTE and
// takes two parameters: ?1 the lower bound on ts, ?2 the caller (see Filter).
type Query struct {
	Name        string // CLI name, kebab-case
	Summary     string // one line, shown by `kb stats`
	Help        string // what each column means and which decision it informs; shown by --help
	SQL         string
	Percentiles []Pct
}

// filterCTE is the filtered view of searches that every named query starts from. Child tables
// are reached by joining or correlating through fs, so the filter reaches them too.
const filterCTE = `WITH fs AS (
    SELECT * FROM searches WHERE ts >= ?1 AND (?2 = '' OR caller = ?2)
)`

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
			"opened_pct is the share of all searches followed by at least one kb show of a returned\n" +
			"entry, of this store or of another store the search reached (kb show REF:path), an\n" +
			"implicit signal that needs no feedback; a search counts once however many entries were\n" +
			"opened. It stays apart from hit_rate_pct: reading a hit does not mean it answered the\n" +
			"question.\n" +
			"zero_result_pct is the share of all searches that returned nothing — that one needs no\n" +
			"feedback, so it is the most trustworthy column when coverage is low. p50_ms and p95_ms\n" +
			"are continuous quantiles of total_ms, and avg_embed_ms is the mean embedding time, NULL\n" +
			"for fts because that stage never runs there.\n" +
			"Decision: which mode to recommend in AGENTS.md; whether embedding latency buys enough\n" +
			"quality.",
		Percentiles: []Pct{{"p50_ms", 0.5, 1}, {"p95_ms", 0.95, 1}},
		SQL: filterCTE + `,
per_search AS (
    SELECT
        s.mode,
        s.n_returned,
        s.total_ms,
        s.embed_ms,
        EXISTS (SELECT 1 FROM search_opens AS o WHERE o.search_id = s.id) AS is_opened,
        (SELECT count(*) FROM search_feedback AS f WHERE f.search_id = s.id) > 0 AS is_judged,
        -- NULL when never judged or never useful; is_judged tells those apart.
        (SELECT min(f.rank) FROM search_feedback AS f
          WHERE f.search_id = s.id AND f.useful) AS first_useful_rank
    FROM fs AS s
)
SELECT
    mode,
    count(*) AS searches,
    count(*) FILTER (WHERE is_judged) AS judged,
    round(100.0 * count(*) FILTER (WHERE is_judged) / nullif(count(*), 0) * 10) / 10 AS coverage_pct,
    round(100.0 * count(*) FILTER (WHERE first_useful_rank IS NOT NULL)
          / nullif(count(*) FILTER (WHERE is_judged), 0) * 10) / 10 AS hit_rate_pct,
    -- Unjudged searches add 0 to the sum and 0 to the denominator, so they drop out.
    round(sum(CASE WHEN first_useful_rank IS NULL THEN 0.0
                   ELSE 1.0 / first_useful_rank END)
          / nullif(count(*) FILTER (WHERE is_judged), 0) * 1000) / 1000 AS mrr,
    round(100.0 * count(*) FILTER (WHERE n_returned = 0) / nullif(count(*), 0) * 10) / 10 AS zero_result_pct,
    round(100.0 * count(*) FILTER (WHERE is_opened) / nullif(count(*), 0) * 10) / 10 AS opened_pct,
    json_group_array(total_ms) FILTER (WHERE total_ms IS NOT NULL) AS p50_ms,
    json_group_array(total_ms) FILTER (WHERE total_ms IS NOT NULL) AS p95_ms,
    round(avg(embed_ms) * 10) / 10 AS avg_embed_ms
FROM per_search
GROUP BY mode
ORDER BY searches DESC, mode`,
	},
	{
		Name:    "sources",
		Summary: "Where returned hits came from (both lists, FTS only, vector only, exact only) and how they rated.",
		Help: "One row per provenance of a result that was actually shown, over hybrid and fts-fallback\n" +
			"searches only — the two modes where both lists could have contributed. source is 'both'\n" +
			"when the chunk carried an fts_rank and a vec_rank, 'fts-only' or 'vec-only' when it\n" +
			"carried just one, and 'exact-only' when only the exact-identifier list (a query with an\n" +
			"identifier such as foo_bar or kb.toml) returned it. with_exact counts the source's rows\n" +
			"that were also in the exact list, so a 'both' row may be exact too. results counts result rows (not searches) and share_pct is that share\n" +
			"of every in-scope result row. judged is how many of those rows have a verdict, useful how\n" +
			"many were marked useful, and useful_rate_pct is useful/judged — NULL when that source was\n" +
			"never judged, which is the usual reading for fts-only rows nobody bothered to rate. A\n" +
			"whole-search --none verdict names no result, so it is not counted here.\n" +
			"avg_rank is the mean display rank of the source's rows, so a high useful_rate_pct at a\n" +
			"deep avg_rank means the fusion is burying good hits.\n" +
			"Decision: is the vector list earning its cost; is the exact list (with_exact, exact-only)\n" +
			"earning its place; should RRF weighting or vecOverFetch change.",
		SQL: filterCTE + `,
in_scope AS (
    -- fts-fallback is a hybrid search whose vector list died: the control group for hybrid
    -- without embeddings.
    SELECT id FROM fs WHERE mode IN ('hybrid', 'fts-fallback')
),
res AS (
    SELECT
        CASE
            WHEN r.fts_rank IS NOT NULL AND r.vec_rank IS NOT NULL THEN 'both'
            WHEN r.fts_rank IS NOT NULL THEN 'fts-only'
            WHEN r.vec_rank IS NOT NULL THEN 'vec-only'
            WHEN r.exact_rank IS NOT NULL THEN 'exact-only'
            ELSE 'unknown'
        END AS source,
        r.exact_rank IS NOT NULL AS in_exact,
        r.rank AS result_rank,
        (SELECT max(f.useful) FROM search_feedback AS f
          WHERE f.search_id = r.search_id AND f.rank = r.rank) AS useful_verdict
    FROM search_results AS r
    JOIN in_scope ON in_scope.id = r.search_id
)
SELECT
    source,
    count(*) AS results,
    round(100.0 * count(*) / nullif((SELECT count(*) FROM res), 0) * 10) / 10 AS share_pct,
    count(useful_verdict) AS judged,
    count(*) FILTER (WHERE useful_verdict) AS useful,
    round(100.0 * count(*) FILTER (WHERE useful_verdict)
          / nullif(count(useful_verdict), 0) * 10) / 10 AS useful_rate_pct,
    round(avg(result_rank) * 100) / 100 AS avg_rank,
    count(*) FILTER (WHERE in_exact) AS with_exact
FROM res
GROUP BY source
ORDER BY CASE source WHEN 'both' THEN 1 WHEN 'fts-only' THEN 2 WHEN 'vec-only' THEN 3 WHEN 'exact-only' THEN 4 ELSE 5 END,
         source`,
	},
	{
		Name:    "trend",
		Summary: "One row per ISO week: volume, mode mix, feedback coverage, hit rate, MRR and latency.",
		Help: "One row per ISO week (weeks start on Monday at 00:00 UTC). searches is the week's volume\n" +
			"and distinct_queries counts distinct lower(trim(query)), so a week with many searches\n" +
			"and few distinct queries is an agent retrying rather than exploring.\n" +
			"hybrid_pct and fallback_pct are the shares of the week's searches that ran as hybrid and\n" +
			"as fts-fallback — a rising fallback_pct means the embedder was down, and every quality\n" +
			"number that week should be read in that light. coverage_pct is judged/searches,\n" +
			"hit_rate_pct and mrr are the same measures as in overview restricted to the week (NULL\n" +
			"when the week judged nothing), and p50_ms is the median total_ms.\n" +
			"Decision: is quality improving as the corpus and the agents' habits change; did a change\n" +
			"(new entries, reindex, model) help.",
		Percentiles: []Pct{{"p50_ms", 0.5, 1}},
		SQL: filterCTE + `,
per_search AS (
    SELECT
        -- 'weekday 0' moves forward to Sunday (or stays on one); -6 days lands on its Monday.
        strftime('%Y-%m-%dT00:00:00Z', s.ts, 'weekday 0', '-6 days') AS week,
        s.mode,
        lower(trim(s.query)) AS norm_query,
        s.total_ms,
        (SELECT count(*) FROM search_feedback AS f WHERE f.search_id = s.id) > 0 AS is_judged,
        (SELECT min(f.rank) FROM search_feedback AS f
          WHERE f.search_id = s.id AND f.useful) AS first_useful_rank
    FROM fs AS s
)
SELECT
    week,
    count(*) AS searches,
    count(DISTINCT norm_query) AS distinct_queries,
    round(100.0 * count(*) FILTER (WHERE mode = 'hybrid') / nullif(count(*), 0) * 10) / 10 AS hybrid_pct,
    round(100.0 * count(*) FILTER (WHERE mode = 'fts-fallback') / nullif(count(*), 0) * 10) / 10 AS fallback_pct,
    round(100.0 * count(*) FILTER (WHERE is_judged) / nullif(count(*), 0) * 10) / 10 AS coverage_pct,
    round(100.0 * count(*) FILTER (WHERE first_useful_rank IS NOT NULL)
          / nullif(count(*) FILTER (WHERE is_judged), 0) * 10) / 10 AS hit_rate_pct,
    round(sum(CASE WHEN first_useful_rank IS NULL THEN 0.0
                   ELSE 1.0 / first_useful_rank END)
          / nullif(count(*) FILTER (WHERE is_judged), 0) * 1000) / 1000 AS mrr,
    json_group_array(total_ms) FILTER (WHERE total_ms IS NOT NULL) AS p50_ms
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
		SQL: filterCTE + `,
result_rows AS (
    SELECT
        r.entry_path,
        r.search_id,
        CAST(r.rank AS REAL) AS result_rank,
        s.ts,
        (SELECT max(f.useful) FROM search_feedback AS f
          WHERE f.search_id = r.search_id AND f.rank = r.rank) AS useful_verdict
    FROM search_results AS r
    JOIN fs AS s ON s.id = r.search_id
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
    -- p.useful is NULL for an entry never returned, so its rate stays NULL rather than 0%.
    round(100.0 * p.useful / nullif(p.useful + p.not_useful, 0) * 10) / 10 AS useful_rate_pct,
    round(p.avg_rank * 100) / 100 AS avg_rank,
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
		SQL: filterCTE + `,
per_search AS (
    SELECT
        lower(trim(s.query)) AS norm_query,
        s.caller,
        s.ts,
        s.n_returned,
        -- Counted per search, then summed per group, so one search with three verdicts is
        -- still one search in times_asked.
        (SELECT count(*) FROM search_feedback AS f WHERE f.search_id = s.id) AS feedback_rows,
        (SELECT count(*) FROM search_feedback AS f
          WHERE f.search_id = s.id AND NOT f.useful) AS not_useful_rows,
        (SELECT count(*) FROM search_feedback AS f
          WHERE f.search_id = s.id AND f.useful) AS useful_rows
    FROM fs AS s
),
grouped AS (
    SELECT
        norm_query,
        count(*) AS times_asked,
        max(ts) AS last_asked,
        group_concat(DISTINCT caller ORDER BY caller) AS callers,
        sum(feedback_rows) AS judged,
        sum(not_useful_rows) AS not_useful,
        sum(useful_rows) AS useful,
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
      WHERE r.search_id = (SELECT s2.id FROM fs AS s2
                            WHERE lower(trim(s2.query)) = g.norm_query
                            ORDER BY s2.ts DESC, s2.id DESC
                            LIMIT 1)
        AND r.rank = 1) AS top_entry,
    (SELECT f.note
       FROM search_feedback AS f
       JOIN fs AS s3 ON s3.id = f.search_id
      WHERE lower(trim(s3.query)) = g.norm_query AND f.note IS NOT NULL
      ORDER BY f.ts DESC, f.search_id DESC, f.rank DESC
      LIMIT 1) AS last_note
FROM grouped AS g
WHERE g.useful = 0
ORDER BY times_asked DESC, last_asked DESC, query`,
	},
	{
		Name:    "settings",
		Summary: "One row per search configuration ever used: volume, hit rate and MRR by settings hash.",
		Help: "One row per distinct settings_hash: the fingerprint of the [embed] and [search] settings a\n" +
			"search ran with (the hash `kb config --json` prints). Change a ranking setting and the new\n" +
			"searches get a new row, so the effect of the change is a comparison of two rows, not of\n" +
			"two time periods. searches, judged, coverage_pct, hit_rate_pct and mrr are the same\n" +
			"measures as in overview, restricted to the searches of that configuration; first_seen and\n" +
			"last_seen are the timestamps of its first and last search. settings_hash shows the first 12\n" +
			"characters of the hash. settings is the canonical JSON the hash covers, so two rows can\n" +
			"be diffed. A row with a NULL hash holds the searches logged before settings were\n" +
			"recorded (schema v3 and older), which cannot be told apart.\n" +
			"Read it with the coverage in mind: a row with few judged searches proves little, and\n" +
			"rows differ in the queries they saw as well as in their settings.\n" +
			"Decision: whether a setting change helped; which configuration to keep.",
		SQL: filterCTE + `,
per_search AS (
    SELECT
        s.settings_hash,
        s.settings,
        s.ts,
        (SELECT count(*) FROM search_feedback AS f WHERE f.search_id = s.id) > 0 AS is_judged,
        (SELECT min(f.rank) FROM search_feedback AS f
          WHERE f.search_id = s.id AND f.useful) AS first_useful_rank
    FROM fs AS s
)
SELECT
    substr(settings_hash, 1, 12) AS settings_hash,
    count(*) AS searches,
    count(*) FILTER (WHERE is_judged) AS judged,
    round(100.0 * count(*) FILTER (WHERE is_judged) / nullif(count(*), 0) * 10) / 10 AS coverage_pct,
    round(100.0 * count(*) FILTER (WHERE first_useful_rank IS NOT NULL)
          / nullif(count(*) FILTER (WHERE is_judged), 0) * 10) / 10 AS hit_rate_pct,
    round(sum(CASE WHEN first_useful_rank IS NULL THEN 0.0
                   ELSE 1.0 / first_useful_rank END)
          / nullif(count(*) FILTER (WHERE is_judged), 0) * 1000) / 1000 AS mrr,
    min(ts) AS first_seen,
    max(ts) AS last_seen,
    min(settings) AS settings
FROM per_search
GROUP BY settings_hash
ORDER BY last_seen DESC, settings_hash`,
	},
}

// Lookup returns the named query.
func Lookup(name string) (Query, bool) {
	for _, q := range Queries {
		if q.Name == name {
			return q, true
		}
	}
	return Query{}, false
}
