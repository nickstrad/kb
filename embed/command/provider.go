package command

import (
	"fmt"
	"strings"
	"time"

	"github.com/nickstrad/kb/embed"
)

// Name is the provider name Provider registers.
const Name = "command"

// Provider returns the "command" provider. Options, each read from {P}EMBED_<KEY> when the
// registry reads an environment:
//
//	program       the program to run (required), as a path or a name looked up on PATH; it is
//	              run directly, without a shell
//	args          its arguments, separated by spaces (wrap anything fancier in a script)
//	batch         texts per run (default 64)
//	timeout       base seconds per run (default 60, plus 5 per text)
//	doc_prefix, query_prefix  task prefixes (default none)
//
// The model and a positive Dim are required: both are passed to the program, and the model is
// part of the vectors' identity.
func Provider() embed.Provider {
	return embed.Provider{
		Name:    Name,
		Summary: "a user-supplied program that reads texts as JSON on stdin and writes vectors on stdout",
		Options: []embed.Option{
			{Key: "program", Doc: "program to run, without a shell"},
			{Key: "args", Doc: "arguments, separated by spaces"},
			{Key: "batch", Doc: "texts per run"},
			{Key: "timeout", Doc: "base seconds per run"},
			{Key: "doc_prefix", KeepEmpty: true, Doc: "prefix for stored documents"},
			{Key: "query_prefix", KeepEmpty: true, Doc: "prefix for queries"},
		},
		New: func(cfg embed.Config) (embed.Embedder, error) {
			program := strings.TrimSpace(cfg.Get("program"))
			if program == "" {
				return nil, fmt.Errorf("embedder command needs a program (option program)")
			}
			if cfg.Model == "" {
				return nil, fmt.Errorf("embedder command needs a model name")
			}
			batch, err := cfg.Int("batch", DefaultBatch)
			if err != nil {
				return nil, err
			}
			secs, err := cfg.Int("timeout", int(DefaultBaseTimeout/time.Second))
			if err != nil {
				return nil, err
			}
			argv := append([]string{program}, strings.Fields(cfg.Get("args"))...)
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
