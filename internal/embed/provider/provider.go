// Package provider builds kb's configured embed.Embedder from flags and environment variables, so
// every kb command picks the same one. It is the first user of the public plugin surface in
// github.com/nickstrad/kb/embed: kb's providers are the built-in set (embed/builtin) registered in
// Registry, configuration is resolved with the environment prefix "KB_", and the default with no
// configuration is Ollama, exactly as before the plugin surface existed.
//
// The embedder is chosen by --embedder, else KB_EMBEDDER, else automatically:
//
//	ollama      a local or remote Ollama server (default when no OpenRouter key is set)
//	openrouter  OpenRouter's embeddings API (default when an OpenRouter key is set)
//	openai      any other server speaking the OpenAI embeddings API (KB_EMBED_URL required)
//	http        a generic HTTP embeddings API: KB_EMBED_URL + KB_EMBED_API_KEY + model, with
//	            KB_EMBED_PATH, KB_EMBED_AUTH_HEADER, KB_EMBED_AUTH_SCHEME for non-OpenAI layouts
//	command     a user-supplied program (KB_EMBED_COMMAND) that reads JSON texts on stdin and
//	            writes JSON vectors on stdout; see embed/command
//	none        no vectors at all: the index is FTS only
//
// The model is --embed-model, else KB_EMBED_MODEL, else the embedder's default. Other knobs:
//
//	KB_OPENROUTER_API_KEY, OPENROUTER_API_KEY  OpenRouter key (either one selects openrouter)
//	KB_EMBED_API_KEY                           key for openai/http (and a fallback for openrouter)
//	KB_EMBED_URL                               base URL for openai/openrouter/http
//	KB_OLLAMA_URL                              Ollama address (default http://127.0.0.1:11434)
//	KB_EMBED_DIM                               vector length (default 768, the only one this build stores)
//	KB_EMBED_DOC_PREFIX, KB_EMBED_QUERY_PREFIX task prefixes (nomic's for ollama, none otherwise)
//	KB_EMBED_SEND_DIMENSIONS=false             leave the dimensions field out of OpenAI-style requests
//	KB_EMBED_TRUNCATE=true|false               shorten longer vectors to KB_EMBED_DIM
//	KB_EMBED_COMMAND, KB_EMBED_BATCH, KB_EMBED_TIMEOUT  the command provider's program, batch, seconds
//
// Changing the embedder or model on an existing index is caught by embed_meta: kb refuses to mix
// vectors and asks for `kb reindex --all`.
package provider

import (
	"github.com/nickstrad/kb/embed"
	"github.com/nickstrad/kb/embed/builtin"
	"github.com/nickstrad/kb/embed/command"
	"github.com/nickstrad/kb/embed/ollama"
	"github.com/nickstrad/kb/embed/openai"
)

// Embedder names accepted by --embedder and KB_EMBEDDER.
const (
	Ollama     = ollama.Name
	OpenRouter = openai.NameOpenRouter
	OpenAI     = openai.NameOpenAI
	HTTP       = openai.NameHTTP
	Command    = command.Name
	None       = embed.NoneModel
)

// Defaults for OpenRouter.
const (
	OpenRouterURL          = openai.OpenRouterURL
	DefaultOpenRouterModel = openai.DefaultOpenRouterModel
)

// DefaultDim is the vector length used when KB_EMBED_DIM is unset.
const DefaultDim = builtin.DefaultDim

// EnvPrefix is the prefix kb's settings use: KB_EMBEDDER, KB_EMBED_MODEL, KB_EMBED_URL, ...
const EnvPrefix = "KB_"

// Registry holds the providers kb can select. It starts as the built-in set; a build of kb that
// wants another provider registers it here (from an init function) and selects it with
// --embedder or KB_EMBEDDER like any other.
var Registry = builtin.NewRegistry()

// Options are the command-line overrides; empty fields defer to the environment.
type Options struct {
	Embedder string
	Model    string
}

// Info describes the embedder in effect, for kb doctor.
type Info struct {
	Name     string // the provider name
	Endpoint string // base URL, empty for none and for the command provider
}

// New returns the embedder selected by opts and the environment.
func New(opts Options) (embed.Embedder, Info, error) {
	e, cfg, err := Registry.OpenEnv(EnvPrefix, embed.Config{Provider: opts.Embedder, Model: opts.Model})
	if err != nil {
		return nil, Info{}, err
	}
	info := Info{Name: cfg.Provider}
	if u, ok := e.(interface{ BaseURL() string }); ok {
		info.Endpoint = u.BaseURL()
	}
	return e, info, nil
}
