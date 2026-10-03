// Package provider builds the configured embed.Embedder from flags and environment variables, so
// every kb command picks the same one.
//
// The embedder is chosen by --embedder, else KB_EMBEDDER, else automatically:
//
//	ollama      a local or remote Ollama server (default when no OpenRouter key is set)
//	openrouter  OpenRouter's embeddings API (default when an OpenRouter key is set)
//	openai      any other server speaking the OpenAI embeddings API (KB_EMBED_URL required)
//	none        no vectors at all: the index is FTS only
//
// The model is --embed-model, else KB_EMBED_MODEL, else the embedder's default. Other knobs:
//
//	KB_OPENROUTER_API_KEY, OPENROUTER_API_KEY  OpenRouter key (either one selects openrouter)
//	KB_EMBED_API_KEY                           key for openai (and a fallback for openrouter)
//	KB_EMBED_URL                               base URL for openai/openrouter
//	KB_OLLAMA_URL                              Ollama address (default http://127.0.0.1:11434)
//	KB_EMBED_DIM                               vector length (default 768, the only one this build stores)
//	KB_EMBED_DOC_PREFIX, KB_EMBED_QUERY_PREFIX task prefixes (nomic's for ollama, none otherwise)
//	KB_EMBED_SEND_DIMENSIONS=false             leave the dimensions field out of OpenAI-style requests
//	KB_EMBED_TRUNCATE=true|false               shorten longer vectors to KB_EMBED_DIM
//
// Changing the embedder or model on an existing index is caught by embed_meta: kb refuses to mix
// vectors and asks for `kb reindex --all`.
package provider

import (
	"fmt"
	"os"
	"strconv"
	"strings"

	"github.com/nickstrad/kb/internal/embed"
	"github.com/nickstrad/kb/internal/embed/ollama"
	"github.com/nickstrad/kb/internal/embed/openai"
)

// Embedder names accepted by --embedder and KB_EMBEDDER.
const (
	Ollama     = "ollama"
	OpenRouter = "openrouter"
	OpenAI     = "openai"
	None       = "none"
)

// Defaults for OpenRouter. text-embedding-3-small is cheap and can be shortened to 768 numbers,
// which is what this build's vector column stores.
const (
	OpenRouterURL          = "https://openrouter.ai/api/v1"
	DefaultOpenRouterModel = "openai/text-embedding-3-small"
)

// DefaultDim is the vector length used when KB_EMBED_DIM is unset.
const DefaultDim = 768

// Options are the command-line overrides; empty fields defer to the environment.
type Options struct {
	Embedder string
	Model    string
}

// Info describes the embedder in effect, for kb doctor.
type Info struct {
	Name     string // one of the embedder names above
	Endpoint string // base URL, empty for none
}

// New returns the embedder selected by opts and the environment.
func New(opts Options) (embed.Embedder, Info, error) {
	name := strings.ToLower(strings.TrimSpace(first(opts.Embedder, os.Getenv("KB_EMBEDDER"))))
	if name == "" || name == "auto" {
		name = Ollama
		if openRouterKey() != "" {
			name = OpenRouter
		}
	}
	model := first(opts.Model, os.Getenv("KB_EMBED_MODEL"))

	dim := DefaultDim
	if s := os.Getenv("KB_EMBED_DIM"); s != "" {
		n, err := strconv.Atoi(s)
		if err != nil || n <= 0 {
			return nil, Info{}, fmt.Errorf("KB_EMBED_DIM=%q is not a positive integer", s)
		}
		dim = n
	}

	switch name {
	case None:
		return embed.None{}, Info{Name: None}, nil

	case Ollama:
		c := ollama.New("", model, dim)
		c.DocPrefix, c.QueryPrefix = prefixes(embed.DocPrefix, embed.QueryPrefix)
		return c, Info{Name: Ollama, Endpoint: c.BaseURL()}, nil

	case OpenRouter:
		key := first(openRouterKey(), os.Getenv("KB_EMBED_API_KEY"))
		if key == "" {
			return nil, Info{}, fmt.Errorf("embedder openrouter needs an API key: set OPENROUTER_API_KEY (or KB_OPENROUTER_API_KEY)")
		}
		if model == "" {
			model = DefaultOpenRouterModel
		}
		truncate, err := boolEnv("KB_EMBED_TRUNCATE", strings.Contains(model, "text-embedding-3"))
		if err != nil {
			return nil, Info{}, err
		}
		return newOpenAI(OpenRouter, first(os.Getenv("KB_EMBED_URL"), OpenRouterURL), key, model, dim, truncate)

	case OpenAI:
		url := os.Getenv("KB_EMBED_URL")
		if url == "" {
			return nil, Info{}, fmt.Errorf("embedder openai needs KB_EMBED_URL, the base URL that serves /embeddings (e.g. https://api.openai.com/v1)")
		}
		if model == "" {
			return nil, Info{}, fmt.Errorf("embedder openai needs a model: set KB_EMBED_MODEL or pass --embed-model")
		}
		truncate, err := boolEnv("KB_EMBED_TRUNCATE", false)
		if err != nil {
			return nil, Info{}, err
		}
		return newOpenAI(OpenAI, url, os.Getenv("KB_EMBED_API_KEY"), model, dim, truncate)

	default:
		return nil, Info{}, fmt.Errorf("unknown embedder %q (want ollama, openrouter, openai or none)", name)
	}
}

func newOpenAI(name, url, key, model string, dim int, truncate bool) (embed.Embedder, Info, error) {
	send, err := boolEnv("KB_EMBED_SEND_DIMENSIONS", true)
	if err != nil {
		return nil, Info{}, err
	}
	doc, query := prefixes("", "")
	c, err := openai.New(openai.Config{
		BaseURL: url, APIKey: key, Model: model, Dim: dim,
		SendDimensions: send, Truncate: truncate,
		DocPrefix: doc, QueryPrefix: query,
	})
	if err != nil {
		return nil, Info{}, err
	}
	return c, Info{Name: name, Endpoint: c.BaseURL()}, nil
}

func openRouterKey() string {
	return first(os.Getenv("KB_OPENROUTER_API_KEY"), os.Getenv("OPENROUTER_API_KEY"))
}

// prefixes applies KB_EMBED_DOC_PREFIX and KB_EMBED_QUERY_PREFIX over the given defaults. A
// variable that is set but empty means "no prefix".
func prefixes(doc, query string) (string, string) {
	if v, ok := os.LookupEnv("KB_EMBED_DOC_PREFIX"); ok {
		doc = v
	}
	if v, ok := os.LookupEnv("KB_EMBED_QUERY_PREFIX"); ok {
		query = v
	}
	return doc, query
}

func boolEnv(name string, def bool) (bool, error) {
	s := os.Getenv(name)
	if s == "" {
		return def, nil
	}
	b, err := strconv.ParseBool(s)
	if err != nil {
		return false, fmt.Errorf("%s=%q is not true or false", name, s)
	}
	return b, nil
}

func first(vals ...string) string {
	for _, v := range vals {
		if v != "" {
			return v
		}
	}
	return ""
}
