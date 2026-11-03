# Embedding provider plugin

kb's embedding layer is a public Go package, `github.com/nickstrad/kb/embed`, that any Go
project can import. A program registers embedding providers in a registry and picks one by
configuration: a provider name plus options, taken from flags or a config file first and
environment variables second. kb is the first user. With no configuration it behaves exactly as
it did before the plugin surface existed.

The motivating case is a machine that cannot load Hugging Face models. The registry has no
built-in model runtime. Every provider either calls a service (Ollama, any HTTP embeddings API)
or runs a program the user supplies. No provider downloads or loads model weights inside kb's
process.

## Packages

| Path | What it holds |
| --- | --- |
| `embed` | The contract (`Embedder`, `Prefixer`, `Pinger`, `ErrUnavailable`, `None`) and the plugin surface (`Provider`, `Option`, `Config`, `Env`, `Registry`) |
| `embed/ollama` | Ollama client; `ollama.Provider()` is `ollama` |
| `embed/openai` | OpenAI-compatible client; `Provider()` is `openai`, `OpenRouterProvider()` is `openrouter`, `HTTPProvider()` is `http` |
| `embed/command` | External-program client; `command.Provider()` is `command` |
| `embed/builtin` | `Register(r)` / `NewRegistry()`: all of the above in kb's order, `ollama` as the default, 768 dims |
| `embed/fake` | Deterministic test double |
| `internal/embed/provider` | kb's glue: `provider.Registry` (the built-in set), the env prefix `KB_`, and `New(Options)` for the CLI |

## The contract

```go
type Embedder interface {
    Embed(ctx context.Context, texts []string) ([][]float32, error) // one vector per text, in order, each Dim() long
    Model() string // recorded in embed_meta
    Dim() int      // recorded in embed_meta; 0 means "no vectors" (FTS only)
}
type Prefixer interface{ Prefixes() (doc, query string) } // optional: per-model task prefixes
type Pinger interface{ Ping(ctx context.Context) error }  // optional: cheap health check (kb doctor, reindex --all)
```

Errors that mean "the embedder can't be reached right now" wrap `embed.ErrUnavailable`: a
transport failure, the client's own timeout, a 5xx/408/429, or the command exiting non-zero.
kb's hybrid search falls back to full-text search on these. A configuration mistake, such as a
bad key, an unknown model or the wrong vector length, is a plain error.

## Providers and registration

```go
type Provider struct {
    Name    string                               // what KB_EMBEDDER / --embedder selects
    Summary string
    Options []Option                             // provider-specific settings it reads
    Auto    func(Env) bool                       // optional: claim the selection when no name is given
    New     func(cfg Config) (Embedder, error)
}
type Option struct {
    Key       string   // read by the provider as cfg.Get(key)
    Env       []string // variables to consult, in order; "{P}" = the registry's prefix; default {P}EMBED_<KEY>
    Default   string
    KeepEmpty bool     // a set-but-empty variable is a value (e.g. "no prefix")
}
```

`Registry.Register` adds a provider. It rejects duplicate names, empty names, the reserved name
`auto`, and a nil `New`. `Registry.Resolve(explicit, env)` builds a `Config` with this
precedence:

- **provider:** `explicit.Provider`, else `{P}EMBEDDER`, else the first provider (in registration
  order) whose `Auto` returns true, else `Registry.Default`. `auto` means the same as empty.
