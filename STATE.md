# kb: STATE

**Goal:** kb, the knowledge-store CLI (see README.md). Current work item: a public embedding
provider plugin, from the brief at [docs/embedding-plugin-brief.md](docs/embedding-plugin-brief.md).
The design is in [docs/embedding-plugin.md](docs/embedding-plugin.md).

**Status:** Astra's review came back FAIL with two findings. The repair round fixed both and pushed
them to `feat/embedding-plugin`, which now waits for Astra's confirm review.

## Done

- `de98a77` embed: public `github.com/nickstrad/kb/embed` (contract + `Registry`), clients moved
  out of `internal/`, `embed/builtin`, new `http` and `command` providers. kb resolves through
  the registry with prefix `KB_`, and its default behavior is unchanged.
- `90e6eea` docs: design note `docs/embedding-plugin.md`, `embed/README.md`, and the brief.
- Repair round (brief: [docs/embedding-plugin-repair-brief.md](docs/embedding-plugin-repair-brief.md)),
  in the `embed: fix Astra review findings` commit. (1) `ollama.Provider` now builds through
  `newClient`, which reads no environment, so a non-`KB_` registry never picks up `KB_EMBED_MODEL`
  or `KB_OLLAMA_URL`. Test: `TestRegistryIgnoresKBEnvironment`. (2) In `embed/command`, a
  program path that does not exist (`fs.ErrNotExist`) is a configuration error, not
  `ErrUnavailable`. Test: `TestMissingAbsoluteProgramIsAConfigurationError`.
- Tests: `go test -tags fts5 ./...` is green (393 passing, 3 skipped by design: live Ollama and
  the two real-repo checks). Without `-tags fts5`, `internal/store` fails because the tag is
  required; that was already true on `ad5c811`.

## In progress

Nothing running.

## Next steps

1. Astra confirm review of the repair commit. A second FAIL goes to arbitration by Muse Wonder Full.
2. On PASS, merge to main. That is Nick's call; do not merge without the review.
3. Optional: drop `ollama.New`'s fallback to `KB_*` environment variables. Only direct callers
   of `New` use it now, and its tests still pin it.

## Open decisions / blockers

- Merge to main: Nick, after review.

## Resume

- Worktree `/root/Software/kb-work/embedding-plugin`, branch `feat/embedding-plugin` (pushed to
  origin).
- Verify with `go vet -tags fts5 ./... && go test -tags fts5 ./...`.
- No background runs belong to this item.
