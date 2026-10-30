# kb

A command-line knowledge store for agents and people. `kb` indexes Markdown with SQLite FTS5 and
sqlite-vec, searches it with hybrid full-text + vector ranking, logs every search, and records
optional feedback so `kb stats` and `kb eval` can show where the store falls short and whether a
setting change helps.

`kb help` and `kb <command> --help` are the reference; this page is the overview.

## Install

On macOS the primary path is `go install`. It needs Go and the Xcode command-line tools
(`xcode-select --install`), because cgo builds SQLite with FTS5 and sqlite-vec.

```bash
go install -tags fts5 github.com/nickstrad/kb/cmd/kb@latest
```

For a prebuilt binary (macOS or Linux, arm64 or amd64), use the install script. It downloads
the latest GitHub release, verifies its sha256, and installs `~/.local/bin/kb` without sudo:

```bash
scripts/install.sh [--version vX.Y.Z] [--prefix DIR] [--from-source]
```

macOS release binaries are unsigned. If Gatekeeper blocks one, clear the quarantine flag:

```bash
xattr -d com.apple.quarantine ~/.local/bin/kb
```

## Locations and stores

A **location** is a directory with a `.kb/` folder (`.kb/kb.toml`). It holds one or more named
**stores**, each with its own config, sources, embedding model, index and search log in
`.kb/stores/<name>/` (`config.toml`, `kb.sqlite`). kb works on the nearest location at or above
the current directory, and in it on the store named by `--store`, else `default_store` in
`kb.toml`. `--root DIR` names a location explicitly; `--global` uses the machine-wide one
(`$XDG_DATA_HOME/kb`, else `~/.local/share/kb`). Only `kb init` creates a location or a store.

```bash
cd <project>
kb init                  # .kb/ and the "default" store (entries in data/); downloads the model
kb init --name adr       # a second store (entries in data-adr/), used with --store adr
kb init --model none     # full-text search only: no model, no download
kb stores                # the stores, their model, entry count and database size, and the aliases
kb add <draft.md>        # copy an entry into the store's entries directory and index it
kb search "<question>" --caller claude
```