- **model:** `explicit.Model`, else `{P}EMBED_MODEL`, else empty (the provider's default).
- **dim:** `explicit.Dim`, else `{P}EMBED_DIM`, else `Registry.DefaultDim`.
- **options:** `explicit.Options[key]`, else the option's environment variables, else
  `Option.Default`.

`Registry.Open(cfg)` calls the provider's `New`. `OpenEnv(prefix, explicit)` does both steps
over the process environment.

Providers are registered explicitly rather than through `init()` side effects. A program can see
in one place which providers it supports and in which order `Auto` is consulted. The `Auto`
order matters for kb, because OpenRouter claims the default when its key is set.

## How kb's default maps to the old behavior

kb uses `builtin.NewRegistry()` with the prefix `KB_`. Each old setting maps to the new design as
follows:

| Old behavior (hard-coded switch) | Now |
| --- | --- |
| No settings: Ollama at `KB_OLLAMA_URL` or `http://127.0.0.1:11434`, `nomic-embed-text`, nomic prefixes | `Registry.Default = "ollama"`; option `url` reads `{P}OLLAMA_URL`, default `http://127.0.0.1:11434`; `doc_prefix`/`query_prefix` default to nomic's, `KeepEmpty` |
| `OPENROUTER_API_KEY` or `KB_OPENROUTER_API_KEY` set: OpenRouter | `openrouter` has `Auto` on those two variables, and it is registered before every other `Auto` provider |
| OpenRouter key order: `KB_OPENROUTER_API_KEY`, `OPENROUTER_API_KEY`, `KB_EMBED_API_KEY` | option `api_key` with `Env` in that order |
| `KB_EMBEDDER=openai` needs `KB_EMBED_URL` and a model | `openai` provider, same checks and messages |
| `KB_EMBEDDER=none` | `embed.NoneProvider()` |
| `KB_EMBED_DIM` default 768, rejected unless a positive integer | `{P}EMBED_DIM`, `DefaultDim = 768`, same check |
| `KB_EMBED_SEND_DIMENSIONS`, `KB_EMBED_TRUNCATE` (truncate defaults on for `text-embedding-3`) | options `send_dimensions`, `truncate` (default computed in `OpenRouterProvider`) |
| `--embedder`, `--embed-model` beat the environment | passed as `explicit.Provider` / `explicit.Model` |

The existing provider tests (`internal/embed/provider/provider_test.go`) still pass, along with
the CLI tests that use the real provider. No environment variable was renamed; the new ones are
additions. The embed_meta guard is unchanged: it compares the configured embedder's `Model()` and
`Dim()` with what the index recorded, so switching providers or models still requires
`kb reindex --all`. `TestSwitchingBetweenCommandAndHTTPProviders` in `internal/cli` covers this
end to end.

## The two new provider shapes

### `http`: a generic HTTP embeddings API

You configure it with a base URL (`KB_EMBED_URL`), an API key (`KB_EMBED_API_KEY`) and a model.
It uses the OpenAI request shape: `POST {"model","input","dimensions"}` returns
`{"data":[{"index","embedding"}]}`. Most hosted APIs and local servers accept that shape,
including llama.cpp's `llama-server`, LM Studio, vLLM, Hugging Face TEI and Ollama's `/v1`. The
parts that differ between vendors can be configured, so no single vendor's layout is assumed:

- `KB_EMBED_PATH`: the endpoint path, default `/embeddings`.
- `KB_EMBED_AUTH_HEADER`: the header that carries the key, default `Authorization`.
- `KB_EMBED_AUTH_SCHEME`: the scheme before the key, default `Bearer`; `none` sends the bare
  key, for an `api-key: <key>` style header.

It reuses the `embed/openai` client, extended with `Path`, `AuthHeader` and `AuthScheme`. It is
tested against an `httptest` server.

### `command`: bring your own local model

`KB_EMBED_COMMAND` names a program and its arguments. They are split on spaces and run without a
shell. The program gets one JSON request on stdin, `{"model","dim","texts"}`, and must write
`{"embeddings":[[...],...]}` or `{"error":"..."}` to stdout, then exit 0. Inputs are sent in
batches of `KB_EMBED_BATCH` texts (default 64). Each run gets a deadline of `KB_EMBED_TIMEOUT`
seconds (default 60) plus 5 seconds per text. A model name is required, because it identifies the
vectors in embed_meta.

Why a command rather than a second HTTP provider for local models: a user-run local server is
already covered by `http` and `openai`. A model that can only be reached as a program is not.
Examples are a locked-down work machine where nothing may listen on a port, or one where the only
approved runtime is a CLI (an ONNX Runtime script, `llama-embedding`, a company tool). A process
boundary also keeps the model's runtime, dependencies and memory out of the Go binary. One process
per batch keeps the protocol stateless and easy to write in any language. A model that is slow to
load should stay resident in a server, with the command as a thin client or with `http` pointed at
the server. The provider is tested by re-running the test binary as the fake program, covering
batching, ordering, error replies, crashes, a missing program, timeouts and cancellation.

Example program (Python, ONNX, no Hugging Face downloads at run time):

```python
#!/usr/bin/env python3
import json, sys
from my_model import embed   # whatever loads your local weights
req = json.load(sys.stdin)
json.dump({"embeddings": [embed(t, req["dim"]) for t in req["texts"]]}, sys.stdout)
```

```bash
KB_EMBEDDER=command KB_EMBED_COMMAND=/opt/embed/run.py KB_EMBED_MODEL=my-onnx-768 kb reindex --all
```

## Adopting it in another project

```go
import (
    "github.com/nickstrad/kb/embed"
    "github.com/nickstrad/kb/embed/builtin"
)

r := builtin.NewRegistry()      // or embed.NewRegistry() and register only what you want
r.MustRegister(embed.Provider{  // your own provider
    Name:    "corp",
    Options: []embed.Option{{Key: "endpoint", Env: []string{"{P}CORP_ENDPOINT"}}},
    New: func(cfg embed.Config) (embed.Embedder, error) {
        return newCorpClient(cfg.Get("endpoint"), cfg.Model, cfg.Dim)
    },
})
e, cfg, err := r.OpenEnv("MYAPP_", embed.Config{Provider: *flagEmbedder, Model: *flagModel})
// MYAPP_EMBEDDER=corp MYAPP_CORP_ENDPOINT=... selects it; MYAPP_EMBEDDER=command, http, ... work too.
vecs, err := e.Embed(ctx, []string{embed.DocText(e, "some document")})
```

Store `e.Model()` and `e.Dim()` next to your vectors, and refuse to mix vectors when they change.
That is what kb's embed_meta does.

To add a provider to kb itself, register it in `provider.Registry` from an `init` function in a
kb build. It then works through `--embedder` / `KB_EMBEDDER` like the built-in providers.

## Known limits

- kb's vector column is 768 numbers wide. A provider must produce 768 dimensions for kb, either
  natively or through the `dimensions` field or truncation. Other programs can use any width.
- `ollama.New` still falls back to `KB_OLLAMA_URL` / `KB_EMBED_MODEL` when its arguments are
  empty, a leftover from before the package was public that its tests pin. `ollama.Provider()`
  always passes explicit values, so code that goes through the registry is unaffected.
- Some provider error messages name kb's variables (`KB_EMBED_URL`), because kb is the main user.
