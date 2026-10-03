package provider

import (
	"testing"

	"github.com/nickstrad/kb/internal/embed"
	"github.com/nickstrad/kb/internal/embed/ollama"
	"github.com/nickstrad/kb/internal/embed/openai"
)

// clearEnv unsets every variable New reads, so the host environment cannot leak into a test.
func clearEnv(t *testing.T) {
	for _, k := range []string{
		"KB_EMBEDDER", "KB_EMBED_MODEL", "KB_EMBED_DIM", "KB_EMBED_URL", "KB_EMBED_API_KEY",
		"KB_OPENROUTER_API_KEY", "OPENROUTER_API_KEY", "KB_OLLAMA_URL",
		"KB_EMBED_SEND_DIMENSIONS", "KB_EMBED_TRUNCATE",
	} {
		t.Setenv(k, "")
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
