# embed: a pluggable embedding layer

`github.com/nickstrad/kb/embed` defines the `Embedder` contract and a `Registry` that picks and
configures an embedding provider from a name plus options (explicit values first, then, if you
pass one, environment variables under a prefix you choose). Any Go program can import it and
register its own providers. Nothing here loads model weights or depends on Hugging Face.

```go
r := builtin.NewRegistry()  // http, command, none
r.MustRegister(myProvider)  // optional: your own embed.Provider
r.Default = "http"          // optional: what an unnamed selection means
e, cfg, err := r.OpenEnv("MYAPP_", embed.Config{Provider: flagEmbedder, Model: flagModel, Dim: 768})
```

Built-in providers: `http` (`embed/httpembed`: any embeddings API in the OpenAI request shape;
URL, key, model, path and auth header configurable), `command` (`embed/command`: your own
program over JSON stdin/stdout) and `none` (FTS only).

kb itself resolves through this registry with its in-process `local` provider added as the
default, and with configuration taken only from its config file. The design is in
[docs/embedding-plugin.md](../docs/embedding-plugin.md).
