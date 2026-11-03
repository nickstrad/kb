# Embedding provider plugin (kb v2)

kb's embedding contract and provider registry are a public Go package,
`github.com/nickstrad/kb/embed`, that any Go project can import. A program registers embedding
providers by name and picks one from configuration. kb is the first user: its CLI resolves its
embedder through the registry, with its in-process model as the default provider. A config that
names no provider behaves exactly as kb v2 did before the registry existed.

The motivating case is a machine that cannot download or run Hugging Face models. The public
package and its providers never load model weights in-process and never touch Hugging Face:
`http` calls an embeddings API, `command` runs a program the user supplies. kb's own `local`
provider (rembed, Hugging Face model ids) stays internal and is used only when selected, which is
the default.

This is a port of the first plugin branch (`feat/embedding-plugin`, built on the pre-v2 base) onto
kb v2. Where v2 already had a mechanism, v2's was kept; see "What came from where" below.

## Packages

| Path | What it holds |
| --- | --- |
| `embed` | The contract (`Embedder`, `Prefixer`, `Pinger`, `ErrUnavailable`, `None`, `DocText`/`QueryText`) and the plugin surface (`Provider`, `Option`, `Config`, `Env`, `Registry`, `NoneProvider`) |
| `embed/httpembed` | The `http` provider: a generic HTTP embeddings API client |
| `embed/command` | The `command` provider: a user-supplied program over JSON stdin/stdout |
| `embed/builtin` | `Register(r)` / `NewRegistry()`: `http`, `command`, `none`; no default |
| `internal/embed` | kb's view: aliases of the public contract, plus v2's caching wrapper (`Cached`) and shared cache |
| `internal/embed/local` | v2's in-process embedder, unchanged, plus `Provider(cacheDir)`: the registry's `local` provider |
| `internal/embed/provider` | kb's glue: `NewRegistry` (local as Default, then the built-ins, then `Register`ed extras), `Explicit` (the `[embed]` table as a registry selection), `Open`, and `Kinded` (provider errors as kb error kinds) |

## The contract

```go
type Embedder interface {
    Embed(ctx context.Context, texts []string) ([][]float32, error) // one vector per text, in order, each Dim() long
    Model() string // the vectors' identity, recorded in embed_meta
    Dim() int      // recorded in embed_meta; 0 means "no vectors" (FTS only)
}
type Prefixer interface{ Prefixes() (doc, query string) } // optional: task prefixes
type Pinger interface{ Ping(ctx context.Context) error }  // optional: cheap health check
```

`internal/embed.Embedder` and friends are type aliases of these, so every v2 embedder (local,
fake, `Cached`) is a public `Embedder` with no change. `DocText`/`QueryText` follow v2: an
embedder without `Prefixer` gets its text as is (the first branch defaulted to nomic prefixes).

`Model()` must name every setting that changes the vectors, because embed_meta and the embedding
cache key on it. The http client reports `http "<endpoint>" model="<model>"` plus
`send_dimensions`, `truncate` and any prefixes; the command client reports
`command ["<program>" "<arg>" ...] model="<model>"` plus any prefixes. Every free-form field is
Go-quoted, so a value can never imitate a flag or another field (`model="m truncate"` is not
`model="m" truncate`). The API key, auth header, batch size and timeouts are not part of it.

The http client also checks that a reply's indexes are exactly `0..n-1`, each once; a duplicate,
negative or out-of-range index is an error rather than vectors attached to the wrong texts.

Errors that mean "cannot be used right now" wrap `embed.ErrUnavailable`: a transport failure, the
client's own deadline, a 5xx/408/429, a program that crashes or exits non-zero. A configuration
mistake (a rejected key, an unknown model, a missing program, the wrong vector length) is a plain
error. In kb, `provider.Kinded` maps the first to `errs.Unavailable` (hybrid search falls back to
FTS, exit 2) and the second to `errs.Usage` (exit 1); the caller's own cancellation passes through.

## Providers and the registry

```go
type Provider struct {
    Name    string                            // what configuration selects it by
    Summary string
    Options []Option                          // provider-specific settings it reads
    New     func(cfg Config) (Embedder, error)
}
type Option struct {
    Key       string   // read by the provider as cfg.Get(key)
    Env       []string // variables to consult, in order; "{P}" = the prefix; default {P}EMBED_<KEY>
    Default   string
    KeepEmpty bool     // a set-but-empty variable is a value (e.g. "no prefix")
}
```

