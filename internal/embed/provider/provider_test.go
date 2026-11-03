package provider

import (
	"context"
	"errors"
	"fmt"
	"reflect"
	"testing"

	pub "github.com/nickstrad/kb/embed"
	"github.com/nickstrad/kb/internal/config"
	"github.com/nickstrad/kb/internal/embed/local"
	"github.com/nickstrad/kb/internal/errs"
)

func cacheDir(t *testing.T) func() (string, error) {
	dir := t.TempDir()
	return func() (string, error) { return dir, nil }
}

func registry(t *testing.T) *pub.Registry {
	t.Helper()
	r, err := NewRegistry(cacheDir(t))
	if err != nil {
		t.Fatal(err)
	}
	return r
}

func TestNewRegistryDefaultsToLocal(t *testing.T) {
	r := registry(t)
	if got, want := r.Names(), []string{"local", "http", "command", "none"}; !reflect.DeepEqual(got, want) {
		t.Fatalf("Names = %v, want %v", got, want)
	}
	if r.Default != local.ProviderName {
		t.Fatalf("Default = %q", r.Default)
	}
}

// Default resolution is v2's embedder: the default config builds local.New(DefaultModel) on the
// model cache directory, nothing else, and loads or downloads nothing.
func TestDefaultResolutionIsV2LocalEmbedder(t *testing.T) {
	dirFn := cacheDir(t)
	r, err := NewRegistry(dirFn)
	if err != nil {
		t.Fatal(err)
	}
	e, err := Open(r, config.Default().Embed)
	if err != nil {
		t.Fatal(err)
	}
	le, ok := e.(*local.Embedder)
	if !ok {
		t.Fatalf("default built %T, want *local.Embedder", e)
	}
	dir, _ := dirFn()
	spec, _ := local.Lookup(local.DefaultModel)
	want := local.New(spec, dir)
	if le.Model() != want.Model() || le.Dim() != want.Dim() || le.ModelDir() != want.ModelDir() || le.Cached() {
		t.Fatalf("got %s/%d in %s, want %s/%d in %s", le.Model(), le.Dim(), le.ModelDir(), want.Model(), want.Dim(), want.ModelDir())
	}
	d, q := le.Prefixes()
	wd, wq := want.Prefixes()
	if d != wd || q != wq {
		t.Errorf("prefixes differ from v2's")
	}
}

func TestExplicitMapsTheConfigTables(t *testing.T) {
	cases := []struct {
		name string
		e    config.Embed
		want pub.Config
	}{
		{"default", config.Default().Embed, pub.Config{Provider: "local", Model: local.DefaultModel, Options: map[string]string{}}},
		{"http minimal", config.Embed{Provider: "HTTP", Model: "m", Dim: 8}, pub.Config{Provider: "http", Model: "m", Dim: 8, Options: map[string]string{}}},
		{"http full", config.Embed{Provider: "http", Model: "m", Dim: 8, HTTP: &config.EmbedHTTP{
			URL: "u", Path: "/p", APIKeyEnv: "K", AuthHeader: "api-key", AuthScheme: "none",
			NoDimensions: true, Truncate: true, DocPrefix: "d", QueryPrefix: "q", TimeoutSeconds: 9,
		}}, pub.Config{Provider: "http", Model: "m", Dim: 8, Options: map[string]string{
			"url": "u", "path": "/p", "api_key_env": "K", "auth_header": "api-key", "auth_scheme": "none",
			"send_dimensions": "false", "truncate": "true", "doc_prefix": "d", "query_prefix": "q", "timeout": "9",
		}}},
		{"command", config.Embed{Provider: "command", Model: "m", Dim: 8, Command: &config.EmbedCommand{
			Program: "/p", Args: []string{"-a", "-b"}, TimeoutSeconds: 3, QueryPrefix: "q",
		}}, pub.Config{Provider: "command", Model: "m", Dim: 8, Options: map[string]string{
			"program": "/p", "args": "-a -b", "timeout": "3", "query_prefix": "q",
		}}},
		{"local ignores provider tables", config.Embed{Model: "m", HTTP: &config.EmbedHTTP{URL: "u"}},
			pub.Config{Provider: "local", Model: "m", Options: map[string]string{}}},
	}
	for _, tc := range cases {
		if got := Explicit(tc.e); !reflect.DeepEqual(got.Options, tc.want.Options) || got.Provider != tc.want.Provider ||
			got.Model != tc.want.Model || got.Dim != tc.want.Dim {
			t.Errorf("%s: got %+v, want %+v", tc.name, got, tc.want)
		}
	}
}

