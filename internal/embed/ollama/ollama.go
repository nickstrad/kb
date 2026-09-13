// Package ollama implements embed.Embedder against a local Ollama server
// (see /root/Raw/knowledge/ollama-local-embeddings.md for the service facts on this droplet).
//
// It calls POST /api/embed with {"model": <model>, "input": [...], "truncate": false} and
// expects {"embeddings": [[...], ...]}, one vector per input in order. truncate is pinned to
// false: the served context is 2048 tokens and the default (true) silently truncates an
// over-long input and still returns a plausible-looking vector, which is worse than a loud 400.
// Requests larger than 16 texts are split into successive calls of at most 16 so callers can
// pass any batch size to Embed.
//
// Configuration: New(baseURL, model string, dim int) falls back, for each empty/zero argument,
// to the environment variables KB_OLLAMA_URL and KB_EMBED_MODEL (checked only when the
// corresponding argument is empty), then to the package defaults (http://127.0.0.1:11434,
// nomic-embed-text, 768). The CLI always calls New("", "", 0), so in practice KB_OLLAMA_URL and
// KB_EMBED_MODEL are the real configuration knobs; dim has no environment override since it is a
// property of whatever model is selected.
//
// Timeouts: there is no fixed http.Client.Timeout, since a real batch of 16 large chunks has been
// measured at 16-39s warm on this box and a flat 30s would abort it. Instead every call gets a
// deadline derived from Client.BaseTimeout + Client.PerTextTimeout*n (n = batch size for Embed, 0
// for Ping), layered on top of the caller's own ctx. Reindex can raise BaseTimeout/PerTextTimeout
// on the Client before a known-large run.
package ollama

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"strings"
	"time"
)

// Defaults used by New when an argument is empty/zero and the corresponding environment variable
// (for baseURL and model) is also unset.
const (
	DefaultBaseURL = "http://127.0.0.1:11434"
	DefaultModel   = "nomic-embed-text"
	DefaultDim     = 768
)

// maxBatch is the largest number of texts sent to Ollama in one /api/embed call. Embed splits
// larger inputs into successive calls of at most this many texts each.
const maxBatch = 16

// Default per-call deadline budget: DefaultBaseTimeout + DefaultPerTextTimeout*n, n being the
// number of texts in the call (0 for Ping). See the package comment.
const (
	DefaultBaseTimeout    = 30 * time.Second
	DefaultPerTextTimeout = 10 * time.Second
)

// ErrUnavailable marks failures that mean the embedder cannot be reached or used at all:
// connection refused, DNS failure, a deadline the client itself imposed (BaseTimeout/
// PerTextTimeout), or a 5xx response. Callers use errors.Is(err, ErrUnavailable) to decide
// whether to fall back to FTS-only search (exit code 2 / mode fts-fallback, per plan.md's CLI
// section). It does NOT cover the caller's own ctx being cancelled or already expired (that
// wraps ctx.Err() instead, since the caller controls it and knows why) or a 4xx response with an
// Ollama error body (e.g. model not found), since retrying or falling back would not help either.
var ErrUnavailable = errors.New("embedder unavailable")

// ErrModelMissing marks a Ping failure where Ollama is reachable but the configured model is not
// present. Callers use errors.Is(err, ErrModelMissing) instead of matching the message text.
var ErrModelMissing = errors.New("model not found")

// Client is an embed.Embedder backed by a local Ollama server.
type Client struct {
	baseURL string
	model   string
	dim     int
	http    *http.Client

	// BaseTimeout and PerTextTimeout set the per-call deadline: BaseTimeout + PerTextTimeout*n,
	// where n is the number of texts in the call (0 for Ping). New sets both to the Default*
	// constants; a caller such as the reindexer may raise them before a large or slow run.
	BaseTimeout    time.Duration
	PerTextTimeout time.Duration
}

// New returns a Client configured from its arguments, then environment variables, then defaults.
// See the package comment for the precedence rule.
func New(baseURL, model string, dim int) *Client {
	if baseURL == "" {
		baseURL = os.Getenv("KB_OLLAMA_URL")
	}
	if baseURL == "" {
		baseURL = DefaultBaseURL
	}
	if model == "" {
		model = os.Getenv("KB_EMBED_MODEL")
	}
	if model == "" {
		model = DefaultModel
	}
	if dim <= 0 {
		dim = DefaultDim
	}
	return &Client{
		baseURL:        strings.TrimRight(baseURL, "/"),
		model:          model,
		dim:            dim,
		http:           &http.Client{},
		BaseTimeout:    DefaultBaseTimeout,
		PerTextTimeout: DefaultPerTextTimeout,
	}
}