`Registry.Register` rejects duplicate names, empty names and a nil `New`. `Registry.Resolve(explicit, env)`:

- **provider:** `explicit.Provider`, else `{P}EMBEDDER`, else `Registry.Default`; none of them
  is an `ErrUnknownProvider` error listing the registered names.
- **model:** `explicit.Model`, else `{P}EMBED_MODEL`, else empty (the provider's default).
- **dim:** `explicit.Dim`, else `{P}EMBED_DIM`, else `Registry.DefaultDim`.
- **options:** `explicit.Options[key]`, else the option's variables, else `Option.Default`.

`Open(cfg)` calls the provider's `New`; `OpenEnv(prefix, explicit)` does both over the process
environment. `NoEnv()` is an `Env` that sees nothing. `Config.LookupEnv` (nil = `os.LookupEnv`)
is how a provider reads a variable the configuration names explicitly, such as http's
`api_key_env`; Resolve passes it through and never sets it.

## How kb selects its embedder

kb builds its registry per command (`provider.NewRegistry(d.modelCacheDir)`) and resolves with
`embed.NoEnv()`: the selection comes only from the store's config file, as everything in v2 does.
No `KB_*` variable selects or configures a provider, so a machine that still exports the first
branch's `KB_EMBEDDER` or `KB_EMBED_MODEL` keeps v2's default (`TestKBIgnoresEmbedderEnvironment`).

```toml
[embed]
provider = "http"          # unset or "local": the in-process model (v2's default)
model = "text-embedding-3-small"
dim = 768                  # required for http and command; still only 0 for local

[embed.http]               # url, path, api_key_env, auth_header, auth_scheme,
url = "https://api.openai.com/v1"   # no_dimensions, truncate, doc_prefix, query_prefix,
api_key_env = "OPENAI_API_KEY"      # timeout_seconds

[embed.command]            # program, args (one word each), timeout_seconds,
program = "/opt/embed/run" # doc_prefix, query_prefix
args = ["--quiet"]
```

`cli.buildEmbedder` keeps v2's order: the test hook (`d.embedder`), then `model = "none"` (FTS
only, whatever the provider), then the harness model hook (local only), then the registry. When
the registry returns a `*local.Embedder`, the v2 download step runs as before: only `reindex`,
`add` and `init` pull a missing model. `http` and `command` never ask for the model cache
directory. `kb model pull` and `kb model path` refuse a non-local provider with a Usage error.

