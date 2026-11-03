package builtin

import (
	"reflect"
	"testing"

	"github.com/nickstrad/kb/embed"
)

func TestNewRegistryMatchesKB(t *testing.T) {
	r := NewRegistry()
	want := []string{"ollama", "openrouter", "openai", "http", "command", "none"}
	if got := r.Names(); !reflect.DeepEqual(got, want) {
		t.Fatalf("Names = %v, want %v", got, want)
	}
	if r.Default != "ollama" || r.DefaultDim != 768 {
		t.Fatalf("Default %q DefaultDim %d", r.Default, r.DefaultDim)
	}
	cfg, err := r.Resolve(embed.Config{}, embed.Env{Prefix: "KB_", Lookup: func(string) (string, bool) { return "", false }})
	if err != nil || cfg.Provider != "ollama" || cfg.Dim != 768 {
		t.Fatalf("empty environment: %+v %v", cfg, err)
	}
}

func TestRegisterKeepsAnExistingDefault(t *testing.T) {
	r := embed.NewRegistry()
	r.Default = "none"
	if err := Register(r); err != nil || r.Default != "none" {
		t.Fatalf("%v %q", err, r.Default)
	}
	if err := Register(r); err == nil {
		t.Fatal("registering twice should fail")
	}
}
