# kb coding style

Audience: contributors to kb, people and agents alike. Read time: about 10 minutes. These rules cover today's code and the v2 rework: the in-process embedder (rembed), the storage-backend interface, root discovery with `.kb/config.toml`, no env vars, and the new commands (`init`, `eval`, `explain`, `context`, `related`, `stale`).

Testing, assertions, fuzzing and build tags are covered in [TESTING.md](TESTING.md). This guide does not repeat them; it says where they meet the code style. §12 explains how to shape code so that those tests are cheap to write: domain code separated from infrastructure, injected inputs and observable results.

---

## 1. Error handling

kb's approach comes from Katherine Cox-Buday, *Concurrency in Go* (O'Reilly, 2017): chapter 5, "Concurrency at Scale", section "Error Propagation". The `Result` pattern in 1.6 comes from the same book, chapter 4, "Concurrency Patterns in Go", section "Error Handling". The text below paraphrases the book; it does not quote it. Each subsection marks what comes from **the book** and what is a **kb adaptation**.

### 1.1 Two kinds of error (book)

An error that reaches the top of the program is one of two kinds:

- A **known edge case**: something we expected could happen and planned for. Examples: a network connection drops, a disk write fails, a user gives a bad argument. These are **well-formed** errors.
- A **bug**: anything else. If an error reaches a module boundary and nobody has turned it into a well-formed error, that is a bug, even when the underlying cause was ordinary. The bug is that no code thought about it.

### 1.2 What a well-formed error carries (book)

1. **What happened**: for example "disk full" or "connection refused".
2. **When and where it happened**: the call stack, the time, and the context the reader needs, such as which machine. In kb the context is the entry path, the search id, the database path or the model name.
3. **A friendly message for the user**: short and free of internal detail.
4. **How the user can get more information**: a pointer to the full detail, for example an ID that matches a log line.

### 1.3 Wrap at module boundaries (book)

The book's example wraps errors in a `MyError` struct with `Inner error`, `Message string`, `StackTrace string` and `Misc map[string]interface{}`, built by a `wrapError(err, format, args...)` helper that records `debug.Stack()`. Each module (the book's examples are `lowlevel` and `intermediate`) defines its own error type around that struct. When an error crosses **out of** a module, the module wraps it in its own type and gives it a message that makes sense at that level. Inside a module, functions return errors as they are, or add context, and do not re-wrap at every call. Lower-level detail does not leak upward unexplained, but it is still there in `Inner` for debugging.

### 1.4 The top level decides (book)

The top of the program checks whether an error is of a known, well-formed type.

- If it is well-formed, show the user its friendly message.
- If it is not, it is a bug. Log the full error (with the stack trace) somewhere the user and developer can find it later. Show the user a generic message ("unexpected error, please file a bug") with the log ID.

### 1.5 The kb version (kb adaptation)

The book was written before Go 1.13. kb uses the newer tools: `%w`, `errors.Is`/`errors.As`, `errors.Join`, `runtime/debug.Stack`. kb also uses **one** error type with a `Kind` field instead of one struct per module. The module name goes in `Op`. A module boundary is any place that calls `errs.Wrap`.

```go
// Package errs is kb's well-formed error: a known edge case with a user message.
package errs

type Kind int

const (
	Internal    Kind = iota // zero value: a bug if it reaches the top level
	Usage                   // bad flag, argument or config value: exit 1
	NotFound                // no index, no entry, no search id: exit 1
	Conflict                // embed_meta mismatch, stale index: exit 1
	Unavailable             // embedder cannot be reached or loaded: exit 2
)

type Error struct {
	Kind  Kind
	Op    string         // "store.ReplaceEntryChunks", "embed.Embed", "search.Search"
	Msg   string         // friendly, one line, no driver/HTTP/SQL detail
	Ctx   map[string]any // path, search_id, model, db: what the reader needs
	Err   error          // inner cause; may be nil
	Time  time.Time
	Stack []byte         // debug.Stack() at the first wrap only
}

func (e *Error) Error() string { return e.Msg } // users see Msg; the log sees everything
func (e *Error) Unwrap() error { return e.Err }

// Wrap makes err well-formed. It keeps the stack from an inner *Error
// rather than capturing a second one.
func Wrap(err error, k Kind, op, format string, args ...any) *Error

func KindOf(err error) Kind // the first *Error in the chain; Internal if none
```

**Where the boundaries are:**