Validation: `embed.provider` must be one word; with the local provider `embed.dim` must still be
0 (v2's message); `embed.command.args` must be single words; timeouts must not be negative.
What a provider needs (`dim`, `http.url`, `command.program`) is checked when the embedder is
built (`config.Embed.Needs`), not by `Validate`, so a switch can be made one `--set` at a time.
`CheckModel` (the supported-model table) applies to the local provider only.

The new keys are `omitempty` pointers/strings, so `Save(Default())` (the `default.toml` golden)
and `EffectiveJSON` (the `settings_hash` in old search logs) are byte-for-byte unchanged for a
config that names no provider. `--set` reaches the sub-tables (`embed.http.url=...`), and `clone`
deep-copies them. The per-command embedder set (`fanout.go`) is keyed by the table's JSON, since
`config.Embed` now holds pointers and a slice.

A kb build can add a provider with `provider.Register(p)` from an `init` function and select it
with `embed.provider`; its options are not read from the config file (there is no generic options
table), so such a provider takes its settings from its own code.

## The two providers

### `http`: a generic HTTP embeddings API

POST `{url}{path}` (path default `/embeddings`) with `{"model","input","dimensions"}`, answered
by `{"data":[{"index","embedding"}]}`: OpenAI, OpenRouter, llama.cpp's `llama-server`, LM Studio,
vLLM, TEI and Ollama's `/v1` all accept it. `auth_header` (default `Authorization`) and
`auth_scheme` (default `Bearer`; `none` sends the bare key) cover vendors that want
`api-key: <key>`. `no_dimensions` leaves out the `dimensions` field; `truncate` shortens longer
vectors to `dim` and re-normalises them (only sound for models trained for it). Requests are
split at 16 texts; each call gets 30 s (or `timeout_seconds`) plus 5 s per text.

### `command`: bring your own local model

kb runs `program args...` without a shell, writes `{"model","dim","texts"}` to its stdin and
expects `{"embeddings":[[...],...]}` or `{"error":"..."}` on stdout, then exit 0. kb's own
`embed.batch` (default 16) bounds each run. Each run gets 60 s (or `timeout_seconds`) plus 5 s per
text. A program that does not exist (a PATH miss or a missing absolute path) is a configuration
error, never `ErrUnavailable`, so search does not quietly fall back
(`TestMissingAbsoluteProgramIsAConfigurationError`, `TestProviderFailuresKeepKBExitCodes`).

Why a command as well as `http`: a user-run local server is covered by `http`; a model that can
only be reached as a program (no listening ports allowed, the only approved runtime is a CLI)
is not. The process boundary also keeps the model's runtime and memory out of kb.

```python
#!/usr/bin/env python3
import json, sys
from my_model import embed   # whatever loads your local weights
req = json.load(sys.stdin)
json.dump({"embeddings": [embed(t, req["dim"]) for t in req["texts"]]}, sys.stdout)
```

## Adopting it in another project

```go
import (
    "github.com/nickstrad/kb/embed"
    "github.com/nickstrad/kb/embed/builtin"
)

r := builtin.NewRegistry()
r.MustRegister(embed.Provider{
    Name:    "corp",
    Options: []embed.Option{{Key: "endpoint", Env: []string{"{P}CORP_ENDPOINT"}}},
    New: func(cfg embed.Config) (embed.Embedder, error) {
        return newCorpClient(cfg.Get("endpoint"), cfg.Model, cfg.Dim)
    },
})
r.Default = "corp"
e, cfg, err := r.OpenEnv("MYAPP_", embed.Config{Provider: *flagEmbedder, Model: *flagModel})
// MYAPP_EMBEDDER=http MYAPP_EMBED_URL=... MYAPP_EMBED_MODEL=... MYAPP_EMBED_DIM=768 works too.
vecs, err := e.Embed(ctx, []string{embed.DocText(e, "some document")})
```

Store `e.Model()` and `e.Dim()` next to your vectors and refuse to mix vectors when they change,
as kb's embed_meta does. A registry under another prefix never sees `KB_*` variables
(`TestRegistryIgnoresKBEnvironment`).

## What came from where

Kept from the first plugin branch: the public `embed` package and `Registry` (`Provider`,
`Option`, `Config`, `Env`, `Resolve`/`Open`/`OpenEnv`), the `http` provider (the OpenAI-shape
client with configurable path and auth header), the `command` provider and its protocol, error
classification, both Astra regression tests in substance, and the provider-switching CLI test.

Adopted from v2 instead of the branch:

- **Default provider and model.** v2's in-process `local` embedder (rembed,
  `BAAI/bge-small-en-v1.5`) is the registry's Default, wrapped as a provider rather than
  rewritten. The branch's Ollama default and its 768-dim `DefaultDim` are gone; v2's vec0 width is
  lazy, so any `dim` works.
- **Configuration source.** v2's config file (`[embed]`) instead of the branch's `KB_EMBEDDER` /
  `KB_EMBED_*` variables and `--embedder` / `--embed-model` flags. kb resolves with `NoEnv`.
- **Secrets.** `api_key_env` names a variable rather than kb reading `KB_EMBED_API_KEY`.
- **Batching.** v2's `embed.batch` bounds each embed call; the branch's `KB_EMBED_BATCH` is
  not a kb setting (the command client keeps its own cap of 64 for library users).
- **Prefixes.** v2's rule (no prefix unless the embedder says) instead of the nomic fallback.
- **Error kinds.** v2's `errs.Unavailable` / `errs.Usage` via `provider.Kinded`.
- **Caching.** v2's shared embedding cache (`embed.Cached`) wraps every provider; keyed by
  `Model()`, which is why `Model()` carries the endpoint and program.
- **`none`.** v2's `model = "none"` (FTS only, any provider).
- **Test double.** v2's `internal/embed/fake`; the branch's public `embed/fake` was not ported.

Dropped: the `ollama`, `openai` and `openrouter` providers and the registry's `Auto` selection.
`http` reaches all three services (Ollama through `/v1`), and `Auto` existed only so an
`OPENROUTER_API_KEY` in the environment could take over the default, which would break v2's
config-only selection.

## Known limits

- A provider registered with `provider.Register` gets no options from the config file.
- `embed.command.args` cannot contain spaces; wrap such a call in a script.
- Some provider error messages are written for kb's users first.
