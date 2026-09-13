package ollama

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"reflect"
	"strings"
	"testing"
	"time"

	"knowledge/kb/internal/embed"
)

// fakeVector returns a deterministic vector of the given dimension, distinct per index so tests
// can tell vectors apart.
func fakeVector(dim int, seed float32) []float32 {
	v := make([]float32, dim)
	for i := range v {
		v[i] = seed + float32(i)*0.001
	}
	return v
}

// vectorForText derives a deterministic vector from a text's own bytes, distinct per distinct
// text, so a test server can echo per-input vectors and the test can check vecs[i] really came
// from texts[i] rather than merely having the right shape.
func vectorForText(text string, dim int) []float32 {
	var sum float32
	for _, b := range []byte(text) {
		sum += float32(b)
	}
	v := make([]float32, dim)
	for i := range v {
		v[i] = sum + float32(i)*0.01
	}
	return v
}

func TestNewDefaults(t *testing.T) {
	t.Setenv("KB_OLLAMA_URL", "")
	t.Setenv("KB_EMBED_MODEL", "")
	c := New("", "", 0)
	if c.baseURL != DefaultBaseURL {
		t.Errorf("baseURL = %q, want %q", c.baseURL, DefaultBaseURL)
	}
	if c.Model() != DefaultModel {
		t.Errorf("Model() = %q, want %q", c.Model(), DefaultModel)
	}
	if c.Dim() != DefaultDim {
		t.Errorf("Dim() = %d, want %d", c.Dim(), DefaultDim)
	}
	if c.BaseTimeout != DefaultBaseTimeout {
		t.Errorf("BaseTimeout = %v, want %v", c.BaseTimeout, DefaultBaseTimeout)
	}
	if c.PerTextTimeout != DefaultPerTextTimeout {
		t.Errorf("PerTextTimeout = %v, want %v", c.PerTextTimeout, DefaultPerTextTimeout)
	}
}

func TestNewEnvOverrides(t *testing.T) {
	t.Setenv("KB_OLLAMA_URL", "http://example.invalid:1234")
	t.Setenv("KB_EMBED_MODEL", "some-other-model")
	c := New("", "", 0)
	if c.baseURL != "http://example.invalid:1234" {
		t.Errorf("baseURL = %q, want env value", c.baseURL)
	}
	if c.Model() != "some-other-model" {
		t.Errorf("Model() = %q, want env value", c.Model())
	}
	// explicit args still win over env
	c2 := New("http://explicit:1", "explicit-model", 5)
	if c2.baseURL != "http://explicit:1" || c2.Model() != "explicit-model" || c2.Dim() != 5 {
		t.Errorf("explicit args overridden by env: %+v", c2)
	}
}

func TestEmbedHappyPath(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/api/embed" {
			t.Errorf("unexpected path %q", r.URL.Path)
		}
		var req embedRequest
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			t.Fatalf("decode request: %v", err)
		}
		if req.Model != "test-model" {
			t.Errorf("model = %q, want test-model", req.Model)
		}
		if req.Truncate != false {
			t.Errorf("truncate = %v, want false", req.Truncate)
		}
		resp := embedResponse{}
		for i := range req.Input {
			resp.Embeddings = append(resp.Embeddings, fakeVector(3, float32(i)))
		}
		json.NewEncoder(w).Encode(resp)
	}))
	defer srv.Close()

	c := New(srv.URL, "test-model", 3)
	var _ embed.Embedder = c // interface compliance

	vecs, err := c.Embed(context.Background(), []string{"hello", "world"})
	if err != nil {
		t.Fatalf("Embed: %v", err)
	}
	if len(vecs) != 2 {
		t.Fatalf("got %d vectors, want 2", len(vecs))
	}
	for i, v := range vecs {
		if len(v) != 3 {
			t.Errorf("vector %d has len %d, want 3", i, len(v))
		}
	}
}

func TestEmbedEmptyInputNoRequest(t *testing.T) {
	called := false
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		called = true
	}))
	defer srv.Close()

	c := New(srv.URL, "test-model", 3)
	vecs, err := c.Embed(context.Background(), nil)
	if err != nil {
		t.Fatalf("Embed: %v", err)
	}
	if len(vecs) != 0 {
		t.Errorf("got %d vectors, want 0", len(vecs))
	}
	if called {
		t.Error("server was called for empty input")
	}
}

