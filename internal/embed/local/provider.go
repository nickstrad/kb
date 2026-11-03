package local

import (
	"strings"

	pub "github.com/nickstrad/kb/embed"
)

// ProviderName is the registry name of the in-process embedder: kb's default provider.
const ProviderName = "local"

// Provider returns kb's in-process embedder as a registry provider. The model is a supported id
// (empty means DefaultModel); cacheDir is asked for the shared model directory only when the
// provider is built, so selecting another provider never touches it. The embedder is New's, as
// is: Provider loads and downloads nothing.
func Provider(cacheDir func() (string, error)) pub.Provider {
	return pub.Provider{
		Name:    ProviderName,
		Summary: "the in-process model (rembed), pulled once into the shared model cache by kb reindex/add/init",
		New: func(cfg pub.Config) (pub.Embedder, error) {
			id := strings.TrimSpace(cfg.Model)
			if id == "" {
				id = DefaultModel
			}
			spec, err := Lookup(id)
			if err != nil {
				return nil, err
			}
			dir, err := cacheDir()
			if err != nil {
				return nil, err
			}
			return New(spec, dir), nil
		},
	}
}
