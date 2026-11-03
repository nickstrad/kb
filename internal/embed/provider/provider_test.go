package provider

import (
	"context"
	"os"
	"sync"
	"testing"

	"github.com/nickstrad/kb/embed"
	"github.com/nickstrad/kb/embed/command"
	"github.com/nickstrad/kb/embed/fake"
	"github.com/nickstrad/kb/embed/ollama"
	"github.com/nickstrad/kb/embed/openai"
)

// clearEnv unsets every variable New reads, so the host environment cannot leak into a test.
func clearEnv(t *testing.T) {
	for _, k := range []string{
		"KB_EMBEDDER", "KB_EMBED_MODEL", "KB_EMBED_DIM", "KB_EMBED_URL", "KB_EMBED_API_KEY",
		"KB_OPENROUTER_API_KEY", "OPENROUTER_API_KEY", "KB_OLLAMA_URL",
		"KB_EMBED_SEND_DIMENSIONS", "KB_EMBED_TRUNCATE",
		"KB_EMBED_COMMAND", "KB_EMBED_BATCH", "KB_EMBED_TIMEOUT", "KB_EMBED_PATH",
		"KB_EMBED_AUTH_HEADER", "KB_EMBED_AUTH_SCHEME",
	} {
		t.Setenv(k, "")
	}
	// The prefix variables mean "no prefix" when set but empty, so they are unset instead.
	for _, k := range []string{"KB_EMBED_DOC_PREFIX", "KB_EMBED_QUERY_PREFIX"} {
		t.Setenv(k, "") // registers the restore
		os.Unsetenv(k)
	}
}

func TestDefaultsToOllama(t *testing.T) {
	clearEnv(t)
	e, info, err := New(Options{})
	if err != nil {
		t.Fatal(err)
	}
	if _, ok := e.(*ollama.Client); !ok || info.Name != Ollama || e.Model() != ollama.DefaultModel || e.Dim() != 768 {
		t.Fatalf("got %T %+v %s %d", e, info, e.Model(), e.Dim())
	}
	if embed.DocText(e, "x") != embed.DocPrefix+"x" {
		t.Error("ollama should keep nomic prefixes")
	}
}

func TestOpenRouterKeySelectsOpenRouter(t *testing.T) {
	for _, key := range []string{"OPENROUTER_API_KEY", "KB_OPENROUTER_API_KEY"} {
		clearEnv(t)
		t.Setenv(key, "sk-or-test")
		e, info, err := New(Options{})
		if err != nil {
			t.Fatal(err)
		}
		c, ok := e.(*openai.Client)
		if !ok || info.Name != OpenRouter || c.BaseURL() != OpenRouterURL || c.Model() != DefaultOpenRouterModel {
			t.Fatalf("%s: got %T %+v", key, e, info)
		}
		if embed.QueryText(e, "q") != "q" {
			t.Error("openrouter should send no prefix by default")
		}
	}
}

func TestFlagsOverrideEnvironment(t *testing.T) {
	clearEnv(t)
	t.Setenv("OPENROUTER_API_KEY", "sk-or-test")
	t.Setenv("KB_EMBED_MODEL", "env/model")
	e, _, err := New(Options{Model: "flag/model"})
	if err != nil || e.Model() != "flag/model" {
		t.Fatalf("got %v %v", e, err)
	}
	e, info, err := New(Options{Embedder: "ollama"})
	if err != nil || info.Name != Ollama || e.Model() != "env/model" {
		t.Fatalf("explicit ollama: %+v %v", info, err)
	}
}

func TestNone(t *testing.T) {
	clearEnv(t)
	t.Setenv("KB_EMBEDDER", "none")
	e, _, err := New(Options{})
	if err != nil || !embed.IsNone(e) || e.Model() != embed.NoneModel {
		t.Fatalf("got %v %v", e, err)
	}
}

func TestConfigurationErrors(t *testing.T) {
	for name, env := range map[string]map[string]string{
		"openrouter without key": {"KB_EMBEDDER": "openrouter"},
		"openai without url":     {"KB_EMBEDDER": "openai", "KB_EMBED_MODEL": "m"},
		"openai without model":   {"KB_EMBEDDER": "openai", "KB_EMBED_URL": "http://x"},
		"unknown embedder":       {"KB_EMBEDDER": "word2vec"},
		"bad dim":                {"KB_EMBED_DIM": "wide"},
		"bad bool":               {"OPENROUTER_API_KEY": "k", "KB_EMBED_TRUNCATE": "maybe"},
	} {
		clearEnv(t)
		for k, v := range env {
			t.Setenv(k, v)
		}
		if _, _, err := New(Options{}); err == nil {
			t.Errorf("%s: want an error", name)
		}
	}
}

