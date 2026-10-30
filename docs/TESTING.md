# kb testing strategy

Audience: contributors, people and agents alike. Read time: about 15 minutes. This covers today's code and the v2 rework (rembed embedder, storage-backend seam, root discovery and `.kb/config.toml`, `kb eval`/`explain`/`context`, cross-store search, edges, synonyms, feedback priors). Error handling is in [CODING_STYLE.md](CODING_STYLE.md) §1; this document covers only where errors and assertions meet (§3.1).

Platform: kb is developed and tested on macOS (darwin/arm64). Tests run there, locally and in CI. Other platforms get release builds only (§9).

## 1. Principles

1. **One behaviour, one owner.** Each claim about the code is checked at exactly one layer (§2). Other layers may *exercise* that code, but they do not check that claim again.
2. **Assertions state what is always true. Tests choose inputs.** A fuzz run is how the two meet: the fuzzer generates inputs, and the assertions in the code check every one of them.
3. **Tests are hermetic.** No network, no model downloads, no host tools (`sed`, `git` config, real editors), no wall-clock sleeps. All files go in `t.TempDir()`.
4. **Decision-heavy code gets a table test per branch outcome (plus a fuzz property where the input space is large); the rest gets statement coverage.** Do not spend this effort on plumbing. MC/DC is not required (§4.2).

## 2. Layers and ownership

| Layer | Owns this kind of claim | Lives in | Runs |
|---|---|---|---|
| L0 Specification | The contract itself: a doc comment on every exported symbol and a `doc.go` per package listing inputs, outputs and invariants. It is not a test. Every other layer is derived from it. | source | n/a |
| L1 Assertions | A pre/postcondition or invariant that one function can check **locally** from its own arguments, result and state | `internal/assert`, called in code | every test and fuzz run |
| L2 Unit / table | **One decision** for chosen inputs: a branch, a validation, a mode choice, an error kind. Decision-heavy functions (§4.2) get their branch-outcome tables here. | `*_test.go` next to the code | `go test` |
| L3 Property / fuzz | A claim about **all inputs** that needs something outside the function to judge it: an oracle (real FTS5), a second run (metamorphic, determinism), or a round trip | `fuzz_test.go` per package | seeds on every `go test`; mutation with `-fuzz` |
| L4 Golden | **Exact bytes** that users or tools depend on: chunk boundaries for real entries, `--json` output, `kb explain` output | `testdata/golden` | `go test`; regenerate with `-update` |
| L5 Contract | Behaviour **every implementation** of an interface (storage backend, `Embedder`) must have | `internal/store/storetest`, `internal/embed/embedtest` | from each implementation's package |
| L6 Integration | **Wiring**: flags reach the service, output and exit code come back. One success and one failure per command. | `internal/cli/*_test.go` | `go test` |

### 2.1 Placement rules

- **Lowest layer wins.** If layer N can catch a bug, the regression test goes at N. Example: the hyphen bug (`draw-visual` sanitised to a token the document lacks) belongs to `FuzzSanitizeFTS` (its "a document equal to the query is found by it" property) plus a seed. It does not belong in a CLI test.
- **A local invariant is an assertion, not a test.** If L1 checks it, no test repeats the check. Tests only need to reach that code. Example: once `chunk.Split` asserts "Ord counts up from 0" and "TextHash == Hash(Text)", `FuzzSplit` and `assertInvariants` in `chunk_golden_test.go` drop those checks. What stays in them is what an assertion cannot see: the context-line format and the deadline.
- **A fixed-input copy of a property is a seed, not a test.** `TestSanitizeFTSAcceptedBySQLite` re-checks the `FuzzSanitizeFTS` property on ten inputs. Those inputs become `f.Add` seeds and the test is deleted. Plain `go test` still runs them.
- **What fuzzing doesn't reach gets a unit test.** Tests are cheap. If a line or branch isn't reached by the fuzz targets (check `go test -cover` with the seeds, or reason about the generator), add an explicit unit test for it rather than bending the fuzz target or leaving it uncovered. This is not duplication: the seed rule above applies only to claims the fuzzer already exercises.
- **Every boundary is tested, by a property or a unit test.** Boundaries are the edges of a range or a limit: empty and nil input; 0, 1 and the maximum; exactly at, one below and one above a limit (`MaxChunkChars`, `MinK`/`MaxK`, `ListLimit`, `CandidateLimit`, `PerEntryCap`, a token budget, a date window); the first and last element; and the switch points between modes. A property may own a boundary only if its generator demonstrably produces it (add the boundary value as an `f.Add` seed); otherwise write a table test with one row per boundary value. Name the rows after the boundary (`"at MaxChunkChars"`, `"MaxChunkChars+1"`).
- **Golden files pin bytes. They do not pin invariants.** If a golden diff is the only thing that would catch a bug, ask whether an L1 assertion or an L3 property should own it instead.
- **Integration tests never re-check service logic.** "`kb search --k 99` returns 20 hits" is a `ClampK` unit test. The CLI test checks only that `--k` reaches `Request.K`.

