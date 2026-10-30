# Store sources ([[sources]])

> A store indexes only its explicit [[sources]] (no sources = nothing); test configs must carry the data/ source; globs use internal/glob, not doublestar.

**Fact.**
- `entry.Discover(root, cfg.Sources)` returns `(entries, skipped, err)`. Layout `entries` (a
  directory kb owns, strict front matter) keeps the old data/ rules; layout `files` walks a glob
  (`internal/glob`: `**` = zero or more segments, other segments `path.Match`), only `*.md`
  regular files, never `.git`/`.kb`/`node_modules`, front matter optional with `fallbackMeta`.
  A symlink resolving outside the location root is never read: it lands in `skipped`.
- A config with no `[[sources]]` indexes nothing and `reindex --all` prunes every row. `kb init`
  writes `data` (default store), `data-<name>` (named store) or the `--source` globs.
- Test helpers add the data/ source for you: cli `addStore`/`writeConfig` (and `dataSourceTOML`),
  harness `setConfig`/`makeLocation` (`dataSource`). A test that writes `config.toml` by hand
  must include it, or the store sees no entries. Run functions take `testConfig()`/`testSources`.
- `Entry.InPlace()` (files layout) drives: empty summary allowed (chunk 0 = title), kb rm
  refuses, index.md skips it, doctor skips the tags/updated warnings.

Reserved names live once, in `config` (`NeverWalkedDir`, `ReservedEntryDir`, `ReservedEntryFile[Fold]`): config validation, entry discovery and `kb add` all ask them. A files-source exclude ending in `/**` whose head matches a directory or an ancestor prunes the walk (`excludesWholeDir`; `discoverWalk`'s onDir hook counts visits in tests).

**Why it matters.** The old implicit data/ is gone (D9/N11); a forgotten source silently empties
a store, and an in-place file must never be deleted or copied by kb.

**How to apply.** Pass `cfg.Sources` to every Discover/Resolve and to `reindex.Options.Sources`.
Overlap is checked twice: statically in `config.validateSources` (same/nested entries dirs,
repeated globs) and per file in Discover (two sources claiming one file → Usage). Spellings that
differ only in case are one file on APFS: Discover folds the key and confirms with `os.SameFile`,
so `docs/*.md` + `Docs/*.md` is an overlap, not two entries. `Resolve` walks lazily (no front
matter read) and loads only the entry it returns; keep `TestResolveMatchesDiscover` green if you
change either. `index.md` describes the location's default store only: `kb index` refuses others.

**Seen in:** internal/entry/{entry,sources,fallback}.go, internal/glob, internal/cli/{sources,init,doctor}.go · 2026-10-30 (W7 pass: case fold, lazy Resolve)
