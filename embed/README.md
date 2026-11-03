# embed: a pluggable embedding layer

`github.com/nickstrad/kb/embed` defines the `Embedder` contract and a `Registry` that picks and
configures an embedding provider from a name plus options (explicit values, then environment
variables under a prefix you choose). kb uses it with the prefix `KB_`. Any Go program can import
it and register its own providers.

```go
r := builtin.NewRegistry() // ollama (default), openrouter, openai, http, command, none
r.MustRegister(myProvider) // optional: your own embed.Provider
e, cfg, err := r.OpenEnv("MYAPP_", embed.Config{Provider: flagEmbedder, Model: flagModel})
```

Built-in providers: `ollama`, `openrouter`, `openai`, `http` (generic HTTP embeddings API),
`command` (your own program over JSON stdin/stdout, so you can bring a local model without
loading Hugging Face weights in-process) and `none`.

The full design (the interface, how providers register, how kb's default maps to its earlier
behavior, and how another project adopts the package) is in
[docs/embedding-plugin.md](../docs/embedding-plugin.md).