### 2.2 Package matrix

`A` assertions, `U` unit/table (`D` = has decision-heavy functions, §4.2), `P` property/fuzz, `G` golden, `C` contract.

| Package | A | U | P | G | C |
|---|---|---|---|---|---|
| entry | yes | yes | `FuzzParseFrontMatter` | | |
| chunk | yes | yes | `FuzzSplit` | yes | |
| embed (+ fake, None; v2 `embed/local` wrapping rembed) | yes | yes | | | **Embedder suite** |
| store (v2: interfaces) + `store/sqlite` | yes | yes | store model test (§5.2); v2: `FuzzSanitizeFTS` | | **Store suite** |
| migrate (+ each artefact's adapter, §6.1) | yes | yes, D (`Run`) | `FuzzSteps` | | |
| reindex | yes | yes, D | | | |
| search | yes | yes, D | `FuzzSanitizeFTS` (until v2), Fuse/applyCap properties | | |
| stats, index | | yes | | | |
| v2 workspace (root discovery), config | yes | yes, D | round trip, discovery | | |
| cli | | flag validation only, D (`runFeedback`) | | `--json`, `explain` | |

v2 moves (PLAN.md §2): `SanitizeFTS`, its fuzz target and its corpus move to `store/sqlite`. `embed/ollama`, `embed/openai`, `embed/provider` and DuckDB (`stats/duck.go`) are deleted, and their tests go with them. Write nothing new against them.

## 3. Assertions: `internal/assert`

### 3.1 Rules

- An assertion catches a **programmer error**: a broken internal invariant. A failed assertion is a kb bug by definition (CODING_STYLE.md §1.7).
- **Never assert on anything the user or the outside world controls**: flags, config, files, front matter, database contents read back from disk, embedder output (vector count or dimension), HTTP responses. Those return a well-formed error (CODING_STYLE.md §1.5).
- Validate once at the boundary and return an error. Downstream code may then assert the validated result. Example: `store.ReplaceEntryChunks` *returns an error* for a wrong-dimension vector (`TestReplaceEntryChunksRejectsBadEmbeddings`), because the vector came from an embedder. Code that runs after that check may assert the dimension.
- An assertion has no side effects. Behaviour must be the same with assertions on and off. The `noassert` CI job checks this (§8).
- An assertion is never the only handling of a required error path.
- **Panics, not `t.Fatal`.** Assertions live in production code and know nothing about `testing`. In a test, a panic fails the test with a stack trace. Under `-fuzz`, a panic is a crasher and gets minimised and saved.
- **Recovery.** Only the top level (`cli.Execute`, CODING_STYLE.md §1.5) may recover. It recovers any panic, an `assert.Violation` or a runtime panic, writes it to the incident log as `Internal` with its stack, and exits with the internal-error code (3). It never continues past a violation. No other code calls `recover`. This also settles the exit-2 clash noted in CODING_STYLE.md §1.5: without a recover, a Go panic exits with status 2, the same code as "embedder unavailable". Tests may recover only to prove that a check fires (§3.4).

### 3.2 Package

```go
// internal/assert/assert.go: shared by both builds.
package assert

// Violation is the panic value of a failed assertion; cli.report recognises it.
type Violation struct{ Kind, Msg string }

func (v Violation) Error() string { return "kb assert: " + v.Kind + " violated: " + v.Msg }
```

```go
// internal/assert/on.go
//go:build !noassert

package assert

import "fmt"

const Enabled = true

func Pre(cond bool, format string, args ...any)       { if !cond { fail("precondition", format, args) } }
func Post(cond bool, format string, args ...any)      { if !cond { fail("postcondition", format, args) } }
func Invariant(cond bool, format string, args ...any) { if !cond { fail("invariant", format, args) } }

func fail(kind, format string, args []any) { panic(Violation{kind, fmt.Sprintf(format, args...)}) }
```

```go
// internal/assert/off.go
//go:build noassert

package assert

const Enabled = false

func Pre(bool, string, ...any)       {}
func Post(bool, string, ...any)      {}
func Invariant(bool, string, ...any) {}
```

**Message format:** `"<func>: <invariant in words>: <values>"`, for example `"applyCap: len(out)=%d > k=%d"`. Name the function and the broken property, and include the values a reader needs to reproduce the failure.

```go
func applyCap(cands []Candidate, entryOf map[int64]string, k int) []Candidate {
	assert.Pre(k >= MinK && k <= MaxK, "applyCap: k=%d outside [MinK,MaxK]", k) // cheap: bare call
	...
	if assert.Enabled { // anything that loops or allocates goes inside the guard
		assert.Post(len(out) <= k, "applyCap: len(out)=%d > k=%d", len(out), k)
		assertPerEntryCap(out, entryOf)
	}
	return out
}
```

### 3.3 Why this costs nothing when compiled out

- `Enabled` is an untyped constant. Under `noassert`, the compiler deletes `if assert.Enabled { ... }` as dead code, including any helper calls inside it.
- The no-op functions are inlined to nothing, but **Go still evaluates their arguments**. In `assert.Post(isSorted(out), ...)`, `isSorted` runs even in a `noassert` build. The compiler may drop pure, cheap argument expressions. It will not drop calls it cannot prove pure.
- So: a bare call may compare a few locals and nothing more. Anything that calls a function, loops, or formats goes inside `if assert.Enabled { }`. This matters most on hot paths such as `Fuse` and the per-chunk loops.

### 3.4 Defaults and the release decision

- `go test`, `go build`, `go run` and fuzzing all run with **assertions on**. No tag is needed.
- **Release binaries keep assertions on**, for three reasons. (1) A wrong ranking or a half-consistent index is worse than a loud failure. (2) A write that panics mid-transaction rolls back, so failing fast is safe. (3) The guarded checks are O(results) or O(chunks of one entry), which is tiny next to SQLite and embedding (`embedQuery` is about 0.4 s warm). Revisit only if a profile shows assertions above about 1% of a search or a reindex. `make build-noassert` exists for that measurement.
- `internal/assert/assert_test.go` (`//go:build !noassert`) checks that each kind panics with a `Violation`. A sibling file (`//go:build noassert`) checks that nothing panics and that `Enabled` is false.

### 3.5 What to assert in kb

| Where | Kind | Condition |
|---|---|---|
| `search.Fuse` | Post (guarded) | sorted by `RRFScore` desc, then FTS rank, vec rank, chunk id; `len <= Params.CandidateLimit`; no duplicate `ChunkID`; each candidate has `FTSRank` or `VecRank` set |
| `search.applyCap` | Pre / Post (guarded) | `MinK <= k <= MaxK`; `perEntryCap >= 1`; `len(out) <= k`; no entry more than `perEntryCap` times; `out` is an order-preserving subsequence of `cands`; no returned chunk is `CapDropped` |
| `search.Searcher.Search` (settings) | Pre | a non-zero `Settings.Params` is `valid()` (positive limits, finite non-negative weights): `config.Validate` guarantees it, so a violation is a caller that skipped validation |
| `sqlite.Store.FTS` | Pre | `Limit > 0`; every bm25 column weight is finite and `>= 0` |
| `search.Searcher.Search` | Post | `Mode` is one of the four `Mode*` constants; `Mode == ModeFTSFallback` ⇔ `FallbackErr != nil`; hit ranks are `1..len(Hits)`; `Result.Hits != nil` |
| `search.SanitizeFTS` | Post | `and` and `or` both empty or both non-empty; even number of `"`; no empty phrase `""` |
| `chunk.Split` | Post (guarded) | `Ord` runs 0..n-1; `TextHash == Hash(Text)`; chunk 0 has an empty heading |
| `store.ReplaceEntryChunks` (v2 `sqlite` `ReplaceEntry`), before commit | Invariant (guarded) | for the entry, the row-id sets of `chunks`, `chunks_fts` and `chunks_vec` (vec skipped for dim 0) are equal. These are rows this transaction just wrote, so a mismatch is a kb bug. |
| `reindex`, after `EnsureEmbedMeta` succeeded | Pre | embedder `Dim()` and `Model()` equal the stored `embed_meta` |
| v2 cross-store merge | Post | as `Fuse`, plus each store's relative order is kept |
| v2 token budget (`kb context`) | Post | sum of included token counts `<= budget` |
| v2 root discovery | Post | returned root is absolute and contains `.kb/` |

**Errors, not assertions:** an empty query, an unknown mode, a bad `--k` string, a bad feedback verdict or rank, a missing `.kb/`, malformed front matter, an unknown config key, an embedder returning the wrong count or dimension, and an `embed_meta` mismatch with the index on disk.

## 4. Coverage and decision-heavy code

### 4.1 What the tools measure (checked October 2026)

| Tool | Measures | Notes |
|---|---|---|
| `go test -cover` | **statement** (basic-block) coverage only | `-covermode=set\|count\|atomic`. Use `atomic` with `-race`. |
| [`vladopajic/go-test-coverage`](https://github.com/vladopajic/go-test-coverage) | **thresholds** over a Go coverprofile (file, package, total), with exclusions and a GitHub Action | `v2.21.0` (27 Oct 2026), actively maintained. It reads the profile only, so build tags and cgo do not affect it. It measures nothing beyond statements. |

Statement coverage tells you which branches ran. It does not show that each condition inside a branch matters. For decision-heavy code (§4.2) we close that gap with tests designed per branch outcome and with fuzz properties, not with a tool.

### 4.2 Decision-heavy functions

Dropped for now (owner decision, 2026-10-29): MC/DC. No Go tool measures it, so it could only be checked by hand in review. It may be revisited later if a tool appears.

The rule instead: a decision-heavy function gets a **table-driven test that covers each branch outcome** (read the coverage profile to see which branches ran), plus a **fuzz property** where the input space is large. Name each row after the outcome it proves. The list is kept short; add to it only by PR.

- `search.Searcher.Search`: the embedder-fallback and mode decision (§4.3) and mode validation. `ClampK`.
- `search.SanitizeFTS` field rules (`ftsBaseRune`, operator words, `*`, `-`). Large input space: also `FuzzSanitizeFTS`.
- `cli.runFeedback`: verdict count, `--last` positional split, none/rank rule.
- `store.EnsureEmbedMeta` and the `reindex` mismatch path (model, dim).
- v2: root discovery, config precedence (flag > `.kb/config.toml` > default), cross-store mode selection, token-budget cut-off, feedback-prior application.

### 4.3 Worked example: embedder-failure decision in `Search`

The code (`internal/search/search.go`):

```go
if !s.unavailable(err) || mode == ModeVec { return nil, err }   // else: degrade to fts-fallback
// unavailable(err) = errors.Is(err, ErrEmbedderUnavailable) || (s.IsUnavailable != nil && s.IsUnavailable(err))
```

The branch outcomes to cover:

- fallback: the error wraps `search.ErrEmbedderUnavailable` (a nil `Embedder` produces this too) in a non-`vec` mode
- error: the same error in `vec` mode
- fallback: the `s.IsUnavailable` hook says yes
- error: the hook says no
- error: there is no hook and the error is not `ErrEmbedderUnavailable`

Note: `embed.ErrUnavailable` is **not** `ErrEmbedderUnavailable`. It counts only through the hook. That is how the CLI wires it today (`internal/cli/search.go`), and a table that used it as the sentinel would test the wrong thing. In v2 both sentinels become `errs.KindOf(err) == errs.Unavailable` (CODING_STYLE.md §1.5). The decision then has fewer outcomes (the first, second and fifth rows).

```go
// TestSearchEmbedFailureDecision replaces the three tests that cover this decision today:
// TestSearchHybridFallsBackWithoutEmbedder, TestSearchVecWithoutEmbedderIsAnError and
// TestSearchFallbackViaIsUnavailable.
func TestSearchEmbedFailureDecision(t *testing.T) {
	wrapped := fmt.Errorf("dial: %w", ErrEmbedderUnavailable)
	hook := func(err error) bool { return errors.Is(err, errServiceDown) }
	for _, tc := range []struct {
		name     string
		err      error
		hook     func(error) bool
		mode     string
		wantMode string // "" = Search returns an error
	}{
		{"sentinel, hybrid: fallback", wrapped, nil, ModeHybrid, ModeFTSFallback},
		{"sentinel, vec: error", wrapped, nil, ModeVec, ""},
		{"hook says yes: fallback", errServiceDown, hook, ModeHybrid, ModeFTSFallback},
		{"hook says no: error", errBoom, hook, ModeHybrid, ""},
		{"no hook: error", errServiceDown, nil, ModeHybrid, ""},
	} { /* searcher with an embedder whose Embed returns tc.err; check mode, err, FallbackErr */ }
}
```

Second example, `runFeedback`. Its guards say "error unless exactly one of none and rank is set". The table covers all four (none, rank) combinations: (T,T) error, (T,F) ok, (F,T) ok, (F,F) error. The verdict-count check adds rows for 0, 1 and 2 verdicts. All of these go in one table in `feedback_test.go`, `TestFeedbackArgValidation`.

### 4.4 Recommended tools, thresholds and gates

| What | Tool | Target | Gate |
|---|---|---|---|
| Statement, per package | `go test -cover` profile + `go-test-coverage` | package ≥ 70%, total ≥ 80% (starting values: measure once, then set the floor to just below current) | CI, every PR |
| Decision-heavy functions (§4.2) | table test per branch outcome + fuzz property where the input space is large | every branch of every §4.2 function runs (read the profile) | code review |
| Excluded | `cmd/`, `internal/embed/fake`, `*_test.go` helpers | | |

**Measure each package with its own tests (no `-coverpkg`).** This is what enforces "lowest layer wins". With `-coverpkg=./...`, a CLI integration test that runs `search` code would count as coverage of `search`, and a missing unit test would go unnoticed. A cross-package total (`-coverpkg=./internal/...`) is fine as information. It is never a gate.

Pin the tool as a module tool (Go 1.24+ `tool` directive), so the version lives in `go.mod` with no `@latest` and no separate install step. Adding them is an owner decision (§11):

```sh
go get -tool github.com/vladopajic/go-test-coverage/v2@v2.21.0
```

`.testcoverage.yml`:

```yaml
profile: cover.out
threshold:
  package: 70
  total: 80
exclude:
  paths:
    - ^github.com/nickstrad/kb/cmd/
    - ^github.com/nickstrad/kb/internal/embed/fake
    - ^github.com/nickstrad/kb/internal/embed/embedtest
```

## 5. Property-based testing with the native fuzzer

No third-party property-based testing library. `testing.F` gives corpus management, coverage guidance and crasher persistence, and kb's fuzz targets already use it. Add a library only if a property needs structured generators that byte mutation cannot reach cheaply, and justify it in the PR.

### 5.1 Writing a property

`f.Fuzz` takes `string`, `[]byte`, integer, float and `bool` arguments. Decode structure inside the target: split on a byte, take `% n`, or read pairs of bytes as (id, rank). Kinds of property, strongest first:

- **Oracle**: compare against an independent judge (real FTS5 `MATCH`; an in-memory map for the store model). Do not write a reference that re-implements the same algorithm. It shares the bugs.
- **Metamorphic**: change the input in a known way and predict how the output changes.
- **Round trip / idempotence**: `Load(Save(c)) == c`; `f(f(x)) == f(x)`.
- **Determinism**: same input, same output, across two runs.
- **No panic**: just call the code. The L1 assertions are the oracle, so every fuzz run checks every invariant in the code it reaches.

```go
// FuzzFuse: a decoder turns bytes into two duplicate-free ranked lists of chunk ids.
func FuzzFuse(f *testing.F) {
	f.Add([]byte{1, 2, 3}, []byte{3, 2, 1})
	f.Fuzz(func(t *testing.T, a, b []byte) {
		fts, vec := ftsRows(a), vecRows(b)
		got := Fuse(fts, vec) // L1 asserts order, uniqueness and the limit; not re-checked here
		// Metamorphic: with one list empty, the fused order is that list's order.
		if len(vec) == 0 && !sameIDs(got, ftsIDs(fts, CandidateLimit)) {
			t.Fatalf("Fuse(fts, nil) reordered fts")
		}
		// Dominance: a chunk in both lists outranks a chunk in one list at a rank
		// no better than the first chunk's best rank.
		checkDominance(t, got)
	})
}
```

### 5.2 Properties to implement

| Package | Property | Kind | Status |
|---|---|---|---|
| entry | `ParseFrontMatter`: no fence means zero front matter and the body unchanged (minus a BOM); an unclosed fence returns the whole source; a nil error means title and summary are set and one line each | oracle (spec) | `FuzzParseFrontMatter` exists |
| chunk | `Split`: chunk 0 is `title: summary`; each later chunk starts with `title › heading` and has a non-blank body; finishes within 2 s | spec, liveness | `FuzzSplit` exists; Ord/hash checks move to L1 |
| chunk | Appending `"\n\n## New\n\nbody\n"` to a body with no open code fence leaves every earlier chunk unchanged and adds chunks only at the end | metamorphic | new |
| chunk | Content preservation: every non-blank, non-heading line of the stripped body appears in some chunk's text | oracle | new; check the exact rule against `splitOversized` first |
| search | `SanitizeFTS`: both outputs parse in a real FTS5 `MATCH`; every phrase has at least one token; for valid UTF-8, the query's own text as a document is found by the AND query | oracle (SQLite) | `FuzzSanitizeFTS` exists |
| search | `Fuse`: with one list empty, the order equals that list's order (truncated); dominance (above); a list of weight 0 adds no score; doubling both weights doubles every score and keeps the order; a larger `rrf_k` never raises a score | metamorphic | `FuzzFuse` exists (internal/search/fuzz_test.go); "moving a chunk up never moves it down" is still new |
| search | `applyCap(c, m, k, cap)` is a prefix of `applyCap(c, m, k+1, cap)`; if fewer than k come back, every skipped mapped candidate's entry is already at `cap` (and is marked `CapDropped`) | metamorphic, maximality | `FuzzApplyCap` exists (internal/search/fuzz_test.go) |
| store | Model test: ops decoded from bytes (replace entry with n chunks, delete entry) applied to the store and to a `map[path][]hash`. After each op, the entries and chunk hashes read back match the map, and the three row sets agree (L1). | oracle (stateful) | new; lives in `storetest` so every backend runs it |
| v2 config | `Load(Save(c)) == c`; `Save(Load(x))` is idempotent | round trip | new |
| v2 root | From any subdirectory under a `.kb/`, discovery returns the same root; from outside, a NotFound error | metamorphic | new |
| v2 cross-store | The set of returned ids and each id's score do not depend on the order of the stores; order does not either, if the tie-break ends with (store name, chunk id) | metamorphic | new; a design requirement |

`ClampK` is a three-row unit table, not a property.

### 5.3 Practicalities

- **Expensive fixtures go outside `f.Fuzz`.** `FuzzSanitizeFTS` opens SQLite once, pins one `*sql.Conn` (temp tables are per connection) and resets its temp table on each iteration. The store model test does the same: one store per fuzz worker, cleared with `ResetForReindex` at the start of each iteration. Never open a new database per input.
- **Seeds:** use `f.Add` for readable cases. Minimised crashers go in `testdata/fuzz/<FuzzName>/` (`internal/search/testdata/fuzz/FuzzSanitizeFTS` exists). Plain `go test` replays both, so every seed is a regression test.
- **When the fuzzer finds a crasher:** fix the bug, commit the crasher file, and add a one-line comment in the target naming the bug. If a readable example helps, add it as an `f.Add` seed as well.
- **One target per run:** `-fuzz` accepts a single package, and its regex must match exactly one target. Always add `-run '^$'` so the unit tests don't run first.
- **`FuzzSplit`** needs `-fuzzminimizetime 2s`. With the default 60 s, minimising stalls on its long seeds (see the comment in `internal/chunk/fuzz_test.go`).

```sh
go test -tags fts5 ./internal/search -run '^$' -fuzz '^FuzzSanitizeFTS$' -fuzztime 60s
```

## 6. Contract tests

There is one suite per interface. Every implementation calls it from its own package's tests. The storage interface (`store.Store` = `Index` + `Retriever` + `SearchLog`, PLAN.md §2.2) arrives with the v2 seam, and the suite comes with it. Until then, `internal/store/store_test.go` holds these cases. They move into the suite unchanged.

```go
// internal/store/storetest/storetest.go
package storetest

// Factory returns a fresh, empty store for vectors of the given dim (0 = FTS only).
// It registers its own cleanup.
type Factory func(t *testing.T, dim int) store.Store

func Run(t *testing.T, newStore Factory) {
	for _, dim := range []int{0, 8} { // FTS-only and vector indexes both
		t.Run(fmt.Sprint("dim=", dim), func(t *testing.T) {
			t.Run("replace writes entry, chunks, fts and vec rows", ...)
			t.Run("replace replaces; no stale chunks", ...)
			t.Run("delete removes all rows", ...)
			t.Run("failed replace leaves the previous version intact", ...)
			t.Run("wrong-dimension vector is an error, nothing written", ...)
			t.Run("embed_meta: first write records, mismatch is a Conflict", ...)
			t.Run("FTS: groups, AND/OR ops, tag filter", ...)
			t.Run("KNN: top-k ordered by distance", ...) // skipped when dim == 0
			t.Run("SearchLog: log, feedback, LastSearch", ...)
			t.Run("model test (seeded ops)", ...)       // §5.2, fixed seed list
		})
	}
}

// internal/store/sqlite/sqlite_test.go
func TestContract(t *testing.T) { storetest.Run(t, openTemp) }
```

```go
// internal/embed/embedtest/embedtest.go
type Caps struct {
	Deterministic bool // fake, local: equal input gives equal vectors
	FTSOnly       bool // None: Dim()==0 and Embed always fails
}

func Run(t *testing.T, e embed.Embedder, caps Caps)
// Checks: Model() is non-empty; one vector per input, in order, each of length Dim();
// an empty input slice gives an empty result; a cancelled ctx returns an error that is
// not Unavailable (CODING_STYLE.md §4); equal inputs give equal vectors if Deterministic;
// FTSOnly: Dim()==0 and Embed returns an error.
```

Callers: `fake`, `None`, and `embed/local` (behind the `model` tag); until v2 deletes them, also `ollama` and `openai` against an `httptest.Server`. Behaviour specific to one implementation (HTTP status to error kind; local model-table prefixes and dims) stays in that implementation's own tests.

Rules: anything a second backend must also do goes in the suite. Behaviour specific to one backend (SQLite PRAGMAs, `Open` idempotence, migrations, concurrent `Open`) stays in that backend's package. Search-layer logic (`Fuse`, `applyCap`) does not depend on the backend and is tested once, in `internal/search`. CLI and search tests do not repeat suite cases.

### 6.1 Migration tests: fixture per version, replay, effect table

Every versioned artefact (CODING_STYLE.md §6.1) has these, in its own package. The framework itself (`internal/migrate`: `Steps.Check`, `Run`'s lock, backup, apply and verify decision table) is tested once, against a fake artefact; adapters do not repeat it.

1. **Fixtures.** One SQL text fixture per version (`testdata/migrations/<artefact>-v<N>.sql`), with realistic rows: entries, chunks, FTS, the embedding model and vectors, and at least one row in every search-log table of that version. It is generated once by the package's `-write-fixtures` test flag (steps `0..N-1` applied, rows inserted, dumped as `quote()` literals) and checked in as text, never binary. **A fixture is never edited or regenerated**: it freezes what an older kb wrote, so an edit to an old step fails the fixture test instead of passing silently. A new step adds the next fixture.
2. **Replay (one per artefact):**
   - building the artefact one version at a time gives the same normalised schema and `user_version` as creating it fresh at the target;
   - every fixture matches the steps' schema at its own version (`Verify`), and upgraded to the target it passes `Verify` with every search-log row preserved;
   - every new step must keep both green.
3. **Per step:** the fixture at `From`, upgraded by that step alone, shows the step's effect (columns renamed, tables cleared, `body_hash` emptied) and keeps the search log (renamed columns compared under their new names). A step without an effect check fails the test.
4. **Effect table:** a test lists `(step ID, Effect)` for every step, so adding a step or changing an effect is a reviewed diff.
5. **Interruption:** a cancel during the last pending step leaves the starting version and schema (one transaction), and a rerun succeeds. A driver fault at each statement of the upgrade does the same (fault driver).

## 7. Test doubles and determinism

- **Embedder:** `internal/embed/fake` (hash-based, deterministic, honours `ctx`) is the default. When a test needs an embedder failure, it wraps `fake` in a stub whose `Embed` returns the chosen error (§4.3).
- **Store:** real SQLite in `t.TempDir()`. Never mock the database in store, search or reindex tests, because FTS5 and sqlite-vec behaviour is the thing under test.
- **HTTP embedders** (until v2 deletes them): `httptest.Server`, never a real endpoint.
- **Real models (`embed/local`, rembed):** these tests carry `//go:build model`, not a `testing.Short` or "skip if not cached" gate. Default runs never download a model. The `model` job fetches it in an explicit step before the tests run. Under the tag, a missing model is `t.Fatal`, not a skip, so the job cannot pass vacuously.
- **Time:** assert durations (`EmbedMs`, `TotalMs`) are `>= 0`, never exact. No `time.Sleep`. Synchronise with channels or `ctx`.
- **Randomness:** seed explicitly and print the seed on failure. Map iteration order must never reach output (the total order in `Fuse` guarantees this for search).
- **No host tools: a defect in today's tests.** `internal/cli/p44_test.go` and `internal/cli/p42_regression_test.go` set `EDITOR` to `sed -i 's/…/…/'`. That is GNU syntax. macOS `/usr/bin/sed` reads the script as a backup suffix and fails. The result on macOS today:
  - `TestMutationCommandsLeaveOptionalIndexAlone` **fails**.
  - `TestEditModelMismatchUsesEditPrefix` **passes vacuously**: it expects exit 1 with a `kb edit:` prefix, and the sed failure produces exactly that.

  The target fix is the injectable `editor func(ctx, path) error` in `deps` (CODING_STYLE.md §5): tests pass a Go function that rewrites the file. PLAN.md's interim fix (a generated `sh` script using POSIX `sed` without `-i`) works on macOS, and is acceptable only until `deps` lands. Either way, the mismatch test must also check that the file actually changed.
- **Env vars:** v2 reads none. Until then, tests that need one use `t.Setenv`. Note that `t.Setenv` and `t.Parallel` cannot be combined. Once config arrives through `deps`, these tests can run in parallel.

## 8. Build tags

| Tag | Meaning | Used by |
|---|---|---|
| `fts5` | required by mattn/go-sqlite3 for FTS5 | every build and test |
| `noassert` | compile assertions out | `make test-noassert` (every PR), `make build-noassert` (profiling only) |
| `model` | tests that load a real rembed model | `make test-model`, nightly and local |

Combine tags with commas: `-tags fts5,noassert`. There is no `integration` tag. Integration tests are ordinary fast tests. Add the tag only if the suite grows past about 60 s.

## 9. Commands, Makefile, CI

```make
TAGS    := fts5
PKGS    := ./...

test:          ; go test -tags $(TAGS) $(PKGS)
test-noassert: ; go test -tags $(TAGS),noassert $(PKGS)
race:          ; go test -tags $(TAGS) -race $(PKGS)
vet:           ; go vet -tags $(TAGS) $(PKGS)

cover:
	go test -tags $(TAGS) -covermode=atomic -coverprofile=cover.out $(PKGS)
	go tool github.com/vladopajic/go-test-coverage/v2 --config=.testcoverage.yml

fuzz:   # time-boxed; FUZZTIME=2m in CI
	go test -tags $(TAGS) ./internal/search -run '^$$' -fuzz '^FuzzSanitizeFTS$$'     -fuzztime $(FUZZTIME)
	go test -tags $(TAGS) ./internal/entry  -run '^$$' -fuzz '^FuzzParseFrontMatter$$' -fuzztime $(FUZZTIME)
	go test -tags $(TAGS) ./internal/chunk  -run '^$$' -fuzz '^FuzzSplit$$'            -fuzztime $(FUZZTIME) -fuzzminimizetime 2s

golden-update: ; go test -tags $(TAGS) ./internal/chunk -run '^TestSplitGolden$$' -update
test-model:    ; go test -tags $(TAGS),model ./internal/embed/local/...
build-noassert:; go build -tags $(TAGS),noassert -o bin/kb-noassert ./cmd/kb

check: vet test test-noassert race cover
FUZZTIME ?= 30s
```

`-update` is a flag that each test package defines. Today only `internal/chunk` defines it (`chunk_golden_test.go`). `go test` rejects a flag that a package does not define, so pass `-update` only to packages that define it. When CLI golden tests arrive, give each its own line in `golden-update`. Always review the golden diff before committing it.

**CI** (one `macos-latest` runner, arm64; Go version from `go.mod`):

- **Every PR:** `make check`. Seeds and crashers replay inside `make test`.
- **Nightly:** `make fuzz FUZZTIME=2m`. On failure, upload the new `testdata/fuzz/` files as an artifact. Also nightly: `make test-model`.
- **Release** (not testing): each release target must build with `-tags fts5`. Cross-compilation with cgo is a release-pipeline concern, and it does not run tests.

## 10. Where does my new test go?

Work down the list and stop at the first yes.

1. **Can the function check it locally, from its own inputs, output and state?** Add an **assertion** (§3.5). Write no test for that claim; make sure some test or fuzz target reaches the code.
2. **Is it true for all inputs, but only something outside the function can judge it** (SQLite, a second run, a round trip, a model)? A **property** in the package's fuzz target (§5.2). Specific nasty inputs become `f.Add` seeds, not new tests.
3. **Is it one decision for chosen inputs** (a branch, validation, mode, error kind), a **boundary** (§2.1), or a line the fuzz targets don't reach? A **unit/table test** in the package that owns it. On the §4.2 list? Cover every branch outcome in the table.
4. **Is it exact bytes others depend on?** **Golden.**
5. **Must every backend or embedder do it?** **Contract suite** (§6).
6. **Is it only that a command wires flags to a service and maps the result to output and exit code?** **Integration**: at most one success and one failure per command.
7. **Already owned by a lower layer?** Stop. If the lower-layer test is missing, add it there.

Bug fixes: put the regression at the layer that owns the root cause. If the fuzzer found the bug, commit the crasher. Any test that needs the network, a model, a host tool or a sleep: redesign it (a double, injection, `t.TempDir()`). If it truly needs a real model, tag it `model`.

### 10.1 Checking the tests themselves

Mutation testing (do the tests fail when the code is wrong?) and the hostile CLI harness
(cross-process, corrupt-database, no-network and path attacks) are in
[ADVERSARIAL_TESTING.md](ADVERSARIAL_TESTING.md). They are a check on these layers, not a new
layer. A survivor or a failed scenario becomes a test at the layer this section picks.

## 11. Open decisions for the owner

- Add `go-test-coverage` as a `tool` dependency in `go.mod` (§4.4).
- The exact coverage floors, once measured.
- Recovering assertion panics at the top level for exit code 3 (§3.1). This changes the "known conflict" note in CODING_STYLE.md §1.5.

Defaults for these have been picked and are tracked in the local, unmerged `DEFAULTS.md` (rows 16–22); revisit there.

- MC/DC revisit: see .agent/knowledge/go-no-mcdc-tool.md
