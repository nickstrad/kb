// Package openai implements embed.Embedder against any server that speaks the OpenAI embeddings
// API: OpenRouter, OpenAI, llama.cpp's llama-server, LM Studio, and Ollama's own /v1 endpoint.
//
// It calls POST {baseURL}/embeddings with {"model": <model>, "input": [...], "dimensions": <dim>}
// and an "Authorization: Bearer <key>" header when a key is set, and expects
// {"data": [{"index": i, "embedding": [...]}, ...]}. The response is put back in input order by
// index. dimensions asks a model that can shorten its vectors (OpenAI's text-embedding-3 family,
// Gemini, Qwen3) for exactly Dim numbers; SendDimensions=false leaves it out for a server that
// rejects the field, in which case the model must already produce Dim numbers.
//
// Requests larger than 16 texts are split into successive calls. Every call gets a deadline of
// BaseTimeout + PerTextTimeout*n on top of the caller's own ctx, as in embed/ollama.
//
// Errors: a transport failure, the client's own deadline, a 5xx, a 408 or a 429 wraps
// embed.ErrUnavailable, so a hybrid search degrades to FTS. Any other 4xx (a bad key, an unknown
// model, a rejected field) is a plain error, since falling back would hide a configuration
// mistake.
package openai

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math"
	"net/http"
	"sort"
	"strings"
	"time"

	"knowledge/kb/internal/embed"
)

// maxBatch is the largest number of texts sent in one /embeddings call.
const maxBatch = 16

// Default per-call deadline budget: DefaultBaseTimeout + DefaultPerTextTimeout*n.
const (
	DefaultBaseTimeout    = 30 * time.Second
	DefaultPerTextTimeout = 5 * time.Second
)

// Config is everything New needs. BaseURL and Model are required and Dim must be positive;
// APIKey may be empty for a local server that takes none.
type Config struct {
	BaseURL        string
	APIKey         string
	Model          string
	Dim            int
	SendDimensions bool
	// Truncate shortens a longer returned vector to Dim numbers and re-normalises it. That is
	// only sound for a model trained to be shortened (OpenAI's text-embedding-3 family); it covers
	// a router that drops the dimensions field on the way upstream.
	Truncate    bool
	DocPrefix   string
	QueryPrefix string
}

// Client is an embed.Embedder backed by an OpenAI-compatible embeddings endpoint.
type Client struct {
	cfg  Config
	http *http.Client

	// BaseTimeout and PerTextTimeout set the per-call deadline: BaseTimeout + PerTextTimeout*n.
	BaseTimeout    time.Duration
	PerTextTimeout time.Duration
}

// New validates cfg and returns a Client for it.
func New(cfg Config) (*Client, error) {
	cfg.BaseURL = strings.TrimRight(cfg.BaseURL, "/")
	switch {
	case cfg.BaseURL == "":
		return nil, errors.New("openai embedder: base URL is required")
	case cfg.Model == "":
		return nil, errors.New("openai embedder: model is required")
	case cfg.Dim <= 0:
		return nil, fmt.Errorf("openai embedder: dimension must be positive, got %d", cfg.Dim)
	}
	return &Client{
		cfg:            cfg,
		http:           &http.Client{},
		BaseTimeout:    DefaultBaseTimeout,
		PerTextTimeout: DefaultPerTextTimeout,
	}, nil
}

// Model reports the configured model name.
func (c *Client) Model() string { return c.cfg.Model }

// Dim reports the configured vector length.
func (c *Client) Dim() int { return c.cfg.Dim }

// Prefixes implements embed.Prefixer.
func (c *Client) Prefixes() (doc, query string) { return c.cfg.DocPrefix, c.cfg.QueryPrefix }

// BaseURL reports the endpoint base in effect.
func (c *Client) BaseURL() string { return c.cfg.BaseURL }

// Embed turns texts into vectors, one per input, in order. Empty input makes no request.
func (c *Client) Embed(ctx context.Context, texts []string) ([][]float32, error) {
	if len(texts) == 0 {
		return [][]float32{}, nil
	}
	out := make([][]float32, 0, len(texts))
	for start := 0; start < len(texts); start += maxBatch {
		end := min(start+maxBatch, len(texts))
		vecs, err := c.embedBatch(ctx, texts[start:end])
		if err != nil {
			return nil, err
		}
		out = append(out, vecs...)
	}
	return out, nil
}

// Ping embeds one short text, since an OpenAI-style API has no cheap "is this model here" call.
// It checks the key, the model name and the returned dimension in one go.
func (c *Client) Ping(ctx context.Context) error {
	_, err := c.embedBatch(ctx, []string{"ping"})
	return err
}

type embedRequest struct {
	Model      string   `json:"model"`
	Input      []string `json:"input"`
	Dimensions int      `json:"dimensions,omitempty"`
}

type embedResponse struct {
	Data []struct {
		Index     int       `json:"index"`
		Embedding []float32 `json:"embedding"`
	} `json:"data"`
}

