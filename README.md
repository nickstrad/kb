---
title: kb knowledge repository CLI
summary: kb indexes, searches, validates, and maintains this knowledge repository, with a rebuildable SQLite index and a SQL-queryable search log.
tags: [knowledge, search, sqlite, tooling]
updated: 2026-09-13
verified: 2026-09-13 against the installed CLI, local Ollama, fresh SQLite index, SQL examples, and tagged Go test suite
---

# kb knowledge repository CLI

`kb` is the command-line front door to `/root/Raw/knowledge`. Markdown files remain the source
of truth. The CLI discovers and validates entries, splits them into sections, indexes their text
with SQLite FTS5, embeds them with the local Ollama `nomic-embed-text` model, and combines lexical
and vector matches with reciprocal rank fusion. It also regenerates `index.md` after content
changes.

The installed command is `/usr/local/bin/kb`. It uses `/root/Raw/knowledge` unless `KB_ROOT` is
set. Search calls should identify their caller with `--caller codex` or `--caller claude`; the
fallback is `KB_CALLER`, then `unknown`.

Install or refresh the binary from this checkout with:

```bash
/root/Raw/knowledge/kb/scripts/install.sh
```

The installer applies the required `fts5` build tag, writes the gitignored binary to
`/root/Raw/knowledge/kb/.cache/kb`, updates the `/usr/local/bin/kb` symlink, and finishes by
running `kb doctor`. Development checks need the same tag:

```bash
cd /root/Raw/knowledge/kb
go build -tags fts5 ./...
go vet -tags fts5 ./...
go test -tags fts5 ./...
```

Plain `go test ./...` fails the database checks without that tag. Ollama HTTP tests bind a local
listener, so they also need loopback networking. On this droplet the default Go cache may point
into the gRPC course tree; set `GOCACHE=/tmp/kb-go-cache` when working in a restricted checkout.

## Commands

Run `kb help <command>` for the complete flag help. These examples use actual entries and paths
on this droplet.

| Command | Real example | What it does |
| --- | --- | --- |
| `add <file.md>` | `kb add /tmp/kb-smoke.md` | Validates and indexes a single-file entry, then regenerates `index.md`. The smoke workflow below first creates this source from a real entry. A source outside the repository is copied to the repository root. |
| `add --dir <topic>/` | `kb add --dir /tmp/kb-protobuf-smoke` | Adds a directory entry whose front door is `README.md`; `docs/*.md` are indexed too. The smoke workflow below first creates this source from the real `protobuf/` entry. |
| `edit <entry>` | `EDITOR=vi kb edit protobuf` | Opens the entry's front-door file, reindexes it only if its content changed, and regenerates `index.md`. `vi` is the default when `EDITOR` is unset. |
| `rm <entry>` | `kb rm kb-smoke.md` | Removes a single-file entry and its index rows, then regenerates `index.md`. Directory removal also needs `--yes` because it removes the whole directory. Use this after adding the scratch entry in the smoke workflow below. |
| `show <entry>` | `kb show protobuf` | Prints an entry. For a directory it prints `README.md` and its indexed `docs/*.md` files. |
| `show <entry> --chunks` | `kb show protobuf --chunks` | Prints the chunks stored for the entry, including ordinal, source file, heading, length, and text hash prefix. |
| `list` | `kb list --tag postgres` | Lists path, summary, updated, and verified fields, optionally restricted to an exact tag. |
| `search <query>` | `kb search "daemon dies when I log out of ssh" --caller codex` | Runs hybrid FTS5 and vector search, returns eight hits by default, records the search, and prints `search_id=N`. Use `-k 5`, `--mode fts`, `--mode vec`, `--tag linux`, or `--json` as needed. |
| `feedback <search_id> <rank>` | `kb feedback "$search_id" 1 --useful` | Marks one returned rank useful; the feedback workflow below assigns `search_id` from a real search. Exactly one of `--useful` and `--not-useful` is required. Repeating the same search ID and rank replaces its verdict. |
| `reindex <entry>` | `kb reindex protobuf` | Rebuilds one entry when files were edited outside `kb edit`; unchanged content is skipped by hash. |
| `reindex --all` | `kb reindex --all` | Reconciles every Markdown entry with the database, removes database entries missing from disk, embeds changed chunks, and regenerates `index.md`. |
| `index` | `kb index` | Regenerates only `index.md` from entry front matter. It does not rebuild search data. |
| `doctor` | `kb doctor` | Checks FTS5 and sqlite-vec, embedding metadata, Ollama reachability, entry validity, database/file drift, `index.md` drift, and the installed binary. It exits nonzero for failures. |
| `version` | `kb version` | Prints the version stamped from the repository's short Git hash by the installer. |
| `completion` | `kb completion bash > /tmp/kb-completion.bash` | Generates a completion script for Bash. The command also supports Fish, PowerShell, and Zsh. |
| `help` | `kb help search` | Prints command help; `kb search --help` is equivalent for this example. |

