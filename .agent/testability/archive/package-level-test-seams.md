# Package-level test seams
> Tests swapped package-level function vars and shelled out to a host `sed` to fake an editor; seams now arrive through `deps` and `local.puller`.

- **Smell:** global/process state · hard-wired I/O/env
- **Status:** resolved (2026-10-29)
- **Where:** `internal/cli/setup.go` (`var newEmbedder`), `internal/embed/local/local.go` (`hubLoad`), cli edit tests (`$EDITOR` = `sed -i …`)
- **Symptoms in tests:** on macOS `TestMutationCommandsLeaveOptionalIndexAlone` failed and `TestEditModelMismatchUsesEditPrefix` passed vacuously, because BSD `sed -i` read the script as a backup suffix (TESTING.md §7); the Pull dimension check was unreachable (Q27); every swap of a package var forbade `t.Parallel`.
- **Why it happens:** the seams were mutable package state instead of fields of an injected struct, and the editor was a real process.
- **Remedy:** CODING_STYLE §5 `deps` struct; `local.puller` field; a Go `deps.editor` func.
- **Cost/benefit:** small; removed a platform-dependent false pass.
- **Resolution:** PLAN 1.2 and the W2/W3 passes: `deps.editor` + `useFakeEditor` (Q33), `puller` with fake pullers in `TestPullDimCheck`/`TestPullCancelMidDownload` (Q27, Q32), `deps.embedder`. Knowledge: `macos-bsd-sed-in-tests.md`. What remains of this shape (the shared `testDeps` itself) is the active entry `cli-shared-test-deps`.
- **Last reviewed:** 2026-10-30 · testability first pass
