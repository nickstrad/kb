// Package provider is kb's glue to the public embedding registry (github.com/nickstrad/kb/embed):
// the providers kb ships, how the [embed] config table maps onto a registry selection, and how a
// registry-built embedder's errors map onto kb's error kinds.
//
// kb's registry holds "local" (the in-process model, the Default, built by internal/embed/local),
// the public built-ins "http" and "command", and "none". Selection comes only from the config
// file: kb resolves with embed.NoEnv, so no KB_* environment variable selects or configures a
// provider, and a config that names no provider resolves to "local" exactly as kb did before the
// registry existed. The only variable kb ever reads for a provider is one the config names
// explicitly ([embed.http] api_key_env).
package provider

import (
	"context"
	"errors"
	"strconv"
	"strings"

	pub "github.com/nickstrad/kb/embed"
	"github.com/nickstrad/kb/embed/builtin"
	"github.com/nickstrad/kb/internal/config"
	"github.com/nickstrad/kb/internal/embed/local"
	"github.com/nickstrad/kb/internal/errs"
)

// EnvPrefix is the prefix kb's registry would read variables under. kb resolves with NoEnv, so it
// only names the prefix in messages and keeps it from colliding with another program's.
const EnvPrefix = "KB_"

// extra holds providers a kb build adds from an init function (Register).
var extra []pub.Provider

// Register adds a provider to every registry NewRegistry builds from now on. Call it from an init
// function in a kb build that ships its own provider; select it with embed.provider and pass its
// options as the provider documents.
func Register(p pub.Provider) { extra = append(extra, p) }

// NewRegistry returns kb's registry: local (the Default) first, then the public built-ins, then
// anything added with Register. cacheDir is the shared model directory, asked for only when the
// local provider is built.
func NewRegistry(cacheDir func() (string, error)) (*pub.Registry, error) {
	r := pub.NewRegistry()
	if err := r.Register(local.Provider(cacheDir)); err != nil {
		return nil, err
	}
	if err := builtin.Register(r); err != nil {
		return nil, err
	}
	for _, p := range extra {
		if err := r.Register(p); err != nil {
			return nil, err
		}
	}
	r.Default = local.ProviderName
	return r, nil
}

// Explicit maps the [embed] table onto the registry's explicit settings. The local provider gets
// the model and nothing else, as before; http and command get their table as options.
func Explicit(e config.Embed) pub.Config {
	cfg := pub.Config{Provider: e.ProviderName(), Model: e.Model, Dim: e.Dim, Options: map[string]string{}}
	set := func(k, v string) {
		if v != "" {
			cfg.Options[k] = v
		}
	}
	switch cfg.Provider {
	case config.ProviderHTTP:
		h := e.HTTP
		if h == nil {
			h = &config.EmbedHTTP{}
		}
		set("url", h.URL)
		set("path", h.Path)
		set("api_key_env", h.APIKeyEnv)
		set("auth_header", h.AuthHeader)
		set("auth_scheme", h.AuthScheme)
		if h.NoDimensions {
			set("send_dimensions", "false")
		}
		if h.Truncate {
			set("truncate", "true")
		}
		set("doc_prefix", h.DocPrefix)
		set("query_prefix", h.QueryPrefix)
		if h.TimeoutSeconds > 0 {
			set("timeout", strconv.Itoa(h.TimeoutSeconds))
		}
	case config.ProviderCommand:
		c := e.Command
		if c == nil {
			c = &config.EmbedCommand{}
		}
		set("program", c.Program)
		set("args", strings.Join(c.Args, " "))
		if c.TimeoutSeconds > 0 {
			set("timeout", strconv.Itoa(c.TimeoutSeconds))
		}
		set("doc_prefix", c.DocPrefix)
		set("query_prefix", c.QueryPrefix)
	}
	return cfg
}

// Open builds the embedder the [embed] table selects from r. Configuration mistakes (an unknown
// provider or model, a missing URL or program) are Usage errors naming the table. An embedder
// from a provider other than local is wrapped so its failures carry kb's error kinds: the local
// embedder already reports them itself.
func Open(r *pub.Registry, e config.Embed) (pub.Embedder, error) {
	const op = "provider.Open"
	if need := e.Needs(); need != "" {
		return nil, errs.New(errs.Usage, op, "%s", need)
	}
	cfg, err := r.Resolve(Explicit(e), pub.NoEnv())
	if err != nil {
		return nil, errs.Wrap(err, errs.Usage, op, "embed.provider: %v", err)
	}
	emb, err := r.Open(cfg)
	if err != nil {
		if errs.KindOf(err) != errs.Internal {
			return nil, err // already kinded (the model cache directory)
		}
		if cfg.Provider == local.ProviderName {
			return nil, errs.Wrap(err, errs.Usage, op, "%v", err)
		}
		return nil, errs.Wrap(err, errs.Usage, op, "embed.provider %s: %v", cfg.Provider, err)
	}
	if cfg.Provider == local.ProviderName || pub.IsNone(emb) {
		return emb, nil
	}
	return Kinded(emb, cfg.Provider), nil
}

// Kinded wraps a registry-built embedder so its errors carry kb's kinds: a failure wrapping
// embed.ErrUnavailable is errs.Unavailable (hybrid search falls back to FTS, exit 2), the
// caller's own cancellation stays as it is, and anything else, a configuration or protocol
// mistake that falling back would hide, is errs.Usage. Prefixes and Ping pass through.
func Kinded(e pub.Embedder, name string) pub.Embedder { return kinded{inner: e, name: name} }

type kinded struct {
	inner pub.Embedder
	name  string
}

func (k kinded) Model() string { return k.inner.Model() }
func (k kinded) Dim() int      { return k.inner.Dim() }

func (k kinded) Prefixes() (doc, query string) {
	if p, ok := k.inner.(pub.Prefixer); ok {
		return p.Prefixes()
	}
	return "", ""
}

func (k kinded) Embed(ctx context.Context, texts []string) ([][]float32, error) {
	vecs, err := k.inner.Embed(ctx, texts)
	if err != nil {
		return nil, k.kind(ctx, "embed", err)
	}
	return vecs, nil
}

// Ping checks the provider when it can (http and command embed one short text); a provider with
// no check passes.
func (k kinded) Ping(ctx context.Context) error {
	p, ok := k.inner.(pub.Pinger)
	if !ok {
		return nil
	}
	if err := p.Ping(ctx); err != nil {
		return k.kind(ctx, "ping", err)
	}
	return nil
}

func (k kinded) kind(ctx context.Context, what string, err error) error {
	op := "provider." + k.name
	switch {
	case ctx.Err() != nil && (errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded)):
		return err
	case errors.Is(err, pub.ErrUnavailable):
		return errs.Wrap(err, errs.Unavailable, op, "%s: %v", what, err)
	default:
		return errs.Wrap(err, errs.Usage, op, "%s: %v", what, err)
	}
}
