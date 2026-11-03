package builtin_test

import (
	"reflect"
	"testing"

	"github.com/nickstrad/kb/embed"
	"github.com/nickstrad/kb/embed/builtin"
)

func TestNewRegistryHoldsTheBuiltInProviders(t *testing.T) {
	r := builtin.NewRegistry()
	if got, want := r.Names(), []string{"http", "command", "none"}; !reflect.DeepEqual(got, want) {
		t.Fatalf("Names = %v, want %v", got, want)
	}
	if r.Default != "" {
		t.Errorf("Default = %q: the public registry must not pick a provider for the program", r.Default)
	}
	if err := builtin.Register(r); err == nil {
		t.Error("registering the built-ins twice must fail")
	}
}

// A registry with another prefix must not pick up kb's KB_* variables (Astra's review finding on
// the first plugin branch, kept in substance): with OTHER_ as the prefix and an empty lookup,
// KB_EMBEDDER, KB_EMBED_MODEL, KB_EMBED_URL, KB_EMBED_PROGRAM and KB_EMBED_API_KEY set in the
// process change nothing, and every built-in provider builds only from explicit values.
func TestRegistryIgnoresKBEnvironment(t *testing.T) {
	for k, v := range map[string]string{
		"KB_EMBEDDER": "command", "KB_EMBED_MODEL": "kb-only-model", "KB_EMBED_DIM": "999",
		"KB_EMBED_URL": "http://kb-only.invalid:1", "KB_EMBED_API_KEY": "kb-only-key",
		"KB_EMBED_PROGRAM": "/kb/only", "KB_EMBED_ARGS": "--kb-only",
	} {
		t.Setenv(k, v)
	}
	r := builtin.NewRegistry()
	env := embed.Env{Prefix: "OTHER_", Lookup: func(string) (string, bool) { return "", false }}
	if _, err := r.Resolve(embed.Config{}, env); err == nil {
		t.Fatal("KB_EMBEDDER leaked into an OTHER_ registry: resolution with no provider succeeded")
	}
	for _, name := range []string{"http", "command"} {
		cfg, err := r.Resolve(embed.Config{Provider: name}, env)
		if err != nil {
			t.Fatal(err)
		}
		if cfg.Model != "" || cfg.Dim != 0 || len(cfg.Options) != boolDefaults(name) {
			t.Errorf("%s: resolved %+v from KB_ variables", name, cfg)
		}
		if _, err := r.Open(cfg); err == nil {
			t.Errorf("%s: opened with no explicit settings, so it read KB_ variables", name)
		}
	}
	// The same lookup under KB_ does see them, so the probe is not vacuous.
	cfg, err := r.Resolve(embed.Config{}, embed.OSEnv("KB_"))
	if err != nil || cfg.Provider != "command" || cfg.Model != "kb-only-model" || cfg.Get("program") != "/kb/only" {
		t.Fatalf("KB_ prefix: %+v %v", cfg, err)
	}
}

// boolDefaults is how many options a provider fills from Option.Default with no environment.
func boolDefaults(name string) int {
	if name == "http" {
		return 1 // send_dimensions
	}
	return 0
}