// stubEmbedder fails with err and optionally pings.
type stubEmbedder struct{ err error }

func (s stubEmbedder) Embed(context.Context, []string) ([][]float32, error) { return nil, s.err }
func (stubEmbedder) Model() string                                          { return "stub" }
func (stubEmbedder) Dim() int                                               { return 4 }
func (s stubEmbedder) Ping(context.Context) error                           { return s.err }

func TestKindedMapsErrorsToKBKinds(t *testing.T) {
	cancelled, cancel := context.WithCancel(context.Background())
	cancel()
	cases := []struct {
		name string
		ctx  context.Context
		err  error
		kind errs.Kind
		raw  bool // returned as is (the caller's own cancellation)
	}{
		{"unavailable", context.Background(), fmt.Errorf("down: %w", pub.ErrUnavailable), errs.Unavailable, false},
		{"config mistake", context.Background(), errors.New("401 bad key"), errs.Usage, false},
		{"caller cancelled", cancelled, fmt.Errorf("x: %w", context.Canceled), errs.Internal, true},
	}
	for _, tc := range cases {
		k := Kinded(stubEmbedder{err: tc.err}, "stub")
		for what, call := range map[string]func() error{
			"embed": func() error { _, err := k.Embed(tc.ctx, []string{"x"}); return err },
			"ping":  func() error { return k.(pub.Pinger).Ping(tc.ctx) },
		} {
			err := call()
			if !errors.Is(err, tc.err) {
				t.Errorf("%s %s: lost the cause: %v", tc.name, what, err)
			}
			if tc.raw && err != tc.err {
				t.Errorf("%s %s: wrapped the caller's cancellation: %v", tc.name, what, err)
			}
			if !tc.raw && errs.KindOf(err) != tc.kind {
				t.Errorf("%s %s: kind %v, want %v", tc.name, what, errs.KindOf(err), tc.kind)
			}
		}
	}
	ok := Kinded(stubEmbedder{}, "stub")
	if d, q := ok.(pub.Prefixer).Prefixes(); d != "" || q != "" || ok.Model() != "stub" || ok.Dim() != 4 {
		t.Error("Kinded must pass identity through and add no prefixes")
	}
	if err := Kinded(pub.None{}, "x").(pub.Pinger).Ping(context.Background()); err != nil {
		t.Errorf("an embedder with no Ping passes: %v", err)
	}
}

// A kb build can ship its own provider: Register makes it selectable by embed.provider.
func TestExtraProviderRegisteredInKB(t *testing.T) {
	saved := extra
	t.Cleanup(func() { extra = saved })
	Register(pub.Provider{
		Name: "corp",
		New: func(cfg pub.Config) (pub.Embedder, error) {
			return stubEmbedder{}, nil
		},
	})
	e, err := Open(registry(t), config.Embed{Provider: "corp", Model: "m", Dim: 4})
	if err != nil {
		t.Fatal(err)
	}
	if e.Model() != "stub" {
		t.Fatalf("got %s", e.Model())
	}
	Register(pub.Provider{Name: "http", New: func(pub.Config) (pub.Embedder, error) { return nil, nil }})
	if _, err := NewRegistry(cacheDir(t)); err == nil {
		t.Error("a provider shadowing a built-in must be refused")
	}
}

func TestOpenErrorsAreUsage(t *testing.T) {
	r := registry(t)
	for name, e := range map[string]config.Embed{
		"unknown provider":    {Provider: "nope", Model: "m", Dim: 4},
		"unknown local model": {Model: "acme/x"},
		"http needs url":      {Provider: "http", Model: "m", Dim: 4},
		"bad key variable":    {Provider: "http", Model: "m", Dim: 4, HTTP: &config.EmbedHTTP{URL: "u", APIKeyEnv: "KB_PROVIDER_TEST_UNSET_VAR"}},
	} {
		if _, err := Open(r, e); errs.KindOf(err) != errs.Usage {
			t.Errorf("%s: %v (kind %v)", name, err, errs.KindOf(err))
		}
	}
	failing := errs.New(errs.Usage, "test", "no cache dir")
	r2, _ := NewRegistry(func() (string, error) { return "", failing })
	if _, err := Open(r2, config.Default().Embed); !errors.Is(err, failing) {
		t.Errorf("cache dir failure: %v", err)
	}
}
