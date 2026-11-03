// Package command implements embed.Embedder by running a user-supplied program and exchanging
// JSON with it. It is the bring-your-own-model provider: the program can wrap any local model
// (ONNX Runtime, llama.cpp, a Python script with sentence-transformers, a company-internal tool,
// or a thin client to a model server the user already runs), and kb never downloads or loads
// model weights itself.
//
// Why a command rather than another HTTP shape: the HTTP providers (embed/openai's "http" and
// "openai") already cover a user-run local server. What they cannot cover is a model that is only
// reachable as a program, for example on a locked-down work machine where nothing may listen on
// a port, or where the only sanctioned runtime is a CLI. A process boundary also keeps the
// model's runtime, dependencies and memory out of the Go binary entirely.
//
// Protocol, one process per batch: the program is started with the configured argv (no shell),
// receives one JSON object on stdin and must write one JSON object on stdout, then exit 0.
//
//	stdin:  {"model": "<model>", "dim": 768, "texts": ["...", "..."]}
//	stdout: {"embeddings": [[0.1, ...], [0.2, ...]]}   one vector per text, in order, each dim long
//	   or:  {"error": "message"}                          a configuration or input problem
//
// Inputs larger than Config.Batch texts are split into successive runs. Each run gets a deadline
// of BaseTimeout + PerTextTimeout*n on top of the caller's ctx. A program that loads a large
// model on every start is slow; for heavy models, keep the model resident in a local server and
// make the command a thin client, or use the "http" provider against that server.
//
// Errors: the program exiting non-zero, being killed by the client's own deadline, or failing to
// start for a reason other than "not found" wraps embed.ErrUnavailable, so a hybrid search falls
// back to FTS. A missing program, an {"error": ...} reply, malformed JSON, or the wrong number or
// length of vectors is a plain error, since falling back would hide a configuration mistake.
package command

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os/exec"
	"strings"
	"time"

	"github.com/nickstrad/kb/embed"
)

// Defaults.
const (
	DefaultBatch          = 64
	DefaultBaseTimeout    = 60 * time.Second
	DefaultPerTextTimeout = 5 * time.Second
)

// Config is everything New needs. Argv and Model are required and Dim must be positive.
type Config struct {
	// Argv is the program and its arguments. It is run directly, not through a shell.
	Argv        []string
	Model       string
	Dim         int
	Batch       int // texts per run; <= 0 means DefaultBatch
	DocPrefix   string
	QueryPrefix string
	// Env, when non-nil, is the program's whole environment; nil inherits the caller's.
	Env []string
}

// Client is an embed.Embedder backed by an external program.
type Client struct {
	cfg Config

	// BaseTimeout and PerTextTimeout set the per-run deadline: BaseTimeout + PerTextTimeout*n.
	BaseTimeout    time.Duration
	PerTextTimeout time.Duration
}

// New validates cfg and returns a Client for it.
func New(cfg Config) (*Client, error) {
	switch {
	case len(cfg.Argv) == 0 || cfg.Argv[0] == "":
		return nil, errors.New("command embedder: a command is required")
	case cfg.Model == "":
		return nil, errors.New("command embedder: a model name is required (it identifies the vectors in embed_meta)")
	case cfg.Dim <= 0:
		return nil, fmt.Errorf("command embedder: dimension must be positive, got %d", cfg.Dim)
	}
	if cfg.Batch <= 0 {
		cfg.Batch = DefaultBatch
	}
	return &Client{cfg: cfg, BaseTimeout: DefaultBaseTimeout, PerTextTimeout: DefaultPerTextTimeout}, nil
}

// Model reports the configured model name.
func (c *Client) Model() string { return c.cfg.Model }

// Dim reports the configured vector length.
func (c *Client) Dim() int { return c.cfg.Dim }

// Prefixes implements embed.Prefixer.
func (c *Client) Prefixes() (doc, query string) { return c.cfg.DocPrefix, c.cfg.QueryPrefix }

// Command reports the program in effect, for diagnostics.
func (c *Client) Command() string { return strings.Join(c.cfg.Argv, " ") }