func TestEmbedBatching(t *testing.T) {
	const dim = 3
	var gotSizes []int
	var gotInputs []string

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var req embedRequest
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			t.Fatalf("decode request: %v", err)
		}
		gotSizes = append(gotSizes, len(req.Input))
		gotInputs = append(gotInputs, req.Input...)
		resp := embedResponse{}
		for _, text := range req.Input {
			resp.Embeddings = append(resp.Embeddings, vectorForText(text, dim))
		}
		json.NewEncoder(w).Encode(resp)
	}))
	defer srv.Close()

	c := New(srv.URL, "test-model", dim)
	texts := make([]string, 35)
	for i := range texts {
		texts[i] = fmt.Sprintf("t%d", i)
	}
	vecs, err := c.Embed(context.Background(), texts)
	if err != nil {
		t.Fatalf("Embed: %v", err)
	}
	if len(vecs) != 35 {
		t.Fatalf("got %d vectors, want 35", len(vecs))
	}

	wantSizes := []int{16, 16, 3}
	if !reflect.DeepEqual(gotSizes, wantSizes) {
		t.Fatalf("request sizes = %v, want %v", gotSizes, wantSizes)
	}

	// The concatenation of what each request carried, in order, must equal the original input.
	if !reflect.DeepEqual(gotInputs, texts) {
		t.Fatalf("concatenated request inputs = %v, want %v", gotInputs, texts)
	}

	// Each returned vector must correspond to its own text, not merely have the right shape.
	for i, text := range texts {
		want := vectorForText(text, dim)
		if !reflect.DeepEqual(vecs[i], want) {
			t.Errorf("vecs[%d] (text %q) = %v, want %v", i, text, vecs[i], want)
		}
	}
}

func TestEmbedWrongCount(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		json.NewEncoder(w).Encode(embedResponse{Embeddings: [][]float32{fakeVector(3, 0)}})
	}))
	defer srv.Close()

	c := New(srv.URL, "test-model", 3)
	_, err := c.Embed(context.Background(), []string{"a", "b"})
	if err == nil {
		t.Fatal("expected error for wrong count")
	}
	if errors.Is(err, ErrUnavailable) {
		t.Errorf("wrong-count error should not be ErrUnavailable: %v", err)
	}
}

func TestEmbedWrongDim(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		json.NewEncoder(w).Encode(embedResponse{Embeddings: [][]float32{fakeVector(5, 0)}})
	}))
	defer srv.Close()

	c := New(srv.URL, "test-model", 3)
	_, err := c.Embed(context.Background(), []string{"a"})
	if err == nil {
		t.Fatal("expected error for wrong dim")
	}
	if errors.Is(err, ErrUnavailable) {
		t.Errorf("wrong-dim error should not be ErrUnavailable: %v", err)
	}
}

func TestEmbed500IsUnavailable(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
		w.Write([]byte(`{"error":"internal error"}`))
	}))
	defer srv.Close()

	c := New(srv.URL, "test-model", 3)
	_, err := c.Embed(context.Background(), []string{"a"})
	if err == nil {
		t.Fatal("expected error")
	}
	if !errors.Is(err, ErrUnavailable) {
		t.Errorf("500 should wrap ErrUnavailable, got: %v", err)
	}
}

func TestEmbedConnectionRefusedIsUnavailable(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {}))
	url := srv.URL
	srv.Close() // nothing listens now

	c := New(url, "test-model", 3)
	_, err := c.Embed(context.Background(), []string{"a"})
	if err == nil {
		t.Fatal("expected error")
	}
	if !errors.Is(err, ErrUnavailable) {
		t.Errorf("closed server should wrap ErrUnavailable, got: %v", err)
	}
	if !strings.Contains(err.Error(), "systemctl status ollama") {
		t.Errorf("error should mention systemctl status ollama, got: %v", err)
	}
}

func TestEmbed404WithErrorBodyIsNotUnavailable(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusNotFound)
		w.Write([]byte(`{"error":"model 'nomic-embed-text' not found"}`))
	}))
	defer srv.Close()

	c := New(srv.URL, "test-model", 3)
	_, err := c.Embed(context.Background(), []string{"a"})
	if err == nil {
		t.Fatal("expected error")
	}
	if errors.Is(err, ErrUnavailable) {
		t.Errorf("404 with error body should not be ErrUnavailable: %v", err)
	}
	if !strings.Contains(err.Error(), "not found") {
		t.Errorf("error should contain body message, got: %v", err)
	}
}

// TestEmbedCancelledCtx: a ctx the caller already cancelled must be reported as that ctx's own
// error, not as ErrUnavailable (nothing is actually wrong with the embedder).
func TestEmbedCancelledCtx(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		json.NewEncoder(w).Encode(embedResponse{Embeddings: [][]float32{fakeVector(3, 0)}})
	}))
	defer srv.Close()

	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	c := New(srv.URL, "test-model", 3)
	_, err := c.Embed(ctx, []string{"a"})
	if err == nil {
		t.Fatal("expected error")
	}
	if !errors.Is(err, context.Canceled) {
		t.Errorf("expected errors.Is(err, context.Canceled), got: %v", err)
	}
	if errors.Is(err, ErrUnavailable) {
		t.Errorf("cancelled ctx should not be ErrUnavailable: %v", err)
	}
}