| Module | Wraps | Typical kinds |
|---|---|---|
| `config` (v2: root discovery, `.kb/config.toml`) | file and TOML errors, unknown keys | Usage, NotFound |
| `entry` | front-matter and path errors from files the user wrote | Usage (with `path` and line) |
| `store` (and each backend under it) | driver errors, `sql.ErrNoRows`, migrations, embed_meta | NotFound, Conflict; driver failures stay Internal |
| `embed` (rembed, ollama, openai) | HTTP status, model load, wrong vector count/dimension | Unavailable, Usage (bad model name) |
| `search`, `reindex`, `stats`, `eval` | errors from store and embed, adding the search id or entry path | keep the inner kind, add context |
| `cli` | **top level**: does not wrap; it reports | n/a |

**Rules:**

- Inside a module, add context with `fmt.Errorf("reindex %s: %w", e.Path, err)` and always use `%w`, not `%v`, so the chain stays intact.
- At a boundary, return an `*errs.Error`. Never return a raw driver, `os` or `net/http` error to another kb package.
- Branch on `errs.KindOf(err)` or `errors.As`, never on the error string. `trimSearchErrorPrefix` in `internal/cli/search.go` is the kind of code this rule removes.
- Keep a sentinel only when a caller in another package must branch on that exact condition and a `Kind` is not specific enough. `search.ErrLogFailed` stays: it marks partial success (see below). `embed.ErrUnavailable` and `search.ErrEmbedderUnavailable` become `Kind == Unavailable`.
- Typed errors that carry data, such as `store.EmbedMetaMismatchError`, stay. They become the `Err` inside an `*errs.Error{Kind: Conflict}`, and the CLI uses `errors.As` to reach their fields.
- Use `errors.Join` when several independent failures must all be reported, for example `Close` after a failed write, or several entries failing in `reindex --all`.
- **Partial success is not an error to hide.** `SearchAndLog` returns `(out, err)` with both non-nil when the search ran but the log write failed. The caller prints the results and a warning on stderr. Document such functions in their doc comment.
- Never discard an error with `_ =` without a comment saying why it is safe. `defer rows.Close()` is fine. On write paths, `defer tx.Rollback()` after a commit is the accepted idiom.

**The top level (`internal/cli/root.go`, `report`):**

```go
func report(stderr io.Writer, logw io.Writer, debug bool, err error) int {
	var e *errs.Error
	if errors.As(err, &e) && e.Kind != errs.Internal {
		fmt.Fprintf(stderr, "kb: %s\n", e.Msg)
		if debug {
			fmt.Fprintf(stderr, "%+v\n", e) // Op, Ctx, inner chain, stack
		}
		return exitCode(e.Kind) // Usage/NotFound/Conflict -> 1, Unavailable -> 2
	}
	id := newIncidentID() // 8 hex chars
	writeIncident(logw, id, err) // time, version, args, full chain, stack
	fmt.Fprintf(stderr, "kb: internal error (id %s). Details: .kb/kb.log, or rerun with --debug. Please report it.\n", id)
	return ExitInternal
}
```

