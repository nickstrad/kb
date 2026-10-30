// Package eval measures search quality against the search log (kb eval): it turns logged
// feedback into gold, replays each judged query with the current settings, and scores the
// rankings with hit@k, MRR and nDCG@k, each with a seeded bootstrap confidence interval.
//
// Inputs: the logged searches (store.JudgedLog), the index to check gold against (Corpus), a
// non-logging Searcher and Options (mode, k, the small-sample threshold eval.min_judged).
// Output: a Report, whose JSON tags are the --json contract.
//
// Gold rules:
//   - A useful verdict is chunk-level gold (entry_path, heading), from the result logged at that
//     rank. Gold is never keyed on chunk_id: a reindex hands old ids to unrelated chunks.
//   - Searches are grouped by Normalize(query); a group's gold is the union over its searches,
//     so a query asked ten times counts once. The newest search's text is replayed.
//   - Not-useful verdicts are negatives, reported as a count in the top k, never scored. A chunk
//     marked useful by any search of the group is not a negative.
//   - A search judged only --none, only not-useful, or not at all has no gold; it is counted in
//     SearchCounts and excluded.
//   - An open (kb show after a search) is entry-level gold of the separate Opens variant. It never
//     counts toward the useful-verdict variants: reading a hit is not the same as it answering.
//   - A cross-store search (kb search --stores) logs other stores' hits too. Gold on them (a
//     verdict or an open of another store's entry) is counted in Report.OtherStore and never
//     scored: kb eval replays this store only, and a same-named path here is another entry.
//   - Gold the index no longer has (a deleted entry, a renamed heading) is dropped from scoring and
//     counted as gold_not_in_index; a query left with none is counted as dropped.
//
// Baselines (baseline.go, compare.go): SaveBaseline writes a Report with its settings, hash,
// embed model, entry count and kb version to <store>/eval/<name>.json (schema BaselineVersion,
// refused when different; the files are derived and git-ignored, so they are rewritten rather
// than migrated). Compare pairs two runs' queries by normalised text and, per metric, reports
// the mean delta with a paired bootstrap interval, wins/losses/ties and the exact two-sided
// sign-test p-value over the wins and losses.
//
// Invariants: every Score has rr and nDCG in [0, 1] and hit ⇔ nDCG > 0; a gold item earns gain
// once; the same log, index, settings and seed give the same Report, intervals included.
package eval