// Model reports the configured model name.
func (c *Client) Model() string { return c.model }

// Dim reports the configured vector length.
func (c *Client) Dim() int { return c.dim }

// timeoutFor returns the per-call deadline budget for a call carrying n texts (0 for Ping).
func (c *Client) timeoutFor(n int) time.Duration {
	return c.BaseTimeout + c.PerTextTimeout*time.Duration(n)
}

// Embed turns texts into vectors, one per input, in order. Empty input returns an empty slice
// without making a request. Inputs longer than maxBatch are split into successive /api/embed
// calls.
func (c *Client) Embed(ctx context.Context, texts []string) ([][]float32, error) {
	if len(texts) == 0 {
		return [][]float32{}, nil
	}
	out := make([][]float32, 0, len(texts))
	for start := 0; start < len(texts); start += maxBatch {
		end := start + maxBatch
		if end > len(texts) {
			end = len(texts)
		}
		vecs, err := c.embedBatch(ctx, texts[start:end])
		if err != nil {
			return nil, err
		}
		out = append(out, vecs...)
	}
	return out, nil
}

type embedRequest struct {
	Model    string   `json:"model"`
	Input    []string `json:"input"`
	Truncate bool     `json:"truncate"`
}

type embedResponse struct {
	Embeddings [][]float32 `json:"embeddings"`
}

type errorResponse struct {
	Error string `json:"error"`
}

func (c *Client) embedBatch(ctx context.Context, texts []string) ([][]float32, error) {
	reqBody, err := json.Marshal(embedRequest{Model: c.model, Input: texts, Truncate: false})
	if err != nil {
		return nil, fmt.Errorf("ollama: encoding /api/embed request: %w", err)
	}

	body, status, err := c.doRequest(ctx, http.MethodPost, "/api/embed", reqBody, len(texts))
	if err != nil {
		return nil, err
	}

	if status >= 500 {
		return nil, fmt.Errorf("ollama returned %d from %s/api/embed: %w", status, c.baseURL, ErrUnavailable)
	}
	if status >= 400 {
		return nil, fmt.Errorf("ollama: %s", extractError(body, status))
	}

	var parsed embedResponse
	if err := json.Unmarshal(body, &parsed); err != nil {
		return nil, fmt.Errorf("ollama: parsing /api/embed response: %w", err)
	}
	if len(parsed.Embeddings) != len(texts) {
		return nil, fmt.Errorf("ollama: expected %d embeddings for %d inputs, got %d", len(texts), len(texts), len(parsed.Embeddings))
	}
	for i, v := range parsed.Embeddings {
		if len(v) != c.dim {
			return nil, fmt.Errorf("ollama: embedding %d has dimension %d, want %d (model %s)", i, len(v), c.dim, c.model)
		}
	}
	return parsed.Embeddings, nil
}

// tagsResponse is the shape of GET /api/tags.
type tagsResponse struct {
	Models []struct {
		Name string `json:"name"`
	} `json:"models"`
}

// Ping checks that Ollama is reachable and that the configured model is loaded/pullable, via
// GET /api/tags. A connection failure wraps ErrUnavailable; a reachable server missing the model
// returns an error wrapping ErrModelMissing that tells the user to run `ollama pull <model>`.
func (c *Client) Ping(ctx context.Context) error {
	body, status, err := c.doRequest(ctx, http.MethodGet, "/api/tags", nil, 0)
	if err != nil {
		return err
	}

	if status >= 500 {
		return fmt.Errorf("ollama returned %d from %s/api/tags: %w", status, c.baseURL, ErrUnavailable)
	}
	if status >= 400 {
		return fmt.Errorf("ollama: %s", extractError(body, status))
	}

	var tags tagsResponse
	if err := json.Unmarshal(body, &tags); err != nil {
		return fmt.Errorf("ollama: parsing /api/tags response: %w", err)
	}
	for _, m := range tags.Models {
		if modelNameMatches(m.Name, c.model) {
			return nil
		}
	}
	return fmt.Errorf("ollama: model %q not found on %s; run `ollama pull %s`: %w", c.model, c.baseURL, c.model, ErrModelMissing)
}

