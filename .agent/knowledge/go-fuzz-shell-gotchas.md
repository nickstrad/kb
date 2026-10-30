# Go fuzz runs: zsh flags and worker shutdown

> In zsh an unquoted `$extra` holding two flags is one argument (exit 2 in 0.1 s); with 18 workers a short `-fuzztime` can end in "context deadline exceeded" with no crasher: rerun with `-parallel 8`.

**Fact.** zsh does not word-split unquoted variables, so `go test ... $extra` with
`extra="-fuzzminimizetime 2s"` passes one bad flag and the run fails at once (exit status 2).
Separately, on an 18-core Mac a 12 s run of `FuzzSplitInPlace` failed with
`context deadline exceeded` at shutdown and wrote nothing to `testdata/fuzz/`; the same run with
`-parallel 8` passed (W7 pass).

**Why it matters.** Both look like a fuzz failure in a wave-end report; neither is a finding.

**How to apply.** Loop over fuzz targets with flags spelled out (or `${=extra}` in zsh). A fuzz
"failure" with no new file under `testdata/fuzz/<Target>/` and no panic is a harness problem:
rerun with `-parallel 8` before reporting it.

**Seen in:** internal/chunk (FuzzSplitInPlace) · 2026-10-30
