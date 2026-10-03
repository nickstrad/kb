# kb

A command-line knowledge store for agents and people. `kb` indexes Markdown entries with SQLite
FTS5 and sqlite-vec, searches them with hybrid full-text + vector ranking, logs every search, and
records feedback on what helped so `kb stats` can show where the store falls short.

This repository holds the Go CLI and the `update-knowledge-store` agent skill. Knowledge entries
live in a separate knowledge repository (`$KB_ROOT`): entries in `$KB_ROOT/data/`, the index and
search log in `$KB_ROOT/.kb/kb.sqlite`.

## Install

Needs Go and gcc (cgo builds SQLite with FTS5 and sqlite-vec).

```bash
KB_DEFAULT_ROOT=/path/to/knowledge scripts/install.sh
```

This builds `.cache/kb`, links `/usr/local/bin/kb` to it, and runs `kb doctor`.
`KB_DEFAULT_ROOT` is remembered in `.cache/default-root`; without it, `kb` defaults to
`~/knowledge` whenever `KB_ROOT` is unset.

## Embeddings

| Setting | Embedder |
| --- | --- |
| `OPENROUTER_API_KEY` (or `KB_OPENROUTER_API_KEY`) set | OpenRouter, `openai/text-embedding-3-small` |
| nothing set | Ollama at `KB_OLLAMA_URL` (default `http://127.0.0.1:11434`), `nomic-embed-text` |
| `KB_EMBEDDER=none` | no embeddings; full-text search only |
| `KB_EMBEDDER=openai` + `KB_EMBED_URL` | any other OpenAI-style `/embeddings` server |

`--embedder` and `--embed-model` (or `KB_EMBEDDER` and `KB_EMBED_MODEL`) override the choice.
Vectors are 768 numbers wide. Changing the embedder or model on an existing store requires
`kb reindex --all`. `kb help` and `internal/embed/provider` list every setting.

## Use in a project

```bash
export KB_ROOT=<project>/knowledge OPENROUTER_API_KEY=<key>   # or KB_EMBEDDER=none
kb add <draft.md>
kb search "<question>" --caller claude
```

Link the skill so Claude Code uses `kb` the intended way:

```bash
ln -s "$PWD/skills/update-knowledge-store" <project>/.claude/skills/update-knowledge-store
```

## Develop

```bash
go test -tags fts5 ./...
```

The `fts5` tag is required: without it the SQLite driver has no FTS5 and the database tests fail.