func TestOllamaDefaultKeepsTodaysSettings(t *testing.T) {
	clearEnv(t)
	t.Setenv("KB_OLLAMA_URL", "http://gpu-box:11434/")
	t.Setenv("KB_EMBED_URL", "http://ignored-by-ollama")
	t.Setenv("KB_EMBED_DOC_PREFIX", "")
	e, info, err := New(Options{})
	if err != nil {
		t.Fatal(err)
	}
	if info.Name != Ollama || info.Endpoint != "http://gpu-box:11434" {
		t.Fatalf("info %+v", info)
	}
	if embed.DocText(e, "x") != "x" || embed.QueryText(e, "q") != embed.QueryPrefix+"q" {
		t.Error("a set-but-empty KB_EMBED_DOC_PREFIX means no prefix; the query prefix stays nomic's")
	}
}

func TestSelectsHTTPAndCommandByConfiguration(t *testing.T) {
	clearEnv(t)
	t.Setenv("KB_EMBEDDER", "http")
	t.Setenv("KB_EMBED_URL", "http://api.example/v2")
	t.Setenv("KB_EMBED_MODEL", "m")
	e, info, err := New(Options{})
	if err != nil {
		t.Fatal(err)
	}
	if _, ok := e.(*openai.Client); !ok || info.Name != HTTP || info.Endpoint != "http://api.example/v2" || e.Dim() != DefaultDim {
		t.Fatalf("http: %T %+v", e, info)
	}

	t.Setenv("KB_EMBEDDER", "command")
	t.Setenv("KB_EMBED_COMMAND", "/opt/my-model/embed --onnx")
	e, info, err = New(Options{})
	if err != nil {
		t.Fatal(err)
	}
	c, ok := e.(*command.Client)
	if !ok || info.Name != Command || info.Endpoint != "" || c.Command() != "/opt/my-model/embed --onnx" || c.Model() != "m" {
		t.Fatalf("command: %T %+v", e, info)
	}

	// The --embedder flag switches providers over the same environment.
	e, info, err = New(Options{Embedder: "none"})
	if err != nil || info.Name != None || !embed.IsNone(e) {
		t.Fatalf("flag override: %+v %v", info, err)
	}

	for name, env := range map[string]map[string]string{
		"http without url":       {"KB_EMBEDDER": "http", "KB_EMBED_URL": "", "KB_EMBED_MODEL": "m"},
		"http without model":     {"KB_EMBEDDER": "http", "KB_EMBED_MODEL": ""},
		"command without prog":   {"KB_EMBEDDER": "command", "KB_EMBED_COMMAND": ""},
		"command without model":  {"KB_EMBEDDER": "command", "KB_EMBED_MODEL": ""},
		"command with bad batch": {"KB_EMBEDDER": "command", "KB_EMBED_BATCH": "lots"},
	} {
		for k, v := range env {
			t.Setenv(k, v)
		}
		if _, _, err := New(Options{}); err == nil {
			t.Errorf("%s: want an error", name)
		}
		t.Setenv("KB_EMBED_URL", "http://api.example/v2")
		t.Setenv("KB_EMBED_MODEL", "m")
		t.Setenv("KB_EMBED_COMMAND", "/opt/my-model/embed")
		t.Setenv("KB_EMBED_BATCH", "")
	}
}

var registerExtra sync.Once

// TestExtraProviderRegisteredInKB shows a kb build adding its own provider to Registry and
// selecting it with KB_EMBEDDER.
func TestExtraProviderRegisteredInKB(t *testing.T) {
	registerExtra.Do(func() {
		Registry.MustRegister(embed.Provider{
			Name:    "test-extra",
			Options: []embed.Option{{Key: "flavour", Default: "plain"}},
			New: func(cfg embed.Config) (embed.Embedder, error) {
				return fake.New(cfg.Model+"/"+cfg.Get("flavour"), cfg.Dim), nil
			},
		})
	})
	clearEnv(t)
	t.Setenv("KB_EMBEDDER", "test-extra")
	t.Setenv("KB_EMBED_FLAVOUR", "spicy")
	e, info, err := New(Options{Model: "x"})
	if err != nil {
		t.Fatal(err)
	}
	if info.Name != "test-extra" || e.Model() != "x/spicy" || e.Dim() != DefaultDim {
		t.Fatalf("got %+v %s %d", info, e.Model(), e.Dim())
	}
	if v, err := e.Embed(context.Background(), []string{"a"}); err != nil || len(v[0]) != DefaultDim {
		t.Fatalf("embed: %v", err)
	}
}
