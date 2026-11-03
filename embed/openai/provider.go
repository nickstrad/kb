package openai

import (
	"fmt"
	"strings"

	"github.com/nickstrad/kb/embed"
)

// Provider names registered by this package.
const (
	NameOpenAI     = "openai"
	NameOpenRouter = "openrouter"
	NameHTTP       = "http"
)

// OpenRouter defaults. text-embedding-3-small is cheap and can be shortened to 768 numbers.
const (
	OpenRouterURL          = "https://openrouter.ai/api/v1"
	DefaultOpenRouterModel = "openai/text-embedding-3-small"
)

func commonOptions() []embed.Option {
	return []embed.Option{
		{Key: "send_dimensions", Default: "true", Doc: "send the dimensions field (false for servers that reject it)"},
		{Key: "truncate", Doc: "shorten longer vectors to the configured dimension"},
		{Key: "doc_prefix", KeepEmpty: true, Doc: "prefix for stored documents (default none)"},
		{Key: "query_prefix", KeepEmpty: true, Doc: "prefix for queries (default none)"},
	}
}

// Provider returns the "openai" provider: any server speaking the OpenAI embeddings API.
//
//	url      {P}EMBED_URL (required), the base that serves /embeddings
//	api_key  {P}EMBED_API_KEY (optional for local servers)
//	plus send_dimensions, truncate, doc_prefix, query_prefix ({P}EMBED_<KEY>)
//
// The model is required.
func Provider() embed.Provider {
	return embed.Provider{
		Name:    NameOpenAI,
		Summary: "any other server speaking the OpenAI embeddings API",
		Options: append([]embed.Option{
			{Key: "url", Doc: "base URL that serves /embeddings"},
			{Key: "api_key", Doc: "API key"},
		}, commonOptions()...),
		New: func(cfg embed.Config) (embed.Embedder, error) {
			if cfg.Get("url") == "" {
				return nil, fmt.Errorf("embedder openai needs KB_EMBED_URL, the base URL that serves /embeddings (e.g. https://api.openai.com/v1)")
			}
			if cfg.Model == "" {
				return nil, fmt.Errorf("embedder openai needs a model: set KB_EMBED_MODEL or pass --embed-model")
			}
			return build(cfg, Config{BaseURL: cfg.Get("url"), APIKey: cfg.Get("api_key"), Model: cfg.Model}, false)
		},
	}
}

// OpenRouterProvider returns the "openrouter" provider. It claims automatic selection when
// {P}OPENROUTER_API_KEY or OPENROUTER_API_KEY is set.
//
//	api_key  {P}OPENROUTER_API_KEY, OPENROUTER_API_KEY, {P}EMBED_API_KEY (required)
//	url      {P}EMBED_URL (default https://openrouter.ai/api/v1)
//	model    default openai/text-embedding-3-small; truncate defaults to true for text-embedding-3
func OpenRouterProvider() embed.Provider {
	return embed.Provider{
		Name:    NameOpenRouter,
		Summary: "OpenRouter's embeddings API (picked automatically when an OpenRouter key is set)",
		Options: append([]embed.Option{
			{Key: "url", Default: OpenRouterURL, Doc: "OpenRouter base URL"},
			{Key: "api_key", Env: []string{"{P}OPENROUTER_API_KEY", "OPENROUTER_API_KEY", "{P}EMBED_API_KEY"}, Doc: "OpenRouter key"},
		}, commonOptions()...),
		Auto: func(env embed.Env) bool {
			return env.Get("{P}OPENROUTER_API_KEY", "OPENROUTER_API_KEY") != ""
		},
		New: func(cfg embed.Config) (embed.Embedder, error) {
			if cfg.Get("api_key") == "" {
				return nil, fmt.Errorf("embedder openrouter needs an API key: set OPENROUTER_API_KEY (or KB_OPENROUTER_API_KEY)")
			}
			model := cfg.Model
			if model == "" {
				model = DefaultOpenRouterModel
			}
			return build(cfg, Config{BaseURL: cfg.Get("url"), APIKey: cfg.Get("api_key"), Model: model},
				strings.Contains(model, "text-embedding-3"))
		},
	}
}

// HTTPProvider returns the "http" provider: a generic embeddings API over HTTP, configured by
// base URL, API key and model. The request and response follow the OpenAI shape (POST
// {"model","input","dimensions"} -> {"data":[{"index","embedding"}]}), which most hosted APIs and
// local servers (llama.cpp's llama-server, LM Studio, vLLM, TEI, Ollama's /v1) accept; the path
// and credential header are configurable so no single vendor's layout is assumed.
//
//	url          {P}EMBED_URL (required)
//	api_key      {P}EMBED_API_KEY
//	path         {P}EMBED_PATH (default /embeddings)
//	auth_header  {P}EMBED_AUTH_HEADER (default Authorization)
//	auth_scheme  {P}EMBED_AUTH_SCHEME (default Bearer; "none" sends the bare key)
//	plus send_dimensions, truncate, doc_prefix, query_prefix
//
// The model is required.
func HTTPProvider() embed.Provider {
	return embed.Provider{
		Name:    NameHTTP,
		Summary: "a generic HTTP embeddings API (OpenAI request shape; URL, key, path and auth header configurable)",
		Options: append([]embed.Option{
			{Key: "url", Doc: "base URL of the API"},
			{Key: "api_key", Doc: "API key"},
			{Key: "path", Doc: "endpoint path below url (default /embeddings)"},
			{Key: "auth_header", Doc: "header carrying the key (default Authorization)"},
			{Key: "auth_scheme", Doc: `scheme before the key (default Bearer, "none" for a bare key)`},
		}, commonOptions()...),
		New: func(cfg embed.Config) (embed.Embedder, error) {
			if cfg.Get("url") == "" {
				return nil, fmt.Errorf("embedder http needs a base URL: set KB_EMBED_URL")
			}
			if cfg.Model == "" {
				return nil, fmt.Errorf("embedder http needs a model: set KB_EMBED_MODEL or pass --embed-model")
			}
			return build(cfg, Config{
				BaseURL: cfg.Get("url"), APIKey: cfg.Get("api_key"), Model: cfg.Model,
				Path: cfg.Get("path"), AuthHeader: cfg.Get("auth_header"), AuthScheme: cfg.Get("auth_scheme"),
			}, false)
		},
	}
}

// build applies the options shared by every OpenAI-style provider and constructs the client.
func build(cfg embed.Config, c Config, truncateDefault bool) (embed.Embedder, error) {
	var err error
	if c.SendDimensions, err = cfg.Bool("send_dimensions", true); err != nil {
		return nil, err
	}
	if c.Truncate, err = cfg.Bool("truncate", truncateDefault); err != nil {
		return nil, err
	}
	c.Dim = cfg.Dim
	c.DocPrefix, c.QueryPrefix = cfg.Get("doc_prefix"), cfg.Get("query_prefix")
	return New(c)
}
