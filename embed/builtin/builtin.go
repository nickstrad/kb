// Package builtin registers every provider that ships with the public embed module:
//
//	http     a generic HTTP embeddings API (embed/httpembed)
//	command  a user-supplied program speaking JSON on stdin/stdout (embed/command)
//	none     no vectors; full-text only
//
// None of them downloads or loads model weights in-process, and none depends on Hugging Face.
// NewRegistry sets no Default: a program names the provider it wants, or sets Registry.Default.
package builtin

import (
	"github.com/nickstrad/kb/embed"
	"github.com/nickstrad/kb/embed/command"
	"github.com/nickstrad/kb/embed/httpembed"
)

// Providers returns the built-in providers in registration order.
func Providers() []embed.Provider {
	return []embed.Provider{
		httpembed.Provider(),
		command.Provider(),
		embed.NoneProvider(),
	}
}

// Register adds the built-in providers to r.
func Register(r *embed.Registry) error {
	for _, p := range Providers() {
		if err := r.Register(p); err != nil {
			return err
		}
	}
	return nil
}

// NewRegistry returns a registry holding the built-in providers.
func NewRegistry() *embed.Registry {
	r := embed.NewRegistry()
	if err := Register(r); err != nil {
		panic(err) // the built-in set has unique, valid names
	}
	return r
}