- **Where the detail goes:** kb appends one JSON line per incident to `<root>/.kb/kb.log`. If no root was found, it uses `os.UserCacheDir()/kb/kb.log`. Each line holds the time, `Version`, args, the full chain and the stack. Once the file is over 1 MiB, trim it on write to the last 1 MiB. `--debug` (a root persistent flag) also prints the full detail to stderr for any error. Nothing else writes to `kb.log`.
- **Exit codes:** `0` OK, `1` well-formed failure (usage, not found, conflict), `2` embedder unavailable (including a hybrid search that fell back to FTS), and **`3` internal error (today bugs exit 1)**. `usageErr` and `embedderErr` become thin helpers that build `*errs.Error` with kinds `Usage` and `Unavailable`. The exit codes are part of the CLI contract. Changing one means changing the README and the skill docs too.
- **Panics exit 3, not 2.** An unrecovered Go panic exits with status 2, the same as `ExitEmbedder`. To keep the codes distinct, the top level (`cli.Execute`, around the command run) has the codebase's **only** `recover`. It catches any panic, whether an `assert.Violation` or a runtime panic such as a nil dereference. It logs the panic as an `Internal` incident with its stack and exits 3. It never continues running the command. See TESTING.md §3.1. Panics in goroutines other than the command's own are not caught there. That is one more reason workers return errors (§1.6) instead of panicking. A worker that may still panic (an assertion in shared code, such as each store's search in `search.fanOut`) hands it over: its deferred `recover` stores `errs.WorkerPanic{Value, Stack}` and the starting goroutine raises it again after the workers finish; `reportPanic` unwraps it. That hand-over is the only other `recover`, and it never lets the command continue.

### 1.6 Concurrency: return errors with results (book, chapter 4)

The book's rule is that a goroutine should not decide what to do with an error it cannot fully handle. It should not log the error and carry on, print it, or exit. It sends the error, coupled with the result, back to the part of the program that has enough state to decide. The book's example is a `Result` struct with an `Error` field and a `*http.Response`, sent over a channel.

kb adaptation, with generics:

```go
type Result[T any] struct {
	Value T
	Err   error
}
```

- **Batch embedding (reindex).** Batches run in order today. If they are parallelised, each worker sends `Result[batchVecs]{Value, Err}` tagged with its batch index. The reindex loop decides whether one failure aborts the entry (it does: an entry is written all-or-nothing) and wraps it with the entry path. When the first error should cancel everything, use `errgroup.WithContext` (`golang.org/x/sync`, added when first needed). It is the same idea packaged.
- **Cross-store fan-out search (v2).** Each store's goroutine returns `Result[[]Hit]` with the store name in the error context. The coordinator decides: if all stores fail, return the error. If some fail, return the hits from the stores that worked and attach a warning, as `ErrLogFailed` does today. A worker never decides that a store "doesn't matter".
- Workers never call `os.Exit`, never write to stdout or stderr, never `recover`, and always honour `ctx.Done()`.

### 1.7 Errors vs assertions

Assertions (`internal/assert`, TESTING.md §3) check preconditions and invariants that only a kb programmer can break. A failed assertion **is a bug by definition**: it panics and is never converted into an `error`. Anything the user can influence (flags, config, files, database contents, embedder output) returns a well-formed error instead. Validate untrusted input once at the boundary and return an error; downstream code then asserts the validated result. The top level turns any panic into an incident and exit 3 (§1.5).

### 1.8 Before and after

Before (`internal/cli/search.go`, abridged). The CLI rebuilds the classification by sentinel checks and string trimming, and flattens the chain into a string:

```go
out, err := searcher.SearchAndLog(cmd.Context(), req)
if err != nil && out == nil {
	if errors.Is(err, search.ErrEmbedderUnavailable) || (isUnavailable != nil && isUnavailable(err)) {
		return embedderErr("%s", err)
	}
	return usageErr("kb search: %s", trimSearchErrorPrefix(err.Error()))
}
```

After. `search` wraps at its boundary and keeps the embedder's kind. The CLI passes the error up, and `report` chooses the message and the exit code:

```go
// internal/search
vecs, err := s.Embedder.Embed(ctx, []string{q})
if err != nil {
	return nil, errs.Wrap(err, errs.KindOf(err), "search.Search",
		"could not embed the query").With("search_id", id, "mode", req.Mode)
}

// internal/cli/search.go
out, err := searcher.SearchAndLog(cmd.Context(), req)
if err != nil && !errors.Is(err, search.ErrLogFailed) {
	return err
}
```

---

## 2. Package layout and dependencies

- Dependencies point one way: `cmd/kb → cli → services (search, reindex, index, stats, eval) → store, embed, entry, chunk`. **No package imports `cli`.** Leaf packages (`chunk`, `entry`) import no other kb package except `errs` and `assert`.
- **The consumer defines the interface, and keeps it small.** `search` declares the two or three store methods it calls. It does not import a wide `store.Store` interface. `embed.Embedder` (`Embed`, `Model`, `Dim`) is the right size.
- Backend seam (v2): `internal/store` holds the backend-neutral types and the contract test suite (TESTING.md §6). Each backend lives in its own subpackage (`internal/store/sqlite`). Nothing outside the backend imports a driver.
- `internal/cli` has one file per command. A command file parses flags, builds dependencies from config, calls one service function and prints. Logic that a test would want to call without cobra belongs in a service package, or at least in a pure function next to the handler (§12.1).
- Do not create `util` or `common` packages. Put each helper next to the code that uses it.

## 3. Naming

- Packages are short lower-case nouns (`store`, `embed`, `search`). Don't stutter: write `store.Open`, not `store.OpenStore`.
- Use full words for exported names. Short names are fine in small scopes (`e` for an entry, `st` for a store, `ctx`, `err`, `i`).
- Sentinels are `ErrXxx`. Error types are `XxxError`. Constructors are `NewXxx` only when `Xxx{}` is not usable.
- Name flags and JSON fields in `snake_case` in output and kebab-case on the command line (`--embed-model`, `search_id`). Once released, a name does not change (see §9).

## 4. Context and cancellation

- `ctx context.Context` is the first parameter of every function that does I/O, embeds, queries or may block. Never store a context in a struct.
- `cmd/kb` wires SIGINT and SIGTERM to the root context, and handlers read `cmd.Context()`. Do not create `context.Background()` below `main` (tests excepted).
- Use the `*Context` variants of `database/sql` methods (`QueryContext`, `ExecContext`, `BeginTx`).
- Keep a caller's cancellation distinct from a timeout. A cancelled `ctx` returns `ctx.Err()` (wrapped). It does not become `Unavailable`, because neither retrying nor falling back would help. A deadline the client set itself is `Unavailable`. The ollama client already makes this distinction; keep it.

## 5. State, seams and configuration

The general rules for what to inject and how are in §12.2 and §12.4. This section covers kb's specific seams.

- **No package-level mutable state.** The allowed exceptions are build-stamped values that never change after start-up (`Version`), `sync.Once` for driver registration (`vecOnce`), and immutable lookup tables.
- `var newEmbedder = func(...)` in `internal/cli/setup.go` is a test seam built from mutable global state, and it goes. Pass dependencies explicitly instead:

```go
type deps struct {
	cfg      config.Config
	embedder func(config.Embed) (embed.Embedder, error)
	openStore func(ctx context.Context, path string) (store.Backend, error)
	editor   func(ctx context.Context, path string) error
}

func Execute(ctx context.Context, args []string, stdout, stderr io.Writer) int // production deps
func execute(ctx context.Context, d deps, args []string, stdout, stderr io.Writer) int // tests
```

  Commands receive `d` from `newRootCmd`. Tests build their own `deps` with `internal/embed/fake`.
- **Configuration (v2): no env vars.** `config.Load(ctx, startDir)` discovers the root by walking up from `startDir` to the nearest `.kb/`, parses `.kb/config.toml`, applies flags on top, validates once and returns a plain `Config` struct. Everything below `cli` takes the parts of `Config` it needs as arguments and never reads files, env vars or flags itself. Unknown keys in `config.toml` are a `Usage` error, not silently ignored.

## 6. SQL

- Always pass values as parameters (`?`). Never use `fmt.Sprintf` to build SQL from values. Identifiers built from a fixed allow-list (for example a sort column) are the only exception, and they need a comment.
- FTS5 `MATCH` strings go through the existing sanitizer (`search.SanitizeFTS`). It is a fuzz target; keep it that way.
- Writes that must stay consistent (an entry and its chunks and vectors; a search and its results and candidates) happen in **one transaction**. Use `defer tx.Rollback()` immediately after `BeginTx`.
- Migrations follow §6.1. The schema version lives in the database (`PRAGMA user_version`).
- Backend-specific SQL, PRAGMAs and extension loading (`vec0`, `fts5`) stay inside the backend package. A future DuckDB or libSQL backend follows the same rule.
- Store per-connection settings in the DSN (as `store.Open` does with `_foreign_keys`), not in a one-off `Exec`.

### 6.1 Migrations

Every versioned file (the store index DB, the embedding cache, later the store config and location file) lists its steps as a `migrate.Steps[T]` and implements `migrate.Artefact` in a small adapter (`internal/migrate`, design `docs/design/migrations.md`).

1. **Append-only.** Never edit or reorder a released step or `schema.sql`. A fix is a new step.
2. Every step has a stable `ID` (`<artefact>/<NNNN>-<slug>`, NNNN = `From`+1), a one-line `Desc` and an `Effect` (`none`, `reindex`, `reembed`). `Steps.Check` enforces the shape; each artefact calls it in a test and an init-time assertion.
3. A step uses only its handle: SQL text, or the map tree. It never calls kb's current Go code (for example `chunk.Split`), because a later change to that code would silently change an old migration. It never begins or commits a transaction, and never touches the network or the model.
4. **Every search-log table is preserved.** A step that alters one has a test that compares every row before and after.
5. Work that needs the model or the corpus never runs in a step: the step clears the derived state and declares `EffectReindex` or `EffectReembed`, honestly. A reviewer checks every step that drops or clears derived data against its declared effect.
6. Migration numbers are pre-assigned (PLAN §6.3). No agent adds a step that is not in that table.
7. An adapter states whether `Apply` is atomic (SQLite: every pending step and the version stamp in one transaction) or stamps after each step so a rerun resumes. On a pinned `*sql.Conn`, check `ctx.Err()` before `COMMIT`: neither database/sql nor the driver refuses a statement on an already-cancelled ctx.

## 7. Comments

- A doc comment goes on every exported symbol, plus a short `doc.go` per package listing its inputs, outputs and invariants (TESTING.md §2, L0). A doc comment says **what** the symbol does and **why** a reader needs to know: the contract, the edge cases, the errors it returns and their kinds.
- Keep comments short. Two or three lines is normal. Most of today's comments run to six or more lines; when you touch one, cut it down.
- Comments do not mention **phases, plan items or plan files** (`P4.2`, `plan.md`, "this skeleton"). Remove them when you touch the code. Tests do not get named after plan items either: `p42_test.go` becomes `edit_test.go`.
- Do not narrate history ("previously this did X", "changed to fix Y"). That belongs in git. A comment may explain a non-obvious constraint that still holds, for example why `journal_mode` is not set in the DSN.
- Write `// TODO(name): ...` only with an owner, and never in place of an error path.

## 8. Logging

- kb is a CLI and does not log during normal operation. Results go to stdout and diagnostics to stderr (§9).
- The only log is the incident log (`.kb/kb.log`, §1.5), written by `cli.report`. Library packages never log. They return errors, or return warnings as values for the CLI to print.
- `--debug` turns on extra stderr detail. Plumb it as an `io.Writer` (or `*slog.Logger`) in `deps`, which is `io.Discard` when the flag is off. Never use a global logger.
- The `searches`, `search_results` and `search_feedback` tables are product data, not logs. They follow §6.

## 9. CLI output

- **stdout** carries results only: hits, entry text, stats, JSON. **stderr** carries warnings, progress and errors. A pipe of stdout never contains a warning.
- `--json` is a **stable contract**. You may add fields. You may not rename, remove or change the type of a field without bumping the documented format. The golden tests (TESTING.md, L4) enforce this.
- Human output may change, but keep the first line of `kb search` (`search N · M hits`) because agents parse it.
- Error messages start with `kb:` (the command name may follow), are one line, and say what to do next when a next step exists ("run `kb reindex --all`"). They never contain SQL, stack traces or HTTP bodies unless `--debug` is set.
- Exit codes are listed in §1.5.

## 10. Dependencies

- Keep few dependencies, and justify each new one in the commit message: what it replaces and why the standard library is not enough.
- Use cgo only where it cannot be avoided: SQLite with FTS5 (`mattn/go-sqlite3`) and sqlite-vec. The rembed embedder is pure Go. Do not add cgo for convenience.
- Use the `fts5` build tag everywhere (TESTING.md §8). Do not add new build tags without adding them to that table.
- Pin versions in `go.mod`. Run `go mod tidy` before committing.
- Development and testing happen on macOS. Other platforms get release builds only, and there are no extra cross-platform rules. Normal Go hygiene still applies: build paths with `path/filepath`, and do work in Go rather than shelling out to host tools (`sed`, `git` config, and so on).

## 11. Formatting and linting

Run these before every commit; CI runs the same:

```sh
gofmt -l .                                   # must print nothing (or: go fmt ./...)
go vet -tags fts5 ./...
go tool staticcheck -tags fts5 ./...         # staticcheck pinned via a `tool` directive in go.mod
go test -tags fts5 ./...                     # plus the other TESTING.md §9 targets
```

- Use `staticcheck` with its default checks. Silence a finding only with a `//lint:ignore CHECK reason` comment on that line.
- Keep lines around 100 columns where that is easy. Use no other formatter than `gofmt`.

## 12. Designing for testability

The ideas come from Maurício Aniche, *Effective Software Testing* (Manning, 2022), chapter 7, "Designing for testability". Section numbers in brackets point to that chapter. The rules below are the kb version: Go packages and functions instead of classes, interfaces declared by the consumer (§2), and plain values passed as arguments. TESTING.md decides which layer owns a test. This section shapes the code so that the test is cheap to write.

### 12.1 Separate domain code from infrastructure code (7.1)

- **Domain code decides.** Examples: ranking, fusion and the per-entry cap, the feedback prior, edge resolution, mode and fallback choice, gating (`--k`, token budgets, staleness), parsing and validation, metric arithmetic, and the output models (`search.Result`, `staleReport`).
- **Infrastructure code touches the world.** Examples: SQLite, the filesystem, the clock, env vars and the cwd, the model (load, download), stdout and stderr, and signals.
- **Rule: decisions live in pure functions or types over values. Adapters do the I/O and call them** ("functional core, imperative shell"). A function either decides or does I/O. When a step needs both, it reads, calls a named pure decision, then writes. A table test can call that decision directly.

| Role | kb packages |
|---|---|
| Pure core: no I/O, no clock | `chunk`, `glob`, `links`, `prior`, `render`, the `store` types and `store.EdgeResolver`, `eval` metrics and `Split`, `search` `Fuse`/`applyCap` |
| Orchestration over interfaces: no direct I/O | `search.Searcher`, `reindex` (collaborators are fields) |
| Adapters | `store/sqlite`, `embed/local`, `embed/cache`, `config.Load`, `workspace.OSEnv`, `entry` discovery, the `eval` baseline file, the `migrate` lock and backups (`migrate.Run` mixes its decision table with backup file I/O; the artefacts are fields) |
| Shell | `internal/cli` and `cmd/kb`: build `deps`, read flags and config, call the core, render, map errors to exit codes |

Good: `buildStale(now, days, entries, edges)` decides and `runStale` reads the store and the clock, then renders. `store.EdgeResolver` is the one rule, `sqlite.ResolveEdges` reads the targets and writes the ids, and `storetest` checks that every backend applies the rule. Fix when you touch the code: `runAddFile`/`runAddDir` in `cli/add.go` and the inline doctor checks (testability `add-validation-interleaved-with-copy`, `doctor-checks-print-text`).

### 12.2 Controllability: inject what varies (7.2)

- A test must be able to set every input that changes the outcome. Inject the clock, the env and cwd, filesystem roots, the store and the log, the embedder and the editor. In `cli` these are `deps` fields (`d.now`, read through `d.clock()`; `workspaceEnv`; `openStore`; `editor`). In services they are struct fields (`Searcher.Store`, `Log`, `Prior`, `Embedder`, `Now`; `migrate.Options.Now`).
- Domain code never calls `time.Now`, `os.Getenv`, `os.Getwd`, `os.UserCacheDir` or unseeded `rand`. An adapter reads them once and passes a value: `workspace.OSEnv` is the only place `workspace` reads process state, and `ResolveLocation` takes the resulting `Env`. Elapsed-time measurements (`EmbedMs`) are exempt, because tests assert only `>= 0` (TESTING.md §7).
- A seam is a field or a parameter, never a package-level var (§5). Make the code that parses outside input injectable too: `harnessClock(lookup, warn)` takes `os.LookupEnv` and `os.Stderr` as arguments, so a table test needs no `t.Setenv`.
- An adapter that can fail in the OS or the driver accepts a fault hook (sqlite `Options.driver`), so that its error paths can be tested. An adapter without a fault hook is a testability gap (`fault-seams-only-in-sqlite`).

### 12.3 Observability: return what happened (7.3)

- A decision returns a structured result that a test can compare: a typed outcome, a reason, counts, the chosen mode (`search.Result.Mode` and `FallbackErr`; `staleReport`). A separate function renders it (`renderStale`, the JSON encoder). Tests assert fields. Golden tests own the bytes (TESTING.md L4).
- Text is never the only place where a decision is visible. If a test must `strings.Contains` stderr to learn which branch ran, the function is missing a return value (`cli-outcomes-only-as-text`).
- **Effects ("void" methods, 7.3.2).** A function that writes returns what it did: an id, counts, the steps applied (`LogSearch` returns the search id, `migrate.Run` returns a `Report`). Or the port offers a narrow read that a test can use to observe the effect (`store.JudgedLog` reads the log back). Tests use that read, not raw SQL against the schema (`tests-read-sqlite-schema`).
- **Methods that help assertions (7.3.1).** Add a read method or a result field if a real caller could also use it. Do not export internals only for tests. `(*sqlite.Store).DB()` is for the backend's own tests.
- When tests must tell two failures apart, the failures differ by `errs.Kind`, by typed error or by result field, not by message (§1.5). Keep one wording test per message family.

### 12.4 Collaborator in a field, value in a parameter (7.4)

- Collaborators that do I/O or have a lifetime go in a struct field or in `deps`. Examples: the store, the log, the embedder, the editor, an opener. The shell sets them once.
- Plain data is a parameter: the instant, settings, limits, a seed, the request. Pure functions then stay pure, and each call shows all of its inputs: `prior.Build(events, params, asOf)`, `buildStale(now, days, ...)`, `eval.Split(keys, holdout, seed)`, `search.Request.AsOf`.
- The clock is a field only where code stamps a write or reads "now" (`Searcher.Now`, `deps.now`). That code reads the clock once and passes the `time.Time` down, as `loadEvidence` does for `prior.Build`. Domain code never takes a `func() time.Time`.
- A struct of values (`Request`, `Params`, `Settings`) as one parameter is fine. A collaborator hidden inside a value struct is not.

### 12.5 Cohesion, coupling and hard tests (7.5)

- **Cohesion.** One function makes one decision. A handler that validates, does I/O and renders in 100 lines is three functions.
- **Coupling.** Take the values, or the one-method interface, that the code uses. Do not take `store.Store` or the whole `Config` to read one field. A double that must implement twenty methods shows that the port is too wide.
- **Complex conditions.** Two or more `&&`/`||` terms that choose a mode, an error or a fallback become a named decision function with a table test that has one row per outcome (TESTING.md §4.2). An example is `search.unavailable` together with the mode check in `Search`.
- **Unexported code.** Tests in the same package may call unexported functions. If a test needs to reach the middle of a function, move that part into its own function. Never export something only so that a test can reach it.
- **Package state and singletons.** These are not allowed (§5). `init` may only set values that never change after start-up (`harnessNow` in a harnessfake build).
- **Ports and adapters.** Ports are consumer interfaces. Test adapters against the real dependency with a contract suite and fault tests (TESTING.md §6: real SQLite, `embed/fake`). Do not mock the database.
- **A hard test is a design signal.** Treat these as a sign that the code mixes concerns or hides an input: a long setup, raw SQL fixups, `t.Chdir`, `t.Setenv`, a sleep, a wide double, or a mutation survivor triaged "unreachable". If your item owns the files, fix the design. If not, record it in `.agent/testability/` (a one-file fix goes to the low-findings queue) and say so in your report.

Layers follow this split (TESTING.md §2.1): the pure core gets L1 to L3, adapters get L5 contracts and fault tests, and the shell gets L6, one success and one failure per command.

## 13. Review checklist

- [ ] Every error leaving a module is an `*errs.Error` with a kind, a friendly `Msg` and context (path, search id, model). Inside a module, wraps use `%w`.
- [ ] No branching on error strings. `errors.Is` and `errors.As` (or `errs.KindOf`) only.
- [ ] Unexpected failures are not dressed up as usage errors. They reach `report` as `Internal`.
- [ ] Goroutines return `Result{Value, Err}` (or use `errgroup`), honour `ctx` and never log, print or exit.
- [ ] User input causes errors, not assertions. Programmer mistakes are assertions, not errors.
- [ ] `ctx` is the first parameter for I/O, and nothing creates `context.Background()` below `main`.
- [ ] No new package-level mutable state and no env var reads. Dependencies arrive via `deps` and `Config`.
- [ ] SQL is parameterised, multi-row writes are in one transaction, migrations are append-only and backend SQL stays in the backend.
- [ ] No package imports `cli`, and interfaces are declared by the consumer.
- [ ] Decisions are pure functions over values. I/O happens in adapters and in the shell around them, never in the middle of a decision (§12.1).
- [ ] The clock, env, cwd, filesystem roots, store, log and embedder are injected. Domain code never calls `time.Now`, `os.Getenv` or `os.Getwd` (§12.2).
- [ ] Each decision returns a structured result or a typed error that a test can assert. Text is rendered from that result, and effects return what they did (§12.3).
- [ ] Collaborators are fields. Plain data (now, settings, limits, seed) is passed as a parameter (§12.4).
- [ ] A test that needed `t.Chdir`, raw SQL, a sleep or a wide double led to a design fix or to a `.agent/testability` entry (§12.5).
- [ ] stdout has only results, `--json` fields are unchanged or only added to, and exit codes match §1.5.
- [ ] Comments are short, with no phase labels, plan references or history.
- [ ] `gofmt`, `go vet` and `staticcheck` are clean, and tests sit at the layer TESTING.md assigns.
