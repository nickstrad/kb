# kb

A command-line knowledge store for agents and people. `kb` indexes Markdown entries with SQLite
FTS5 and sqlite-vec, searches them with hybrid full-text + vector ranking, logs every search, and
records feedback on what helped so `kb stats` can show where the store falls short.

This repository holds the Go CLI. Knowledge entries
live in a separate knowledge repository (the store): entries in `<store>/data/`, the index and
search log in `<store>/.kb/kb.sqlite`. kb uses the nearest directory at or above the current one
that has a `.kb/`; `--root DIR` and `--global` name a store explicitly.

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

`kb model pull` downloads the local embedding model (about 130 MB) into
`~/Library/Caches/kb/models`.

## Embeddings

kb embeds in-process with a local model (default `BAAI/bge-small-en-v1.5`, ~67 MB); no service or
API key is involved. Models are downloaded once per machine to `~/Library/Caches/kb/models`: run
`kb model pull` up front, or let `kb reindex` / `kb add` fetch it on first use. `kb search`,
`kb show` and `kb doctor` never download; without the model, search falls back to full-text
search (exit 2) and says to run `kb model pull`.

- `kb model list` shows the supported models and which are downloaded. The model is the `[embed]`
  `model` key of the store's config file (`kb config --path` says where; `kb config` shows the
  effective settings).
- `model = "none"` in `[embed]` indexes and searches with full-text search only.
- Changing the model on an existing index needs `kb reindex --all`.
- The old `KB_EMBEDDER`, `KB_EMBED_*`, `KB_OLLAMA_URL` and OpenRouter variables are gone and
  ignored.

## Use in a project

```bash
cd <project>/knowledge   # kb finds the store by walking up to .kb/; or pass --root DIR / --global
kb model pull
kb add <draft.md>
kb search "<question>" --caller claude
```

## Exit codes

| Code | Meaning |
|---|---|
| 0 | success |
| 1 | a well-formed failure: bad flag or argument, no such entry or search, an embedding-model mismatch, a damaged or locked database, or an interrupt (Ctrl-C) |
| 2 | the embedder is unavailable: a hybrid search fell back to full-text search, or indexing could not load the model |
| 3 | internal error (a kb bug). The details go to `<root>/.kb/kb.log` (or `~/Library/Caches/kb/kb.log` when there is no store) under the id printed on stderr; `--debug` prints them too. Please report it. |

## Using kb from an agent

`kb help` is the reference. Paste this block into the project's `CLAUDE.md` or `AGENTS.md`:

```markdown
## Knowledge store (kb)

Search the knowledge store before answering questions about this project, its environment or
its tools, and before adding a finding (prefer updating an existing entry).

- `kb search "<question in plain words>" --caller claude` lists ranked hits.
- `kb show <path>` reads a hit's entry. Do not read or grep the store's files directly.
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
