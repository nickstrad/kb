// Package builtin registers every provider that ships with the embed module, in kb's selection
// order, so a program gets kb's behaviour with one call:
//
//	ollama      a local or remote Ollama server (the Default)
//	openrouter  OpenRouter (claims automatic selection when an OpenRouter key is set)
//	openai      any OpenAI-compatible embeddings server
//	http        a generic HTTP embeddings API (configurable path and auth header)
//	command     a user-supplied program speaking JSON on stdin/stdout
//	none        no vectors; full-text only
package builtin

import (
	"github.com/nickstrad/kb/embed"
	"github.com/nickstrad/kb/embed/command"
	"github.com/nickstrad/kb/embed/ollama"
	"github.com/nickstrad/kb/embed/openai"
)

// DefaultDim is the vector length NewRegistry configures: what kb's vector column stores.
const DefaultDim = 768

// Providers returns the built-in providers in registration order.
func Providers() []embed.Provider {
	return []embed.Provider{
		ollama.Provider(),
		openai.OpenRouterProvider(),
		openai.Provider(),
		openai.HTTPProvider(),
		command.Provider(),
		embed.NoneProvider(),
	}
}

// Register adds the built-in providers to r and makes ollama its Default if it has none.
func Register(r *embed.Registry) error {
	for _, p := range Providers() {
		if err := r.Register(p); err != nil {
			return err
		}
	}
	if r.Default == "" {
		r.Default = ollama.Name
	}
	return nil
}

// NewRegistry returns a registry holding the built-in providers, with ollama as the Default and
// DefaultDim 768: exactly kb's configuration.
func NewRegistry() *embed.Registry {
	r := embed.NewRegistry()
	r.DefaultDim = DefaultDim
	if err := Register(r); err != nil {
		panic(err) // the built-in set has unique, valid names
	}
	return r
}
