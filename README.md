# kb

A command-line knowledge store for agents and people. `kb` indexes Markdown entries with SQLite
FTS5 and sqlite-vec, searches them with hybrid full-text + vector ranking, logs every search, and
records feedback on what helped so `kb stats` can show where the store falls short.

This repository holds the Go CLI. Knowledge entries
live in a separate knowledge repository. A **location** is a directory with a `.kb/` folder
(`.kb/kb.toml`); it holds one or more named **stores**, each with its own config, embedder, index
and search log in `.kb/stores/<name>/` (`config.toml`, `kb.sqlite`). What a store indexes is its
`[[sources]]` list (see "Sources" below): by default the entries in `<location>/data/`.
kb uses the nearest directory at or above the current one that has `.kb/kb.toml`; `--root DIR` and
`--global` name a location explicitly, and `--store NAME` picks a store (default: `default_store`
in `kb.toml`). Only `kb init` creates a location or a store; `kb stores` lists them.

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
cd <project>/knowledge   # kb finds the location by walking up to .kb/kb.toml; or pass --root DIR / --global
kb init                  # creates .kb/ and the "default" store, and downloads the model
kb init --name adr       # optional: a second store, used with --store adr
kb stores                # list the stores
kb add <draft.md>
kb search "<question>" --caller claude
```

### Sources

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

## Upgrading

A new kb can change the format of the files it keeps: each store's index database (`kb.sqlite`,
which holds the search log), its `config.toml`, and the location's `.kb/kb.toml`. After
installing a new kb, a command that meets an older file stops with exit 1 and

```
store database /path/.kb/stores/default/kb.sqlite is at schema v2; this kb needs v4. Run: kb migrate up
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
version.

## Exit codes

| Code | Meaning |
|---|---|
| 0 | success |
| 1 | a well-formed failure: bad flag or argument, no such entry or search, an embedding-model mismatch, a damaged or locked database, or an interrupt (Ctrl-C) |
| 2 | the embedder is unavailable: a hybrid search fell back to full-text search, or indexing could not load the model |
| 3 | internal error (a kb bug). The details go to `<location>/.kb/kb.log` (or `~/Library/Caches/kb/kb.log` when there is no location) under the id printed on stderr; `--debug` prints them too. Please report it. |

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
