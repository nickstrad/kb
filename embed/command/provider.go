package command

import (
	"fmt"
	"strings"
	"time"

	"github.com/nickstrad/kb/embed"
)

// Name is the provider name command.Provider registers.
const Name = "command"

// Provider returns the "command" provider.
//
//	command       {P}EMBED_COMMAND (required): the program and its arguments, split on spaces
//	              (no shell; wrap anything fancier in a script)
//	batch         {P}EMBED_BATCH: texts per run (default 64)
//	timeout       {P}EMBED_TIMEOUT: base seconds per run (default 60, plus 5 per text)
//	doc_prefix    {P}EMBED_DOC_PREFIX (default none)
//	query_prefix  {P}EMBED_QUERY_PREFIX (default none)
//
// The model is required: it is passed to the program and recorded in embed_meta.
func Provider() embed.Provider {
	return embed.Provider{
		Name:    Name,
		Summary: "a user-supplied program that reads texts as JSON on stdin and writes vectors on stdout",
		Options: []embed.Option{
			{Key: "command", Doc: "program and arguments, split on spaces"},
			{Key: "batch", Doc: "texts per run"},
			{Key: "timeout", Doc: "base seconds per run"},
			{Key: "doc_prefix", KeepEmpty: true, Doc: "prefix for stored documents"},
			{Key: "query_prefix", KeepEmpty: true, Doc: "prefix for queries"},
		},
		New: func(cfg embed.Config) (embed.Embedder, error) {
			argv := strings.Fields(cfg.Get("command"))
			if len(argv) == 0 {
				return nil, fmt.Errorf("embedder command needs a program: set KB_EMBED_COMMAND")
			}
			if cfg.Model == "" {
				return nil, fmt.Errorf("embedder command needs a model name: set KB_EMBED_MODEL or pass --embed-model")
			}
			batch, err := cfg.Int("batch", DefaultBatch)
			if err != nil {
				return nil, err
			}
			secs, err := cfg.Int("timeout", int(DefaultBaseTimeout/time.Second))
			if err != nil {
				return nil, err
			}
			c, err := New(Config{
				Argv: argv, Model: cfg.Model, Dim: cfg.Dim, Batch: batch,
				DocPrefix: cfg.Get("doc_prefix"), QueryPrefix: cfg.Get("query_prefix"),
			})
			if err != nil {
				return nil, err
			}
			if secs > 0 {
				c.BaseTimeout = time.Duration(secs) * time.Second
			}
			return c, nil
		},
	}
}
