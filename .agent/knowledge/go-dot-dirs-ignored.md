# Go `./...` skips dot-directories such as .agent

> `go build/test/vet ./...` ignore `.agent/`, but `go run ./.agent/scripts/<pkg>` and `go test ./.agent/scripts/<pkg>` work.

**Fact.** Package patterns with `...` skip directories whose names start with `.` or `_` (and
`testdata`). Named explicitly, a package under `.agent/` builds as part of the main module
(import path `github.com/nickstrad/kb/.agent/scripts/<pkg>`), with no `go.mod` of its own.
Checked 2026-10-29: `go list ./...` lists nothing from `.agent`; `go run`, `go vet` and `go test`
by explicit path all succeed.

**Why it matters.** Agent helpers never slow down or break `go test -tags fts5 ./...`, and they
are not covered by its coverage gates either.

**How to apply.** Run and test helpers by path. Keep them stdlib-only so `go.mod` stays clean;
a helper needing a dependency gets its own `go.mod` in its folder (ask the owner first).

**Seen in:** .agent/scripts/index-check · 2026-10-29