`--store` (and `--stores`, `kb show REF:path`) take a **store ref**: a store name, an alias, `global`,
`NAME@LOCATION` (a location is `global` or a path), or a path (that location's default store).
Aliases live in `kb.toml`:

```toml
[aliases]
notes = "default@~/notes"
```

Other commands: `kb list`, `kb show <path>` (`--chunks` for the indexed chunks), `kb edit <path>`
(opens `$EDITOR`, then reindexes), `kb rm <path>`, `kb reindex [path | --all]`, `kb doctor`,
`kb index` (writes an optional `index.md` snapshot), `kb version`.

## Sources

Each store's `config.toml` lists what it indexes, with paths relative to the location root.
`kb init` writes them: `data/` for the default store, `data-<name>/` for a named one, or the
`--source GLOB` you pass (repeatable) to index Markdown where it already lives:

```toml
[[sources]]
path   = "data"            # layout "entries": kb add writes here; front matter required
layout = "entries"

[[sources]]
glob    = "docs/**/*.md"   # layout "files": every *.md the glob matches, in place
layout  = "files"
exclude = ["docs/archive/**"]
```

- In-place files need no front matter: the title falls back to the first `# ` heading, then the
  file name; the summary to the first paragraph (cut to 200 characters), else none.
- A file may belong to one source of a store (two that match it is an error); several stores
  may index the same file. `.git`, `.kb` and `node_modules` are never walked, and a symlink that
  leads outside the location root is skipped with a warning (`kb doctor` lists them).
- `kb rm` refuses an in-place file (delete it and run `kb reindex --all`), and `kb add` needs a
  store with exactly one `entries` source. A store with no sources indexes nothing.
- Chunks follow `##` sections. A document that uses only `#` headings becomes one intro section,
  split at paragraph boundaries every 1500 characters.

```bash
kb init --model none --source 'README.md' --source 'docs/**/*.md'
kb reindex --all
kb search install
```

## Configuration

`kb config` prints the store's effective settings as TOML (every key, with its default), so it
is also a starting point for `config.toml`. `kb config --path` says where that file is,
`kb config --json` prints the settings that decide search results with their `settings_hash`,
and `--set section.key=value` (repeatable) shows an override without writing it; `kb explain`
and `kb eval` take the same `--set` to try a setting for one run. Commonly changed keys:

| Key | Default | What it does |
|---|---|---|
| `embed.model` | `BAAI/bge-small-en-v1.5` | the embedding model (`kb model list`), or `none` |
| `search.k` | 8 | hits returned (1–20) |
| `search.rrf_k`, `search.list_weights.*`, `search.bm25.*` | 60, 1, 1/2/3 | ranking (see `kb explain`) |
| `search.exact_list` | true | the exact-identifier list (below) |
| `search.synonyms` | `""` | the synonyms file (default: the store's `synonyms.txt`, below) |
| `search.spell` | true | correct a query word when the full-text list is short (below) |
| `search.stores` | `[]` | stores every search also reaches |
| `search.supersede_penalty` | 0.5 | score multiplier for a superseded entry (0–1; 1 turns it off) |
| `output.budget`, `output.expand` | 0, `none` | defaults for `--budget` and `--expand` |
| `agent.caller` | `""` | default `--caller` |
| `log.open_window` | `30m` | how recent a search must be for `kb show` to count as an open of it |
| `stale.days` | 180 | default `--days` of `kb stale` |

kb reads no environment variables except `$EDITOR`.

## Searching

```
$ kb search "embed meta" --caller claude
search 7 · 2 hits
[1] data/a.md
Embed meta: How EnsureEmbedMeta records the model

[2] data/a.md › Details
Embed meta › Details
EnsureEmbedMeta records the embedding model and dim. Use reindex_test to check.
search_id=7
```

The first line, `search N · M hits`, carries the search id. Read a hit with `kb show <path>`; that
read is recorded as an "open" of the search. Agents and scripts should use `--json`: the hits
with `rank`, `rrf_score`, `fts_rank`, `vec_rank`, `exact_rank` (omitted when the chunk was not in
the exact list), `path`, `heading` and `text`, plus the exact `kb feedback` commands for that search.

- `--mode hybrid|fts|vec` (default hybrid). `-k N`, `--tag T`. Tags match without regard to case:
  `--tag Docker` finds an entry tagged `docker`. An entry's `aliases` are searched like its title.
- `--budget N` caps the human output at about N tokens (`ceil(chars/4)`): lower-ranked hits are
  condensed first, and the output says how many were.
- `--expand section|entry` shows each hit's whole section or whole entry instead of the chunk.
  Hits that fall in the same section (or entry) are shown once, at the best rank, with
  ` · also [r]`. Expanded text counts toward `--budget`. In `--json` every hit keeps its rank and
  gains an `expanded` field.
- `--stores a,b` also searches other stores (refs as above; `all` is every store of this location,
  at most 8; `''` turns off `search.stores`). Each store uses its own model and this store's
  `[search]` settings, and the lists are merged on rank. Another store's hit shows as `REF:path`
  (read it with `kb show REF:path`); a store that cannot be searched is skipped with a warning. The
  search is logged in this store only.
- `--neighbors` adds a `Related:` section after the hits: the entries linked to each hit's entry
  (as `kb related` lists them), without the entries already returned, at most 5 per hit. It is not
  logged; `--json` carries it as `neighbors`.
- A hit whose entry another entry supersedes (its front matter lists it under `supersedes`) is
  marked `· superseded` (`"superseded": true` in `--json`) and its score is multiplied by
  `search.supersede_penalty` (0.5 halves it), so it usually ranks below the entry that replaces it.
- **Identifier queries.** A query field that looks like an identifier (`foo_bar`, `pkg.Func`,
  `--flag`, `path/to`, `a:b`, `camelCase`, `v1beta`) also runs an exact-identifier list that
  matches it only as a whole token, so `kb search reindex_test` ranks the chunk that says
  `reindex_test` above one that says "reindex test". `exact_list = false` in `[search]` turns it
  off (`kb eval --set search.exact_list=false --compare` shows what that costs). A query that
  starts with `-` goes after the flags and `--`: `kb search --json -- --embed-model`.

**Synonyms.** The file `synonyms.txt` in the store's directory (`.kb/stores/<name>/`) lists words
that mean the same thing. Each line is one comma-separated group; a line that starts with `#` is a
comment:

```
# product names
gadget, gizmo
```

A query word that is in a group also matches the other words of that group in the full-text list.
Synonyms do not reach the exact-identifier list or vector search. `search.synonyms` names another
file (kb stops with an error if it is missing). Without a store file, kb uses the machine-wide
store's `synonyms.txt`.

`kb synonyms suggest` proposes groups from the search log. It finds reworded searches: one caller
searches, gets no useful hit, and searches again within five minutes with one word changed, and
the second search works. Each candidate prints as a `synonyms.txt` line under a comment with its
evidence. The output also lists gap words (words of unsuccessful searches that no chunk holds).
Flags: `--since`, `--window`, `--min`, `--gaps` and `--json`. kb never writes `synonyms.txt`; copy in
the groups you want.

**Spelling.** When the full-text list finds fewer than 3 rows, a query word that no chunk holds is
also searched as the closest common word in the store: one edit away, or two for a word of 8
letters or more. A note on stderr names the word used (`note: also searched "gadget" for
"gadgte"`). Identifiers, numbers and words under 4 letters are not corrected. `search.spell = false`
turns this off.

**Links.** kb reindex stores each entry's links: Markdown links and `[[wikilinks]]` in the body
(`links`) and the front-matter lists `related`, `supersedes` and `depends_on` (paths relative to
the file, or `[[wikilinks]]`). `kb related <path>` lists them both ways: `→` for a link the entry makes, `←` for
one that points at it. `--depth 2` or `3` also follows the links of the entries reached (a line
then names the entry it came through); a link to nothing indexed shows as `(unresolved)`.
`REF:path` reads another store; `--json` gives the edges.

`kb stale` lists the entries that need a look, read from the index: no usable date, a date
(`verified`, else `updated`) at least `--days` days old (default `stale.days`, 180), superseded by
another entry, or a path link to nothing. `--days 0` lists every dated entry; `--json` gives the
groups. It exits 0 whether or not anything is listed.

Feedback is optional: `kb feedback N <rank> --useful` (or `--not-useful`, or `kb feedback N --none`
when nothing helped; `--last` targets your newest search, `--note` adds a reason).

## Measuring and tuning

- `kb explain "<query>"` runs the search `kb search` would run, without logging it, and prints
  every fused candidate: its rank in each list (FTS with per-column bm25 scores, vector, exact),
  each reciprocal-rank-fusion term, and whether it was returned, dropped by the per-entry cap or
  cut by k. `--set` tries a setting; `--json` is machine-readable. It covers one store.
- `kb stats [overview|sources|trend|entries|gaps|settings]` reads the search log: coverage, hit
  rate, MRR, latency, which list each hit came from (`sources`: both, fts-only, vec-only,
  exact-only, and `with_exact`), what was never answered (`gaps`), and results per settings hash.
  `--since 7d`, `--caller`, `--json`; `--sql` runs read-only SQL on the log.
- `kb eval` replays every query that has a useful verdict and scores the ranking (hit@k, MRR,
  nDCG@k with 95% bootstrap intervals; per path+heading, per entry, and opens apart). It reads the
  log and never writes it. Baselines make a change measurable:

  ```bash
  kb eval --save-baseline                                # .kb/stores/<store>/eval/default.json
  kb eval --set search.rrf_k=20 --compare                # deltas, CIs, wins/losses/ties, sign test
  kb eval --set search.exact_list=false --compare        # what the exact list is worth
  kb eval --save-baseline tuned && kb eval --compare tuned --json
  ```

  `--compare` warns (on stderr) when the model, the entry count or the other-store gold changed
  since the baseline, and refuses a baseline taken at another `-k`. Below `eval.min_judged`
  (30) paired queries it says the sample is small.

## Embedding models

kb embeds in-process with a local model (default `BAAI/bge-small-en-v1.5`, about 130 MB on disk);
no service or API key is involved. Models are downloaded once per machine to
`~/Library/Caches/kb/models`: run `kb model pull` up front, or let `kb init`, `kb reindex` or
`kb add` fetch it. `kb search`, `kb show` and `kb doctor` never download; without the model,
search falls back to full-text search (exit 2) and says to run `kb model pull`.

```bash
kb model pull            # download the model named by [embed] model
kb model list            # supported models, and which are downloaded
kb model path            # where the configured model lives
kb model cache           # the machine-wide embedding cache (--stats, the default, or --clear)
```

Changing `embed.model` on an existing index needs `kb reindex --all`. `model = "none"` indexes and
searches with full-text search only. Embeddings are cached per machine by model and text, so
the same text is never embedded twice in any store.

## Upgrading

A new kb can change the format of the files it keeps: each store's index database (`kb.sqlite`,
which holds the search log), its `config.toml`, and the location's `.kb/kb.toml`. After
installing a new kb, a command that meets an older file stops with exit 1 and

```
store database /path/.kb/stores/default/kb.sqlite is at schema v5; this kb needs v6. Run: kb migrate up (with the same --store, if any)
```

Run that command. It copies each file to `<file>.bak-v<N>-<time>` beside it (the newest 3 are
kept, and `.kb/.gitignore` ignores them), upgrades it, and ends with `needs: kb reindex` when a
step cleared derived data (`kb reindex` rebuilds it; entries are never lost).

```bash
kb migrate status                 # versions and pending steps; exit 1 if any are pending
kb migrate up --dry-run           # the plan and the backups it would write; nothing changes
kb migrate up                     # back up, then upgrade this store and the location file
kb migrate up --all               # every store of the location, and the global location
kb migrate verify                 # integrity and schema check; exit 1 on drift
```

Many projects: `for d in ~/work/*/; do (cd "$d" && kb migrate up); done`. A file written by a newer
kb is refused with "upgrade kb" and is never changed; to go back, restore a backup (copy
`kb.sqlite.bak-v<N>-<time>` over `kb.sqlite`). The embedding cache is rebuilt by itself and never
needs `kb migrate`. `kb doctor`, `kb stores`, `kb config --path` and `kb version` work on any
version, and `kb doctor` lists pending steps.

## Exit codes and logs

| Code | Meaning |
|---|---|
| 0 | success |
| 1 | a well-formed failure: bad flag or argument, no such entry, store or search, an embedding-model mismatch, an older file that needs `kb migrate up`, a damaged or locked database, or an interrupt (Ctrl-C) |
| 2 | the embedder is unavailable: a hybrid search (or `kb eval` replay) fell back to full-text search, or indexing could not load the model |
| 3 | internal error (a kb bug). The details go to `<location>/.kb/kb.log` (or `~/Library/Caches/kb/kb.log` when there is no location) under the id printed on stderr. Please report it. |

Errors are one line on stderr starting with `kb`; warnings go to stderr too, never into stdout.
`--debug` prints the full error chain and best-effort steps that failed quietly.

## Using kb from an agent

Paste this block into the project's `CLAUDE.md` or `AGENTS.md`:

```markdown
## Knowledge store (kb)

Search the knowledge store before answering questions about this project, its environment or
its tools, and before adding a finding (prefer updating an existing entry).

- `kb search "<question in plain words>" --caller claude` lists ranked hits (`--json` for
  structured output; `--expand section` for whole sections; `--budget N` to cap the size).
  Identifiers such as `foo_bar`, `pkg.Func` or `--flag` match exactly; put a query starting
  with `-` after `--`.
- `kb show <path>` (or `kb show REF:path` for another store's hit) reads a hit's entry. Do not
  read or grep the store's files directly.
- `kb add <draft.md>` saves a durable, non-obvious finding.
- Optional: `kb feedback <search_id> <rank> --useful` (or `--not-useful`, or `<search_id> --none`)
  records whether a search helped.
- Run `kb help` or `kb <command> --help` for details.
```

## Develop

```bash
go test -tags fts5 ./...
```

The `fts5` tag is required: without it the SQLite driver has no FTS5 and the database tests fail.
`docs/TESTING.md` and `docs/CODING_STYLE.md` hold the project's rules.
