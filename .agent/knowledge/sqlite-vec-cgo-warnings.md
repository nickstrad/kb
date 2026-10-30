# sqlite-vec cgo deprecation warnings on macOS are noise

> `'sqlite3_auto_extension' is deprecated` warnings from sqlite-vec-go-bindings/cgo are harmless; ignore them.

**Fact.** Building `github.com/asg017/sqlite-vec-go-bindings/cgo` (v0.1.6) on macOS prints
`warning: 'sqlite3_auto_extension' is deprecated … not supported on Apple platforms` and the same
for `sqlite3_cancel_auto_extension`. The declarations come from the macOS SDK's `sqlite3.h`; at
link time the symbols resolve to the SQLite that mattn/go-sqlite3 compiles in, so the vector
extension registers and the vector tests pass.

**Why it matters.** Agents waste time chasing them, or mistake them for a failed build.

**How to apply.** Treat them as expected output of any first or `-a` build. Only investigate if
vector search actually fails (e.g. `no such module: vec0`).

**Seen in:** `go build -a -tags fts5 github.com/asg017/sqlite-vec-go-bindings/cgo` · 2026-10-29
