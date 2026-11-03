package httpembed

import (
	"fmt"
	"time"

	"github.com/nickstrad/kb/embed"
)

// Name is the provider name Provider registers.
const Name = "http"

// Provider returns the "http" provider: a generic HTTP embeddings API. Options, each read from
// {P}EMBED_<KEY> when the registry reads an environment:
//
//	url              base URL of the API (required)
//	api_key          the key itself
//	api_key_env      the name of a variable holding the key, read when api_key is empty (lets a
//	                 config file name the secret without containing it)
//	path             endpoint path below url (default /embeddings)
//	auth_header      header carrying the key (default Authorization)
//	auth_scheme      scheme before the key (default Bearer; "none" sends the bare key)
//	send_dimensions  send the dimensions field (default true)
//	truncate         shorten longer vectors to Dim and re-normalise (default false)
//	timeout          base seconds per call (default 30, plus 5 per text)
//	doc_prefix, query_prefix  task prefixes (default none)
//
// The model and a positive Dim are required.
func Provider() embed.Provider {
	return embed.Provider{
		Name:    Name,
		Summary: "a generic HTTP embeddings API (OpenAI request shape; URL, key, model, path and auth header configurable)",
		Options: []embed.Option{
			{Key: "url", Doc: "base URL of the API"},
			{Key: "api_key", Doc: "API key"},
			{Key: "api_key_env", Doc: "name of the environment variable holding the API key"},
			{Key: "path", Doc: "endpoint path below url (default /embeddings)"},
			{Key: "auth_header", Doc: "header carrying the key (default Authorization)"},
			{Key: "auth_scheme", Doc: `scheme before the key (default Bearer, "none" for a bare key)`},
			{Key: "send_dimensions", Default: "true", Doc: "send the dimensions field (false for servers that reject it)"},
			{Key: "truncate", Doc: "shorten longer vectors to the configured dimension"},
			{Key: "timeout", Doc: "base seconds per call"},
			{Key: "doc_prefix", KeepEmpty: true, Doc: "prefix for stored documents (default none)"},
			{Key: "query_prefix", KeepEmpty: true, Doc: "prefix for queries (default none)"},
		},
		New: func(cfg embed.Config) (embed.Embedder, error) {
			if cfg.Get("url") == "" {
				return nil, fmt.Errorf("embedder http needs a base URL (option url)")
			}
			if cfg.Model == "" {
				return nil, fmt.Errorf("embedder http needs a model")
			}
			if cfg.Dim <= 0 {
				return nil, fmt.Errorf("embedder http needs the model's vector width (dim), got %d", cfg.Dim)
			}
			key := cfg.Get("api_key")
			if name := cfg.Get("api_key_env"); key == "" && name != "" {
				if key = cfg.Getenv(name); key == "" {
					return nil, fmt.Errorf("embedder http: the API key variable %s is not set", name)
				}
			}
			c := Config{
				BaseURL: cfg.Get("url"), APIKey: key, Model: cfg.Model, Dim: cfg.Dim,
				Path: cfg.Get("path"), AuthHeader: cfg.Get("auth_header"), AuthScheme: cfg.Get("auth_scheme"),
				DocPrefix: cfg.Get("doc_prefix"), QueryPrefix: cfg.Get("query_prefix"),
			}
			var err error
			if c.SendDimensions, err = cfg.Bool("send_dimensions", true); err != nil {
				return nil, err
			}
			if c.Truncate, err = cfg.Bool("truncate", false); err != nil {
				return nil, err
			}
			secs, err := cfg.Int("timeout", 0)
			if err != nil {
				return nil, err
			}
			cl, err := New(c)
			if err != nil {
				return nil, err
			}
			if secs > 0 {
				cl.BaseTimeout = time.Duration(secs) * time.Second
			}
			return cl, nil
		},
	}
}