// Embed turns texts into vectors, one per input, in order. Empty input runs nothing.
func (c *Client) Embed(ctx context.Context, texts []string) ([][]float32, error) {
	out := make([][]float32, 0, len(texts))
	for start := 0; start < len(texts); start += c.cfg.Batch {
		end := min(start+c.cfg.Batch, len(texts))
		vecs, err := c.run(ctx, texts[start:end])
		if err != nil {
			return nil, err
		}
		out = append(out, vecs...)
	}
	return out, nil
}

// Ping embeds one short text, checking that the program starts, answers and returns Dim numbers.
func (c *Client) Ping(ctx context.Context) error {
	_, err := c.run(ctx, []string{"ping"})
	return err
}

type request struct {
	Model string   `json:"model"`
	Dim   int      `json:"dim"`
	Texts []string `json:"texts"`
}

type response struct {
	Embeddings [][]float32 `json:"embeddings"`
	Error      string      `json:"error"`
}

func (c *Client) run(ctx context.Context, texts []string) ([][]float32, error) {
	in, err := json.Marshal(request{Model: c.cfg.Model, Dim: c.cfg.Dim, Texts: texts})
	if err != nil {
		return nil, fmt.Errorf("command embedder: encoding request: %w", err)
	}
	timeout := c.BaseTimeout + c.PerTextTimeout*time.Duration(len(texts))
	callCtx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()

	cmd := exec.CommandContext(callCtx, c.cfg.Argv[0], c.cfg.Argv[1:]...)
	cmd.Env = c.cfg.Env
	cmd.Stdin = bytes.NewReader(in)
	var stdout, stderr bytes.Buffer
	cmd.Stdout, cmd.Stderr = &stdout, &stderr
	cmd.WaitDelay = time.Second

	name := c.cfg.Argv[0]
	if err := cmd.Run(); err != nil {
		switch {
		case ctx.Err() != nil:
			return nil, fmt.Errorf("command embedder %s: %w", name, ctx.Err())
		case callCtx.Err() != nil:
			return nil, fmt.Errorf("command embedder %s: no answer within %s: %w", name, timeout, embed.ErrUnavailable)
		case errors.Is(err, exec.ErrNotFound), errors.Is(err, fs.ErrNotExist):
			// A PATH miss or a program path that does not exist is a configuration mistake,
			// not a temporary outage, so it is never ErrUnavailable.
			return nil, fmt.Errorf("command embedder: %w", err)
		}
		var exit *exec.ExitError
		if errors.As(err, &exit) {
			if msg := replyError(stdout.Bytes()); msg != "" {
				return nil, fmt.Errorf("command embedder %s: %s", name, msg)
			}
			return nil, fmt.Errorf("command embedder %s exited with %d: %s: %w",
				name, exit.ExitCode(), tail(stderr.String()), embed.ErrUnavailable)
		}
		return nil, fmt.Errorf("command embedder %s: %v: %w", name, err, embed.ErrUnavailable)
	}

	var resp response
	if err := json.Unmarshal(stdout.Bytes(), &resp); err != nil {
		return nil, fmt.Errorf("command embedder %s: stdout is not the expected JSON (%v): %q", name, err, tail(stdout.String()))
	}
	if resp.Error != "" {
		return nil, fmt.Errorf("command embedder %s: %s", name, resp.Error)
	}
	if len(resp.Embeddings) != len(texts) {
		return nil, fmt.Errorf("command embedder %s: expected %d embeddings for %d inputs, got %d",
			name, len(texts), len(texts), len(resp.Embeddings))
	}
	for i, v := range resp.Embeddings {
		if len(v) != c.cfg.Dim {
			return nil, fmt.Errorf("command embedder %s: embedding %d has dimension %d, want %d (model %s)",
				name, i, len(v), c.cfg.Dim, c.cfg.Model)
		}
	}
	return resp.Embeddings, nil
}

// replyError returns the "error" field of a JSON reply, if there is one.
func replyError(b []byte) string {
	var r response
	if json.Unmarshal(b, &r) == nil {
		return r.Error
	}
	return ""
}

func tail(s string) string {
	s = strings.TrimSpace(s)
	if len(s) > 500 {
		s = "..." + s[len(s)-500:]
	}
	if s == "" {
		return "(no output)"
	}
	return s
}
