package openai

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/nickstrad/kb/embed"
)

// fakeAPI serves an OpenAI-shaped embeddings endpoint at path and records the last request.
type fakeAPI struct {
	path    string
	headers http.Header
	body    embedRequest
}

func (f *fakeAPI) server(t *testing.T) *httptest.Server {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != f.path {
			http.Error(w, `{"error":{"message":"no route"}}`, http.StatusNotFound)
			return
		}
		f.headers = r.Header.Clone()
		json.NewDecoder(r.Body).Decode(&f.body)
		var resp embedResponse
		for i := len(f.body.Input) - 1; i >= 0; i-- { // out of order on purpose
			v := make([]float32, f.body.Dimensions)
			v[0] = float32(i)
			resp.Data = append(resp.Data, struct {
				Index     int       `json:"index"`
				Embedding []float32 `json:"embedding"`
			}{i, v})
		}
		json.NewEncoder(w).Encode(resp)
	}))
	t.Cleanup(srv.Close)
	return srv
}

func registry() *embed.Registry {
	r := embed.NewRegistry()
	r.DefaultDim = 4
	r.MustRegister(Provider(), OpenRouterProvider(), HTTPProvider())
	return r
}

func openWith(t *testing.T, vars map[string]string) embed.Embedder {
	t.Helper()
	env := embed.Env{Prefix: "KB_", Lookup: func(k string) (string, bool) { v, ok := vars[k]; return v, ok }}
	r := registry()
	cfg, err := r.Resolve(embed.Config{}, env)
	if err != nil {
		t.Fatal(err)
	}
	e, err := r.Open(cfg)
	if err != nil {
		t.Fatal(err)
	}
	return e
}

func TestHTTPProviderDefaultsToOpenAIShape(t *testing.T) {
	f := &fakeAPI{path: "/v1/embeddings"}
	srv := f.server(t)
	e := openWith(t, map[string]string{
		"KB_EMBEDDER": "http", "KB_EMBED_URL": srv.URL + "/v1/", "KB_EMBED_API_KEY": "secret", "KB_EMBED_MODEL": "e5",
	})
	vecs, err := e.Embed(context.Background(), []string{"a", "b"})
	if err != nil {
		t.Fatal(err)
	}
	if vecs[0][0] != 0 || vecs[1][0] != 1 || len(vecs[1]) != 4 {
		t.Fatalf("vectors %v", vecs)
	}
	if got := f.headers.Get("Authorization"); got != "Bearer secret" {
		t.Errorf("Authorization = %q", got)
	}
	if f.body.Model != "e5" || f.body.Dimensions != 4 {
		t.Errorf("request %+v", f.body)
	}
	if embed.QueryText(e, "q") != "q" {
		t.Error("http should send no prefix by default")
	}
}

func TestHTTPProviderCustomPathAndAuthHeader(t *testing.T) {
	f := &fakeAPI{path: "/models/embed"}
	srv := f.server(t)
	e := openWith(t, map[string]string{
		"KB_EMBEDDER": "http", "KB_EMBED_URL": srv.URL, "KB_EMBED_API_KEY": "secret", "KB_EMBED_MODEL": "m",
		"KB_EMBED_PATH": "models/embed", "KB_EMBED_AUTH_HEADER": "api-key", "KB_EMBED_AUTH_SCHEME": "none",
		"KB_EMBED_SEND_DIMENSIONS": "false", "KB_EMBED_DIM": "3",
	})
	// The server sizes vectors by the dimensions field; with it left out, it returns zero-length
	// vectors, which the client must reject.
	if _, err := e.Embed(context.Background(), []string{"a"}); err == nil {
		t.Fatal("want a dimension error when the server ignores the size")
	}
	if f.headers.Get("api-key") != "secret" || f.headers.Get("Authorization") != "" || f.body.Dimensions != 0 {
		t.Fatalf("headers %v body %+v", f.headers, f.body)
	}
}

func TestHTTPProviderRequiresURLAndModel(t *testing.T) {
	r := registry()
	for name, cfg := range map[string]embed.Config{
		"no url":   {Provider: NameHTTP, Model: "m", Dim: 4},
		"no model": {Provider: NameHTTP, Dim: 4, Options: map[string]string{"url": "http://x"}},
		"bad bool": {Provider: NameHTTP, Model: "m", Dim: 4, Options: map[string]string{"url": "http://x", "truncate": "maybe"}},
	} {
		if _, err := r.Open(cfg); err == nil {
			t.Errorf("%s: want an error", name)
		}
	}
}
