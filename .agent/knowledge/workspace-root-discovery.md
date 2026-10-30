# Location and store resolution: workspace, config.Location, kb init

> A location is the nearest ancestor with .kb/kb.toml (or --root/--global); the store is --store, else kb.toml default_store; only kb init creates either; cli tests chdir into a made location.

**Fact.** `workspace.ResolveLocation(rootFlag, global, env)` (internal/workspace/location.go) is pure apart from stat
calls: `--root` > `--global` ($XDG_DATA_HOME/kb, else ~/.local/share/kb) > walk up for `.kb/kb.toml` as a regular file
(physical parent chain; a nested location wins). `--root`/`--global` name a location that may not exist. `Env`
carries cwd, home, XDG_DATA_HOME and the cache base; `workspace.OSEnv()` is the only os reader and cli reaches it
through `deps.workspaceEnv`. `TargetLocation` is the same for `kb init`: no flag means the working directory itself,
never an ancestor. `Location.Store(name)` needs `stores/<name>/config.toml` as a regular file (else NotFound listing the
stores); `StoreAt` builds the Workspace without that check (init). `ValidName` (`^[a-z0-9][a-z0-9_-]{0,31}$`, not
`global`) is what keeps StoreDir a direct child of `.kb/stores`. `Workspace` keeps `Root` (the location root), `KBDir`,
`DBPath`/`ConfigPath` (inside `StoreDir`), `Location`, `Store`. `workspace.ModelsDirOf(cacheBase)` is the one rule for the
model cache; production `deps.modelCacheDir` uses it.

cli: `deps.location(cmd)` = resolve + `config.ReadLocation` (a missing kb.toml is NotFound "run kb init"); `deps.workspace(cmd)`
adds the store (`--store` through `d.resolveRef` in cli/refs.go: `workspace.ParseRef` is syntax only (NAME, `global`, NAME@LOC, PATH = `/`, `~`, `./`, `../`, `.`, `..`; `docs/x` and `c:x` are refused), `workspace.Resolver` (resolve.go; config is passed in as `LocationInfo`/`Read` because config imports workspace) resolves a bare NAME: local store, then alias of the current location, then of the global location; the store wins with a stderr warning; an alias target's bare NAME is a store of the declaring location (no chaining) and its relative paths are relative to the declaring root; `global`/NAME@LOC/PATH name their own location, so with `--root`/`--global` they and aliases are Usage; `global` + `--root` is Usage; `deps.openStoreReadOnly(cmd, ref)` is the read-only open for 3.2: unusable stores warn and return nil). A
`default_store` that names no store is NotFound naming kb.toml. `--debug` prints `kb <cmd>: location <root> store <name> (--root |
--global | --store global | discovered from <cwd>)` first. The incident log is `<KBDir>/kb.log`, per location, else the user
cache dir. `kb stores`, `kb doctor` use `d.location` directly so a broken store does not hide the others.

`kb init` is the only creator (O_EXCL writes; idempotent): `.kb/kb.toml` + `.kb/.gitignore` for a new location (refused at $HOME or `/`
without `--force`, DEFAULTS #121), `stores/<name>/config.toml`, `data/`, then the database + embed_meta, then loads the model (exit 2
leaves the store created). `[[sources]]` is still rejected by config.Validate, so init writes none and every store indexes `data/`
until 1.5. `entry.Discover` reads a missing `data/` as an empty corpus; `kb doctor` warns about it.

**Why it matters.** Tests that expect "nothing created" must check `.kb/stores/default/kb.sqlite` or the location dir. Roots on
macOS differ between `t.TempDir()` (/var) and discovery (/private/var).

**How to apply.** CLI tests call `newTempRoot(t)` / `useRoot(t, dir)` (make `.kb/kb.toml` + `stores/default/config.toml` unless
present, `t.Chdir`, return the symlink-resolved path), `addStore`, `makeLocation`, `testWS(root)`, `testDBPath(root)`; never
parallel. TestMain overrides home/XDG/cache in `testDeps.workspaceEnv`, so `--global` tests write under a temp home (clean up
`<home>/.local`). The harness runs `kb init` for the scenario root and the global location (`H.initStores`; default-model scenarios
init with the default model and accept exit 2), `H.makeLocation` writes the files of an initialised-but-never-indexed store. Under
`--debug` tests asserting an empty stderr must skip the location line.

**Seen in:** internal/workspace, internal/config/location.go, internal/cli/{root,setup,stores,init,doctor}.go, .agent/scripts/adversarial/harness · 2026-10-30 W6, W7 (3.1)