For a disposable end-to-end check of the file-entry form of `add`, `show`, and `rm`, use a copy
of a real entry:

```bash
cp /root/Raw/knowledge/tmux-daemons-die-at-logout.md /tmp/kb-smoke.md
kb add /tmp/kb-smoke.md
kb show kb-smoke.md
kb rm kb-smoke.md
```

The equivalent directory-entry check is:

```bash
cp -a /root/Raw/knowledge/protobuf /tmp/kb-protobuf-smoke
kb add --dir /tmp/kb-protobuf-smoke
kb show kb-protobuf-smoke
kb rm kb-protobuf-smoke --yes
```

For feedback, capture the ID printed by a real JSON search and mark the returned first rank:

```bash
search_id=$(kb search "daemon dies when I log out of ssh" --json --caller codex | jq -r .search_id)
kb feedback "$search_id" 1 --useful
```

`add` rejects invalid front matter, secret-like text, and duplicate destinations. `add --dir`
also rejects binary companions containing a NUL byte in their first 8 KiB. When it copies an external source, an indexing failure
rolls the new repository copy back; an in-place source is retained. Directory copies omit dot
entries, `node_modules`, and non-regular files, and retain executable permission bits.

Hybrid search falls back to FTS when Ollama is unavailable. It still prints and logs the FTS
results as mode `fts-fallback`, warns on stderr, and exits 2. A pure `--mode vec` search cannot
fall back and exits 2 without a logged result.

A live `kb doctor` check on the verified date included:

```text
ok   driver: fts5 enabled, sqlite 3.53.4, sqlite-vec v0.1.6
ok   ollama: reachable at http://127.0.0.1:11434, model nomic-embed-text
doctor: 0 failed, 0 warnings
```

## Database and rebuilds

The database is `/root/Raw/knowledge/.kb/kb.sqlite`; with `KB_ROOT` set, it is
`$KB_ROOT/.kb/kb.sqlite`. The `entries`, `chunks`, `chunks_fts`, `chunks_vec`, and `embed_meta`
content is derived from the Markdown entries. Reconcile or rebuild that content with:

```bash
cd /root/Raw/knowledge
kb reindex --all
kb doctor
```

`reindex --all` keeps the search log. The `searches`, `search_results`, `search_candidates`, and
`search_feedback` tables exist only in the SQLite database and cannot be reconstructed from the
Markdown files. Deleting `kb.sqlite` makes a fresh content index possible, but permanently loses
that log unless the file was backed up first.

The search log deliberately copies `entry_path` and `heading` into its result tables. Use those
columns for historical analysis: `chunk_id` is only forensic metadata because a later reindex can
reuse the same numeric ID for a different chunk.

## Querying search quality

Open the log with:

```bash
sqlite3 -header -column /root/Raw/knowledge/.kb/kb.sqlite
```

This query defines caller hit rate at the search level. A search is a hit when at least one rated
result is useful. Its rate denominator is only searches with at least one feedback row; searches
with no feedback are unknown, not failures. `total_searches`, `evaluated_searches`, and
`feedback_coverage_pct` keep the denominator and the missing coverage visible.