func (c *Client) embedBatch(ctx context.Context, texts []string) ([][]float32, error) {
	req := embedRequest{Model: c.cfg.Model, Input: texts}
	if c.cfg.SendDimensions {
		req.Dimensions = c.cfg.Dim
	}
	reqBody, err := json.Marshal(req)
	if err != nil {
		return nil, fmt.Errorf("openai embedder: encoding request: %w", err)
	}

	body, status, err := c.post(ctx, reqBody, len(texts))
	if err != nil {
		return nil, err
	}
	url := c.cfg.BaseURL + "/embeddings"
	switch {
	case status >= 500, status == http.StatusTooManyRequests, status == http.StatusRequestTimeout:
		return nil, fmt.Errorf("%s returned %s: %w", url, extractError(body, status), embed.ErrUnavailable)
	case status >= 400:
		return nil, fmt.Errorf("%s returned %s", url, extractError(body, status))
	}

	var parsed embedResponse
	if err := json.Unmarshal(body, &parsed); err != nil {
		return nil, fmt.Errorf("openai embedder: parsing response from %s: %w", url, err)
	}
	if len(parsed.Data) != len(texts) {
		return nil, fmt.Errorf("openai embedder: expected %d embeddings for %d inputs, got %d (%s)",
			len(texts), len(texts), len(parsed.Data), extractError(body, status))
	}
	sort.SliceStable(parsed.Data, func(i, j int) bool { return parsed.Data[i].Index < parsed.Data[j].Index })
	out := make([][]float32, len(parsed.Data))
	for i, d := range parsed.Data {
		if c.cfg.Truncate && len(d.Embedding) > c.cfg.Dim {
			d.Embedding = shorten(d.Embedding, c.cfg.Dim)
		}
		if len(d.Embedding) != c.cfg.Dim {
			return nil, fmt.Errorf("openai embedder: embedding %d has dimension %d, want %d (model %s); "+
				"pick a model that supports the dimensions field or set KB_EMBED_DIM to its native size",
				i, len(d.Embedding), c.cfg.Dim, c.cfg.Model)
		}
		out[i] = d.Embedding
	}
	return out, nil
}

// post sends one request with a deadline of BaseTimeout + PerTextTimeout*n layered onto ctx.
func (c *Client) post(ctx context.Context, jsonBody []byte, n int) ([]byte, int, error) {
	timeout := c.BaseTimeout + c.PerTextTimeout*time.Duration(n)
	callCtx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()

	url := c.cfg.BaseURL + "/embeddings"
	req, err := http.NewRequestWithContext(callCtx, http.MethodPost, url, bytes.NewReader(jsonBody))
	if err != nil {
		return nil, 0, fmt.Errorf("openai embedder: building request: %w", err)
	}
	req.Header.Set("Content-Type", "application/json")
	if c.cfg.APIKey != "" {
		req.Header.Set("Authorization", "Bearer "+c.cfg.APIKey)
	}

	resp, err := c.http.Do(req)
	if err != nil {
		return nil, 0, classifyTransportErr(ctx, callCtx, err, url, timeout)
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, 0, classifyTransportErr(ctx, callCtx, err, url, timeout)
	}
	return body, resp.StatusCode, nil
}

// classifyTransportErr reports the caller's own cancelled ctx as itself, and anything else (the
// client's deadline, connection refused, DNS failure) as embed.ErrUnavailable.
func classifyTransportErr(origCtx, callCtx context.Context, err error, url string, timeout time.Duration) error {
	if origCtx.Err() != nil {
		return fmt.Errorf("openai embedder: %w", origCtx.Err())
	}
	if errors.Is(callCtx.Err(), context.DeadlineExceeded) {
		return fmt.Errorf("request to %s timed out after %s: %w", url, timeout, embed.ErrUnavailable)
	}
	return fmt.Errorf("embedder unreachable at %s (%v): %w", url, err, embed.ErrUnavailable)
}

// extractError renders an error body as a message: OpenAI and OpenRouter send
// {"error": {"message": "..."}}, some servers send {"error": "..."}, anything else is shown raw.
func extractError(body []byte, status int) string {
	var nested struct {
		Error struct {
			Message string `json:"message"`
		} `json:"error"`
	}
	if json.Unmarshal(body, &nested) == nil && nested.Error.Message != "" {
		return fmt.Sprintf("%s (status %d)", nested.Error.Message, status)
	}
	var flat struct {
		Error string `json:"error"`
	}
	if json.Unmarshal(body, &flat) == nil && flat.Error != "" {
		return fmt.Sprintf("%s (status %d)", flat.Error, status)
	}
	msg := strings.TrimSpace(string(body))
	if len(msg) > 300 {
		msg = msg[:300] + "…"
	}
	if msg == "" {
		msg = "no error body"
	}
	return fmt.Sprintf("%s (status %d)", msg, status)
}

// shorten keeps the first dim numbers of v and scales them back to unit length.
func shorten(v []float32, dim int) []float32 {
	out := make([]float32, dim)
	copy(out, v[:dim])
	var sum float64
	for _, x := range out {
		sum += float64(x) * float64(x)
	}
	if sum == 0 {
		return out
	}
	scale := float32(1 / math.Sqrt(sum))
	for i := range out {
		out[i] *= scale
	}
	return out
}
