# Root discovery: workspace.Resolve and how cli tests pick a store

> The store is --root, else --global, else the nearest ancestor with .kb/config.toml or .kb/kb.sqlite; there is no KB_ROOT, and tests chdir into the store.

**Fact.** `workspace.Resolve(rootFlag, global, Env)` (internal/workspace) is pure apart from stat calls; `Env`
carries cwd, home, XDG_DATA_HOME and the cache base, and `workspace.OSEnv()` is the only os reader. cli reaches it
through `deps.workspaceEnv` and `d.workspace(cmd)`; `incidentLogPath(d, cmd)` resolves the same way and falls back
to the user cache dir. Discovery resolves symlinks in the cwd first (physical parent chain); `--root`/`--global`
are only made absolute and cleaned. Discovery never creates `.kb/`; `--root`/`--global` may name a missing dir and
add/reindex create `.kb/` there (interim until `kb init`, 1.4).

**Why it matters.** Tests that expect "no .kb created" must check `.kb/kb.sqlite`, since the test store already has
`.kb/config.toml`. Roots on macOS differ between `t.TempDir()` (/var) and discovery (/private/var).

**How to apply.** CLI tests call `newTempRoot(t)` / `useRoot(t, dir)` (adds an empty `.kb/config.toml`, `t.Chdir`,
returns the symlink-resolved path); never parallel. The harness passes `--root` to every command. TestMain overrides
home/XDG/cache in `testDeps.workspaceEnv`, so a `--global` CLI test writes under a temp home (clean up
`<home>/.local`). The harness passes `--root` to every command except `workspace-discovery`, which uses
`h.kbAt(dir, ...)`; its not-found check runs in an `os.MkdirTemp` dir outside the repo and skips if any ancestor
holds `.kb/`, because the sandbox lives inside the repo and the repo may become a store (PLAN 1.5 accept). Under
`--debug` every command prints `kb <cmd>: store <root> (<how>)` on stderr first (Q48): tests asserting an empty
stderr under --debug must skip that line.

**Seen in:** internal/workspace, internal/cli/root.go, internal/cli/add_test.go · 2026-10-30; .agent/scripts/adversarial/harness/scenarios.go (scWorkspaceDiscovery) · 2026-10-30 W4 pass
