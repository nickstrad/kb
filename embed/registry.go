package embed

import (
	"errors"
	"fmt"
	"os"
	"sort"
	"strconv"
	"strings"
	"sync"
)

// Option declares one provider-specific setting, so a Registry can fill it from the environment
// and document it.
type Option struct {
	// Key is the name the provider reads with Config.Get, lowercase with underscores ("url").
	Key string
	// Env lists the environment variables consulted, in order; the first one that is set wins.
	// "{P}" is replaced by the registry's environment prefix (kb uses "KB_"). Empty means the
	// single variable "{P}EMBED_<KEY>", e.g. KB_EMBED_URL for key "url".
	Env []string
	// Default is used when no variable is set. Empty means "unset" (Config.Lookup reports false).
	Default string
	// KeepEmpty makes a variable that is set to "" count as a value (e.g. "no prefix") rather than
	// as unset.
	KeepEmpty bool
	// Doc is a one-line description for help text.
	Doc string
}

// EnvNames returns the expanded variable names for o under prefix.
func (o Option) EnvNames(prefix string) []string {
	names := o.Env
	if len(names) == 0 {
		names = []string{"{P}EMBED_" + strings.ToUpper(o.Key)}
	}
	out := make([]string, len(names))
	for i, n := range names {
		out[i] = strings.ReplaceAll(n, "{P}", prefix)
	}
	return out
}

// Config is a resolved provider selection: which provider, the model and vector length it should
// produce, and its provider-specific options. Model and Dim may be empty/zero, meaning the
// provider's own default.
type Config struct {
	Provider string
	Model    string
	Dim      int
	Options  map[string]string

	// LookupEnv reads a variable the configuration names explicitly (the http provider's
	// api_key_env). Nil means os.LookupEnv. Resolve never sets it: it is a seam for tests and for
	// programs that keep secrets somewhere other than the process environment.
	LookupEnv func(name string) (string, bool)
}

// Getenv returns the variable name as cfg.LookupEnv sees it, or "" when it is unset.
func (c Config) Getenv(name string) string {
	lookup := c.LookupEnv
	if lookup == nil {
		lookup = os.LookupEnv
	}
	v, _ := lookup(name)
	return v
}

// Lookup returns the option value and whether it is set.
func (c Config) Lookup(key string) (string, bool) {
	v, ok := c.Options[key]
	return v, ok
}

// Get returns the option value, or "" when unset.
func (c Config) Get(key string) string { return c.Options[key] }

// Bool parses a boolean option, returning def when it is unset or empty.
func (c Config) Bool(key string, def bool) (bool, error) {
	s := c.Options[key]
	if s == "" {
		return def, nil
	}
	b, err := strconv.ParseBool(s)
	if err != nil {
		return false, fmt.Errorf("embedder option %s=%q is not true or false", key, s)
	}
	return b, nil
}

// Int parses an integer option, returning def when it is unset or empty.
func (c Config) Int(key string, def int) (int, error) {
	s := c.Options[key]
	if s == "" {
		return def, nil
	}
	n, err := strconv.Atoi(s)
	if err != nil {
		return 0, fmt.Errorf("embedder option %s=%q is not an integer", key, s)
	}
	return n, nil
}

// Env reads environment variables under a prefix. Lookup defaults to os.LookupEnv; tests pass a
// map-backed function, and a program whose configuration must come only from its own files (kb)
// passes NoEnv.
type Env struct {
	Prefix string
	Lookup func(string) (string, bool)
}

// NoEnv returns an Env that sees no variables at all, so Resolve takes everything from the
// explicit Config and the option defaults.
func NoEnv() Env { return Env{Lookup: func(string) (string, bool) { return "", false }} }

// OSEnv returns an Env over the process environment.
func OSEnv(prefix string) Env { return Env{Prefix: prefix, Lookup: os.LookupEnv} }

// Get returns the first non-empty value among names, with "{P}" expanded to the prefix.
func (e Env) Get(names ...string) string {
	for _, n := range names {
		if v, ok := e.lookup(strings.ReplaceAll(n, "{P}", e.Prefix)); ok && v != "" {
			return v
		}
	}
	return ""
}

func (e Env) lookup(name string) (string, bool) {
	if e.Lookup == nil {
		return os.LookupEnv(name)
	}
	return e.Lookup(name)
}

// option resolves o from the environment.
func (e Env) option(o Option) (string, bool) {
	for _, n := range o.EnvNames(e.Prefix) {
		if v, ok := e.lookup(n); ok && (v != "" || o.KeepEmpty) {
			return v, true
		}
	}
	if o.Default != "" {
		return o.Default, true
	}
	return "", false
}

// Provider is one embedding backend a Registry can build.
type Provider struct {
	// Name is what configuration selects it by ("http"); stored lowercase.
	Name string
	// Summary is a one-line description for help text.
	Summary string
	// Options are the provider-specific settings it reads from Config.
	Options []Option
	// New builds the embedder. cfg.Options holds every declared option that is set (explicit
	// values, then environment, then Option.Default) plus any extra explicit options.
	New func(cfg Config) (Embedder, error)
}

// Registry holds the providers a program supports and picks one from configuration. The zero
// value is not usable; call NewRegistry. A Registry is safe for concurrent use.
type Registry struct {
	mu        sync.RWMutex
	order     []string
	providers map[string]Provider

	// Default is the provider used when none is named. Empty means a provider must be named.
	Default string
	// DefaultDim is the vector length used when neither the caller nor {P}EMBED_DIM sets one.
	// Zero leaves it to the provider.
	DefaultDim int
}

// NewRegistry returns an empty registry.
func NewRegistry() *Registry {
	return &Registry{providers: map[string]Provider{}}
}

