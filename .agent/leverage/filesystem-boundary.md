# Filesystem boundary
> What kb reads, copies, edits and deletes on disk: entry discovery, symlinks, path arguments, and (from 1.5) in-place sources across a project.

- **Category:** external boundary · data integrity
- **Status:** needs work
- **Where:** `internal/entry` (`Discover`, `Resolve`), `internal/cli/add.go` (copy and rollback), `rm.go` (directory deletes), `edit.go` (`$EDITOR`); planned root discovery (1.1) and configured globs (1.5).
- **Why it matters:** a mistake here deletes or exposes user files outside the store. In-place sources widen it from `data/` to whole repositories.
- **Evidence:** the harness found that symlinks under `data/` pointing outside the root are followed and indexed (DEFAULTS #65, carried to 1.5). `path-traversal` scenarios pass. W2 pass: front-matter titles/summaries with control bytes or 100 KB length were accepted (Q20, fixed: caps 200/1000 runes, no control characters, valid UTF-8); `reindex --all` on a root with no `data/` now reads it as an empty corpus and prunes every row (Q23) — correct for a derived index, but it is a whole-index delete triggered by a missing directory.
- **Coverage today:** harness `path-traversal`, `markdown-edges`, `front-matter-hostile`; add rollback tests.
- **Next checks:** after 1.1/1.5: root discovery from nested and symlinked directories; globs that match `.git`, `node_modules` or huge trees; `kb rm` safety on in-place sources (should it ever delete a source file?); with configured sources, whether a missing source directory should prune its rows or refuse (1.4 step 5 decides); case-insensitive macOS filesystem quirks.
- **Last reviewed:** 2026-10-29 · W2 wave-end pass
