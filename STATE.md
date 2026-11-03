# kb: STATE

**Goal:** kb, the knowledge-store CLI (see README.md). Current work item: a public embedding
provider plugin, from the brief at [docs/embedding-plugin-brief.md](docs/embedding-plugin-brief.md).
The design is in [docs/embedding-plugin.md](docs/embedding-plugin.md).

**Status:** the embedding plugin is built, tested and pushed on `feat/embedding-plugin`. It waits
for a cross-family review before it can merge to main.

## Done

- `de98a77` embed: public `github.com/nickstrad/kb/embed` (contract + `Registry`), clients moved
  out of `internal/`, `embed/builtin`, new `http` and `command` providers. kb resolves through
  the registry with prefix `KB_`, and its default behavior is unchanged.
- `90e6eea` docs: design note `docs/embedding-plugin.md`, `embed/README.md`, and the brief.
- Tests: `go test -tags fts5 ./...` is green (389 passing, 3 skipped by design: live Ollama and
  the two real-repo checks). Without `-tags fts5`, `internal/store` fails because the tag is
  required; that was already true on `ad5c811`.

## In progress

Nothing running.

## Next steps

1. Cross-family review of `feat/embedding-plugin` against the brief (Codex or Muse seat). The
   verdict is PASS, NITS or FAIL, with one repair round allowed.
2. On PASS, merge to main. That is Nick's call; do not merge without the review.
3. Optional: drop `ollama.New`'s fallback to `KB_*` environment variables (its tests pin it today).

## Open decisions / blockers

- Merge to main: Nick, after review.

## Resume

- Worktree `/root/Software/kb-work/embedding-plugin`, branch `feat/embedding-plugin` (pushed to
  origin).
- Verify with `go vet -tags fts5 ./... && go test -tags fts5 ./...`.
- No background runs belong to this item.