// Register adds p. Registering an empty name, a nil New, or a name twice is an error.
func (r *Registry) Register(p Provider) error {
	p.Name = strings.ToLower(strings.TrimSpace(p.Name))
	if p.Name == "" {
		return fmt.Errorf("embed: invalid provider name %q", p.Name)
	}
	if p.New == nil {
		return fmt.Errorf("embed: provider %q has no New function", p.Name)
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	if _, dup := r.providers[p.Name]; dup {
		return fmt.Errorf("embed: provider %q is already registered", p.Name)
	}
	r.providers[p.Name] = p
	r.order = append(r.order, p.Name)
	return nil
}

// MustRegister is Register that panics on error, for registration at program start.
func (r *Registry) MustRegister(ps ...Provider) {
	for _, p := range ps {
		if err := r.Register(p); err != nil {
			panic(err)
		}
	}
}

// Provider returns the provider registered under name.
func (r *Registry) Provider(name string) (Provider, bool) {
	r.mu.RLock()
	defer r.mu.RUnlock()
	p, ok := r.providers[strings.ToLower(strings.TrimSpace(name))]
	return p, ok
}

// Names lists the registered providers in registration order.
func (r *Registry) Names() []string {
	r.mu.RLock()
	defer r.mu.RUnlock()
	return append([]string(nil), r.order...)
}

// ErrUnknownProvider is wrapped by Resolve and Open when the named provider is not registered.
var ErrUnknownProvider = errors.New("unknown embedder")

// Resolve turns explicit settings plus the environment into a Config. Explicit values (from
// flags or a config file) win over the environment:
//
//	provider  explicit.Provider, else {P}EMBEDDER, else r.Default
//	model     explicit.Model, else {P}EMBED_MODEL, else empty (the provider's default)
//	dim       explicit.Dim, else {P}EMBED_DIM, else r.DefaultDim
//	options   explicit.Options[key], else the Option's environment variables, else Option.Default
func (r *Registry) Resolve(explicit Config, env Env) (Config, error) {
	name := strings.ToLower(strings.TrimSpace(explicit.Provider))
	if name == "" {
		name = strings.ToLower(strings.TrimSpace(env.Get("{P}EMBEDDER")))
	}
	if name == "" {
		name = r.defaultName()
	}
	if name == "" {
		return Config{}, fmt.Errorf("%w: no embedder selected (want %s)", ErrUnknownProvider, r.want())
	}
	cfg := Config{Provider: name, Model: explicit.Model, Dim: explicit.Dim, Options: map[string]string{}, LookupEnv: explicit.LookupEnv}
	if cfg.Model == "" {
		cfg.Model = env.Get("{P}EMBED_MODEL")
	}
	if cfg.Dim == 0 {
		if s := env.Get("{P}EMBED_DIM"); s != "" {
			n, err := strconv.Atoi(s)
			if err != nil || n <= 0 {
				return Config{}, fmt.Errorf("%sEMBED_DIM=%q is not a positive integer", env.Prefix, s)
			}
			cfg.Dim = n
		} else {
			cfg.Dim = r.DefaultDim
		}
	}
	if cfg.Dim < 0 {
		return Config{}, fmt.Errorf("embedding dimension must be positive, got %d", cfg.Dim)
	}

	p, ok := r.Provider(name)
	if !ok {
		return Config{}, r.unknown(name)
	}
	for _, o := range p.Options {
		if v, ok := explicit.Options[o.Key]; ok {
			cfg.Options[o.Key] = v
		} else if v, ok := env.option(o); ok {
			cfg.Options[o.Key] = v
		}
	}
	for k, v := range explicit.Options {
		if _, ok := cfg.Options[k]; !ok {
			cfg.Options[k] = v
		}
	}
	return cfg, nil
}

// Open builds the embedder cfg names. cfg is used as given; call Resolve first to apply the
// environment and option defaults.
func (r *Registry) Open(cfg Config) (Embedder, error) {
	p, ok := r.Provider(cfg.Provider)
	if !ok {
		return nil, r.unknown(cfg.Provider)
	}
	if cfg.Options == nil {
		cfg.Options = map[string]string{}
	}
	return p.New(cfg)
}

// OpenEnv is Resolve followed by Open over the process environment with the given prefix. It
// returns the resolved Config too, so callers can report what is in effect.
func (r *Registry) OpenEnv(prefix string, explicit Config) (Embedder, Config, error) {
	cfg, err := r.Resolve(explicit, OSEnv(prefix))
	if err != nil {
		return nil, Config{}, err
	}
	e, err := r.Open(cfg)
	if err != nil {
		return nil, Config{}, err
	}
	return e, cfg, nil
}

func (r *Registry) defaultName() string {
	r.mu.RLock()
	defer r.mu.RUnlock()
	return strings.ToLower(strings.TrimSpace(r.Default))
}

// want lists the registered names for a message: "a, b or c".
func (r *Registry) want() string {
	names := r.Names()
	sort.Strings(names)
	want := strings.Join(names, ", ")
	if i := strings.LastIndex(want, ", "); i >= 0 {
		want = want[:i] + " or " + want[i+2:]
	}
	return want
}

func (r *Registry) unknown(name string) error {
	return fmt.Errorf("%w %q (want %s)", ErrUnknownProvider, name, r.want())
}

// NoneProvider is the "none" provider: no vectors, FTS-only search.
func NoneProvider() Provider {
	return Provider{
		Name:    NoneModel,
		Summary: "no vectors at all: the index is full-text only",
		New:     func(Config) (Embedder, error) { return None{}, nil },
	}
}