// TestEmbedClientTimeout: a server that outlives the client's own deadline budget must be
// reported as ErrUnavailable (it is a timeout the client imposed), not as context.Canceled.
func TestEmbedClientTimeout(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		time.Sleep(150 * time.Millisecond)
		json.NewEncoder(w).Encode(embedResponse{Embeddings: [][]float32{fakeVector(3, 0)}})
	}))
	defer srv.Close()

	c := New(srv.URL, "test-model", 3)
	c.BaseTimeout = 10 * time.Millisecond
	c.PerTextTimeout = 0

	_, err := c.Embed(context.Background(), []string{"a"})
	if err == nil {
		t.Fatal("expected timeout error")
	}
	if !errors.Is(err, ErrUnavailable) {
		t.Errorf("client timeout should be ErrUnavailable, got: %v", err)
	}
	if errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
		// The error should wrap ErrUnavailable, not be classified as the caller's own ctx problem
		// (the caller's ctx, context.Background(), never expires).
		t.Errorf("client timeout should not be classified as the caller's ctx error: %v", err)
	}
	if !strings.Contains(err.Error(), "timed out") {
		t.Errorf("error should mention a timeout, got: %v", err)
	}
}

func TestPingModelPresent(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/api/tags" {
			t.Errorf("unexpected path %q", r.URL.Path)
		}
		json.NewEncoder(w).Encode(map[string]any{
			"models": []map[string]string{
				{"name": "nomic-embed-text:latest"},
				{"name": "llama3:latest"},
			},
		})
	}))
	defer srv.Close()

	c := New(srv.URL, "nomic-embed-text", 768)
	if err := c.Ping(context.Background()); err != nil {
		t.Fatalf("Ping: %v", err)
	}
}

// TestPingModelPresentDifferentTagFails: a differently-tagged model (e.g. quantized) must NOT
// satisfy Ping, because Embed always requests exactly c.model with no tag substitution and would
// 404 against that same server.
func TestPingModelPresentDifferentTagFails(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		json.NewEncoder(w).Encode(map[string]any{
			"models": []map[string]string{
				{"name": "nomic-embed-text:q4_0"},
			},
		})
	}))
	defer srv.Close()

	c := New(srv.URL, "nomic-embed-text", 768)
	err := c.Ping(context.Background())
	if err == nil {
		t.Fatal("expected error: only a differently-tagged model is present")
	}
	if !errors.Is(err, ErrModelMissing) {
		t.Errorf("expected errors.Is(err, ErrModelMissing), got: %v", err)
	}
}

func TestPingModelMissing(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		json.NewEncoder(w).Encode(map[string]any{
			"models": []map[string]string{
				{"name": "llama3:latest"},
			},
		})
	}))
	defer srv.Close()

	c := New(srv.URL, "nomic-embed-text", 768)
	err := c.Ping(context.Background())
	if err == nil {
		t.Fatal("expected error for missing model")
	}
	if !errors.Is(err, ErrModelMissing) {
		t.Errorf("expected errors.Is(err, ErrModelMissing), got: %v", err)
	}
	if errors.Is(err, ErrUnavailable) {
		t.Errorf("missing model should not be ErrUnavailable: %v", err)
	}
	if !strings.Contains(err.Error(), "ollama pull nomic-embed-text") {
		t.Errorf("error should suggest ollama pull, got: %v", err)
	}
}

func TestPingConnectionRefused(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {}))
	url := srv.URL
	srv.Close()

	c := New(url, "nomic-embed-text", 768)
	err := c.Ping(context.Background())
	if !errors.Is(err, ErrUnavailable) {
		t.Errorf("closed server should wrap ErrUnavailable, got: %v", err)
	}
}

// TestLiveOllama exercises the real local Ollama service. Skipped unless KB_LIVE_OLLAMA=1, since
// it depends on ollama.service being up on this droplet (see ollama-local-embeddings.md).
func TestLiveOllama(t *testing.T) {
	if os.Getenv("KB_LIVE_OLLAMA") != "1" {
		t.Skip("set KB_LIVE_OLLAMA=1 to run against the real Ollama service")
	}
	c := New("", "", 0)
	vecs, err := c.Embed(context.Background(), []string{embed.QueryPrefix + "hi"})
	if err != nil {
		t.Fatalf("Embed against live Ollama: %v", err)
	}
	if len(vecs) != 1 {
		t.Fatalf("got %d vectors, want 1", len(vecs))
	}
	if len(vecs[0]) != 768 {
		t.Fatalf("got dim %d, want 768", len(vecs[0]))
	}
}
