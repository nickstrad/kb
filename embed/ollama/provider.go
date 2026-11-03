package ollama

import "github.com/nickstrad/kb/embed"

// Name is the provider name ollama.Provider registers.
const Name = "ollama"

// Provider returns the "ollama" provider: a local or remote Ollama server, nomic-embed-text by
// default, with nomic's task prefixes unless overridden.
//
//	url           {P}OLLAMA_URL (default http://127.0.0.1:11434)
//	doc_prefix    {P}EMBED_DOC_PREFIX (default "search_document: "; set-but-empty means none)
//	query_prefix  {P}EMBED_QUERY_PREFIX (default "search_query: ")
func Provider() embed.Provider {
	return embed.Provider{
		Name:    Name,
		Summary: "a local or remote Ollama server",
		Options: []embed.Option{
			{Key: "url", Env: []string{"{P}OLLAMA_URL"}, Default: DefaultBaseURL, Doc: "Ollama address"},
			{Key: "doc_prefix", Default: embed.DocPrefix, KeepEmpty: true, Doc: "prefix for stored documents"},
			{Key: "query_prefix", Default: embed.QueryPrefix, KeepEmpty: true, Doc: "prefix for queries"},
		},
		New: func(cfg embed.Config) (embed.Embedder, error) {
			// newClient, not New: the registry path must not fall back to KB_* variables.
			c := newClient(cfg.Get("url"), cfg.Model, cfg.Dim)
			c.DocPrefix, c.QueryPrefix = embed.DocPrefix, embed.QueryPrefix
			if v, ok := cfg.Lookup("doc_prefix"); ok {
				c.DocPrefix = v
			}
			if v, ok := cfg.Lookup("query_prefix"); ok {
				c.QueryPrefix = v
			}
			return c, nil
		},
	}
}
