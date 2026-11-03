package embed_test

import (
	"context"
	"errors"
	"reflect"
	"testing"

	"github.com/nickstrad/kb/embed"
)

// mapEnv is an embed.Env over a map, so tests never see the host environment.
func mapEnv(prefix string, vars map[string]string) embed.Env {
	return embed.Env{Prefix: prefix, Lookup: func(k string) (string, bool) { v, ok := vars[k]; return v, ok }}
}

// stub is a minimal provider-built embedder that records the Config it was built from.
type stub struct {
	name string
	cfg  embed.Config
}

func (s *stub) Embed(_ context.Context, texts []string) ([][]float32, error) {
	out := make([][]float32, len(texts))
	for i := range out {
		out[i] = make([]float32, s.cfg.Dim)
	}
	return out, nil
}
func (s *stub) Model() string { return s.name + ":" + s.cfg.Model }
func (s *stub) Dim() int      { return s.cfg.Dim }

func stubProvider(name string, opts ...embed.Option) embed.Provider {
	return embed.Provider{
		Name:    name,
		Options: opts,
		New:     func(cfg embed.Config) (embed.Embedder, error) { return &stub{name: name, cfg: cfg}, nil },
	}
}

// newRegistry is what "another project" would write: its own providers, its own env prefix.
func newRegistry(t *testing.T) *embed.Registry {
	t.Helper()
	r := embed.NewRegistry()
	r.Default = "local"
	r.DefaultDim = 4
	r.MustRegister(
		stubProvider("local", embed.Option{Key: "socket", Default: "/tmp/model.sock"}),
		embed.Provider{
			Name:    "cloud",
			Options: []embed.Option{{Key: "api_key", Env: []string{"{P}CLOUD_KEY", "CLOUD_KEY"}}},
			New: func(cfg embed.Config) (embed.Embedder, error) {
				if cfg.Get("api_key") == "" {
					return nil, errors.New("cloud needs a key")
				}
				return &stub{name: "cloud", cfg: cfg}, nil
			},
		},
		embed.NoneProvider(),
	)
	return r
}

func open(t *testing.T, r *embed.Registry, explicit embed.Config, env embed.Env) (embed.Embedder, embed.Config) {
	t.Helper()
	cfg, err := r.Resolve(explicit, env)
	if err != nil {
		t.Fatalf("Resolve: %v", err)
	}
	e, err := r.Open(cfg)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	return e, cfg
}

func TestRegisterRejectsBadProviders(t *testing.T) {
	r := embed.NewRegistry()
	if err := r.Register(stubProvider("a")); err != nil {
		t.Fatal(err)
	}
	for name, p := range map[string]embed.Provider{
		"duplicate":       stubProvider("A"),
		"empty name":      stubProvider(" "),
		"missing factory": {Name: "b"},
	} {
		if err := r.Register(p); err == nil {
			t.Errorf("%s: want an error", name)
		}
	}
	if got := r.Names(); !reflect.DeepEqual(got, []string{"a"}) {
		t.Errorf("Names = %v", got)
	}
}

func TestDefaultProviderWithNoConfiguration(t *testing.T) {
	e, cfg := open(t, newRegistry(t), embed.Config{}, mapEnv("APP_", nil))
	if cfg.Provider != "local" || e.Dim() != 4 || cfg.Get("socket") != "/tmp/model.sock" {
		t.Fatalf("got %+v", cfg)
	}
}

func TestNoDefaultMeansAProviderMustBeNamed(t *testing.T) {
	r := embed.NewRegistry()
	r.MustRegister(stubProvider("a"), stubProvider("b"))
	_, err := r.Resolve(embed.Config{}, mapEnv("APP_", nil))
	if !errors.Is(err, embed.ErrUnknownProvider) || err.Error() != "unknown embedder: no embedder selected (want a or b)" {
		t.Fatalf("got %v", err)
	}
	// Names are case- and space-insensitive.
	_, cfg := open(t, r, embed.Config{Provider: " B "}, mapEnv("APP_", nil))
	if cfg.Provider != "b" {
		t.Errorf("Provider = %q", cfg.Provider)
	}
}

// NoEnv sees nothing, however the process environment is set: a program whose configuration
// comes only from its own files (kb) resolves through it.
func TestNoEnvIgnoresTheProcessEnvironment(t *testing.T) {
	t.Setenv("APP_EMBEDDER", "none")
	t.Setenv("APP_EMBED_MODEL", "env-model")
	t.Setenv("APP_EMBED_SOCKET", "/env.sock")
	env := embed.NoEnv()
	env.Prefix = "APP_"
	_, cfg := open(t, newRegistry(t), embed.Config{}, env)
	if cfg.Provider != "local" || cfg.Model != "" || cfg.Get("socket") != "/tmp/model.sock" {
		t.Fatalf("got %+v", cfg)
	}
}

func TestGetenvUsesTheInjectedLookup(t *testing.T) {
	t.Setenv("SECRET_FROM_OS", "os")
	c := embed.Config{LookupEnv: func(k string) (string, bool) { return "map:" + k, true }}
	if got := c.Getenv("K"); got != "map:K" {
		t.Errorf("injected: %q", got)
	}
	if got := (embed.Config{}).Getenv("SECRET_FROM_OS"); got != "os" {
		t.Errorf("default lookup: %q", got)
	}
	// Resolve carries the lookup through to the provider.
	cfg, err := newRegistry(t).Resolve(embed.Config{LookupEnv: c.LookupEnv}, mapEnv("APP_", nil))
	if err != nil || cfg.Getenv("K") != "map:K" {
		t.Errorf("Resolve dropped LookupEnv: %v", err)
	}
}