```sql
WITH per_search AS (
  SELECT
    s.id,
    s.caller,
    MAX(f.useful) AS any_useful
  FROM searches AS s
  LEFT JOIN search_feedback AS f ON f.search_id = s.id
  GROUP BY s.id, s.caller
)
SELECT
  caller,
  COUNT(*) AS total_searches,
  COUNT(any_useful) AS evaluated_searches,
  SUM(CASE WHEN any_useful = 1 THEN 1 ELSE 0 END) AS searches_with_useful_hit,
  ROUND(100.0 * COUNT(any_useful) / COUNT(*), 1) AS feedback_coverage_pct,
  ROUND(
    100.0 * SUM(CASE WHEN any_useful = 1 THEN 1 ELSE 0 END)
    / NULLIF(COUNT(any_useful), 0),
    1
  ) AS hit_rate_pct
FROM per_search
GROUP BY caller
ORDER BY hit_rate_pct DESC, caller;
```

This query finds repeated normalized query text with no positive verdict. It includes both
never-rated searches and searches rated only not useful, so `feedback_marks` and
`not_useful_marks` expose which case applies.

```sql
SELECT
  LOWER(TRIM(s.query)) AS query,
  COUNT(DISTINCT s.id) AS searches,
  COUNT(f.search_id) AS feedback_marks,
  SUM(CASE WHEN f.useful = 0 THEN 1 ELSE 0 END) AS not_useful_marks
FROM searches AS s
LEFT JOIN search_feedback AS f ON f.search_id = s.id
GROUP BY LOWER(TRIM(s.query))
HAVING COALESCE(SUM(f.useful), 0) = 0
ORDER BY searches DESC, query;
```

This query lists current entries that have never been shown to a caller. It tests
`search_results`, not `search_candidates`, because candidates outside the returned limit were not
visible to the caller.

```sql
SELECT e.path
FROM entries AS e
WHERE NOT EXISTS (
  SELECT 1
  FROM search_results AS r
  WHERE r.entry_path = e.path
)
ORDER BY e.path;
```

Missing measurements are SQL `NULL`, not zero. In `searches`, `tag_filter`, `embed_model`, and
`kb_version` may be `NULL`. `fts_ms`, `vec_ms`, and `embed_ms` are `NULL` when that stage did not
run; a recorded `0` means it ran in less than one millisecond. Thus `AVG(embed_ms)` already ignores
FTS-only searches. In the result and candidate tables, FTS rank/score is `NULL` for a vector-only
candidate and vector rank/distance is `NULL` for an FTS-only candidate. A feedback `note` is also
`NULL` when omitted.

Only completed searches with an outcome are logged. Hybrid fallback is a completed outcome and is
logged. A search that fails outright is absent, and if the single log transaction fails, none of
its search, result, candidate, or feedback-parent rows are present.

## Files in this directory

| Path | What it is |
| --- | --- |
| `README.md` | This knowledge entry and operator reference. |
| `.cache/kb` | Generated, gitignored binary produced by the installer. |
| `go.mod`, `go.sum` | Go module declaration and locked dependency checksums. |
| `cmd/kb/main.go` | Process entry point, signal handling, and version injection. |
| `internal/chunk/` | Markdown section chunking and its tests and fixtures. |
| `internal/cli/` | Cobra commands and integration tests. |
| `internal/embed/` | Embedder interface, deterministic test fake, and Ollama client. |
| `internal/entry/` | Entry discovery, front-matter validation, loading, and hashing. |
| `internal/index/` | Generated `index.md` renderer and tests. |
| `internal/reindex/` | File-to-database reconciliation and embedding reuse. |
| `internal/search/` | FTS/vector retrieval, reciprocal-rank fusion, logging, and feedback. |
| `internal/store/` | SQLite opening, schema, migrations, and transactional writes. |
| `scripts/install.sh` | Builds `kb` with the `fts5` tag into `kb/.cache/kb`, links `/usr/local/bin/kb` to it, prints the version, and runs `kb doctor`. |
