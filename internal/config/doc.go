// Package config is the kb store's configuration: the schema of a store's config file, its defaults,
// loading with strict validation, the --set override syntax and the canonical effective-settings
// JSON with its hash.
//
// Inputs: the path of a config file (Read, Load), a "a.b.c=v" override (Apply). Outputs: a plain
// Config value; Save renders it back as TOML and EffectiveJSON as canonical JSON.
//
// Invariants:
//   - A missing file is not an error: it means every default. A key the file omits keeps its
//     default. An unknown key, a wrong type or an out-of-range value is a Usage error that names
//     the file and the key (docs/CODING_STYLE.md §1.5).
//   - The file's version is judged before anything else: a newer schema is a Usage error ("written
//     by a newer kb"), an older one a Conflict that says to run kb migrate up, so a file from
//     another kb version never reads as "unknown key".
//   - Load(Save(c)) == c for every valid c, and Save(Load(x)) is a fixed point.
//   - Nothing here reads the environment. Flags and --set are applied by the caller over the
//     file, which is over the defaults. Location-level keys (default store, aliases) are not
//     part of a store's Config.
//
// Keys wired today (a command reads them): embed.model, embed.batch, search.k,
// search.list_limit, search.candidate_limit, search.per_entry_cap, search.rrf_k,
// search.vec_overfetch, search.bm25 (text, heading, title), search.list_weights.fts and
// search.list_weights.vec, output.budget, log.open_window, agent.caller, edit.editor and
// [[sources]] (path or glob, layout, exclude: the store's corpus, relative to the location root;
// none means nothing is indexed; see Source and validateSources). The
// search.* tuning keys have upper bounds (Max* constants) so no value can overflow a score or
// the vector index.
//
// Keys reserved for later work: they are parsed and range-checked, so a config written today
// stays valid, but no command acts on them yet: embed.workers, search.list_weights.exact,
// search.list_weights.memory, search.phrase_boost, search.exact_list, search.synonyms,
// search.spell, search.supersede_penalty, search.stores (store refs: name, alias, name@path or
// global), output.context_budget, output.expand, log.corpus_rev, [stale], [eval], [prior].
// embed.dim and embed.int8 change what the index holds, so a value other than the default is
// rejected instead of being ignored.
package config
