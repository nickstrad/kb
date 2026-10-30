# CLI tests run in a per-test world

> `depsFor(t)` gives each CLI test its own deps, home, caches and working directory, found by test name, so tests are parallel without `t.Chdir`.

**Fact:** `internal/cli/main_test.go` keeps a registry `t.Name() -> *world` (deps + home + cwd under `t.TempDir()`). `worldFor` walks up the name segments, so a subtest shares its parent's world. `chdir(t, dir)` sets `workspace.Env.Cwd` in that world; the process cwd never moves. `execute` runs on `deps.forRun()` (fresh embedding-cache list and synonyms memo). A command that resolves a relative path against the real cwd (`filepath.Abs`, in `kb add`) would see the package directory, so tests pass absolute paths.

**Why it matters:** `t.Chdir` and `t.Setenv` forbid `t.Parallel`; a shared deps made a forgotten restore leak across tests. A new test that reads the real cwd or env silently becomes order-dependent.

**Subtests share the parent's world** (deps fields and cwd): a subtest that swaps a field or calls `useRoot` changes it for later siblings, which is fine while subtests are sequential. A `t.Parallel` subtest that does either would race its siblings without any failure naming the cause; give it its own state instead (call `newWorld`-style setup in a top-level test, or don't swap).

**How to apply:** start with `t.Parallel()`, use `useRoot`/`newTempRoot`, swap fields on `depsFor(t)`, never `testDeps` (gone). Use `runErr` + `wantKind` for decisions. Verify with `go test -race -count=3 ./internal/cli/`.

**Seen in:** internal/cli/main_test.go, internal/cli/root.go (`forRun`, `executeErr`) · 2026-10-30
