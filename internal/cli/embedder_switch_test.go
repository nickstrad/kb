package cli

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
)

// TestMain lets this test binary double as a bring-your-own embedding program for the command
// provider: run with KB_TEST_EMBED_PROGRAM=1 it answers one embed request on stdin and exits.
func TestMain(m *testing.M) {
	if os.Getenv("KB_TEST_EMBED_PROGRAM") == "1" {
		os.Exit(fakeEmbedProgram())
	}
	os.Exit(m.Run())
}

// fakeVector is a deterministic vector from a text's words, so related texts land near each
// other and a vector search finds the right entry.
func fakeVector(text string, dim int) []float32 {
	v := make([]float32, dim)
	for _, w := range strings.Fields(strings.ToLower(text)) {
		h := 0
		for _, c := range w {
			h = h*31 + int(c)
		}
		if h < 0 {
			h = -h
		}
		v[h%dim]++
	}
	v[0] += 0.01
	return v
}

func fakeEmbedProgram() int {
	var req struct {
		Model string   `json:"model"`
		Dim   int      `json:"dim"`
		Texts []string `json:"texts"`
	}
	if err := json.NewDecoder(os.Stdin).Decode(&req); err != nil {
		fmt.Fprintln(os.Stderr, err)
		return 2
	}
	out := struct {
		Embeddings [][]float32 `json:"embeddings"`
	}{}
	for _, t := range req.Texts {
		out.Embeddings = append(out.Embeddings, fakeVector(t, req.Dim))
	}
	json.NewEncoder(os.Stdout).Encode(out)
	return 0
}

// fakeEmbeddingsAPI is an OpenAI-shaped HTTP embeddings API that counts its requests.
func fakeEmbeddingsAPI(t *testing.T, calls *atomic.Int32) *httptest.Server {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/embeddings" || r.Header.Get("Authorization") != "Bearer test-key" {
			http.Error(w, "bad request", http.StatusBadRequest)
			return
		}
		calls.Add(1)
		var req struct {
			Input      []string `json:"input"`
			Dimensions int      `json:"dimensions"`
		}
		json.NewDecoder(r.Body).Decode(&req)
		type item struct {
			Index     int       `json:"index"`
			Embedding []float32 `json:"embedding"`
		}
		var resp struct {
			Data []item `json:"data"`
		}
		for i, s := range req.Input {
			resp.Data = append(resp.Data, item{i, fakeVector(s, req.Dimensions)})
		}
		json.NewEncoder(w).Encode(resp)
	}))
	t.Cleanup(srv.Close)
	return srv
}

func vecSearchTop(t *testing.T, query string) string {
	t.Helper()
	out, errOut, code := run(t, "search", query, "--mode", "vec", "--json", "--caller", "test")
	if code != 0 {
		t.Fatalf("vec search: %d %q", code, errOut)
	}
	var res struct {
		Mode string `json:"mode"`
		Hits []struct {
			Path string `json:"path"`
		} `json:"hits"`
	}
	if err := json.Unmarshal([]byte(out), &res); err != nil || res.Mode != "vec" || len(res.Hits) == 0 {
		t.Fatalf("vec search result %q: %v", out, err)
	}
	return res.Hits[0].Path
}

// TestSwitchingBetweenCommandAndHTTPProviders runs kb through the real provider registry (no fake
// hook): it indexes with the command provider (this binary as the user's program), proves the
// embed_meta guard refuses a switch to the http provider, rebuilds with kb reindex --all, and then
// searches through the HTTP API.
func TestSwitchingBetweenCommandAndHTTPProviders(t *testing.T) {
	newTempRoot(t)
	for _, k := range []string{"OPENROUTER_API_KEY", "KB_OPENROUTER_API_KEY", "KB_EMBED_DIM", "KB_EMBED_DOC_PREFIX", "KB_EMBED_QUERY_PREFIX"} {
		t.Setenv(k, "")
	}
	t.Setenv("KB_TEST_EMBED_PROGRAM", "1") // inherited by the program the command provider runs
	t.Setenv("KB_EMBEDDER", "command")
	t.Setenv("KB_EMBED_COMMAND", os.Args[0])
	t.Setenv("KB_EMBED_MODEL", "byo-local-model")

	dir := t.TempDir()
	for name, body := range map[string]string{
		"lantern-notes.md": validFrontMatter("Lantern notes", "Lanterns hang in the courtyard."),
		"river-notes.md":   validFrontMatter("River notes", "Boats drift along the river delta."),
	} {
		src := filepath.Join(dir, name)
		writeFile(t, src, body)
		if out, errOut, code := run(t, "add", src); code != 0 || !strings.Contains(out, "added") {
			t.Fatalf("add %s: %d %q %q", name, code, out, errOut)
		}
	}
	if got := vecSearchTop(t, "boats river delta"); got != "data/river-notes.md" {
		t.Fatalf("command provider top hit = %s", got)
	}
	if out, _, code := run(t, "doctor"); code != 0 || !strings.Contains(out, "byo-local-model") {
		t.Fatalf("doctor (command): %d %q", code, out)
	}

	var calls atomic.Int32
	srv := fakeEmbeddingsAPI(t, &calls)
	t.Setenv("KB_EMBEDDER", "http")
	t.Setenv("KB_EMBED_URL", srv.URL)
	t.Setenv("KB_EMBED_API_KEY", "test-key")
	t.Setenv("KB_EMBED_MODEL", "api-model")

	if _, errOut, code := run(t, "search", "lantern", "--mode", "vec"); code != ExitUsage || !strings.Contains(errOut, "reindex --all") {
		t.Fatalf("switch without reindex: %d %q", code, errOut)
	}
	if out, errOut, code := run(t, "reindex", "--all"); code != 0 {
		t.Fatalf("reindex --all: %d %q %q", code, out, errOut)
	}
	if got := vecSearchTop(t, "lanterns courtyard"); got != "data/lantern-notes.md" {
		t.Fatalf("http provider top hit = %s", got)
	}
	if calls.Load() == 0 {
		t.Fatal("the HTTP API was never called")
	}

	// And back: the index now holds api-model vectors, so the command provider is refused too.
	t.Setenv("KB_EMBEDDER", "command")
	t.Setenv("KB_EMBED_MODEL", "byo-local-model")
	if _, errOut, code := run(t, "search", "lantern", "--mode", "vec"); code != ExitUsage || !strings.Contains(errOut, "reindex --all") {
		t.Fatalf("switch back without reindex: %d %q", code, errOut)
	}
}
