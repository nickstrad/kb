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

	"github.com/nickstrad/kb/internal/config"
	"github.com/nickstrad/kb/internal/embed"
	"github.com/nickstrad/kb/internal/embed/local"
	"github.com/nickstrad/kb/internal/errs"
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
	if len(os.Args) < 2 || os.Args[1] != "--byo" {
		fmt.Fprintln(os.Stderr, "the configured argument did not arrive")
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

// fakeEmbeddingsAPI is an OpenAI-shaped HTTP embeddings API that counts its requests and wants
// "Bearer test-key". While *down is true it answers 503, as an overloaded API does.
func fakeEmbeddingsAPI(t *testing.T, calls *atomic.Int32, down *atomic.Bool) *httptest.Server {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if down != nil && down.Load() {
			http.Error(w, `{"error":{"message":"overloaded"}}`, http.StatusServiceUnavailable)
			return
		}
		if r.URL.Path != "/embeddings" || r.Header.Get("Authorization") != "Bearer test-key" {
			http.Error(w, `{"error":{"message":"bad key or path"}}`, http.StatusUnauthorized)
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

func commandConfig() string {
	return fmt.Sprintf("[embed]\nprovider = \"command\"\nmodel = \"byo-local-model\"\ndim = 32\n\n"+
		"[embed.command]\nprogram = %q\nargs = [\"--byo\"]\n", os.Args[0])
}

func httpConfig(url string) string {
	return fmt.Sprintf("[embed]\nprovider = \"http\"\nmodel = \"api-model\"\ndim = 32\n\n"+
		"[embed.http]\nurl = %q\napi_key_env = \"KB_TEST_EMBED_KEY\"\n", url)
}

// TestSwitchingBetweenCommandAndHTTPProviders runs kb through the real provider registry (no fake
// hook), selected only by the store's config file: it indexes with the command provider (this
// binary as the user's program), proves the embed_meta guard refuses a switch to the http
// provider, rebuilds with kb reindex --all, searches through the HTTP API, and is refused again on
// the way back. Neither provider touches the model cache or tries a download.
func TestSwitchingBetweenCommandAndHTTPProviders(t *testing.T) {
	root := newTempRoot(t)
	t.Setenv("KB_TEST_EMBED_PROGRAM", "1") // inherited by the program the command provider runs
	t.Setenv("KB_TEST_EMBED_KEY", "test-key")
	d := depsFor(t)
	d.modelCacheDir = func() (string, error) {
		t.Error("a non-local provider asked for the model cache")
		return "", errFakeCache
	}
	writeConfig(t, root, commandConfig())

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
	if _, errOut, code := run(t, "model", "pull"); code != ExitUsage || !strings.Contains(errOut, `embed.provider is "command"`) {
		t.Fatalf("model pull (command): %d %q", code, errOut)
	}

	var calls atomic.Int32
	srv := fakeEmbeddingsAPI(t, &calls, nil)
	writeConfig(t, root, httpConfig(srv.URL))
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
	writeConfig(t, root, commandConfig())
	if _, errOut, code := run(t, "search", "lantern", "--mode", "vec"); code != ExitUsage || !strings.Contains(errOut, "reindex --all") {
		t.Fatalf("switch back without reindex: %d %q", code, errOut)
	}
}

// TestProviderFailuresKeepKBExitCodes: an unreachable API is "embedder unavailable" (a hybrid
// search falls back to FTS, --mode vec exits 2), a rejected key is a Usage error (exit 1), and a
// command that does not exist is a configuration error, never a silent fallback.
func TestProviderFailuresKeepKBExitCodes(t *testing.T) {
	root := newTempRoot(t)
	t.Setenv("KB_TEST_EMBED_PROGRAM", "1")
	t.Setenv("KB_TEST_EMBED_KEY", "test-key")
	var calls atomic.Int32
	var down atomic.Bool
	srv := fakeEmbeddingsAPI(t, &calls, &down)
	writeConfig(t, root, httpConfig(srv.URL))
	src := filepath.Join(t.TempDir(), "lantern-notes.md")
	writeFile(t, src, validFrontMatter("Lantern notes", "Lanterns hang in the courtyard."))
	if _, errOut, code := run(t, "add", src); code != 0 {
		t.Fatalf("add: %d %q", code, errOut)
	}

	down.Store(true)
	out, errOut, code := run(t, "search", "lantern", "--json", "--caller", "test")
	if code != ExitEmbedder || !strings.Contains(out, `"fts-fallback"`) {
		t.Fatalf("API down, hybrid: %d %q %q", code, out, errOut)
	}
	if _, errOut, code := run(t, "search", "lantern", "--mode", "vec"); code != ExitEmbedder {
		t.Fatalf("API down, vec: %d %q", code, errOut)
	}
	down.Store(false)

	t.Setenv("KB_TEST_EMBED_KEY", "wrong-key")
	if _, errOut, code := run(t, "search", "lantern", "--mode", "vec"); code != ExitUsage || !strings.Contains(errOut, "bad key") {
		t.Fatalf("rejected key: %d %q", code, errOut)
	}
	t.Setenv("KB_TEST_EMBED_KEY", "")
	if _, errOut, code := run(t, "search", "lantern", "--mode", "vec"); code != ExitUsage || !strings.Contains(errOut, "KB_TEST_EMBED_KEY is not set") {
		t.Fatalf("unset key variable: %d %q", code, errOut)
	}

	missing := filepath.Join(t.TempDir(), "no-such-embedder")
	writeConfig(t, root, fmt.Sprintf("[embed]\nprovider = \"command\"\nmodel = \"api-model\"\ndim = 32\n[embed.command]\nprogram = %q\n", missing))
	// The index holds api-model vectors at a different Model() identity, so ask the embedder
	// directly rather than through the embed_meta guard.
	_, cfg, err := depsFor(t).workspaceConfig(newRootCmd(depsFor(t), nil, nil))
	if err != nil {
		t.Fatal(err)
	}
	e, err := depsFor(t).buildEmbedder(nil, cfg.Embed)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := e.Embed(t.Context(), []string{"x"}); errs.KindOf(err) != errs.Usage {
		t.Fatalf("missing program = %v (kind %v), want a Usage error", err, errs.KindOf(err))
	}
}

// TestProviderSelection is the decision table for which embedder an [embed] table builds through
// the registry: no provider is exactly v2's local embedder; "none" is FTS only whatever the
// provider; http and command build their clients without touching the model cache; a missing key
// or an unknown provider is a Usage error naming what to set.
func TestProviderSelection(t *testing.T) {
	t.Parallel()
	d := depsFor(t)
	cmd := newRootCmd(d, nil, nil)
	cmdTable := &config.EmbedCommand{Program: "/opt/embed", Args: []string{"--x"}}
	cases := []struct {
		name    string
		e       config.Embed
		model   string // the built embedder's Model(), "" when an error is wanted
		dim     int
		wantErr string
	}{
		{"default", config.Default().Embed, local.DefaultModel, 384, ""},
		{"explicit local", config.Embed{Provider: "local", Model: "BAAI/bge-base-en-v1.5", Batch: 1}, "BAAI/bge-base-en-v1.5", 768, ""},
		{"none model, http provider", config.Embed{Provider: "http", Model: "none"}, "none", 0, ""},
		{"http", config.Embed{Provider: "http", Model: "m", Dim: 8, HTTP: &config.EmbedHTTP{URL: "https://api.example/v1"}}, "http https://api.example/v1/embeddings model=m", 8, ""},
		{"command", config.Embed{Provider: "command", Model: "m", Dim: 8, Command: cmdTable}, `command ["/opt/embed" "--x"] model=m`, 8, ""},
		{"http no url", config.Embed{Provider: "http", Model: "m", Dim: 8}, "", 0, "embed.http.url"},
		{"command no dim", config.Embed{Provider: "command", Model: "m", Command: cmdTable}, "", 0, "embed.dim"},
		{"unknown provider", config.Embed{Provider: "word2vec", Model: "m", Dim: 8}, "", 0, `unknown embedder "word2vec" (want command, http, local or none)`},
		{"unknown local model", config.Embed{Model: "acme/unknown"}, "", 0, "supported: "},
	}
	for _, tc := range cases {
		e, err := d.buildEmbedder(cmd, tc.e)
		if tc.wantErr != "" {
			if errs.KindOf(err) != errs.Usage || !strings.Contains(err.Error(), tc.wantErr) {
				t.Errorf("%s: err = %v, want a Usage error with %q", tc.name, err, tc.wantErr)
			}
			continue
		}
		if err != nil {
			t.Errorf("%s: %v", tc.name, err)
			continue
		}
		if e.Model() != tc.model || e.Dim() != tc.dim {
			t.Errorf("%s: built %s/%d, want %s/%d", tc.name, e.Model(), e.Dim(), tc.model, tc.dim)
		}
		_, isLocal := e.(*local.Embedder)
		if wantLocal := tc.model == local.DefaultModel || tc.name == "explicit local"; isLocal != wantLocal {
			t.Errorf("%s: %T, local=%v", tc.name, e, wantLocal)
		}
		if tc.model == "none" && !embed.IsNone(e) {
			t.Errorf("%s: %T is not FTS only", tc.name, e)
		}
	}
}

// TestKBIgnoresEmbedderEnvironment: kb's provider selection comes only from the config file. The
// KB_* variables the first plugin branch read (KB_EMBEDDER, KB_EMBED_MODEL, ...) change nothing,
// so a machine that still exports them keeps v2's default.
func TestKBIgnoresEmbedderEnvironment(t *testing.T) {
	for k, v := range map[string]string{
		"KB_EMBEDDER": "command", "KB_EMBED_MODEL": "env-model", "KB_EMBED_DIM": "8",
		"KB_EMBED_PROGRAM": "/env/only", "KB_EMBED_URL": "http://env.invalid", "OPENROUTER_API_KEY": "k",
	} {
		t.Setenv(k, v)
	}
	cmd := newRootCmd(depsFor(t), nil, nil)
	e, err := depsFor(t).buildEmbedder(cmd, config.Default().Embed)
	if err != nil {
		t.Fatal(err)
	}
	if le, ok := e.(*local.Embedder); !ok || le.Model() != local.DefaultModel {
		t.Fatalf("with KB_* set, the default built %T %s", e, e.Model())
	}
}
