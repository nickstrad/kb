# kb

A command-line knowledge store for agents and people. `kb` indexes Markdown entries with SQLite
FTS5 and sqlite-vec, searches them with hybrid full-text + vector ranking, logs every search, and
records feedback on what helped so `kb stats` can show where the store falls short.

This repository holds the Go CLI. Knowledge entries
live in a separate knowledge repository (`$KB_ROOT`): entries in `$KB_ROOT/data/`, the index and
search log in `$KB_ROOT/.kb/kb.sqlite`.

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

- `kb model list` shows the supported models (pick one with `--embed-model <id>`) and which are
  downloaded.
- `--embedder none` indexes and searches with full-text search only.
- Changing the model or embedder on an existing index needs `kb reindex --all`.
- The old `KB_EMBEDDER`, `KB_EMBED_*`, `KB_OLLAMA_URL` and OpenRouter variables are gone and
  ignored.

## Use in a project

```bash
export KB_ROOT=<project>/knowledge   # until root discovery and `kb init` land
kb model pull
kb add <draft.md>
kb search "<question>" --caller claude
```

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