func TestExplicitBeatsEnvironmentBeatsDefaults(t *testing.T) {
	r := newRegistry(t)
	env := mapEnv("APP_", map[string]string{
		"APP_EMBEDDER": "none", "APP_EMBED_MODEL": "env-model", "APP_EMBED_DIM": "8",
		"APP_EMBED_SOCKET": "/env.sock",
	})
	_, cfg := open(t, r, embed.Config{Provider: "local"}, env)
	if cfg.Model != "env-model" || cfg.Dim != 8 || cfg.Get("socket") != "/env.sock" {
		t.Fatalf("env layer: %+v", cfg)
	}
	e, cfg := open(t, r, embed.Config{
		Provider: "local", Model: "flag-model", Dim: 16,
		Options: map[string]string{"socket": "/flag.sock", "extra": "x"},
	}, env)
	if cfg.Model != "flag-model" || e.Dim() != 16 || cfg.Get("socket") != "/flag.sock" || cfg.Get("extra") != "x" {
		t.Fatalf("explicit layer: %+v", cfg)
	}
	if e.Model() != "local:flag-model" {
		t.Errorf("Model = %q", e.Model())
	}
}

func TestOptionEnvironmentRules(t *testing.T) {
	r := embed.NewRegistry()
	r.Default = "p"
	r.MustRegister(stubProvider("p",
		embed.Option{Key: "url"}, // {P}EMBED_URL
		embed.Option{Key: "prefix", Default: "doc: ", KeepEmpty: true},
		embed.Option{Key: "host", Env: []string{"{P}HOST", "HOST"}, Default: "localhost"},
	))
	_, cfg := open(t, r, embed.Config{}, mapEnv("X_", map[string]string{
		"X_EMBED_URL": "http://u", "X_EMBED_PREFIX": "", "X_HOST": "", "HOST": "fallback",
	}))
	if cfg.Get("url") != "http://u" {
		t.Errorf("url = %q", cfg.Get("url"))
	}
	if v, ok := cfg.Lookup("prefix"); !ok || v != "" {
		t.Errorf("set-but-empty KeepEmpty option: %q %v", v, ok)
	}
	if cfg.Get("host") != "fallback" {
		t.Errorf("empty var should fall through to the next: %q", cfg.Get("host"))
	}
	_, cfg = open(t, r, embed.Config{}, mapEnv("X_", nil))
	if _, ok := cfg.Lookup("url"); ok || cfg.Get("prefix") != "doc: " || cfg.Get("host") != "localhost" {
		t.Errorf("defaults: %+v", cfg.Options)
	}
}

func TestSwitchingProvidersByConfiguration(t *testing.T) {
	r := newRegistry(t)
	seen := map[string]string{}
	for _, name := range []string{"local", "cloud", "none"} {
		e, cfg := open(t, r, embed.Config{}, mapEnv("APP_", map[string]string{"APP_EMBEDDER": name, "APP_CLOUD_KEY": "k"}))
		seen[cfg.Provider] = e.Model()
	}
	want := map[string]string{"local": "local:", "cloud": "cloud:", "none": embed.NoneModel}
	if !reflect.DeepEqual(seen, want) {
		t.Fatalf("got %v, want %v", seen, want)
	}
}

func TestResolveErrors(t *testing.T) {
	r := newRegistry(t)
	_, err := r.Resolve(embed.Config{}, mapEnv("APP_", map[string]string{"APP_EMBEDDER": "word2vec"}))
	if !errors.Is(err, embed.ErrUnknownProvider) || err.Error() != `unknown embedder "word2vec" (want cloud, local or none)` {
		t.Errorf("unknown: %v", err)
	}
	if _, err := r.Resolve(embed.Config{}, mapEnv("APP_", map[string]string{"APP_EMBED_DIM": "wide"})); err == nil {
		t.Error("bad dim: want an error")
	}
	if _, err := r.Open(embed.Config{Provider: "nope"}); !errors.Is(err, embed.ErrUnknownProvider) {
		t.Errorf("Open unknown: %v", err)
	}
	cfg, err := r.Resolve(embed.Config{Provider: "cloud"}, mapEnv("APP_", nil))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := r.Open(cfg); err == nil {
		t.Error("provider validation error should surface from Open")
	}
}

func TestConfigParsers(t *testing.T) {
	c := embed.Config{Options: map[string]string{"b": "true", "n": "7", "bad": "x"}}
	if b, err := c.Bool("b", false); err != nil || !b {
		t.Error("Bool")
	}
	if b, err := c.Bool("missing", true); err != nil || !b {
		t.Error("Bool default")
	}
	if n, err := c.Int("n", 0); err != nil || n != 7 {
		t.Error("Int")
	}
	if _, err := c.Bool("bad", false); err == nil {
		t.Error("Bool bad")
	}
	if _, err := c.Int("bad", 0); err == nil {
		t.Error("Int bad")
	}
}