// modelNameMatches reports whether an /api/tags entry name (e.g. "nomic-embed-text:latest")
// identifies the configured model name (e.g. "nomic-embed-text"), tolerating an implicit
// ":latest" tag on either side only. A different tag (e.g. "nomic-embed-text:q4_0") does NOT
// match: Ping must fail the same way Embed would (Embed always asks Ollama for exactly c.model,
// with no tag substitution).
func modelNameMatches(name, want string) bool {
	return name == want || name == want+":latest" || want == name+":latest"
}

// doRequest sends one HTTP request with a deadline of c.timeoutFor(n) layered onto ctx, and
// returns the response body and status code. Transport-level failures are classified by
// classifyTransportErr.
func (c *Client) doRequest(ctx context.Context, method, path string, jsonBody []byte, n int) ([]byte, int, error) {
	timeout := c.timeoutFor(n)
	callCtx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()

	var bodyReader io.Reader
	if jsonBody != nil {
		bodyReader = bytes.NewReader(jsonBody)
	}
	req, err := http.NewRequestWithContext(callCtx, method, c.baseURL+path, bodyReader)
	if err != nil {
		return nil, 0, fmt.Errorf("ollama: building %s request: %w", path, err)
	}
	if jsonBody != nil {
		req.Header.Set("Content-Type", "application/json")
	}

	resp, err := c.http.Do(req)
	if err != nil {
		return nil, 0, c.classifyTransportErr(ctx, callCtx, err, path, timeout, n)
	}
	defer resp.Body.Close()

	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, 0, c.classifyTransportErr(ctx, callCtx, err, path, timeout, n)
	}
	return body, resp.StatusCode, nil
}

// classifyTransportErr turns a failure from doRequest's http.Client.Do or body read into one of
// three kinds of error, in priority order:
//  1. The caller's own ctx was already cancelled or expired: return a plain error wrapping
//     ctx.Err(), since the caller controls that ctx and knows why.
//  2. The deadline this Client itself imposed (BaseTimeout + PerTextTimeout*n) was exceeded:
//     ErrUnavailable, but with a message about the client's own timeout budget, not "check
//     ollama.service" (nothing was necessarily wrong with the service).
//  3. Anything else (connection refused, DNS failure, connection reset mid-response, ...):
//     ErrUnavailable with the systemd-check message.
func (c *Client) classifyTransportErr(origCtx, callCtx context.Context, err error, path string, timeout time.Duration, n int) error {
	if origCtx.Err() != nil {
		return fmt.Errorf("ollama: %w", origCtx.Err())
	}
	if errors.Is(callCtx.Err(), context.DeadlineExceeded) {
		if n > 0 {
			return fmt.Errorf("ollama request to %s%s timed out after %s (batch of %d texts): %w", c.baseURL, path, timeout, n, ErrUnavailable)
		}
		return fmt.Errorf("ollama request to %s%s timed out after %s: %w", c.baseURL, path, timeout, ErrUnavailable)
	}
	return c.unavailableErr(err)
}

// unavailableErr wraps a transport-level failure (connection refused, DNS failure, ...) in
// ErrUnavailable with a message pointing at the local systemd unit, per plan.md D2.
func (c *Client) unavailableErr(err error) error {
	return fmt.Errorf("ollama unreachable at %s (%v): check ollama.service is running on 127.0.0.1:11434 (systemctl status ollama): %w", c.baseURL, err, ErrUnavailable)
}

// extractError renders a non-2xx HTTP body as a message: the Ollama {"error": "..."} field when
// present, otherwise the raw body.
func extractError(body []byte, status int) string {
	var e errorResponse
	if err := json.Unmarshal(body, &e); err == nil && e.Error != "" {
		return fmt.Sprintf("%s (status %d)", e.Error, status)
	}
	msg := strings.TrimSpace(string(body))
	if msg == "" {
		msg = "no error body"
	}
	return fmt.Sprintf("%s (status %d)", msg, status)
}
