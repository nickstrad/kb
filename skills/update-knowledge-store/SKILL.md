---
name: update-knowledge-store
description: Search, read, add, revise, or remove durable findings about this project, its environment, and its tools using the kb CLI. Use when the user asks to search or update the knowledge store, before answering environment or tooling questions, and when a durable finding is worth saving.
---

# Use the knowledge CLI

Use the `kb` CLI for all knowledge discovery, reading, and maintenance. `kb` operates on the
knowledge repository named by `$KB_ROOT` (run `kb help` to see the current default); entries live
under `data/` there and the search index in `.kb/kb.sqlite`. If the repository has a contract
(`$KB_ROOT/AGENTS.md`, often also exposed as `CLAUDE.md`), read it first; its rules override the
defaults below. Entry paths returned by `kb` are identifiers to pass back to the CLI, not an
invitation to open or browse the source filesystem.

**Do not inspect the corpus directory, read its files directly, or search it with `rg`, `grep`,
`find`, or similar tools. Do not generate `index.md` to discover knowledge.** Use the commands
below. If the CLI fails, report the error or diagnose it with `kb doctor` and `kb help`; do not
silently bypass it. Direct filesystem investigation is appropriate only when the user explicitly
asks to debug or repair the CLI or its storage.

## Find and read

```bash
kb search "<question in plain words>" --caller claude
kb list
kb list --tag <tag>
kb show <entry>
kb show <entry> --chunks
```

Use `--caller claude` from Claude Code and `--caller codex` from Codex. Search before adding;
read relevant hits with `kb show <entry>` and prefer updating an existing finding over creating
a duplicate. `kb list` helps when the right search terms are unclear. Use `--json` on search
when structured results help.

If a search misses, vary the query, use `kb list`, or run `kb doctor`. When the embedder is
unavailable, `kb search --mode fts "<query>" --caller claude` still searches the indexed text.
Hybrid fallback prints usable FTS results but exits 2; distinguish that from a search that
returns no results.

## Embedder setup

`kb doctor` names the embedder in effect. It is chosen per project by environment:

- `OPENROUTER_API_KEY` (or `KB_OPENROUTER_API_KEY`) set: OpenRouter with
  `openai/text-embedding-3-small`.
- Otherwise: a local Ollama server (`KB_OLLAMA_URL`, default `http://127.0.0.1:11434`).
- `KB_EMBEDDER=none`: no embeddings; indexing and search use full-text search only.
- `KB_EMBEDDER` (or `--embedder`) forces `ollama`, `openrouter`, `openai` or `none`;
  `KB_EMBED_MODEL` (or `--embed-model`) overrides the model.

Keep the embedder and model fixed for a store: changing either makes `kb` refuse to mix vectors
until `kb reindex --all` rebuilds the index. Do not switch embedders unless the user asks.

Judge every search before moving on, including one that returned nothing. The first line of
`kb search` output is `search N · M hits`, where N is the `search_id` (it survives `| head`);
each hit starts with its `#rank`; the last line prints the exact feedback commands, and `--json`
carries them in its `feedback` field:

```text
kb feedback <search_id> <rank> --useful
kb feedback <search_id> <rank> --not-useful --note "Why the result did not help"
kb feedback <search_id> --none --note "What was missing"
```

`--none` records that nothing in the search helped; it takes no rank and is the only verdict a
zero-result search can get. A later `--useful` on a hit withdraws it, and it is refused while a
hit is marked useful. `kb feedback --last <rank> --useful --caller claude` (or `--last --none`)
judges the caller's newest search and echoes its query; with concurrent sessions under the same
caller, prefer the explicit `search_id`. If you reworded a query, judge the search you used and
mark the earlier rewordings `--none`. An unjudged search counts as unknown in `kb stats`.

Angle-bracket arguments in this skill must be replaced with values from the task or CLI output.

## What belongs in the store

Durable, non-obvious findings a future agent or the user can act on: environment facts (hosts,
services, paths, versions, quirks), tool usage specific to this project or environment, edge
cases that would otherwise waste time again, and runnable helper scripts with their purpose
explained.

Record the reusable fact, not the errand that produced it: the behavior, its consequence, and how
to act on it. Keep a specific incident only as a short **Seen in** note. One-session
observations, personal preferences, and feedback about agent behavior belong in agent memory,
not here. Run `kb stats gaps` for a ranked source of what to write next: it lists normalised
queries that never produced a useful verdict.

## Add or update

For a new finding, draft outside the corpus in a temporary directory (your scratchpad when one
exists). Use `templates/single-file-entry.md` for self-contained prose, or
`templates/topic-readme.md` for a topic with companion `scripts/`, `docs/`, or `tools/`. The
draft is a lowercase kebab-case `.md` file named for its subject (`docker-networking.md`), or a
topic directory with `README.md`. Register the draft through the CLI:

```text
kb add <absolute-draft-file.md>
kb add --dir <absolute-draft-topic-directory>
```

`kb add` validates and imports the source, then indexes it in SQLite. Do not place drafts into
the corpus by hand. Unless the contract says otherwise:

- Front matter needs `title`, a one-line `summary` that states the finding, `tags`, and
  `updated` (bump it whenever content changes).
- Include `verified` only when you actually ran the documented commands, with the date and the
  environment tested. Otherwise mark what is unverified in the body. Never claim a command was
  verified unless it was run.
- Commands must be copy-pasteable; explain any required substitutions.
- Never store secrets or private connection strings; record where a credential lives, never its
  value.
- Scripts need a shebang, `set -euo pipefail` for Bash, executable permissions, and a one-line
  header stating their purpose. List every companion in the topic README and verify scripts
  before importing.

For an existing finding, first read it with `kb show <entry>`, then edit it through:

```text
kb edit <entry>
```

`kb edit` opens `$EDITOR` (default `vi`) and reindexes changed content. For an automated edit,
set `EDITOR` to a temporary editor script that edits only the path the CLI passes as its
argument. Do not discover source paths yourself or modify other entries from that script.
Confirm the result with `kb show <entry>`.

Remove an obsolete entry with `kb rm <entry>`; directory removal also requires `--yes` and
removes its companions. Correct a finding in place rather than appending contradictory advice.
If the CLI lacks an operation the task needs, explain the gap rather than bypassing it.

## Health and optional export

```bash
kb doctor
kb help
```

Use `kb reindex <entry>` or `kb reindex --all` when a stale SQLite index needs rebuilding.
These commands preserve search history and feedback. The database also holds that history;
do not delete it as a routine repair.

`index.md` is optional and normally absent. Only when the user requests a Markdown map, run
`kb index`. This recreates the snapshot from source metadata without requiring the search
database or Ollama. It is safe to delete later; normal mutations do not recreate it.

## Finish

Report the entry identifier, the finding added or changed, and relevant verification. Commit
only when the user asks. Stage only the relevant source and tooling changes; never commit the
SQLite database or the optional `index.md` export.
