package openai

import (
	"context"
	"encoding/json"
	"errors"
	"math"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/nickstrad/kb/internal/embed"
)

// server answers POST /embeddings with one vector of length dim per input, in reverse index order
// so the client has to sort them, and records the last request.
func server(t *testing.T, dim int, last *embedRequest, auth *string) *httptest.Server {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost || r.URL.Path != "/v1/embeddings" {
			http.Error(w, `{"error":{"message":"not found"}}`, http.StatusNotFound)
			return
		}
		*auth = r.Header.Get("Authorization")
		if err := json.NewDecoder(r.Body).Decode(last); err != nil {
			t.Errorf("decode request: %v", err)
		}
		type item struct {
			Index     int       `json:"index"`
			Embedding []float32 `json:"embedding"`
		}
		var data []item
		for i := len(last.Input) - 1; i >= 0; i-- {
			v := make([]float32, dim)
			v[0] = float32(i + 1)
			data = append(data, item{Index: i, Embedding: v})
		}
		json.NewEncoder(w).Encode(map[string]any{"data": data})
	}))
	t.Cleanup(srv.Close)
	return srv
}

func TestEmbedSendsModelKeyDimensionsAndRestoresOrder(t *testing.T) {
	var req embedRequest
	var auth string
	srv := server(t, 4, &req, &auth)
	c, err := New(Config{BaseURL: srv.URL + "/v1/", APIKey: "sk-test", Model: "m", Dim: 4, SendDimensions: true})
	if err != nil {
		t.Fatal(err)
	}
	vecs, err := c.Embed(context.Background(), []string{"a", "b", "c"})
	if err != nil {
		t.Fatal(err)
	}
	if auth != "Bearer sk-test" || req.Model != "m" || req.Dimensions != 4 || len(req.Input) != 3 {
		t.Fatalf("request: auth=%q %+v", auth, req)
	}
	for i, v := range vecs {
		if v[0] != float32(i+1) {
			t.Errorf("vector %d out of order: %v", i, v)
		}
	}
}

func TestEmbedSplitsLargeBatches(t *testing.T) {
	var req embedRequest
	var auth string
	srv := server(t, 2, &req, &auth)
	c, _ := New(Config{BaseURL: srv.URL + "/v1", Model: "m", Dim: 2})
	texts := make([]string, maxBatch+3)
	vecs, err := c.Embed(context.Background(), texts)
	if err != nil || len(vecs) != len(texts) {
		t.Fatalf("got %d vectors, err %v", len(vecs), err)
	}
	if len(req.Input) != 3 || req.Dimensions != 0 || auth != "" {
		t.Fatalf("last batch: %+v auth=%q", req, auth)
	}
}

func TestEmbedTruncatesAndNormalises(t *testing.T) {
	var req embedRequest
	var auth string
	srv := server(t, 6, &req, &auth)
	c, _ := New(Config{BaseURL: srv.URL + "/v1", Model: "m", Dim: 3, Truncate: true})
	vecs, err := c.Embed(context.Background(), []string{"a"})
	if err != nil {
		t.Fatal(err)
	}
	if len(vecs[0]) != 3 || math.Abs(float64(vecs[0][0])-1) > 1e-6 {
		t.Fatalf("got %v", vecs[0])
	}

	c, _ = New(Config{BaseURL: srv.URL + "/v1", Model: "m", Dim: 3})
	if _, err := c.Embed(context.Background(), []string{"a"}); err == nil {
		t.Fatal("a wrong dimension without Truncate should fail")
	}
}

func TestErrorClassification(t *testing.T) {
	for _, tc := range []struct {
		status      int
		body        string
		unavailable bool
	}{
		{http.StatusUnauthorized, `{"error":{"message":"bad key"}}`, false},
		{http.StatusBadRequest, `{"error":"no such model"}`, false},
		{http.StatusTooManyRequests, `{"error":{"message":"slow down"}}`, true},
		{http.StatusBadGateway, `upstream down`, true},
	} {
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			w.WriteHeader(tc.status)
			w.Write([]byte(tc.body))
		}))
		c, _ := New(Config{BaseURL: srv.URL, Model: "m", Dim: 2})
		err := c.Ping(context.Background())
		srv.Close()
		if err == nil || errors.Is(err, embed.ErrUnavailable) != tc.unavailable {
			t.Errorf("status %d: err=%v, want unavailable=%v", tc.status, err, tc.unavailable)
		}
	}

	srv := httptest.NewServer(http.NotFoundHandler())
	srv.Close()
	c, _ := New(Config{BaseURL: srv.URL, Model: "m", Dim: 2})
	if err := c.Ping(context.Background()); !errors.Is(err, embed.ErrUnavailable) {
		t.Errorf("closed server should be unavailable, got %v", err)
	}
}

func TestPrefixes(t *testing.T) {
	c, _ := New(Config{BaseURL: "http://x", Model: "m", Dim: 2})
	if got := embed.QueryText(c, "q"); got != "q" {
		t.Errorf("no prefix configured, got %q", got)
	}
}
